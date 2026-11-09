package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip44"
	"fiatjaf.com/nostr/nip46"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/gezibash/arc/sdk/storage"
	"github.com/gezibash/arc/sdk/terminal"
	"github.com/spf13/cobra"
)

func bunkerCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "bunker",
		Short: "Sign for others through a relay, as NIP-46 defines",
		Long: "arc prints a bunker:// URI. A machine that uses it, such as an agent,\n" +
			"signs with this citizen's key and never holds it. With --allow-kind,\n" +
			"the bunker signs only those kinds, and authentication for relays. By\n" +
			"default it decrypts only what the owner sealed to themselves, which\n" +
			"the journal and the files need; mail needs --decrypt all. When a\n" +
			"device list exists, the bunker admits only its devices: see\n" +
			"arc keys devices.",
		Args: cobra.NoArgs,
		RunE: serveBunker,
	}
	command.Flags().String("relay", "", "the relay that carries the requests")
	command.Flags().StringArray("allow-kind", nil, "a kind that the bunker signs; with none, it signs every kind")
	command.Flags().String("decrypt", "self", "what the bunker decrypts: none, self (drafts and the keyed root), or all (needed for mail)")
	return command
}

func serveBunker(command *cobra.Command, _ []string) error {
	url, _ := command.Flags().GetString("relay")
	if url == "" {
		return errors.New("name the relay with --relay")
	}
	// A bad URL fails now, and not at each try to reach the relay.
	if err := checkRelayURL(url); err != nil {
		return fmt.Errorf("--relay: %w", err)
	}
	decrypt, _ := command.Flags().GetString("decrypt")
	if !slices.Contains([]string{"none", "self", "all"}, decrypt) {
		return fmt.Errorf("--decrypt is none, self or all, not %q", decrypt)
	}
	var allowed []nostr.Kind
	texts, _ := command.Flags().GetStringArray("allow-kind")
	for _, text := range texts {
		n, err := strconv.Atoi(text)
		if err != nil {
			return fmt.Errorf("--allow-kind %q is not a kind", text)
		}
		allowed = append(allowed, nostr.Kind(n))
	}

	dir, err := home(command)
	if err != nil {
		return err
	}
	id, err := loadIdentity(command.Context(), dir)
	if err != nil {
		return err
	}
	if id.remote {
		return errRemote("a bunker")
	}
	secret, err := bunkerSecret(dir)
	if err != nil {
		return err
	}

	// A machine that signs through this bunker reads the keyed root from
	// the bunker's relay.
	sess, err := open(command)
	if err != nil {
		return err
	}
	rootRelay := relay.Relay{URL: url, Signer: sess.Signer}
	if !slices.Contains(sess.URLs, url) {
		fmt.Fprintln(os.Stderr, terminal.EscapeLine(fmt.Sprintf("%s is not a relay of this citizen: a new device list reaches the bunker only after a restart; add it with arc relay add", url)))
	}
	g := &guard{secret: secret, authorized: map[nostr.PubKey]bool{}}
	if err := startDevices(command.Context(), sess, rootRelay, g); err != nil {
		sess.Close()
		return err
	}
	if err := sess.PublishKeyedRoot(command.Context()); err != nil {
		sess.Close()
		return err
	}
	wraps, err := sess.Node.Store.Query(citizen.RootFilter(sess.Key.Public))
	if err != nil {
		sess.Close()
		return err
	}
	for _, wrap := range wraps {
		if err := rootRelay.Send(command.Context(), wrap); err != nil {
			fmt.Fprintln(os.Stderr, terminal.EscapeLine(fmt.Sprintf("the keyed root did not reach %s: %v", url, err)))
		}
	}
	sess.Close()

	signer := nip46.NewStaticKeySigner(id.key.Secret)
	signer.AuthorizeRequest = func(_ bool, from nostr.PubKey, given string) bool {
		return g.authorize(from, given)
	}

	ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	r := relay.Relay{URL: url}
	pk := id.key.Public

	// With a device list, the secret admits nobody, so the URI leaves it out.
	if g.listed() {
		fmt.Fprintf(command.OutOrStdout(), "bunker://%s?relay=%s\n", pk.Hex(), terminal.EscapeLine(url))
	} else {
		fmt.Fprintf(command.OutOrStdout(), "bunker://%s?relay=%s&secret=%s\n", pk.Hex(), terminal.EscapeLine(url), secret)
	}
	go watchDevices(ctx, relay.Relay{URL: url, Signer: id.key}, id.key, g, log)
	for {
		requests, err := r.Watch(ctx, nostr.Filter{Kinds: []nostr.Kind{nostr.KindNostrConnect}, Tags: nostr.TagMap{"p": {pk.Hex()}}, Since: nostr.Now()})
		if err == nil {
			for request := range requests {
				response, ok := answer(ctx, &signer, id.key.Secret, request, policy{kinds: allowed, decrypt: decrypt, admit: g.refusal}, log)
				if ok {
					if err := r.Send(ctx, response); err != nil {
						log.Warn("the answer did not reach the relay", "error", err)
					}
				}
			}
		}
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "the bunker is stopping")
			return nil
		}
		log.Warn("the relay ended the watch; watching again", "relay", url, "error", err)
		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			return nil
		}
	}
}

