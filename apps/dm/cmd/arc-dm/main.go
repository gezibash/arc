// Command arc-dm is the direct messages app of a person. It drives
// arc message, so arc signs, seals and sends each message:
//
//	arc-dm [ls]                  the conversations, newest first, with unread counts
//	arc-dm open <peer>           one conversation; it marks its messages read
//	arc-dm send <peer> [text...] send a message; peers separated by commas make a group
//	arc-dm reply <id> [text...]  reply to a message, to each member of its conversation
//	arc-dm read [peer]           mark a conversation read, or all of them
//	arc-dm watch                 print and notify each message as it arrives
//
// The read state stays on this device.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/gezibash/arc/sdk/arcprogram"
	"github.com/gezibash/arc/sdk/terminal"
)

const usage = `usage:
  arc-dm [ls] [flags]                  the conversations, newest first
  arc-dm open [flags] <peer>           one conversation; marks it read
  arc-dm send [flags] <peer> [text...] send a message (no text: read standard input)
  arc-dm reply [flags] <id> [text...]  reply to a message, to its conversation
  arc-dm read [flags] [peer]           mark a conversation read, or all of them
  arc-dm watch [flags]                 print and notify each message as it arrives

A peer is a public key or a petname, the start of either, or the end of a
petname: the eight hex digits at the end of a petname come from a hash of
the key, not from its start. Several peers, separated by commas, name a
group: arc-dm send ada,cy hello sends one message that both see, and a
reply goes to each member. The command is ls when none is given. A peer
with no direct conversation that shares one group with you opens that
group. An id is the start of the id of a message, as arc-dm open shows
it beside each message: at least 4 hex digits. Flags can come before or
after the command.
arc-dm <command> -h shows the flags of a command.`

func main() {
	commands := map[string]func(context.Context, []string) error{"ls": list, "open": open, "send": send, "reply": reply, "read": read, "watch": watch}
	name, args := "ls", os.Args[1:]
	if len(args) > 0 && slices.Contains([]string{"help", "-h", "-help", "--help"}, args[0]) {
		fmt.Println(usage)
		return
	}
	if i := commandAt(args); i >= 0 {
		name, args = args[i], slices.Concat(args[:i], args[i+1:])
	}
	run, ok := commands[name]
	if !ok {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, args)
	stop()
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		// An error can hold what arc wrote, which can hold text that a
		// relay chose.
		fmt.Fprintln(os.Stderr, "arc-dm:", terminal.EscapeLine(err.Error()))
		os.Exit(1)
	}
}

// commandAt is the index of the command in args, or -1 for none. The flags
// can come before the command; each flag but -h takes a value, in the next
// argument unless it holds "=".
func commandAt(args []string) int {
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--":
			return -1
		case slices.Contains([]string{"-h", "-help", "--help"}, arg):
		case strings.HasPrefix(arg, "-"):
			if !strings.Contains(arg, "=") {
				i++
			}
		default:
			return i
		}
	}
	return -1
}

// settings are the flags that each command takes.
type settings struct {
	arc   arcprogram.Arc
	state string
}

func newFlags(name string) (*flag.FlagSet, *settings) {
	flags := flag.NewFlagSet("arc-dm "+name, flag.ContinueOnError)
	s := &settings{}
	flags.StringVar(&s.arc.Program, "arc", "arc", "the arc program")
	flags.StringVar(&s.arc.Home, "home", "", "the arc home (default: the home that arc picks)")
	flags.StringVar(&s.state, "state", "", "the state directory (default DM_STATE, or ~/.local/state/arc-dm)")
	return flags, s
}

// list prints the conversations, newest first.
func list(ctx context.Context, args []string) error {
	flags, s := newFlags("ls")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: arc-dm ls [flags]")
	}
	all, err := conversations(ctx, s.arc)
	if err != nil {
		return err
	}
	read, err := loadRead(stateDir(s.state))
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Println("no conversations: run arc sync, or keep arc-dm watch running")
		return nil
	}
	for _, c := range all {
		last := c.last()
		status := ""
		if n := c.unread(read); n > 0 {
			status = fmt.Sprintf("  %d unread", n)
		}
		who := ""
		switch {
		case last.Mine:
			who = "you: "
		case !c.direct():
			who = last.From + ": "
		}
		fmt.Printf("%s  %s%s\n  %s%s\n", stamp(last.At), c.Name, status, who, terminal.Escape(preview(last.Text, 72))) //arc:raw-ok c.Name holds petnames; status is arc-dm's own
	}
	return nil
}

