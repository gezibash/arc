package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gezibash/arc/sdk/arcprogram"
	"github.com/gezibash/arc/sdk/arcprogram/arcprogramtest"
)

// The tests build the arc program and this program, and run them as a
// person does: a relay on this machine, and new citizens for each test.
var env struct {
	net     *arcprogramtest.Net
	program string
}

func TestMain(m *testing.M) {
	os.Exit(setup(m))
}

func setup(m *testing.M) int {
	dir, err := os.MkdirTemp("", "arc-dm-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	env.program = filepath.Join(dir, "arc-dm")
	if out, err := exec.Command("go", "build", "-o", env.program, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "go build arc-dm: %v\n%s", err, out)
		return 1
	}
	if env.net, err = arcprogramtest.Start(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer env.net.Close()
	return m.Run()
}

// person is one citizen, with its own read state.
type person struct {
	t     *testing.T
	arc   arcprogram.Arc
	key   string
	name  string
	state string
}

func newPerson(t *testing.T, name string) person {
	t.Helper()
	arc, key, err := env.net.Citizen(name)
	if err != nil {
		t.Fatal(err)
	}
	out, err := arc.Command(context.Background(), "whoami").Output()
	if err != nil {
		t.Fatal(err)
	}
	// The first line of arc whoami is the petname.
	petname, _, _ := strings.Cut(string(out), "\n")
	return person{t: t, arc: arc, key: key, name: petname, state: t.TempDir()}
}

func (p person) command(args ...string) *exec.Cmd {
	flags := []string{"-arc", p.arc.Program, "-home", p.arc.Home, "-state", p.state}
	return exec.Command(env.program, append(append(args[:1:1], flags...), args[1:]...)...)
}

// dm runs arc-dm and returns its standard output. The command must work.
func (p person) dm(args ...string) string {
	p.t.Helper()
	var stderr bytes.Buffer
	command := p.command(args...)
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		p.t.Fatalf("arc-dm %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}

// refused runs arc-dm, and returns its standard error. The command must fail.
func (p person) refused(args ...string) string {
	p.t.Helper()
	out, err := p.command(args...).CombinedOutput()
	if err == nil {
		p.t.Fatalf("arc-dm %s did not fail:\n%s", strings.Join(args, " "), out)
	}
	return string(out)
}

func (p person) sync() {
	p.t.Helper()
	if out, err := p.arc.Command(context.Background(), "sync").CombinedOutput(); err != nil {
		p.t.Fatalf("arc sync: %v\n%s", err, out)
	}
}

// block is one conversation of arc-dm ls: its header and its preview.
type block struct{ header, preview string }

func (p person) list() []block {
	p.t.Helper()
	lines := strings.Split(strings.TrimRight(p.dm("ls"), "\n"), "\n")
	if len(lines)%2 != 0 {
		p.t.Fatalf("arc-dm ls printed %d lines, want two for each conversation:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	var blocks []block
	for i := 0; i < len(lines); i += 2 {
		blocks = append(blocks, block{lines[i], strings.TrimSpace(lines[i+1])})
	}
	return blocks
}

// The time of a message has a resolution of one second, so a test waits
// a second where the order of two messages matters.
func nextSecond() { time.Sleep(1100 * time.Millisecond) }

// TestConversations groups the messages by peer, with the replies of this
// citizen, and counts the unread messages until the citizen opens them.
func TestConversations(t *testing.T) {
	ada, bob, cy := newPerson(t, "ada"), newPerson(t, "bob"), newPerson(t, "cy")

	ada.dm("send", bob.key, "meet at noon")
	nextSecond()
	ada.dm("send", bob.key, "bring the map")
	// A message that arc message sent, and not arc-dm, is in the conversation too.
	if _, err := cy.arc.Send(context.Background(), bob.key, "hello bob"); err != nil {
		t.Fatal(err)
	}
	nextSecond()
	bob.sync()
	bob.dm("send", ada.name, "see you there")

	got := bob.list()
	want := []block{
		{"  " + ada.name + "  2 unread", "you: see you there"},
		{"  " + cy.name + "  1 unread", "hello bob"},
	}
	if len(got) != len(want) {
		t.Fatalf("bob has %d conversations, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		// The header starts with the time of the newest message.
		if !strings.HasSuffix(got[i].header, want[i].header) || got[i].preview != want[i].preview {
			t.Errorf("conversation %d is %q, want %q after the time", i, got[i], want[i])
		}
	}

	conv := bob.dm("open", ada.name)
	order := []string{
		ada.name + "  new\n  meet at noon\n",
		ada.name + "  new\n  bring the map\n",
		"you  pending\n  see you there\n",
	}
	assertInOrder(t, conv, order)

	got = bob.list()
	if strings.Contains(got[0].header, "unread") || !strings.HasSuffix(got[1].header, cy.name+"  1 unread") {
		t.Fatalf("after bob opened ada, the conversations are %q; want ada read and cy with 1 unread", got)
	}
	if again := bob.dm("open", ada.name); strings.Contains(again, "new") {
		t.Errorf("the second open still shows new messages:\n%s", again)
	}

	// A message that arrives after the open is unread.
	nextSecond()
	ada.dm("send", bob.key, "one more thing")
	bob.sync()
	got = bob.list()
	if !strings.HasSuffix(got[0].header, ada.name+"  1 unread") || got[0].preview != "one more thing" {
		t.Errorf("after a new message, ada's conversation is %q, want 1 unread", got[0])
	}

	bob.dm("read")
	for _, b := range bob.list() {
		if strings.Contains(b.header, "unread") {
			t.Errorf("after arc-dm read, %q is still unread", b.header)
		}
	}
}

// TestPeers selects a conversation by the start of a key, and refuses a
// name that selects no conversation or more than one.
func TestPeers(t *testing.T) {
	ada, bob, cy := newPerson(t, "ada"), newPerson(t, "bob"), newPerson(t, "cy")
	ada.dm("send", bob.key, "from ada")
	cy.dm("send", bob.key, "from cy")
	bob.sync()

	if conv := bob.dm("open", ada.key[:10]); !strings.Contains(conv, "from ada") || strings.Contains(conv, "from cy") {
		t.Errorf("open with the start of ada's key printed:\n%s", conv)
	}
	if out := bob.refused("open", "nobody"); !strings.Contains(out, `no conversation with "nobody"`) {
		t.Errorf("open nobody said:\n%s", out)
	}
	// Each name starts with the empty text.
	if out := bob.refused("send", "", "hello"); !strings.Contains(out, "names 2 conversations") {
		t.Errorf("send to a text that names two conversations said:\n%s", out)
	}
}

// TestControlCharacters shows no control character of a sender on the
// terminal.
func TestControlCharacters(t *testing.T) {
	ada, bob := newPerson(t, "ada"), newPerson(t, "bob")
	ada.dm("send", bob.key, "\x1b[2Jcleared\x07\nline two\tend")
	bob.sync()
	for _, out := range []string{bob.dm("ls"), bob.dm("open", ada.name)} {
		if strings.ContainsAny(out, "\x1b\x07") || !strings.Contains(out, "cleared") {
			t.Errorf("arc-dm printed a control character, or lost the text: %q", out)
		}
	}
	if out := bob.dm("open", ada.name); !strings.Contains(out, "  \ufffd[2Jcleared\ufffd\n  line two\tend\n") {
		t.Errorf("open printed %q, want each control character replaced, and the newline and tab kept", out)
	}
}

// TestWatch prints and notifies a message that arrives while the watch
// runs. The watch keeps the message on this machine, and leaves it unread.
func TestWatch(t *testing.T) {
	ada, bob := newPerson(t, "ada"), newPerson(t, "bob")
	notes := filepath.Join(t.TempDir(), "notifications")
	notifier := filepath.Join(t.TempDir(), "notify")
	script := "#!/bin/sh\nprintf '%s|%s\\n' \"$1\" \"$2\" >> " + notes + "\n"
	if err := os.WriteFile(notifier, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	watch := bob.command("watch", "-notify", notifier)
	stdout, stderr := &lockedBuffer{}, &lockedBuffer{}
	watch.Stdout, watch.Stderr = stdout, stderr
	if err := watch.Start(); err != nil {
		t.Fatal(err)
	}
	defer watch.Process.Kill()
	if !arcprogramtest.WaitUntil(15*time.Second, func() bool { return strings.Contains(stderr.String(), "watching") }) {
		t.Fatalf("the watch did not start:\n%s", stderr.String())
	}

	ada.dm("send", bob.key, "are you there")
	want := ada.name + "|are you there\n"
	if !arcprogramtest.WaitUntil(15*time.Second, func() bool {
		data, _ := os.ReadFile(notes)
		return string(data) == want
	}) {
		data, _ := os.ReadFile(notes)
		t.Fatalf("the notifications are %q, want %q\nwatch:\n%s", data, want, stderr.String())
	}
	if !strings.Contains(stdout.String(), ada.name+"\n  are you there\n") {
		t.Errorf("the watch printed:\n%s", stdout.String())
	}

	// No arc sync: the watch kept the message.
	if got := bob.list(); len(got) != 1 || !strings.HasSuffix(got[0].header, ada.name+"  1 unread") {
		t.Errorf("after the watch, bob's conversations are %q, want ada with 1 unread", got)
	}

	if err := watch.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := watch.Wait(); err != nil {
		t.Errorf("the watch ended with %v after an interrupt, want success\n%s", err, stderr.String())
	}
}

func assertInOrder(t *testing.T, text string, parts []string) {
	t.Helper()
	rest := text
	for _, part := range parts {
		i := strings.Index(rest, part)
		if i < 0 {
			t.Fatalf("%q is missing, or out of order, in:\n%s", part, text)
		}
		rest = rest[i+len(part):]
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
