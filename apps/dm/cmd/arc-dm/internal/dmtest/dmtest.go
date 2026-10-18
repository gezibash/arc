// Package dmtest runs arc-dm as a person does, for the tests of arc-dm: it
// builds arc and arc-dm, runs a relay on this machine, and makes citizens.
package dmtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/gezibash/arc/sdk/arcprogram"
	"github.com/gezibash/arc/sdk/arcprogram/arcprogramtest"
	"github.com/gezibash/arc/sdk/terminal"
)

var (
	net     *arcprogramtest.Net
	program string
)

// Main builds arc-dm and starts a relay, runs the tests, and stops the
// relay. A test package of arc-dm calls it from TestMain.
func Main(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "arc-dm-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, terminal.EscapeLine(err.Error()))
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	program = filepath.Join(dir, "arc-dm")
	if out, err := exec.Command("go", "build", "-o", program, "github.com/gezibash/arc/apps/dm/cmd/arc-dm").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "go build arc-dm: %s\n%s", terminal.EscapeLine(err.Error()), terminal.Escape(string(out)))
		return 1
	}
	if net, err = arcprogramtest.Start(dir); err != nil {
		fmt.Fprintln(os.Stderr, terminal.EscapeLine(err.Error()))
		return 1
	}
	defer net.Close()
	return m.Run()
}

// Program is the path of the arc-dm that Main built.
func Program() string { return program }

// RefreshEvery is refreshEvery of apps/dm/cmd/arc-dm/main.go: a watch
// reads the outbox at most once each RefreshEvery. A test in package main
// checks that the two agree.
const RefreshEvery = 5 * time.Second

// FakeID is an id that no message has: the hash of n.
func FakeID(n int) string {
	sum := sha256.Sum256([]byte(fmt.Sprint("fake", n)))
	return hex.EncodeToString(sum[:])
}

// Person is one citizen, with its own read state.
type Person struct {
	t     *testing.T
	Arc   arcprogram.Arc
	Key   string
	Name  string
	State string
}

func NewPerson(t *testing.T, name string) Person {
	t.Helper()
	arc, key, err := net.Citizen(name)
	if err != nil {
		t.Fatal(err)
	}
	out, err := arc.Command(context.Background(), "whoami").Output()
	if err != nil {
		t.Fatal(err)
	}
	// The first line of arc whoami is the petname.
	petname, _, _ := strings.Cut(string(out), "\n")
	return Person{t: t, Arc: arc, Key: key, Name: petname, State: t.TempDir()}
}

func (p Person) Command(args ...string) *exec.Cmd {
	flags := []string{"-arc", p.Arc.Program, "-home", p.Arc.Home, "-state", p.State}
	return exec.Command(Program(), append(append(args[:1:1], flags...), args[1:]...)...)
}

// DM runs arc-dm and returns its standard output. The command must work.
func (p Person) DM(args ...string) string {
	p.t.Helper()
	var stderr bytes.Buffer
	command := p.Command(args...)
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		p.t.Fatalf("arc-dm %s: %s\n%s", terminal.EscapeLine(strings.Join(args, " ")), terminal.EscapeLine(err.Error()), terminal.Escape(stderr.String()))
	}
	return string(out)
}

// Refused runs arc-dm, and returns its standard error. The command must fail.
func (p Person) Refused(args ...string) string {
	p.t.Helper()
	out, err := p.Command(args...).CombinedOutput()
	if err == nil {
		p.t.Fatalf("arc-dm %s did not fail:\n%s", strings.Join(args, " "), out)
	}
	return string(out)
}

func (p Person) Sync() {
	p.t.Helper()
	if out, err := p.Arc.Command(context.Background(), "sync").CombinedOutput(); err != nil {
		p.t.Fatalf("arc sync: %s\n%s", terminal.EscapeLine(err.Error()), terminal.Escape(string(out)))
	}
}

// Block is one conversation of arc-dm ls: its header and its preview.
type Block struct{ Header, Preview string }

