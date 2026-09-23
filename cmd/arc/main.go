// Command arc is the command line tool of ARC, on Nostr events. See
// docs/delivery/SPEC.md.
//
//	arc keys gen | add | list | use | remove | encrypt | bunker
//	arc whoami
//	arc relay add <url> | rm <url> | ls | serve
//	arc message send | inbox | outbox
//	arc serve | announce | discover | install | call
//	arc sync [--dir <path>]
//	arc tool list | info | remove
//	arc info | resolve | apps init | version
//	arc lists add | rm | ls
//	arc update [check | apply]
//	arc release sign
//	arc <capability> <command...>
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/keyer"
	"fiatjaf.com/nostr/khatru"
	"github.com/gezibash/arc/delivery/draft"
	"github.com/gezibash/arc/delivery/groups"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/limits"
	"github.com/gezibash/arc/delivery/mail"
	"github.com/gezibash/arc/delivery/node"
	"github.com/gezibash/arc/delivery/sealed"
	"github.com/gezibash/arc/delivery/store"
	"github.com/gezibash/arc/delivery/transport"
	"github.com/gezibash/arc/delivery/transport/file"
	"github.com/gezibash/arc/delivery/transport/relay"
	"github.com/gezibash/arc/iface"
	"github.com/gezibash/arc/wake"
	"github.com/spf13/cobra"
)

func main() {
	if err := root().Execute(); err != nil {
		// A capability set the exit status from its reply, and wrote its
		// output already.
		var status iface.ExitError
		if errors.As(err, &status) {
			os.Exit(status.Code)
		}
		fmt.Fprintln(os.Stderr, "arc:", err)
		os.Exit(1)
	}
}

func root() *cobra.Command {
	command := &cobra.Command{
		Use:   "arc",
		Short: "ARC on signed events, over any transport",
		Long: "ARC on signed events, over any transport.\n\n" +
			"An installed capability adds its own commands: arc <name> <command>.\n" +
			"arc help <name> lists them.",
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		RunE:               dispatch,
		SilenceUsage:       true,
		SilenceErrors:      true,
	}
	command.PersistentFlags().String("home", "", "the directory of arc (ARC_HOME, default ~/.config/arc)")
	command.PersistentFlags().String("key", "", "the identity to use, by petname (ARC_KEY)")
	command.AddCommand(keysCommand(), whoamiCommand(), relayCommand(), messageCommand(),
		serveCmd(), announceCmd(), discoverCmd(), installCmd(), callCmd(), syncCommand(),
		toolCommand(), infoCommand(), resolveCommand(), appsCommand(), versionCommand(), listsCommand(), updateCommand(), releaseCommand())
	command.SetHelpCommand(helpCommand(command))
	command.Version = version
	return command
}

// rootDir is the directory of arc. It holds one directory for each identity,
// and the files of a relay that this machine runs.
func rootDir(command *cobra.Command) (string, error) {
	if dir, _ := command.Flags().GetString("home"); dir != "" {
		return dir, nil
	}
	if dir := os.Getenv("ARC_HOME"); dir != "" {
		return dir, nil
	}
	user, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(user, ".config", "arc"), nil
}

// home is the directory of the identity that the command uses. It holds the
// key, the relay list, the installs and the store.
func home(command *cobra.Command) (string, error) {
	dir, _, err := chosen(command)
	return dir, err
}

func keyPath(dir string) string      { return filepath.Join(dir, "key") }
func relaysPath(dir string) string   { return filepath.Join(dir, "relays") }
func indexersPath(dir string) string { return filepath.Join(dir, "indexers") }

func readRelays(dir string) ([]string, error) { return readURLs(relaysPath(dir)) }

func writeRelays(dir string, urls []string) error { return writeURLs(dir, relaysPath(dir), urls) }

// An indexer relay holds only relay lists. A citizen publishes its NIP-65
// and NIP-17 lists there, and looks up the lists of others there.
func readIndexers(dir string) ([]string, error) { return readURLs(indexersPath(dir)) }

