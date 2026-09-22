package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
	"fiatjaf.com/nostr/keyer"
	"fiatjaf.com/nostr/nip44"
	"fiatjaf.com/nostr/nip46"
	"github.com/gezibash/arc/delivery/draft"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/node"
	"github.com/gezibash/arc/delivery/transport/relay"
	"github.com/gezibash/arc/iface"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// signerIdentity is who this citizen is on this machine: a secret key in
// the key file, or a remote signer of NIP-46 that the key file names.
type signerIdentity struct {
	key    keys.Key
	keyer  nostr.Keyer
	signer keys.Signer
	remote bool
}

// loadIdentity reads the key file. It holds a secret key as hex or nsec, an
// ncryptsec of NIP-49 that a passphrase opens, or a bunker URI of NIP-46.
func loadIdentity(ctx context.Context, dir string) (signerIdentity, error) {
	text, err := keys.Read(keyPath(dir))
	if err != nil {
		return signerIdentity{}, err
	}
	switch {
	case strings.HasPrefix(text, "bunker://"):
		return remoteIdentity(ctx, dir, text)
	case strings.HasPrefix(text, "ncryptsec1"):
		passphrase, err := askPassphrase("passphrase of the key: ")
		if err != nil {
			return signerIdentity{}, err
		}
		k, err := keys.Decrypt(text, passphrase)
		if err != nil {
			return signerIdentity{}, err
		}
		return signerIdentity{key: k, keyer: keyer.NewPlainKeySigner(k.Secret), signer: k}, nil
	}
	k, err := keys.Parse(text)
	if err != nil {
		return signerIdentity{}, err
	}
	return signerIdentity{key: k, keyer: keyer.NewPlainKeySigner(k.Secret), signer: k}, nil
}

// remoteIdentity connects to a NIP-46 signer. This machine keeps a key of its
// own for the connection, in <home>/bunker-client; it signs nothing else.
func remoteIdentity(ctx context.Context, dir, uri string) (signerIdentity, error) {
	path := filepath.Join(dir, "bunker-client")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := keys.Save(path, keys.Generate()); err != nil {
			return signerIdentity{}, err
		}
	}
	client, err := keys.Load(path)
	if err != nil {
		return signerIdentity{}, err
	}
	// The client listens for answers for as long as its context lives, so
	// the context is the command's own. A timer bounds the first contact.
	type result struct {
		signer nostr.Keyer
		pk     nostr.PubKey
		err    error
	}
	done := make(chan result, 1)
	go func() {
		signer, err := keyer.New(ctx, nil, uri, &keyer.SignerOptions{BunkerClientSecretKey: client.Secret, BunkerSignTimeout: 15 * time.Second})
		if err != nil {
			done <- result{err: fmt.Errorf("the remote signer did not answer: %w", err)}
			return
		}
		pk, err := signer.GetPublicKey(ctx)
		if err != nil {
			err = fmt.Errorf("the remote signer did not give its key: %w", err)
		}
		done <- result{signer, pk, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return signerIdentity{}, r.err
		}
		remote := timed{r.signer}
		return signerIdentity{key: keys.Key{Public: r.pk}, keyer: remote, signer: remoteSigner{r.pk, remote}, remote: true}, nil
	case <-time.After(15 * time.Second):
		return signerIdentity{}, errors.New("the remote signer did not answer in 15 seconds")
	}
}

// askPassphrase reads ARCN_PASSPHRASE, or asks on the terminal without echo.
func askPassphrase(prompt string) (string, error) {
	if value := os.Getenv("ARCN_PASSPHRASE"); value != "" {
		return value, nil
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("the key needs its passphrase: set ARCN_PASSPHRASE, or run arcn on a terminal")
	}
	defer tty.Close()
	fmt.Fprint(tty, prompt)
	value, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(tty)
	if err != nil {
		return "", err
	}
	return string(value), nil
}

// Each identity of this machine has its own directory, <root>/citizens/<name>,
// because the store, the relays and the installs belong to one key. The file
// <root>/default names the identity that commands use when --key does not.

