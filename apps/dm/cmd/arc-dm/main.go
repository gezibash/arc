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
	"sync"
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
after the command. After the peer of send, only -reply counts; each other
word is text, and after -- each word is.
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
	// arc-dm ends on an interrupt, SIGTERM and SIGHUP, as when its terminal
	// closes. The end of the context ends each arc that it runs.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	second, interruptAgain := context.WithCancel(context.Background())
	go func() {
		// A second signal ends the work that the first one began, such as
		// the last read of a watch on a hung arc. A third one ends arc-dm
		// as with no handler.
		<-ctx.Done()
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		stop()
		<-signals
		signal.Stop(signals)
		interruptAgain()
	}()
	err := run(context.WithValue(ctx, secondSignal{}, second), args)
	stop()
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		// An error can hold what arc wrote, which can hold text that a
		// relay chose.
		fmt.Fprintln(os.Stderr, "arc-dm:", terminal.EscapeLine(err.Error()))
		os.Exit(1)
	}
}

// secondSignal is the key of the context that a second signal ends.
type secondSignal struct{}

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

// parse parses the flags of a command before its first n arguments, and
// returns the arguments. The flag package stops at the first argument that
// is not a flag, so without this arc-dm send ada -reply 3f9a hi would send
// "-reply 3f9a hi" as text.
//
// After the n arguments, only the flags named in after count, and the
// first other word starts the text: arc-dm send ada -home is where the
// heart is sends that text, and changes no home. After "--", each word is
// text.
func parse(flags *flag.FlagSet, args []string, n int, after ...string) ([]string, error) {
	out, _, err := parseText(flags, args, n, after...)
	return out, err
}

// parseText is parse, and also reports whether the text came after "--".
func parseText(flags *flag.FlagSet, args []string, n int, after ...string) (out []string, literal bool, err error) {
	for n == 0 || len(out) < n {
		if err := flags.Parse(args); err != nil {
			return nil, false, err
		}
		rest := flags.Args()
		ended := len(rest) < len(args) && args[len(args)-len(rest)-1] == "--"
		if n == 0 || ended || len(rest) == 0 {
			return append(out, rest...), ended, nil
		}
		out = append(out, rest[0])
		args = rest[1:]
	}
	for len(args) > 0 {
		arg := args[0]
		if arg == "--" {
			return append(out, args[1:]...), true, nil
		}
		name, value, inline := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-"), "=")
		if !strings.HasPrefix(arg, "-") || !slices.Contains(after, name) {
			break
		}
		if !inline {
			if len(args) < 2 {
				return nil, false, fmt.Errorf("flag needs an argument: %s", arg)
			}
			value, args = args[1], args[1:]
		}
		if err := flags.Set(name, value); err != nil {
			return nil, false, fmt.Errorf("invalid value %q for flag %s: %w", value, arg, err)
		}
		args = args[1:]
	}
	return append(out, args...), false, nil
}

// watcher prints the messages of a watch.
//
// A reply can name a parent that this citizen sent after the watch
// started, from another process: only a read of the outbox finds it. A
// sender can name many ids that no message has, and each read of the
// outbox opens each sealed message that this citizen sent. So the watch
// reads the outbox at most once each refreshEvery. A reply whose parent
// the watch does not hold waits for the next read, and each message after
// it waits too. An id that a read did not find is not waited for again;
// the watch forgets such ids when it holds maxMissed.
//
// A waiting reply prints without its quote only after a read of the
// outbox that came after it, with one exception: a sender that holds more
// than maxHeldPerSender messages prints its oldest at once. Past
// maxWaiting waiting replies, the watch reads the outbox at once, or as
// soon as overflowRead has passed since the last read. Past maxQueue held
// messages with the read blocked, it prints early only plain messages that
// lead the held messages of their sender, and then waits for the read,
// without its lock. The messages of one sender always print in their
// order; a sender's messages can print ahead of the messages of others.
type watcher struct {
	mu       sync.Mutex
	outbox   func(context.Context) ([]arcprogram.Sent, error)
	print    func(arcprogram.Message, *store)
	me       string
	messages *store
	missed   map[string]bool
	last     time.Time // the last read of the outbox
	// queue holds the held messages in the order of their arrival; an
	// entry is nil once its message printed early.
	queue []*arcprogram.Message
	// bySender holds the places in queue of each sender's held messages,
	// oldest first.
	bySender map[string][]int
	live     int         // the messages in queue that did not print
	waiting  int         // the messages in queue that wait for a read
	timer    *time.Timer // the next read, when a message waits for it
	due      time.Time   // the time of the next read
	// close bounds the read of the outbox at the end of the watch: a
	// second interrupt ends it.
	closeCtx context.Context
}