// policy is what a bunker does for the machines that use it.
type policy struct {
	// kinds are the kinds that it signs; none means every kind.
	kinds []nostr.Kind
	// decrypt is none, self or all. Self opens only what the owner sealed
	// to their own key: drafts, the keyed root, the private relay list.
	decrypt string
	// admit says why the bunker refuses a device, or nothing. Nil admits
	// each device.
	admit func(nostr.PubKey) error
}

// refusal says why the policy refuses a request of the device key from,
// or nothing when it allows it. Authentication for relays is always
// allowed, and so is a token for the device that asks, and for no other key.
func (p policy) refusal(req nip46.Request, owner, from nostr.PubKey) error {
	switch req.Method {
	case "sign_event":
		if len(req.Params) != 1 {
			return nil
		}
		var event nostr.Event
		if err := event.UnmarshalJSON([]byte(req.Params[0])); err != nil {
			return nil
		}
		// A device must not change the list of devices that the signer admits.
		if event.Kind == devices.Kind {
			return errors.New("the bunker never signs a device list; run arc keys devices on the signer")
		}
		// A device must not change which devices hold the data roots.
		if event.Kind == devices.RootsKind {
			return errors.New("the bunker never signs the data roots; run arc keys devices on the signer")
		}
		// A device must not change which storage citizens hold the data.
		if event.Kind == storage.ListKind {
			return errors.New("the bunker never signs a storage list; run arc keys storage on the signer")
		}
		// A token lets one device key read the sealed data on a relay.
		if event.Kind == devices.TokenKind {
			if _, err := devices.TokenEnd(event, from, nostr.Now()); err != nil {
				return fmt.Errorf("the bunker signs a token only for the device that asks, for at most 7 days: %w", err)
			}
			return nil
		}
		if len(p.kinds) == 0 {
			return nil
		}
		if event.Kind != nostr.KindClientAuthentication && !slices.Contains(p.kinds, event.Kind) {
			return fmt.Errorf("the bunker does not sign kind %d", event.Kind)
		}
	case "nip44_decrypt", "nip04_decrypt":
		switch {
		case p.decrypt == "none":
			return errors.New("the bunker does not decrypt")
		case p.decrypt == "self" && (len(req.Params) == 0 || req.Params[0] != owner.Hex()):
			return errors.New("the bunker decrypts only what its owner sealed to themselves; mail needs --decrypt all")
		}
	}
	return nil
}

// answer handles one request. A request that the policy refuses gets a
// refusal, and the signer never sees it.
func answer(ctx context.Context, signer *nip46.StaticKeySigner, secret nostr.SecretKey, request nostr.Event, p policy, log *slog.Logger) (nostr.Event, bool) {
	conversation, err := nip44.GenerateConversationKey(request.PubKey, secret)
	if err != nil {
		return nostr.Event{}, false
	}
	session := nip46.Session{PublicKey: secret.Public(), ConversationKey: conversation}
	if parsed, err := session.ParseRequest(request); err == nil {
		reason := p.refusal(parsed, secret.Public(), request.PubKey)
		if p.admit != nil {
			if refused := p.admit(request.PubKey); refused != nil {
				reason = refused
			}
		}
		if reason != nil {
			log.Info("refused a request", "method", parsed.Method, "reason", reason, "from", request.PubKey.Hex()[:8])
			_, refusal, err := session.MakeResponse(parsed.ID, request.PubKey, "", reason)
			if err != nil || refusal.Sign(secret) != nil {
				return nostr.Event{}, false
			}
			return refusal, true
		}
	}
	_, _, response, err := signer.HandleRequest(ctx, request)
	if err != nil {
		log.Debug("a request failed", "error", err)
		return nostr.Event{}, false
	}
	return response, true
}

// bunkerSecret is the secret of the bunker URI. It stays the same, so a
// machine that uses the URI keeps working after the bunker restarts.
func bunkerSecret(dir string) (string, error) {
	path := filepath.Join(dir, "bunker-secret")
	if body, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(body)), nil
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
		return "", err
	}
	return secret, nil
}