func citizensDir(root string) string { return filepath.Join(root, "citizens") }
func defaultPath(root string) string { return filepath.Join(root, "default") }
func publicPath(dir string) string   { return filepath.Join(dir, "public") }

// chosen returns the directory of the identity that the command uses, and
// what chose it: --key, ARCN_KEY, or the default.
func chosen(command *cobra.Command) (string, string, error) {
	root, err := rootDir(command)
	if err != nil {
		return "", "", err
	}
	name, source := "", ""
	if flag, _ := command.Flags().GetString("key"); flag != "" {
		name, source = flag, "--key"
	} else if env := os.Getenv("ARCN_KEY"); env != "" {
		name, source = env, "ARCN_KEY"
	} else if body, err := os.ReadFile(defaultPath(root)); err == nil {
		name, source = strings.TrimSpace(string(body)), "the default"
	}
	if name == "" {
		return "", "", errors.New("no identity: make one with arcn keys gen, or add one with arcn keys add")
	}
	dir := filepath.Join(citizensDir(root), name)
	if !validName(name) {
		return "", "", fmt.Errorf("no identity %q on this machine: see arcn keys list", name)
	}
	if _, err := os.Stat(dir); err != nil {
		return "", "", fmt.Errorf("no identity %q on this machine: see arcn keys list", name)
	}
	return dir, source, nil
}