const (
	refreshEvery     = 5 * time.Second
	maxMissed        = 1024
	maxWaiting       = 256
	maxHeldPerSender = 64
	maxQueue         = 4096
	overflowRead     = time.Second
	// closeRead bounds the read of the outbox at the end of a watch,
	// after the signal that ended it.
	closeRead = 10 * time.Second
)

// take prints m, or holds it until the next read of the outbox.
func (w *watcher) take(m arcprogram.Message) {
	w.mu.Lock()
	defer w.mu.Unlock()
	waits := w.waits(m)
	if w.live == 0 && !waits {
		w.print(m, w.messages)
		return
	}
	w.bySender[m.From] = append(w.bySender[m.From], len(w.queue))
	w.queue = append(w.queue, &m)
	w.live++
	if waits {
		w.waiting++
	}
	for len(w.bySender[m.From]) > maxHeldPerSender {
		w.printOldest(m.From)
	}
	for w.live >= maxQueue {
		if time.Since(w.last) >= overflowRead {
			w.readAndFlush(context.Background())
			return
		}
		if w.printPlainHeads() {
			continue
		}
		// Each held message waits, or follows one that waits, of its
		// sender: wait for the read, and let the timer run meanwhile.
		wait := overflowRead - time.Since(w.last)
		w.mu.Unlock()
		time.Sleep(wait)
		w.mu.Lock()
	}
	switch {
	case w.live == 0:
	case w.waiting == 0:
		w.flush(false)
	case w.waiting >= maxWaiting && time.Since(w.last) >= overflowRead:
		w.readAndFlush(context.Background())
	case w.waiting >= maxWaiting:
		w.readIn(overflowRead - time.Since(w.last))
	case w.timer != nil:
		// A read is due, and m waits for it.
	case time.Since(w.last) >= refreshEvery:
		w.readAndFlush(context.Background())
	default:
		w.readIn(refreshEvery - time.Since(w.last))
	}
}

// readIn reads the outbox and prints what is held after wait, or sooner
// if a read is due sooner.
func (w *watcher) readIn(wait time.Duration) {
	if w.timer != nil {
		if time.Until(w.due) <= wait {
			return
		}
		w.stop()
	}
	w.due = time.Now().Add(wait)
	w.timer = time.AfterFunc(wait, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.timer = nil
		if w.live > 0 {
			w.readAndFlush(context.Background())
		}
	})
}

// printPlainHeads prints, for each sender, the plain messages that lead
// its held messages: they wait for no read, and no message of their
// sender comes before them. It reports whether it printed any.
func (w *watcher) printPlainHeads() bool {
	printed := false
	for sender := range w.bySender {
		for places, ok := w.bySender[sender]; ok && !w.waits(*w.queue[places[0]]); places, ok = w.bySender[sender] {
			w.printOldest(sender)
			printed = true
		}
	}
	return printed
}

// printOldest prints the oldest held message of a sender, as it is: a
// reply that waits shows no quote, and its parent is not missed.
func (w *watcher) printOldest(sender string) {
	places := w.bySender[sender]
	i := places[0]
	if len(places) == 1 {
		delete(w.bySender, sender)
	} else {
		w.bySender[sender] = places[1:]
	}
	m := w.queue[i]
	w.queue[i] = nil
	w.live--
	if w.waits(*m) {
		w.waiting--
	}
	w.print(*m, w.messages)
	if len(w.queue) > 2*w.live+64 {
		w.compact()
	}
}

// compact drops the entries of the messages that printed early.
func (w *watcher) compact() {
	var queue []*arcprogram.Message
	clear(w.bySender)
	for _, m := range w.queue {
		if m != nil {
			w.bySender[m.From] = append(w.bySender[m.From], len(queue))
			queue = append(queue, m)
		}
	}
	w.queue = queue
}

