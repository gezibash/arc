// Command arc is the command line tool of ARC, on Nostr events. See
// docs/delivery/SPEC.md.
//
//	arc keys gen | recover | add | list | use | remove | encrypt | bunker
//	arc whoami
//	arc relay add <url> | rm <url> | ls | turn | serve
//	arc message send | inbox | outbox | watch
//	arc serve | announce | discover | install | call | session
//	arc sync [--dir <path>]
//	arc apps list | info | remove
//	arc info | resolve | apps init | version
//	arc lists add | rm | ls
//	arc update [check | apply]
//	arc release sign
//	arc <app> <command...>
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/khatru"
	"github.com/gezibash/arc/adapters/relay/limits"
	"github.com/gezibash/arc/adapters/relay/sealed"
	"github.com/gezibash/arc/adapters/relay/stun"
	"github.com/gezibash/arc/adapters/relay/turn"
	"github.com/gezibash/arc/adapters/transport/file"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/core/transport"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/gezibash/arc/runtime/iface"
	"github.com/spf13/cobra"
)

func main() {
	if err := root().Execute(); err != nil {
		// An app set the exit status from its reply, and wrote its
		// output already.
		if status, ok := errors.AsType[iface.ExitError](err); ok {
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
			"An installed app adds its own commands: arc <name> <command>.\n" +
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
		serveCmd(), announceCmd(), discoverCmd(), installCmd(), callCmd(), sessionCmd(), syncCommand(),
		appsCommand(), infoCommand(), resolveCommand(), versionCommand(), listsCommand(), updateCommand(), releaseCommand())
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
	for line := range strings.SplitSeq(string(body), "\n") {
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
		turnCommand(),
		serveCommand(),
	)
	return command
}

// turnCommand asks a relay for a TURN credential with a NIP-98 request,
// and prints the JSON reply. The transfer app runs it when it finds no
// direct path.
func turnCommand() *cobra.Command {
	return &cobra.Command{
		Use: "turn [url]", Short: "Get a TURN credential from a relay (default: the first relay)", Args: cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			dir, err := home(command)
			if err != nil {
				return err
			}
			relay := ""
			if len(args) == 1 {
				relay = args[0]
			} else {
				relays, err := readRelays(dir)
				if err != nil {
					return err
				}
				if len(relays) == 0 {
					return errors.New("no relays: add one with arc relay add <url>")
				}
				relay = relays[0]
			}
			endpoint, err := turnURL(relay)
			if err != nil {
				return err
			}
			id, err := loadIdentity(command.Context(), dir)
			if err != nil {
				return err
			}
			authorization, err := turn.AuthHeader(command.Context(), id.signer, http.MethodGet, endpoint)
			if err != nil {
				return err
			}
			request, err := http.NewRequestWithContext(command.Context(), http.MethodGet, endpoint, nil)
			if err != nil {
				return err
			}
			request.Header.Set("Authorization", authorization)
			client := &http.Client{Timeout: 15 * time.Second}
			response, err := client.Do(request)
			if err != nil {
				return err
			}
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
			if err != nil {
				return err
			}
			switch {
			case response.StatusCode == http.StatusNotFound:
				return fmt.Errorf("%s serves no TURN", relay)
			case response.StatusCode != http.StatusOK:
				return fmt.Errorf("%s refused: %s %s", relay, response.Status, strings.TrimSpace(string(body)))
			}
			var credential turn.Credential
			if json.Unmarshal(body, &credential) != nil || credential.Username == "" || len(credential.URLs) == 0 {
				return fmt.Errorf("%s serves no TURN: its reply is not a credential", relay)
			}
			_, err = os.Stdout.Write(body)
			return err
		},
	}
}

// turnURL is the HTTP URL of the TURN credential of a relay URL.
func turnURL(relay string) (string, error) {
	u, err := url.Parse(relay)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%q is not a relay URL", relay)
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	default:
		return "", errors.New("a relay is a ws:// or wss:// URL")
	}
	u.Path, u.RawQuery, u.Fragment = turn.Path, "", ""
	return u.String(), nil
}

// announceRelays publishes the relay list of this citizen, when it has a key
// and at least one relay.
func announceRelays(command *cobra.Command) {
	sess, err := open(command)
	if err != nil {
		return
	}
	defer sess.Close()
	if len(sess.Relays) > 0 {
		publishRelayList(command.Context(), sess)
	}
}

