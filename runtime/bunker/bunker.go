// Package bunker signs for the devices of a citizen through a relay, as
// NIP-46 defines, with the key that the signer of the citizen holds.
package bunker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip46"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/gezibash/arc/sdk/storage"
	"github.com/gezibash/arc/sdk/terminal"
)

// Prepare loads the device list into the guard, and puts the keyed
// root on the relay of the bunker, before the bunker answers anyone.
func Prepare(ctx context.Context, sess *citizen.Session, url string, g *Guard, errs io.Writer) error {
	rootRelay := relay.Relay{URL: url, Signer: sess.Signer}
	if !slices.Contains(sess.URLs, url) {
		fmt.Fprintln(errs, terminal.EscapeLine(fmt.Sprintf("%s is not a relay of this citizen: a new device list reaches the bunker only after a restart; add it with arc relay add", url)))
	}
	if err := startDevices(ctx, sess, rootRelay, g, errs); err != nil {
		return err
	}
	if err := sess.PublishKeyedRoot(ctx); err != nil {
		return err
	}
	wraps, err := sess.Node.Store.Query(citizen.RootFilter(sess.Key.Public))
	if err != nil {
		return err
	}
	for _, wrap := range wraps {
		if err := rootRelay.Send(ctx, wrap); err != nil {
			fmt.Fprintln(errs, terminal.EscapeLine(fmt.Sprintf("the keyed root did not reach %s: %v", url, err)))
		}
	}
	return nil
}

// Watch answers the requests that reach the relay of the bunker,
// and watches again when the relay ends the watch, until ctx ends.
func Watch(ctx context.Context, url string, pk nostr.PubKey, remote keys.Remote, p Policy, authorize func(nostr.PubKey, string) bool, log *slog.Logger, errs io.Writer) error {
	r := relay.Relay{URL: url}
	for {
		requests, err := r.Watch(ctx, nostr.Filter{Kinds: []nostr.Kind{nostr.KindNostrConnect}, Tags: nostr.TagMap{"p": {pk.Hex()}}, Since: nostr.Now()})
		if err == nil {
			for request := range requests {
				response, ok := Answer(ctx, remote, request, p, authorize, log)
				if ok {
					if err := r.Send(ctx, response); err != nil {
						log.Warn("the answer did not reach the relay", "error", err)
					}
				}
			}
		}
		if ctx.Err() != nil {
			fmt.Fprintln(errs, "the bunker is stopping")
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

// Policy is what a bunker does for the machines that use it.
type Policy struct {
	// Kinds are the Kinds that it signs; none means every kind.
	Kinds []nostr.Kind
	// Decrypt is none, self or all. Self opens only what the owner sealed
	// to their own key: drafts, the keyed root, the private relay list.
	Decrypt string
	// Admit says why the bunker refuses a device, or nothing. Nil admits
	// each device.
	Admit func(nostr.PubKey) error
}

// refusal says why the policy refuses a request of the device key from,
// or nothing when it allows it. Authentication for relays is always
// allowed, and so is a token for the device that asks, and for no other key.
func (p Policy) refusal(req nip46.Request, owner, from nostr.PubKey) error {
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
		if len(p.Kinds) == 0 {
			return nil
		}
		if event.Kind != nostr.KindClientAuthentication && !slices.Contains(p.Kinds, event.Kind) {
			return fmt.Errorf("the bunker does not sign kind %d", event.Kind)
		}
	case "nip44_decrypt", "nip04_decrypt":
		switch {
		case p.Decrypt == "none":
			return errors.New("the bunker does not decrypt")
		case p.Decrypt == "self" && (len(req.Params) == 0 || req.Params[0] != owner.Hex()):
			return errors.New("the bunker decrypts only what its owner sealed to themselves; mail needs --decrypt all")
		}
	}
	return nil
}

// Answer handles one request. The request and its Answer travel sealed
// with NIP-44 between the device and the citizen key, so the program signer
// opens, seals and signs them. A request that the policy refuses gets a
// refusal, and the signer answers nothing for it. A holder request never
// passes: it would give a device the powers of the owner.
func Answer(ctx context.Context, signer keys.Remote, request nostr.Event, p Policy, authorize func(from nostr.PubKey, given string) bool, log *slog.Logger) (nostr.Event, bool) {
	plain, err := signer.Decrypt(ctx, request.Content, request.PubKey)
	if err != nil {
		log.Debug("a request does not open", "error", err)
		return nostr.Event{}, false
	}
	var req nip46.Request
	if err := json.Unmarshal([]byte(plain), &req); err != nil {
		return nostr.Event{}, false
	}
	reason := p.refusal(req, signer.Public, request.PubKey)
	if p.Admit != nil {
		if refused := p.Admit(request.PubKey); refused != nil {
			reason = refused
		}
	}
	if keys.HolderMethod(req.Method) {
		reason = errors.New("a bunker never answers a holder request over a relay; run the command on the signer")
	}
	given := ""
	if req.Method == "connect" && len(req.Params) >= 2 {
		given = req.Params[1]
	}
	if reason == nil && !authorize(request.PubKey, given) {
		reason = errors.New("unauthorized")
	}
	response := keys.Response{ID: req.ID}
	switch {
	case reason != nil:
		log.Info("refused a request", "method", req.Method, "reason", reason, "from", request.PubKey.Hex()[:8])
		response.Error = reason.Error()
	case req.Method == "connect":
		response.Result = "ack"
	case req.Method == "switch_relays":
		// NIP-46: null keeps the relays that the device uses now. The
		// bunker watches one relay.
		response.Result = "null"
	default:
		result, err := signer.Call(ctx, req.Method, req.Params...)
		if err != nil {
			response.Error = err.Error()
		} else {
			response.Result = result
		}
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return nostr.Event{}, false
	}
	sealed, err := signer.Encrypt(ctx, string(encoded), request.PubKey)
	if err != nil {
		log.Debug("an answer does not seal", "error", err)
		return nostr.Event{}, false
	}
	event := nostr.Event{Kind: nostr.KindNostrConnect, CreatedAt: nostr.Now(), Content: sealed, Tags: nostr.Tags{{"p", request.PubKey.Hex()}}}
	if err := signer.SignEvent(ctx, &event); err != nil {
		log.Debug("an answer is not signed", "error", err)
		return nostr.Event{}, false
	}
	return event, true
}

// Secret is the secret of the bunker URI. It stays the same, so a
// machine that uses the URI keeps working after the bunker restarts.
func Secret(dir string) (string, error) {
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