func (w *watcher) stop() {
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

// waits reports whether m names a parent that a read of the outbox could
// find.
func (w *watcher) waits(m arcprogram.Message) bool {
	_, held := w.messages.byID[m.ReplyTo]
	return m.ReplyTo != "" && !held && !w.missed[m.ReplyTo]
}

// readAndFlush reads the outbox, and prints each held message.
func (w *watcher) readAndFlush(ctx context.Context) {
	w.stop()
	w.refresh(ctx)
	w.flush(true)
}

func (w *watcher) refresh(ctx context.Context) {
	w.last = time.Now()
	outbox, err := w.outbox(ctx)
	if err != nil {
		return
	}
	for _, sent := range sentMessages(outbox) {
		w.messages.add(sent.entry, conversation{Peers: peersOf(w.me, sent.members)})
	}
}

// flush prints each held message, in order. After a read of the outbox, a
// parent that is still not here is missed, and no later reply waits for
// it.
func (w *watcher) flush(read bool) {
	for _, m := range w.queue {
		if m == nil {
			continue
		}
		if read && w.waits(*m) {
			w.miss(m.ReplyTo)
		}
		w.print(*m, w.messages)
	}
	w.queue, w.live, w.waiting = nil, 0, 0
	clear(w.bySender)
}

// miss remembers an id that a read of the outbox did not find.
func (w *watcher) miss(id string) {
	if len(w.missed) >= maxMissed {
		clear(w.missed)
	}
	w.missed[id] = true
}

// close prints the messages that still wait, at the end of the watch. If
// one waits for a parent, it reads the outbox once more, for at most
// closeRead, or until a second interrupt. It drops no message: without a
// read, it prints them as they are.
func (w *watcher) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stop()
	if w.waiting > 0 {
		parent := w.closeCtx
		if parent == nil {
			parent = context.Background()
		}
		ctx, cancel := context.WithTimeout(parent, closeRead)
		w.refresh(ctx)
		cancel()
	}
	w.flush(true)
}

// refuseHelp refuses a text of only -h, -H, --h, --H, -help or --help: a person who
// asks for help must not send the request as a message. After -- it is
// text.
func refuseHelp(words []string, literal bool, command string) error {
	if len(words) != 1 || literal || !slices.Contains([]string{"-h", "-H", "--h", "--H", "-help", "--help"}, words[0]) {
		return nil
	}
	name, _, _ := strings.Cut(command, " ")
	return fmt.Errorf("the text is only %s: for help, run arc-dm %s -h; to send it, write arc-dm %s -- %s", words[0], name, command, words[0])
}