// newRelay makes the relay of arc relay serve, with its store in the file at
// path. Call the returned function to close the store.
func newRelay(path string, p limits.Policy) (*khatru.Relay, func(), error) {
	db := &boltdb.BoltBackend{Path: path}
	if err := db.Init(); err != nil {
		return nil, nil, err
	}
	rl := khatru.NewRelay()
	rl.Log = log.New(io.Discard, "", 0)
	rl.UseEventstore(db, 500)
	rl.Negentropy = true
	rl.Info.SupportedNIPs = append(rl.Info.SupportedNIPs, 77)
	sealed.Protect(rl)
	limits.Apply(rl, db.DB, p)
	return rl, db.Close, nil
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

			rl, closeDB, err := newRelay(path, writePolicy(command))
			if err != nil {
				return err
			}
			defer closeDB()

			listener, err := net.Listen("tcp", listen)
			if err != nil {
				return err
			}
			fmt.Printf("relay listens on ws://%s\n", listener.Addr())
			turnConfig, err := turnSetting(command)
			if err != nil {
				return err
			}
			var turnServer *turn.Server
			if stunAddress := stunListen(command, listen); stunAddress != "" {
				// A second relay on one machine cannot take the default
				// port. It runs with no STUN server. A port that the
				// operator sets must open.
				conn, err := net.ListenPacket("udp", stunAddress)
				switch {
				case err != nil && (command.Flags().Changed("stun") || turnConfig != nil):
					return fmt.Errorf("STUN: %w", err)
				case err != nil:
					fmt.Fprintf(os.Stderr, "relay answers no STUN: %v (set --stun)\n", err)
				case turnConfig != nil:
					// The TURN server answers STUN binding requests too.
					host, _, _ := net.SplitHostPort(stunAddress)
					turnConfig.RelayHost = host
					if turnServer, err = turn.Serve(conn, *turnConfig); err != nil {
						_ = conn.Close()
						return fmt.Errorf("TURN: %w", err)
					}
					defer func() { _ = turnServer.Close() }()
					fmt.Printf("relay answers STUN and TURN on udp://%s, relay ports %d-%d on %s\n", conn.LocalAddr(), turnConfig.MinPort, turnConfig.MaxPort, turnConfig.PublicIP)
				default:
					defer func() { _ = conn.Close() }()
					go func() {
						if err := stun.Serve(conn); err != nil {
							fmt.Fprintln(os.Stderr, "the STUN server stopped:", err)
						}
					}()
					fmt.Printf("relay answers STUN on udp://%s\n", conn.LocalAddr())
				}
			} else if turnConfig != nil {
				return errors.New("TURN needs the STUN port; do not set --stun none with --turn-ports")
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			var handler http.Handler = rl
			if turnServer != nil {
				mux := http.NewServeMux()
				mux.Handle("/", rl)
				mux.Handle(turn.Path, turnServer)
				handler = mux
			}
			server := &http.Server{Handler: handler}
			go func() { <-ctx.Done(); _ = server.Close() }()
			if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
	command.Flags().String("listen", "127.0.0.1:7447", "the address to listen on")
	command.Flags().String("db", "", "the file that holds the events (default <home>/relay.db)")
	command.Flags().Int("max-event-bytes", 0, "refuse an event larger than this, as JSON (0: no cap, except the mail limit on gift wraps)")
	command.Flags().Bool("wrap-auth", false, "take a gift wrap only after NIP-42 authentication, or with proof of work")
	command.Flags().Int("wrap-pow", 0, "the NIP-13 difficulty that lets a gift wrap in without authentication (0: none)")
	command.Flags().Int("rate", 0, "the events that one IP address can write each minute (0: no limit)")
	command.Flags().Int("burst", 0, "the events that one IP address can write at once (default the rate)")
	command.Flags().String("ip-header", "", "the HTTP header that holds the client IP address, when a proxy sets it")
	command.Flags().Int64("max-store-mb", 0, "refuse new stored events when the store uses this many MiB (0: no cap)")
	command.Flags().String("stun", "", "the UDP address that answers STUN, or none (default: the host of --listen, port 3478)")
	command.Flags().String("turn-ports", "", "serve TURN on the STUN port, with relay sockets on this UDP port range, such as 49152-49251 (needs TURN_SECRET)")
	command.Flags().String("turn-ip", "", "the public IP address of the TURN relay sockets (default: the host of --listen)")
	command.Flags().Int64("turn-quota", 1024, "the MiB that one citizen can relay through TURN in 24 hours (0: refuse every allocation)")
	return command
}

// turnSetting reads the TURN flags of a relay. It returns nil if the relay
// serves no TURN. The secret comes from the environment variable
// TURN_SECRET, so it is not in the process list.
func turnSetting(command *cobra.Command) (*turn.Config, error) {
	flags := command.Flags()
	ports, _ := flags.GetString("turn-ports")
	if ports == "" {
		return nil, nil
	}
	low, high, ok := strings.Cut(ports, "-")
	minPort, err1 := strconv.ParseUint(low, 10, 16)
	maxPort, err2 := strconv.ParseUint(high, 10, 16)
	if !ok || err1 != nil || err2 != nil || minPort == 0 || maxPort < minPort {
		return nil, fmt.Errorf("--turn-ports %q is not a range such as 49152-49251", ports)
	}
	secret := os.Getenv("TURN_SECRET")
	if secret == "" {
		return nil, errors.New("--turn-ports needs the environment variable TURN_SECRET")
	}
	address, _ := flags.GetString("turn-ip")
	if address == "" {
		listen, _ := flags.GetString("listen")
		address, _, _ = net.SplitHostPort(listen)
	}
	ip := net.ParseIP(address)
	if ip == nil || ip.IsUnspecified() {
		return nil, fmt.Errorf("TURN needs the public IP address of this machine: set --turn-ip (not %q)", address)
	}
	quota, _ := flags.GetInt64("turn-quota")
	return &turn.Config{Secret: secret, PublicIP: ip, MinPort: uint16(minPort), MaxPort: uint16(maxPort), Quota: quota << 20}, nil
}

// stunListen is the UDP address of the STUN server of a relay, or empty for
// none. A citizen asks the host of its relay on port 3478, so the default
// keeps the host of the relay.
func stunListen(command *cobra.Command, listen string) string {
	value, _ := command.Flags().GetString("stun")
	switch value {
	case "none":
		return ""
	case "":
		host, _, _ := net.SplitHostPort(listen)
		return net.JoinHostPort(host, strconv.Itoa(stun.Port))
	}
	return value
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

// session is one open store, with the key and the relays of this citizen.
func open(command *cobra.Command) (*citizen.Session, error) {
	dir, err := home(command)
	if err != nil {
		return nil, err
	}
	id, err := loadIdentity(command.Context(), dir)
	if err != nil {
		return nil, err
	}
	urls, err := readRelays(dir)
	if err != nil {
		return nil, err
	}
	indexers, err := readIndexers(dir)
	if err != nil {
		return nil, err
	}
	root, err := rootDir(command)
	if err != nil {
		return nil, err
	}
	return citizen.Open(citizen.Config{Home: dir, Root: root, Key: id.key, Signer: id.signer, Remote: id.remote, URLs: urls, IndexerURLs: indexers, Errors: os.Stderr})
}

func errRemote(what string) error {
	return fmt.Errorf("%s needs the secret key on this machine; a remote signer cannot do it yet", what)
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
				body, err := io.ReadAll(io.LimitReader(os.Stdin, private.MaxMailContent+1))
				if err != nil {
					return err
				}
				text = strings.TrimRight(string(body), "\n")
			}
			if text == "" {
				return errors.New("a message holds at least 1 byte")
			}
			if len(text) > private.MaxMailContent {
				return fmt.Errorf("the message is too large for mail, which holds at most %d bytes: send it over a direct path, such as the transfer app", private.MaxMailContent)
			}

			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()

			out, err := sess.Mail.Send(command.Context(), to, text)
			if err != nil {
				return err
			}
			if asJSON, _ := command.Flags().GetBool("json"); asJSON {
				if len(sess.Relays) == 0 {
					fmt.Fprintln(os.Stderr, "no relays: run arc sync --dir <path> to hand it to a courier")
				}
				return json.NewEncoder(os.Stdout).Encode(struct {
					ID    string `json:"id"`
					To    string `json:"to"`
					State string `json:"state"`
				}{out.Rumor, out.To, out.State(time.Now())})
			}
			fmt.Printf("queued for %s until %s\n", keys.Name(to[:]), out.Expires.Local().Format("2006-01-02 15:04"))
			if len(sess.Relays) == 0 {
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
			defer sess.Close()

			msgs, err := sess.Mail.Inbox(command.Context())
			if err != nil {
				return err
			}
			asJSON, _ := command.Flags().GetBool("json")
			printer := newMessagePrinter(os.Stdout, asJSON)
			if asJSON {
				for _, m := range msgs {
					if err := printer.print(m); err != nil {
						return err
					}
				}
				return nil
			}
			if len(msgs) == 0 {
				fmt.Println("no messages: run arc sync")
			}
			for _, m := range msgs {
				if err := printer.print(m); err != nil {
					return err
				}
			}
			n, err := sess.Mail.Carrying()
			if err != nil {
				return err
			}
			if n > 0 {
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
			defer sess.Close()

			out, err := sess.Mail.Outbox(command.Context())
			if err != nil {
				return err
			}
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

	send.Flags().Bool("json", false, "print the queued message as one JSON object")
	inbox.Flags().Bool("json", false, "print one JSON object for each message")
	command.AddCommand(send, inbox, outbox, messageWatchCommand())
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
			defer sess.Close()
			sealed := sealedFilter(sess.Key.Public)

			targets := sess.Relays
			if dir, _ := command.Flags().GetString("dir"); dir != "" {
				targets = []transport.Transport{file.Dir{Path: dir}}
			}
			if len(targets) == 0 {
				return errors.New("nothing to sync with: add a relay, or give --dir")
			}

			failed := false
			for _, t := range targets {
				report, err := sess.Node.Sync(command.Context(), sealed, t)
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

				if offers, err := sess.Node.Sync(command.Context(), announcements, t); err == nil && offers.Received > 0 {
					fmt.Printf("%s: announcements received %d\n", t.Name(), offers.Received)
				}

				mails, err := sess.Mail.Sync(command.Context(), t)
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