// open prints one conversation, and marks its messages read.
func open(ctx context.Context, args []string) error {
	flags, s := newFlags("open")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: arc-dm open [flags] <peer>")
	}
	all, err := conversations(ctx, s.arc)
	if err != nil {
		return err
	}
	c, err := find(all, flags.Arg(0))
	if err != nil {
		return err
	}
	c = sharedGroup(all, c)
	if len(c.Entries) == 0 {
		fmt.Println(noMessages(all, c))
		return nil
	}
	dir := stateDir(s.state)
	read, err := loadRead(dir)
	if err != nil {
		return err
	}
	messages := index(all)
	for _, e := range c.Entries {
		who, status := cmp.Or(e.From, c.Name), ""
		if e.Mine {
			who, status = "you", "  "+e.State
		} else if !read[e.ID] {
			status = "  new"
		}
		fmt.Printf("%s  %s  %s%s\n", stamp(e.At), messages.short(e.ID), who, status) //arc:raw-ok status is a delivery state of arc, or new; the id is hex
		if q := messages.quote(e, c); q != "" {
			fmt.Println(q)
		}
		fmt.Println(indent(terminal.Escape(e.Text)))
	}
	return markRead(dir, c)
}

// send sends a message to a peer.
func send(ctx context.Context, args []string) error {
	flags, s := newFlags("send")
	replyTo := flags.String("reply", "", "reply to the message with this id, or the start of it")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() < 1 {
		return errors.New("usage: arc-dm send [flags] <peer> [text...]")
	}
	text, err := messageText(ctx, flags.Args()[1:])
	if err != nil {
		return err
	}
	all, err := conversations(ctx, s.arc)
	if err != nil {
		return err
	}
	c, err := find(all, flags.Arg(0))
	if err != nil {
		return err
	}
	if *replyTo == "" {
		if _, err := s.arc.Send(ctx, c.Peers, text); err != nil {
			return err
		}
		fmt.Printf("queued for %s\n", cmp.Or(c.Name, c.key())) //arc:raw-ok c.Name holds petnames, or a key
		return nil
	}
	messages := index(all)
	parent, err := messages.lookup(*replyTo)
	if err != nil {
		return err
	}
	if parent.conv.key() != c.key() {
		return fmt.Errorf("%s is in another conversation: arc-dm reply %s sends to its members", messages.short(parent.ID), messages.short(parent.ID))
	}
	return sendReply(ctx, s.arc, messages, parent, text)
}

// reply sends a reply to a message, to each member of its conversation.
func reply(ctx context.Context, args []string) error {
	flags, s := newFlags("reply")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() < 1 {
		return errors.New("usage: arc-dm reply [flags] <id> [text...]")
	}
	all, err := conversations(ctx, s.arc)
	if err != nil {
		return err
	}
	messages := index(all)
	parent, err := messages.lookup(flags.Arg(0))
	if err != nil {
		return err
	}
	text, err := messageText(ctx, flags.Args()[1:])
	if err != nil {
		return err
	}
	return sendReply(ctx, s.arc, messages, parent, text)
}

// sendReply sends text to the members of the parent, and names them: a
// mistyped id could answer in another conversation.
func sendReply(ctx context.Context, arc arcprogram.Arc, messages *store, parent held, text string) error {
	if _, err := arc.SendReply(ctx, parent.conv.Peers, parent.ID, text); err != nil {
		return err
	}
	fmt.Printf("replied to %s in %s\n", messages.short(parent.ID), parent.conv.Name) //arc:raw-ok the id is hex; the name holds petnames
	return nil
}

// messageText is the words of a command, or standard input when there are
// none.
func messageText(ctx context.Context, words []string) (string, error) {
	text := strings.Join(words, " ")
	if len(words) == 0 {
		body, err := readAll(ctx, os.Stdin)
		if err != nil {
			return "", err
		}
		text = strings.TrimRight(string(body), "\n")
	}
	if terminal.Blank(text) {
		return "", errors.New("a message holds at least one character that a person can see")
	}
	return text, nil
}