func writeIndexers(dir string, urls []string) error {
	return writeURLs(dir, indexersPath(dir), urls)
}

func readURLs(path string) ([]string, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

func writeURLs(dir, path string, urls []string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	body := strings.Join(urls, "\n")
	if body != "" {
		body += "\n"
	}
	return os.WriteFile(path, []byte(body), 0o600)
}

func relayCommand() *cobra.Command {
	command := &cobra.Command{Use: "relay", Short: "Manage the relays of this citizen, or run one"}

	add := &cobra.Command{
		Use: "add <url>", Short: "Send to and sync with a relay", Args: cobra.ExactArgs(1),
		Long: "With --index, the relay is an indexer: arc publishes only its relay\n" +
			"lists there, and looks up the relay lists of other citizens there.",
		RunE: func(command *cobra.Command, args []string) error {
			url := args[0]
			if !strings.HasPrefix(url, "ws://") && !strings.HasPrefix(url, "wss://") {
				return errors.New("a relay is a ws:// or wss:// URL")
			}
			dir, err := home(command)
			if err != nil {
				return err
			}
			relays, err := readRelays(dir)
			if err != nil {
				return err
			}
			indexers, err := readIndexers(dir)
			if err != nil {
				return err
			}
			index, _ := command.Flags().GetBool("index")
			if index {
				if slices.Contains(relays, url) {
					return fmt.Errorf("%s is a relay of this citizen; remove it with arc relay rm first", url)
				}
				if !slices.Contains(indexers, url) {
					indexers = append(indexers, url)
				}
				if err := writeIndexers(dir, indexers); err != nil {
					return err
				}
			} else {
				if slices.Contains(indexers, url) {
					return fmt.Errorf("%s is an indexer of this citizen; remove it with arc relay rm first", url)
				}
				if !slices.Contains(relays, url) {
					relays = append(relays, url)
				}
				if err := writeRelays(dir, relays); err != nil {
					return err
				}
			}
			announceRelays(command)
			return nil
		},
	}
	add.Flags().Bool("index", false, "use the relay only to publish and look up relay lists")

	command.AddCommand(
		add,
		&cobra.Command{
			Use: "rm <url>", Short: "Stop using a relay", Args: cobra.ExactArgs(1),
			RunE: func(command *cobra.Command, args []string) error {
				dir, err := home(command)
				if err != nil {
					return err
				}
				relays, err := readRelays(dir)
				if err != nil {
					return err
				}
				indexers, err := readIndexers(dir)
				if err != nil {
					return err
				}
				other := func(u string) bool { return u == args[0] }
				if err := writeRelays(dir, slices.DeleteFunc(relays, other)); err != nil {
					return err
				}
				if err := writeIndexers(dir, slices.DeleteFunc(indexers, other)); err != nil {
					return err
				}
				announceRelays(command)
				return nil
			},
		},
		&cobra.Command{
			Use: "ls", Short: "List the relays", Args: cobra.NoArgs,
			RunE: func(command *cobra.Command, _ []string) error {
				dir, err := home(command)
				if err != nil {
					return err
				}
				urls, err := readRelays(dir)
				if err != nil {
					return err
				}
				indexers, err := readIndexers(dir)
				if err != nil {
					return err
				}
				if len(urls) == 0 {
					fmt.Println("no relays: add one with arc relay add <url>")
				}
				for _, url := range urls {
					fmt.Println(url)
				}
				for _, url := range indexers {
					fmt.Println(url + "  (index)")
				}
				return nil
			},
		},
		serveCommand(),
	)
	return command
}

// announceRelays publishes the relay list of this citizen, when it has a key
// and at least one relay.
func announceRelays(command *cobra.Command) {
	sess, err := open(command)
	if err != nil {
		return
	}
	defer sess.close()
	if len(sess.relays) > 0 {
		publishRelayList(command.Context(), sess)
	}
}

func serveCommand() *cobra.Command {
	command := &cobra.Command{
		Use: "serve", Short: "Run a relay on this machine", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			listen, _ := command.Flags().GetString("listen")
			path, _ := command.Flags().GetString("db")
			if path == "" {
				dir, err := rootDir(command)
				if err != nil {
					return err
				}
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return err
				}
				path = filepath.Join(dir, "relay.db")
			}

			db := &boltdb.BoltBackend{Path: path}
			if err := db.Init(); err != nil {
				return err
			}
			defer db.Close()

			rl := khatru.NewRelay()
			rl.Log = log.New(io.Discard, "", 0)
			rl.UseEventstore(db, 500)
			rl.Negentropy = true
			rl.Info.SupportedNIPs = append(rl.Info.SupportedNIPs, 77)
			sealed.Protect(rl)
			limits.Apply(rl, db.DB, writePolicy(command))

			if ids, _ := command.Flags().GetStringArray("group"); len(ids) > 0 {
				if err := hostGroups(command, rl, db, ids); err != nil {
					return err
				}
			}

			listener, err := net.Listen("tcp", listen)
			if err != nil {
				return err
			}
			fmt.Printf("relay listens on ws://%s\n", listener.Addr())

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			server := &http.Server{Handler: rl}
			go func() { <-ctx.Done(); server.Close() }()
			if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
	command.Flags().String("listen", "127.0.0.1:7447", "the address to listen on")
	command.Flags().String("db", "", "the file that holds the events (default <home>/relay.db)")
	command.Flags().StringArray("group", nil, "host an open NIP-29 group with this id")
	command.Flags().StringArray("admin", nil, "a public key that administers the groups")
	command.Flags().Int("max-event-bytes", 0, "refuse an event larger than this, as JSON (0: no cap)")
	command.Flags().Bool("wrap-auth", false, "take a gift wrap only after NIP-42 authentication, or with proof of work")
	command.Flags().Int("wrap-pow", 0, "the NIP-13 difficulty that lets a gift wrap in without authentication (0: none)")
	command.Flags().Int("rate", 0, "the events that one IP address can write each minute (0: no limit)")
	command.Flags().Int("burst", 0, "the events that one IP address can write at once (default the rate)")
	command.Flags().String("ip-header", "", "the HTTP header that holds the client IP address, when a proxy sets it")
	command.Flags().Int64("max-store-mb", 0, "refuse new stored events when the store uses this many MiB (0: no cap)")
	return command
}

// writePolicy reads the write limits of a relay from its flags.
func writePolicy(command *cobra.Command) limits.Policy {
	flags := command.Flags()
	var p limits.Policy
	p.MaxEventBytes, _ = flags.GetInt("max-event-bytes")
	p.WrapAuth, _ = flags.GetBool("wrap-auth")
	p.WrapPoW, _ = flags.GetInt("wrap-pow")
	p.Rate, _ = flags.GetInt("rate")
	p.Burst, _ = flags.GetInt("burst")
	p.IPHeader, _ = flags.GetString("ip-header")
	mb, _ := flags.GetInt64("max-store-mb")
	p.MaxStoreBytes = mb << 20
	return p
}

// hostGroups makes the relay host NIP-29 groups. The relay signs the state
// of each group with its own key, which it keeps in <home>/relay.key.
func hostGroups(command *cobra.Command, rl *khatru.Relay, db *boltdb.BoltBackend, ids []string) error {
	dir, err := rootDir(command)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "relay.key")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := keys.Save(path, keys.Generate()); err != nil {
			return err
		}
	}
	key, err := keys.Load(path)
	if err != nil {
		return err
	}

	var admins []nostr.PubKey
	texts, _ := command.Flags().GetStringArray("admin")
	for _, text := range texts {
		pk, err := nostr.PubKeyFromHex(text)
		if err != nil {
			return fmt.Errorf("--admin %q is not a public key", text)
		}
		admins = append(admins, pk)
	}

	hosted, err := groups.Attach(rl, db, key.Secret)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := hosted.Create(id, id, false, admins); err != nil {
			return err
		}
	}
	fmt.Printf("relay hosts groups %s, signed by %s\n", strings.Join(ids, ", "), key.Public.Hex())
	return nil
}