// given reports whether a flag was set, even to an empty value.
func given(flags *flag.FlagSet, name string) bool {
	set := false
	flags.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
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
	args, err := parse(flags, args, 0)
	if err != nil {
		return err
	}
	if len(args) != 0 {
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
	args, err := parse(flags, args, 1)
	if err != nil {
		return err
	}
	if len(args) != 1 {
		return errors.New("usage: arc-dm open [flags] <peer>")
	}
	all, err := conversations(ctx, s.arc)
	if err != nil {
		return err
	}
	c, err := find(all, args[0])
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
	if err := saveShown(dir, messages.shown); err != nil {
		return err
	}
	return markRead(dir, c)
}

// send sends a message to a peer.
func send(ctx context.Context, args []string) error {
	flags, s := newFlags("send")
	replyTo := flags.String("reply", "", "reply to the message with this id, or the start of it")
	args, literal, err := parseText(flags, args, 1, "reply")
	if err != nil {
		return err
	}
	if len(args) < 1 {
		return errors.New("usage: arc-dm send [flags] <peer> [text...]")
	}
	if err := refuseHelp(args[1:], literal, "send <peer>"); err != nil {
		return err
	}
	if given(flags, "reply") {
		// An empty -reply= must not send a plain message.
		if err := checkID(*replyTo); err != nil {
			return err
		}
	}
	all, err := conversations(ctx, s.arc)
	if err != nil {
		return err
	}
	c, err := find(all, args[0])
	if err != nil {
		return err
	}
	if !given(flags, "reply") {
		text, err := messageText(ctx, args[1:])
		if err != nil {
			return err
		}
		if _, err := s.arc.Send(ctx, c.Peers, text); err != nil {
			return err
		}
		fmt.Printf("queued for %s\n", cmp.Or(c.Name, c.key())) //arc:raw-ok c.Name holds petnames, or a key
		return nil
	}
	// The id is checked before the text, so a wrong id does not wait for
	// standard input.
	messages := index(all)
	if messages.avoid, err = loadShown(stateDir(s.state)); err != nil {
		return err
	}
	parent, err := messages.lookup(*replyTo)
	if err != nil {
		return err
	}
	if parent.conv.key() != c.key() {
		return fmt.Errorf("%s is in another conversation: arc-dm reply %s sends to its members", messages.short(parent.ID), messages.short(parent.ID))
	}
	text, err := messageText(ctx, args[1:])
	if err != nil {
		return err
	}
	return sendReply(ctx, s.arc, messages, parent, text)
}

// reply sends a reply to a message, to each member of its conversation.
func reply(ctx context.Context, args []string) error {
	flags, s := newFlags("reply")
	args, literal, err := parseText(flags, args, 1)
	if err != nil {
		return err
	}
	if len(args) < 1 {
		return errors.New("usage: arc-dm reply [flags] <id> [text...]")
	}
	if err := refuseHelp(args[1:], literal, "reply <id>"); err != nil {
		return err
	}
	if err := checkID(args[0]); err != nil {
		return err
	}
	all, err := conversations(ctx, s.arc)
	if err != nil {
		return err
	}
	messages := index(all)
	if messages.avoid, err = loadShown(stateDir(s.state)); err != nil {
		return err
	}
	parent, err := messages.lookup(args[0])
	if err != nil {
		return err
	}
	text, err := messageText(ctx, args[1:])
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
	args, err := parse(flags, args, 1)
	if err != nil {
		return err
	}
	if len(args) > 1 {
		return errors.New("usage: arc-dm read [flags] [peer]")
	}
	all, err := conversations(ctx, s.arc)
	if err != nil {
		return err
	}
	if len(args) == 1 {
		c, err := find(all, args[0])
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
	args, err := parse(flags, args, 0)
	if err != nil {
		return err
	}
	if len(args) != 0 {
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
	w := &watcher{
		outbox:   s.arc.Outbox,
		me:       me,
		messages: index(all),
		missed:   map[string]bool{},
		bySender: map[string][]int{},
		last:     time.Now(), // conversations read the outbox
	}
	if second, ok := ctx.Value(secondSignal{}).(context.Context); ok {
		w.closeCtx = second
	}
	w.print = func(m arcprogram.Message, messages *store) {
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
		e := entry{ID: m.ID, At: m.At, Text: m.Text, From: m.Name, ReplyTo: m.ReplyTo}
		messages.add(e, c)
		fmt.Printf("%s  %s  %s%s\n", stamp(m.At), messages.short(m.ID), m.Name, group) //arc:raw-ok m.Name is a petname; the id is hex
		if q := messages.quote(e, c); q != "" {
			fmt.Println(q)
		}
		fmt.Println(indent(terminal.Escape(m.Text)))
		if len(notifier) == 0 {
			return
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
	}
	// The ids that the watch shows as missing go to the state at most once
	// a second, and at its end, so a reply in another terminal refuses them.
	var lastSave time.Time
	save := func(force bool) {
		if !force && time.Since(lastSave) < time.Second {
			return
		}
		w.mu.Lock()
		ids := w.messages.shown
		w.messages.shown, w.messages.isShown = nil, nil
		w.mu.Unlock()
		lastSave = time.Now()
		if err := saveShown(stateDir(s.state), ids); err != nil {
			fmt.Fprintln(os.Stderr, "arc-dm:", terminal.EscapeLine(err.Error()))
		}
	}
	err = s.arc.Watch(ctx, arcprogram.WatchOptions{Notes: os.Stderr}, func(m arcprogram.Message) error {
		w.take(m)
		save(false)
		return nil
	})
	w.close()
	save(true)
	return err
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
