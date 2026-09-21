// Command arcn runs the new ARC stack beside the old one, until the new stack
// covers every command. See docs/delivery/SPEC.md.
//
//	arcn key new | show
//	arcn relay add <url> | rm <url> | ls | serve
//	arcn journal write | append | read | ls | tail
//	arcn message send | inbox | outbox
//	arcn sync [--dir <path>]
package main

import (
	"bufio"
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
	"fiatjaf.com/nostr/khatru"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/mail"
	"github.com/gezibash/arc/delivery/node"
	"github.com/gezibash/arc/delivery/store"
	"github.com/gezibash/arc/delivery/transport"
	"github.com/gezibash/arc/delivery/transport/file"
	"github.com/gezibash/arc/delivery/transport/relay"
	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/journal"
	"github.com/spf13/cobra"
)

func main() {
	if err := root().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "arcn:", err)
		os.Exit(1)
	}
}

func root() *cobra.Command {
	command := &cobra.Command{
		Use:           "arcn",
		Short:         "ARC on signed events, over any transport",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	command.PersistentFlags().String("home", "", "the directory of arcn (ARCN_HOME, default ~/.config/arc/next)")
	command.AddCommand(keyCommand(), relayCommand(), journalCommand(), messageCommand(), syncCommand())
	return command
}

// home is the directory that holds the key, the relay list and the store.
func home(command *cobra.Command) (string, error) {
	if dir, _ := command.Flags().GetString("home"); dir != "" {
		return dir, nil
	}
	if dir := os.Getenv("ARCN_HOME"); dir != "" {
		return dir, nil
	}
	user, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(user, ".config", "arc", "next"), nil
}

func keyPath(dir string) string    { return filepath.Join(dir, "key") }
func relaysPath(dir string) string { return filepath.Join(dir, "relays") }

func keyCommand() *cobra.Command {
	command := &cobra.Command{Use: "key", Short: "Manage the identity of this citizen"}
	command.AddCommand(
		&cobra.Command{
			Use: "new", Short: "Make a new identity", Args: cobra.NoArgs,
			RunE: func(command *cobra.Command, _ []string) error {
				dir, err := home(command)
				if err != nil {
					return err
				}
				k := keys.Generate()
				if err := keys.Save(keyPath(dir), k); err != nil {
					return err
				}
				fmt.Printf("%s\n%s\n", k.Name(), k.Public.Hex())
				return nil
			},
		},
		&cobra.Command{
			Use: "show", Short: "Show the identity of this citizen", Args: cobra.NoArgs,
			RunE: func(command *cobra.Command, _ []string) error {
				dir, err := home(command)
				if err != nil {
					return err
				}
				k, err := keys.Load(keyPath(dir))
				if err != nil {
					return err
				}
				fmt.Printf("%s\n%s\n", k.Name(), k.Public.Hex())
				return nil
			},
		},
	)
	return command
}

func readRelays(dir string) ([]string, error) {
	body, err := os.ReadFile(relaysPath(dir))
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

func writeRelays(dir string, urls []string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	body := strings.Join(urls, "\n")
	if body != "" {
		body += "\n"
	}
	return os.WriteFile(relaysPath(dir), []byte(body), 0o600)
}

func relayCommand() *cobra.Command {
	command := &cobra.Command{Use: "relay", Short: "Manage the relays of this citizen, or run one"}

	command.AddCommand(
		&cobra.Command{
			Use: "add <url>", Short: "Send to and sync with a relay", Args: cobra.ExactArgs(1),
			RunE: func(command *cobra.Command, args []string) error {
				url := args[0]
				if !strings.HasPrefix(url, "ws://") && !strings.HasPrefix(url, "wss://") {
					return errors.New("a relay is a ws:// or wss:// URL")
				}
				dir, err := home(command)
				if err != nil {
					return err
				}
				urls, err := readRelays(dir)
				if err != nil {
					return err
				}
				if !slices.Contains(urls, url) {
					urls = append(urls, url)
				}
				return writeRelays(dir, urls)
			},
		},
		&cobra.Command{
			Use: "rm <url>", Short: "Stop using a relay", Args: cobra.ExactArgs(1),
			RunE: func(command *cobra.Command, args []string) error {
				dir, err := home(command)
				if err != nil {
					return err
				}
				urls, err := readRelays(dir)
				if err != nil {
					return err
				}
				return writeRelays(dir, slices.DeleteFunc(urls, func(u string) bool { return u == args[0] }))
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
				if len(urls) == 0 {
					fmt.Println("no relays: add one with arcn relay add <url>")
				}
				for _, url := range urls {
					fmt.Println(url)
				}
				return nil
			},
		},
		serveCommand(),
	)
	return command
}

func serveCommand() *cobra.Command {
	command := &cobra.Command{
		Use: "serve", Short: "Run a relay on this machine", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			listen, _ := command.Flags().GetString("listen")
			path, _ := command.Flags().GetString("db")
			if path == "" {
				dir, err := home(command)
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
	return command
}

// session is one open store, with the key and the relays of this citizen.
type session struct {
	key    keys.Key
	node   *node.Node
	mail   *mail.Mail
	relays []transport.Transport
	urls   []string
}

func open(command *cobra.Command) (*session, error) {
	dir, err := home(command)
	if err != nil {
		return nil, err
	}
	k, err := keys.Load(keyPath(dir))
	if err != nil {
		return nil, err
	}
	s, err := store.Open(filepath.Join(dir, "store"))
	if err != nil {
		return nil, err
	}
	urls, err := readRelays(dir)
	if err != nil {
		s.Close()
		return nil, err
	}

	sess := &session{key: k, node: &node.Node{Store: s}, urls: urls}
	for _, url := range urls {
		sess.relays = append(sess.relays, relay.Relay{URL: url})
	}

	sess.mail, err = mail.Open(filepath.Join(dir, "store"), k, sess.node, sess.relays)
	if err != nil {
		s.Close()
		return nil, err
	}
	return sess, nil
}

func (s *session) close() {
	s.mail.Close()
	s.node.Store.Close()
}

func (s *session) journal() (*journal.Journal, error) {
	return journal.New(s.key, s.node, s.relays)
}

func journalCommand() *cobra.Command {
	command := &cobra.Command{Use: "journal", Short: "A private notebook, sealed to your own key"}

	write := &cobra.Command{
		Use: "write <project/notebook/page>", Short: "Replace a page with the standard input", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			sess, j, err := openJournal(command)
			if err != nil {
				return err
			}
			defer sess.close()

			title, _ := command.Flags().GetString("title")
			page, err := j.Write(command.Context(), args[0], title, bufio.NewReader(os.Stdin))
			if err != nil {
				return err
			}
			fmt.Printf("wrote %s: %d parts, %d bytes\n", page.Address, len(page.Parts), page.Bytes())
			reportUnsent(j)
			return nil
		},
	}
	write.Flags().String("title", "", "the title of the page")

	appendCmd := &cobra.Command{
		Use: "append <project/notebook/page> [text...]", Short: "Add text to the end of a page", Args: cobra.MinimumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			sess, j, err := openJournal(command)
			if err != nil {
				return err
			}
			defer sess.close()

			var text io.Reader = bufio.NewReader(os.Stdin)
			if len(args) > 1 {
				text = strings.NewReader(strings.Join(args[1:], " ") + "\n")
			}
			page, err := j.Append(command.Context(), args[0], text)
			if err != nil {
				return err
			}
			fmt.Printf("appended to %s: %d parts, %d bytes\n", page.Address, len(page.Parts), page.Bytes())
			reportUnsent(j)
			return nil
		},
	}

	readCmd := &cobra.Command{
		Use: "read <project/notebook/page>", Short: "Write a page, or a range of its lines", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			sess, j, err := openJournal(command)
			if err != nil {
				return err
			}
			defer sess.close()

			value, _ := command.Flags().GetString("lines")
			lines, err := journal.ParseLines(value)
			if err != nil {
				return err
			}
			_, err = j.Read(command.Context(), args[0], lines, os.Stdout)
			return err
		},
	}
	readCmd.Flags().String("lines", "", "a range of lines, as a:b, a: or :b")

	ls := &cobra.Command{
		Use: "ls", Short: "List the pages", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			sess, j, err := openJournal(command)
			if err != nil {
				return err
			}
			defer sess.close()

			pages, unreadable := j.List()
			if len(pages) == 0 {
				fmt.Println("no pages: sync first, or write one")
			}
			for _, page := range pages {
				fmt.Printf("%s\t%s\t%d bytes\t%s\n", page.Address, page.Title, page.Bytes(), page.Updated.Format("2006-01-02 15:04"))
			}
			if unreadable > 0 {
				fmt.Fprintf(os.Stderr, "%d heads did not open with this key\n", unreadable)
			}
			return nil
		},
	}

	tail := &cobra.Command{
		Use: "tail <project/notebook/page>", Short: "Write a page, then each text appended to it", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			sess, j, err := openJournal(command)
			if err != nil {
				return err
			}
			defer sess.close()

			url, _ := command.Flags().GetString("relay")
			if url == "" {
				if len(sess.urls) == 0 {
					return errors.New("tail needs a relay: add one with arcn relay add <url>")
				}
				url = sess.urls[0]
			}

			ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if err := j.Tail(ctx, args[0], relay.Relay{URL: url}, os.Stdout); !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}
	tail.Flags().String("relay", "", "the relay to watch (default the first relay)")

	command.AddCommand(write, appendCmd, readCmd, ls, tail)
	return command
}