// session is one open store, with the key and the relays of this citizen.
type session struct {
	key    keys.Key
	node   *node.Node
	mail   *mail.Mail
	relays []transport.Transport
	urls   []string
	// indexers hold only relay lists. A one-time key answers their NIP-42
	// challenge, so an indexer does not learn who looks up a list.
	indexers []transport.Transport
	keyer    nostr.Keyer
	// remote says that a NIP-46 signer holds the secret key. The key then
	// holds the public key alone.
	remote bool
	// signer signs and seals for the mail layer and for calls.
	signer keys.Signer
	// waker runs the wake hook of a citizen before a live call to it.
	waker *wake.Waker
}

func open(command *cobra.Command) (*session, error) {
	dir, err := home(command)
	if err != nil {
		return nil, err
	}
	id, err := loadIdentity(command.Context(), dir)
	if err != nil {
		return nil, err
	}
	k := id.key
	s, err := store.Open(filepath.Join(dir, "store"))
	if err != nil {
		return nil, err
	}
	urls, err := readRelays(dir)
	if err != nil {
		s.Close()
		return nil, err
	}

	indexers, err := readIndexers(dir)
	if err != nil {
		s.Close()
		return nil, err
	}

	sess := &session{key: k, node: &node.Node{Store: s}, urls: urls, keyer: id.keyer, remote: id.remote, signer: id.signer}
	for _, url := range urls {
		sess.relays = append(sess.relays, relay.Relay{URL: url, Signer: sess.keyer})
	}
	once := keyer.NewPlainKeySigner(nostr.Generate())
	for _, url := range indexers {
		sess.indexers = append(sess.indexers, relay.Relay{URL: url, Signer: once})
	}

	sess.mail, err = mail.Open(filepath.Join(dir, "store"), sess.signer, sess.node, sess.relays)
	if err != nil {
		s.Close()
		return nil, err
	}
	sess.mail.Indexers = sess.indexers

	// The wake hooks belong to the machine, not to one identity.
	root, err := rootDir(command)
	if err != nil {
		sess.close()
		return nil, err
	}
	sess.waker = wake.Load(filepath.Join(root, wake.FileName), filepath.Join(root, wake.StateDirName))
	return sess, nil
}