// readAll reads r to its end, or returns at the end of ctx: a read of
// standard input cannot be stopped, and a signal must still stop arc-dm.
func readAll(ctx context.Context, r io.Reader) ([]byte, error) {
	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		body, err := io.ReadAll(r)
		done <- result{body, err}
	}()
	select {
	case got := <-done:
		return got.body, got.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// read marks the messages of one conversation, or of all, read.
func read(ctx context.Context, args []string) error {
	flags, s := newFlags("read")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return errors.New("usage: arc-dm read [flags] [peer]")
	}
	all, err := conversations(ctx, s.arc)
	if err != nil {
		return err
	}
	if flags.NArg() == 1 {
		c, err := find(all, flags.Arg(0))
		if err != nil {
			return err
		}
		all = []conversation{sharedGroup(all, c)}
	}
	return markRead(stateDir(s.state), all...)
}

// watch prints each message as it arrives, and posts a notification. It
// marks no message read.
func watch(ctx context.Context, args []string) error {
	flags, s := newFlags("watch")
	notify := flags.String("notify", "", "a program to run for each message, as <program> <sender> <text>; none for no notification (default: the notifications of this system)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: arc-dm watch [flags]")
	}
	notifier := notifierOf(*notify)
	me, err := s.arc.PublicKey(ctx)
	if err != nil {
		return err
	}
	all, err := conversations(ctx, s.arc)
	if err != nil {
		return err
	}
	messages := index(all)
	return s.arc.Watch(ctx, arcprogram.WatchOptions{Notes: os.Stderr}, func(m arcprogram.Message) error {
		group := ""
		if len(m.Members) > 2 {
			group = fmt.Sprintf("  in a group of %d", len(m.Members))
		}
		members := m.Members
		if members == nil {
			// An arc older than group messages prints no members.
			members = []string{m.From}
		}
		c := conversation{Peers: peersOf(me, members)}
		if _, ok := messages.byID[m.ReplyTo]; m.ReplyTo != "" && !ok {
			// The parent can be a message that this citizen sent since the
			// watch started.
			if all, err := conversations(ctx, s.arc); err == nil {
				messages = index(all)
			}
		}
		e := entry{ID: m.ID, At: m.At, Text: m.Text, From: m.Name, ReplyTo: m.ReplyTo}
		messages.add(e, c)
		fmt.Printf("%s  %s  %s%s\n", stamp(m.At), messages.short(m.ID), m.Name, group) //arc:raw-ok m.Name is a petname; the id is hex
		if q := messages.quote(e, c); q != "" {
			fmt.Println(q)
		}
		fmt.Println(indent(terminal.Escape(m.Text)))
		if len(notifier) == 0 {
			return nil
		}
		body := terminal.Escape(preview(m.Text, 200))
		if filepath.Base(notifier[0]) == "notify-send" {
			// Many notification servers read the body as markup.
			body = html.EscapeString(body)
		}
		command := exec.CommandContext(ctx, notifier[0], append(notifier[1:], m.Name, body)...)
		if out, err := command.CombinedOutput(); err != nil && ctx.Err() == nil {
			// A failed notification does not stop the watch.
			fmt.Fprintf(os.Stderr, "arc-dm: the notification failed: %s\n", terminal.EscapeLine(err.Error()+" "+strings.TrimSpace(string(out))))
		}
		return nil
	})
}

// notifierOf is the command that posts a notification, before its two
// arguments: the sender and the text.
func notifierOf(setting string) []string {
	switch setting {
	case "none":
		return nil
	case "":
	default:
		return []string{setting}
	}
	switch runtime.GOOS {
	case "darwin":
		return []string{"osascript", "-e", "on run argv", "-e", "display notification (item 2 of argv) with title (item 1 of argv)", "-e", "end run"}
	case "linux":
		if _, err := exec.LookPath("notify-send"); err == nil {
			return []string{"notify-send", "--"}
		}
	}
	return nil
}

func stamp(at time.Time) string { return at.Local().Format("2006-01-02 15:04") }

// indent puts two spaces before each line of text.
func indent(text string) string { return "  " + strings.ReplaceAll(text, "\n", "\n  ") }

// preview is the first line of text, at most limit characters.
func preview(text string, limit int) string {
	line, _, more := strings.Cut(text, "\n")
	if runes := []rune(line); len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	if more {
		return line + " …"
	}
	return line
}