// validName refuses a name that would leave the directory of identities.
func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\`) && !strings.HasPrefix(name, ".")
}

// localID is one identity of this machine.
type localID struct {
	name   string
	public string
}

// listCitizens reads the identities of this machine, by name.
func listCitizens(root string) ([]localID, error) {
	entries, err := os.ReadDir(citizensDir(root))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []localID
	for _, entry := range entries {
		if !entry.IsDir() || !validName(entry.Name()) {
			continue
		}
		public, _ := os.ReadFile(publicPath(filepath.Join(citizensDir(root), entry.Name())))
		out = append(out, localID{name: entry.Name(), public: strings.TrimSpace(string(public))})
	}
	return out, nil
}

func defaultName(root string) string {
	body, _ := os.ReadFile(defaultPath(root))
	return strings.TrimSpace(string(body))
}

// addCitizen keeps a key file as a new identity. It reads the key in a
// directory of its own first, so that a key that does not open, or a signer
// that does not answer, leaves nothing behind. The first identity becomes the
// default. A caller that made the key passes it as known, so a sealed key does
// not ask for its passphrase again.
func addCitizen(ctx context.Context, root, text string, known *keys.Key) (keys.Key, error) {
	if err := os.MkdirAll(citizensDir(root), 0o700); err != nil {
		return keys.Key{}, err
	}
	staging, err := os.MkdirTemp(citizensDir(root), ".adding-")
	if err != nil {
		return keys.Key{}, err
	}
	defer os.RemoveAll(staging)

	if err := keys.Write(keyPath(staging), text); err != nil {
		return keys.Key{}, err
	}
	var k keys.Key
	if known != nil {
		k = *known
	} else {
		id, err := loadIdentity(ctx, staging)
		if err != nil {
			return keys.Key{}, err
		}
		k = id.key
	}
	if err := os.WriteFile(publicPath(staging), []byte(k.Public.Hex()+"\n"), 0o600); err != nil {
		return keys.Key{}, err
	}
	name := k.Name()
	if err := os.Rename(staging, filepath.Join(citizensDir(root), name)); err != nil {
		if _, statErr := os.Stat(filepath.Join(citizensDir(root), name)); statErr == nil {
			return keys.Key{}, fmt.Errorf("%s is already on this machine", name)
		}
		return keys.Key{}, err
	}
	if defaultName(root) == "" {
		if err := os.WriteFile(defaultPath(root), []byte(name+"\n"), 0o600); err != nil {
			return keys.Key{}, err
		}
	}
	return k, nil
}

func keysCommand() *cobra.Command {
	command := &cobra.Command{Use: "keys", Short: "Make, add, list and pick identities"}

	gen := &cobra.Command{
		Use: "gen", Short: "Make an identity, and make it the default when there is none", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			root, err := rootDir(command)
			if err != nil {
				return err
			}
			k := keys.Generate()
			text := k.Secret.Hex()
			if encrypt, _ := command.Flags().GetBool("encrypt"); encrypt {
				if text, err = newPassphrase(k); err != nil {
					return err
				}
			}
			if _, err := addCitizen(command.Context(), root, text, &k); err != nil {
				return err
			}
			fmt.Printf("%s\n%s\n", k.Name(), k.Public.Hex())
			return nil
		},
	}
	gen.Flags().Bool("encrypt", false, "seal the key with a passphrase, as NIP-49 defines")

	add := &cobra.Command{
		Use:   "add [bunker://...]",
		Short: "Add an identity that exists already",
		Long: "arcn reads the key from standard input: 64 characters of hex, an nsec,\n" +
			"or an ncryptsec of NIP-49. With a bunker:// URI, the identity signs\n" +
			"through a remote signer, as NIP-46 defines, and this machine holds no\n" +
			"secret key. An agent uses this: its owner runs arcn keys bunker.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			root, err := rootDir(command)
			if err != nil {
				return err
			}
			var text string
			if len(args) == 1 {
				if !nip46.IsValidBunkerURL(args[0]) {
					return errors.New("that is not a bunker:// URI; give a secret key on standard input")
				}
				text = args[0]
			} else {
				body, err := io.ReadAll(io.LimitReader(command.InOrStdin(), 4096))
				if err != nil {
					return err
				}
				text = strings.TrimSpace(string(body))
				if text == "" {
					return errors.New("give the key on standard input, or a bunker:// URI")
				}
			}
			k, err := addCitizen(command.Context(), root, text, nil)
			if err != nil {
				return err
			}
			fmt.Printf("%s\n%s\n", k.Name(), k.Public.Hex())
			return nil
		},
	}

	list := &cobra.Command{
		Use: "list", Short: "List the identities of this machine", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			root, err := rootDir(command)
			if err != nil {
				return err
			}
			all, err := listCitizens(root)
			if err != nil {
				return err
			}
			if len(all) == 0 {
				fmt.Println("no identities: make one with arcn keys gen")
				return nil
			}
			active := defaultName(root)
			for _, c := range all {
				mark := " "
				if c.name == active {
					mark = "*"
				}
				fmt.Printf("%s %s  %s\n", mark, c.name, c.public)
			}
			return nil
		},
	}

	use := &cobra.Command{
		Use: "use <name>", Short: "Make one identity the default", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			root, err := rootDir(command)
			if err != nil {
				return err
			}
			if _, err := os.Stat(filepath.Join(citizensDir(root), args[0])); err != nil || !validName(args[0]) {
				return fmt.Errorf("no identity %q on this machine: see arcn keys list", args[0])
			}
			if err := os.WriteFile(defaultPath(root), []byte(args[0]+"\n"), 0o600); err != nil {
				return err
			}
			fmt.Printf("the default is %s\n", args[0])
			return nil
		},
	}

	remove := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove one identity from this machine",
		Long: "This removes the key, and the store, the relays and the installs of\n" +
			"the identity. A key that no other machine holds is lost for good.",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			root, err := rootDir(command)
			if err != nil {
				return err
			}
			name := args[0]
			dir := filepath.Join(citizensDir(root), name)
			if _, err := os.Stat(dir); err != nil || !validName(name) {
				return fmt.Errorf("no identity %q on this machine: see arcn keys list", name)
			}
			if yes, _ := command.Flags().GetBool("yes"); !yes {
				fmt.Printf("Remove %s, its key and its store? [y/N] ", name)
				answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if strings.ToLower(strings.TrimSpace(answer)) != "y" {
					return errors.New("nothing was removed")
				}
			}
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
			if defaultName(root) == name {
				if err := os.Remove(defaultPath(root)); err != nil {
					return err
				}
			}
			fmt.Printf("removed %s\n", name)
			return nil
		},
	}
	remove.Flags().Bool("yes", false, "remove without asking")

	encrypt := &cobra.Command{
		Use: "encrypt", Short: "Seal the key with a passphrase, as NIP-49 defines", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			dir, err := home(command)
			if err != nil {
				return err
			}
			k, err := keys.Load(keyPath(dir))
			if err != nil {
				return err
			}
			sealed, err := newPassphrase(k)
			if err != nil {
				return err
			}
			if err := keys.Write(keyPath(dir), sealed); err != nil {
				return err
			}
			fmt.Println("the key is sealed; arcn asks for the passphrase, or reads ARCN_PASSPHRASE")
			return nil
		},
	}

	command.AddCommand(gen, add, list, use, remove, encrypt, bunkerCommand())
	return command
}

func whoamiCommand() *cobra.Command {
	return &cobra.Command{
		Use: "whoami", Short: "Show the identity that commands use here", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			dir, source, err := chosen(command)
			if err != nil {
				return err
			}
			public, err := os.ReadFile(publicPath(dir))
			if err != nil {
				return err
			}
			fmt.Printf("%s\n%s\nchosen by %s\n", filepath.Base(dir), strings.TrimSpace(string(public)), source)
			return nil
		},
	}
}

// newPassphrase asks for a passphrase twice, and seals the key with it.
func newPassphrase(k keys.Key) (string, error) {
	first, err := askPassphrase("new passphrase: ")
	if err != nil {
		return "", err
	}
	if os.Getenv("ARCN_PASSPHRASE") == "" {
		again, err := askPassphrase("the passphrase again: ")
		if err != nil {
			return "", err
		}
		if again != first {
			return "", errors.New("the two passphrases differ")
		}
	}
	return keys.Encrypt(k, first)
}

func bunkerCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "bunker",
		Short: "Sign for others through a relay, as NIP-46 defines",
		Long: "arcn prints a bunker:// URI. A machine that uses it, such as an agent,\n" +
			"signs with this citizen's key and never holds it. With --allow-kind,\n" +
			"the bunker signs only those kinds, and authentication for relays. By\n" +
			"default it decrypts only what the owner sealed to themselves, which\n" +
			"the journal and the files need; mail needs --decrypt all.",
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
	rootRelay := relay.Relay{URL: url, Signer: sess.keyer}
	publishKeyedRoot(command.Context(), sess)
	for _, wrap := range sess.node.Store.Query(rootFilter(sess.key.Public)) {
		if err := rootRelay.Send(command.Context(), wrap); err != nil {
			fmt.Fprintf(os.Stderr, "the keyed root did not reach %s: %v\n", url, err)
		}
	}
	sess.close()

	signer := nip46.NewStaticKeySigner(id.key.Secret)
	authorized := map[nostr.PubKey]bool{}
	signer.AuthorizeRequest = func(_ bool, from nostr.PubKey, given string) bool {
		if given == secret {
			authorized[from] = true
		}
		return authorized[from]
	}

	ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	r := relay.Relay{URL: url}
	pk := id.key.Public

	fmt.Printf("bunker://%s?relay=%s&secret=%s\n", pk.Hex(), url, secret)
	for {
		requests, err := r.Watch(ctx, nostr.Filter{Kinds: []nostr.Kind{nostr.KindNostrConnect}, Tags: nostr.TagMap{"p": {pk.Hex()}}, Since: nostr.Now()})
		if err == nil {
			for request := range requests {
				response, ok := answer(ctx, &signer, id.key.Secret, request, policy{kinds: allowed, decrypt: decrypt}, log)
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
}

// refusal says why the policy refuses a request, or nothing when it allows
// it. Authentication for relays is always allowed.
func (p policy) refusal(req nip46.Request, owner nostr.PubKey) error {
	switch req.Method {
	case "sign_event":
		if len(p.kinds) == 0 || len(req.Params) != 1 {
			return nil
		}
		var event nostr.Event
		if err := event.UnmarshalJSON([]byte(req.Params[0])); err != nil {
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
		if reason := p.refusal(parsed, secret.Public()); reason != nil {
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

// timed bounds each request to a remote signer, so a lost answer fails the
// command instead of stopping it.
type timed struct{ nostr.Keyer }

const signerTimeout = 20 * time.Second

func (t timed) SignEvent(ctx context.Context, event *nostr.Event) error {
	ctx, cancel := context.WithTimeout(ctx, signerTimeout)
	defer cancel()
	return t.Keyer.SignEvent(ctx, event)
}

func (t timed) Encrypt(ctx context.Context, plaintext string, to nostr.PubKey) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, signerTimeout)
	defer cancel()
	return t.Keyer.Encrypt(ctx, plaintext, to)
}

func (t timed) Decrypt(ctx context.Context, ciphertext string, from nostr.PubKey) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, signerTimeout)
	defer cancel()
	return t.Keyer.Decrypt(ctx, ciphertext, from)
}

func (t timed) GetPublicKey(ctx context.Context) (nostr.PubKey, error) {
	ctx, cancel := context.WithTimeout(ctx, signerTimeout)
	defer cancel()
	return t.Keyer.GetPublicKey(ctx)
}

// remoteSigner lets the mail layer and calls seal and sign through NIP-46.
type remoteSigner struct {
	pk    nostr.PubKey
	keyer nostr.Keyer
}

func (r remoteSigner) PublicKey() nostr.PubKey { return r.pk }

func (r remoteSigner) Sign(event *nostr.Event) error {
	return r.keyer.SignEvent(context.Background(), event)
}

func (r remoteSigner) Encrypt(plaintext string, to nostr.PubKey) (string, error) {
	return r.keyer.Encrypt(context.Background(), plaintext, to)
}

func (r remoteSigner) Decrypt(ciphertext string, from nostr.PubKey) (string, error) {
	return r.keyer.Decrypt(context.Background(), ciphertext, from)
}

// rootFilter matches the draft that holds a citizen's keyed root.
func rootFilter(me nostr.PubKey) nostr.Filter {
	return nostr.Filter{Kinds: []nostr.Kind{draft.Kind}, Authors: []nostr.PubKey{me}, Tags: nostr.TagMap{"d": {iface.KeyedRootD}}}
}

// publishKeyedRoot seals the keyed root to this citizen's own key, once, so
// that a machine that signs through NIP-46 can read it. The root follows
// from the secret key, so every machine with the key makes the same one.
func publishKeyedRoot(ctx context.Context, sess *session) {
	if len(sess.node.Store.Query(rootFilter(sess.key.Public))) > 0 {
		return
	}
	root, err := iface.KeyedRoot(sess.key.Secret)
	if err != nil {
		return
	}
	inner := nostr.Event{Kind: iface.KeyedRootKind, CreatedAt: nostr.Now(), Content: hex.EncodeToString(root),
		Tags: nostr.Tags{{"d", iface.KeyedRootD}}}
	wrap, err := draft.Wrap(ctx, sess.keyer, iface.KeyedRootD, inner, nostr.Now())
	if err != nil {
		return
	}
	sess.node.Publish(ctx, wrap, sess.relays)
}

// keyedRoot is the root of this citizen's keyed values. A machine with the
// key derives it. A machine that signs through NIP-46 opens the draft that
// holds it, through the signer.
func keyedRoot(ctx context.Context, sess *session) ([]byte, error) {
	if !sess.remote {
		return iface.KeyedRoot(sess.key.Secret)
	}
	filter := rootFilter(sess.key.Public)
	reports, errs := sess.node.Pull(ctx, filter, sess.relays)
	wraps := sess.node.Store.Query(filter)
	if unreached := node.Unreached(reports, errs); len(wraps) == 0 && unreached != nil {
		return nil, fmt.Errorf("this machine holds no keyed root, and %w", unreached)
	}
	if len(wraps) == 0 {
		return nil, errors.New("no keyed root yet: run any arcn command on a machine that holds the key, then sync")
	}
	opened, err := draft.Open(ctx, sess.keyer, wraps[0])
	if err != nil {
		return nil, fmt.Errorf("the keyed root does not open: %w", err)
	}
	root, err := hex.DecodeString(opened.Event.Content)
	if err != nil || len(root) != 32 || opened.Event.Kind != iface.KeyedRootKind {
		return nil, errors.New("the keyed root draft holds no root")
	}
	return root, nil
}