func (p Person) List() []Block {
	p.t.Helper()
	lines := strings.Split(strings.TrimRight(p.DM("ls"), "\n"), "\n")
	if len(lines)%2 != 0 {
		p.t.Fatalf("arc-dm ls printed %d lines, want two for each conversation:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	var blocks []Block
	for i := 0; i < len(lines); i += 2 {
		blocks = append(blocks, Block{lines[i], strings.TrimSpace(lines[i+1])})
	}
	return blocks
}

// WaitLimit bounds each wait for a watch. The tests run in parallel, and
// under the race detector each arc program starts slowly, so a wait can
// take far longer than on an idle machine. A wait ends as soon as it sees
// what it waits for.
const WaitLimit = 90 * time.Second

// The time of a message has a resolution of one second, so a test waits
// a second where the order of two messages matters.
func NextSecond() { time.Sleep(1100 * time.Millisecond) }

// GroupName is the name of the conversation of some peers, as one member
// sees it: the names in the order of the keys.
func GroupName(peers ...Person) string {
	slices.SortFunc(peers, func(a, b Person) int { return strings.Compare(a.Key, b.Key) })
	names := make([]string, len(peers))
	for i, p := range peers {
		names[i] = p.Name
	}
	return strings.Join(names, ", ")
}

func Must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func AssertInOrder(t *testing.T, text string, parts []string) {
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

type LockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *LockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *LockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// GroupNames is the names of the peers, sorted by key and separated by
// commas, as arc-dm open takes them.
func GroupNames(peers ...Person) string {
	return strings.ReplaceAll(GroupName(peers...), ", ", ",")
}

// Finish waits at most WaitLimit for a command to end, and kills it after
// that.
// It returns false if the command did not end by itself.
func Finish(t *testing.T, command *exec.Cmd) (ended bool) {
	t.Helper()
	done := make(chan struct{})
	go func() { _ = command.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(WaitLimit):
		_ = command.Process.Kill()
		<-done
		return false
	}
}

// ShortID is the short id that a conversation of arc-dm open shows in the
// header of the message with this first line.
func ShortID(t *testing.T, conv, text string) string {
	t.Helper()
	lines := strings.Split(conv, "\n")
	for i := 1; i < len(lines); i++ {
		if lines[i] == "  "+text {
			for j := i - 1; j >= 0; j-- {
				if fields := strings.Split(lines[j], "  "); len(fields) >= 3 && !strings.HasPrefix(lines[j], " ") && !strings.HasPrefix(lines[j], "↳") {
					return fields[1]
				}
			}
		}
	}
	t.Fatalf("no message %q in:\n%s", text, conv)
	return ""
}

// Quotes are the lines of a conversation that start at column 0 with ↳.
func Quotes(conv string) []string {
	var out []string
	for line := range strings.SplitSeq(conv, "\n") {
		if strings.HasPrefix(line, "↳") {
			out = append(out, line)
		}
	}
	return out
}

// ArcSend runs arc message send as a sender that arc-dm does not check,
// with the tags of a test build of arc.
func (p Person) ArcSend(to, text string, flags ...string) {
	p.t.Helper()
	args := append(append([]string{"message", "send"}, flags...), to, text)
	if out, err := p.Arc.Command(context.Background(), args...).CombinedOutput(); err != nil {
		p.t.Fatalf("arc %s: %s\n%s", terminal.EscapeLine(strings.Join(args, " ")), terminal.EscapeLine(err.Error()), terminal.Escape(string(out)))
	}
}

func (p Person) InboxID(text string) string {
	p.t.Helper()
	for _, m := range Must(p.Arc.Inbox(context.Background())) {
		if m.Text == text {
			return m.ID
		}
	}
	p.t.Fatalf("no message %s in the inbox", terminal.EscapeLine(text))
	return ""
}

// Arcs counts the arc processes of an arc home.
func Arcs(home string) int {
	out, _ := exec.Command("pgrep", "-f", home).Output()
	return len(strings.Fields(string(out)))
}

// NoArcLeft waits for each arc process of an arc home to end.
func NoArcLeft(t *testing.T, home string) {
	t.Helper()
	if !arcprogramtest.WaitUntil(10*time.Second, func() bool { return Arcs(home) == 0 }) {
		out, _ := exec.Command("pgrep", "-fl", home).Output()
		t.Errorf("arc processes of the home are still running:\n%s", out)
		_ = exec.Command("pkill", "-f", home).Run()
	}
}

// OnTerminal starts arc-dm on a pseudo-terminal, as a person does, and
// gathers what the terminal shows.
func OnTerminal(t *testing.T, args ...string) (*exec.Cmd, *os.File, *LockedBuffer) {
	t.Helper()
	command := exec.Command(Program(), args...)
	terminal, err := pty.Start(command)
	if err != nil {
		t.Fatal(err)
	}
	out := &LockedBuffer{}
	go func() { _, _ = io.Copy(out, terminal) }()
	t.Cleanup(func() { _ = command.Process.Kill(); _ = terminal.Close() })
	return command, terminal, out
}