func errRemote(what string) error {
	return fmt.Errorf("%s needs the secret key on this machine; a remote signer cannot do it yet", what)
}

func (s *session) close() {
	s.mail.Close()
	s.node.Store.Close()
}

func messageCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "message",
		Short: "Private messages, carried by relays or by hand",
		Long: "A message waits in the outbox until its recipient acknowledges it.\n" +
			"arc sync moves it: through relays, or through a directory that\n" +
			"someone carries. A machine that carries a directory also carries\n" +
			"other citizens' mail, which it cannot read.",
	}

	send := &cobra.Command{
		Use: "send <public key> [text...]", Short: "Send a message", Args: cobra.MinimumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			to, err := nostr.PubKeyFromHex(args[0])
			if err != nil {
				return errors.New("a recipient is 64 characters of hex, as arc whoami prints it")
			}

			text := strings.Join(args[1:], " ")
			if text == "" {
				body, err := io.ReadAll(io.LimitReader(os.Stdin, 32*1024+1))
				if err != nil {
					return err
				}
				text = strings.TrimRight(string(body), "\n")
			}
			if text == "" || len(text) > 32*1024 {
				return errors.New("a message holds 1 to 32768 bytes")
			}

			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.close()

			out, err := sess.mail.Send(command.Context(), to, text)
			if err != nil {
				return err
			}
			fmt.Printf("queued for %s until %s\n", keys.Name(to[:]), out.Expires.Local().Format("2006-01-02 15:04"))
			if len(sess.relays) == 0 {
				fmt.Println("no relays: run arc sync --dir <path> to hand it to a courier")
			}
			return nil
		},
	}

	inbox := &cobra.Command{
		Use: "inbox", Short: "List the messages you received", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.close()

			msgs := sess.mail.Inbox()
			if len(msgs) == 0 {
				fmt.Println("no messages: run arc sync")
			}
			for _, m := range msgs {
				fmt.Printf("%s  %s\n  %s\n", m.At.Local().Format("2006-01-02 15:04"), keys.Name(m.From[:]), m.Text)
			}
			if n := sess.mail.Carrying(); n > 0 {
				fmt.Printf("\ncarrying %d sealed messages for other citizens\n", n)
			}
			return nil
		},
	}

	outbox := &cobra.Command{
		Use: "outbox", Short: "List the messages you sent, and whether they arrived", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.close()

			out := sess.mail.Outbox()
			if len(out) == 0 {
				fmt.Println("no messages sent")
			}
			now := time.Now()
			for _, o := range out {
				to, _ := nostr.PubKeyFromHex(o.To)
				fmt.Printf("%s  to %s  %s, sent %d times\n  %s\n",
					o.Created.Local().Format("2006-01-02 15:04"), keys.Name(to[:]), o.State(now), o.Attempts, o.Text)
			}
			return nil
		},
	}

	command.AddCommand(send, inbox, outbox)
	return command
}

func syncCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "sync",
		Short: "Reconcile this machine with its relays, or with a directory",
		Long: "Without --dir, arc syncs with every relay. With --dir, it syncs with\n" +
			"that directory: a USB stick, a shared folder, or a disk you carry.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.close()
			sealed := sealedFilter(sess.key.Public)

			targets := sess.relays
			if dir, _ := command.Flags().GetString("dir"); dir != "" {
				targets = []transport.Transport{file.Dir{Path: dir}}
			}
			if len(targets) == 0 {
				return errors.New("nothing to sync with: add a relay, or give --dir")
			}

			failed := false
			for _, t := range targets {
				report, err := sess.node.Sync(command.Context(), sealed, t)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s: %v\n", t.Name(), err)
					failed = true
					continue
				}
				fmt.Printf("%s: sealed data received %d, sent %d, already held %d\n",
					report.Transport, report.Received, report.Sent, report.Duplicate+report.Superseded)
				for _, reason := range report.Refused {
					fmt.Fprintf(os.Stderr, "  refused %s\n", reason)
				}
				if report.Unreadable > 0 {
					fmt.Fprintf(os.Stderr, "  %d items did not parse\n", report.Unreadable)
				}
				for _, err := range report.SendFailed {
					fmt.Fprintf(os.Stderr, "  not sent: %v\n", err)
				}

				if offers, err := sess.node.Sync(command.Context(), announcements, t); err == nil && offers.Received > 0 {
					fmt.Printf("%s: announcements received %d\n", t.Name(), offers.Received)
				}

				mails, err := sess.mail.Sync(command.Context(), t)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s: mail: %v\n", t.Name(), err)
					failed = true
					continue
				}
				fmt.Printf("%s: mail received %d, delivered %d, carried %d, sent %d\n",
					mails.Transport, mails.Received, mails.Delivered, mails.Carried, mails.Sent)
				for _, reason := range mails.Refused {
					fmt.Fprintf(os.Stderr, "  refused %s\n", reason)
				}
			}
			if failed {
				return errors.New("some transports failed")
			}
			return nil
		},
	}
	command.Flags().String("dir", "", "sync with this directory instead of the relays")
	return command
}

// sealedFilter matches what a citizen seals to their own key: drafts, their
// checkpoints and parts, deletion requests, and the private relay list.
func sealedFilter(me nostr.PubKey) nostr.Filter {
	return nostr.Filter{
		Kinds:   []nostr.Kind{draft.Kind, draft.CheckpointKind, draft.PartKind, nostr.KindDeletion, draft.RelayListKind},
		Authors: []nostr.PubKey{me},
	}
}