// reportUnsent tells the citizen which relays did not get the write. The
// write is safe in the store either way.
func reportUnsent(j *journal.Journal) {
	for name, err := range j.Unsent() {
		fmt.Fprintf(os.Stderr, "not sent to %s: %v\nthe page is saved here; run arcn sync later\n", name, err)
	}
}

func openJournal(command *cobra.Command) (*session, *journal.Journal, error) {
	sess, err := open(command)
	if err != nil {
		return nil, nil, err
	}
	j, err := sess.journal()
	if err != nil {
		sess.close()
		return nil, nil, err
	}
	return sess, j, nil
}

func messageCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "message",
		Short: "Private messages, carried by relays or by hand",
		Long: "A message waits in the outbox until its recipient acknowledges it.\n" +
			"arcn sync moves it: through relays, or through a directory that\n" +
			"someone carries. A machine that carries a directory also carries\n" +
			"other citizens' mail, which it cannot read.",
	}

	send := &cobra.Command{
		Use: "send <public key> [text...]", Short: "Send a message", Args: cobra.MinimumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			to, err := nostr.PubKeyFromHex(args[0])
			if err != nil {
				return errors.New("a recipient is 64 characters of hex, as arcn key show prints it")
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
			fmt.Printf("queued for %s until %s\n", identity.Name(to[:]), out.Expires.Local().Format("2006-01-02 15:04"))
			if len(sess.relays) == 0 {
				fmt.Println("no relays: run arcn sync --dir <path> to hand it to a courier")
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
				fmt.Println("no messages: run arcn sync")
			}
			for _, m := range msgs {
				fmt.Printf("%s  %s\n  %s\n", m.At.Local().Format("2006-01-02 15:04"), identity.Name(m.From[:]), m.Text)
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
					o.Created.Local().Format("2006-01-02 15:04"), identity.Name(to[:]), o.State(now), o.Attempts, o.Text)
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
		Long: "Without --dir, arcn syncs with every relay. With --dir, it syncs with\n" +
			"that directory: a USB stick, a shared folder, or a disk you carry.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			sess, j, err := openJournal(command)
			if err != nil {
				return err
			}
			defer sess.close()

			targets := sess.relays
			if dir, _ := command.Flags().GetString("dir"); dir != "" {
				targets = []transport.Transport{file.Dir{Path: dir}}
			}
			if len(targets) == 0 {
				return errors.New("nothing to sync with: add a relay, or give --dir")
			}

			failed := false
			for _, t := range targets {
				report, err := sess.node.Sync(command.Context(), j.Filter(), t)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s: %v\n", t.Name(), err)
					failed = true
					continue
				}
				fmt.Printf("%s: journal received %d, sent %d, already held %d\n",
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
