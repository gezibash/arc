package arcprogram_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gezibash/arc/sdk/arcprogram"
	"github.com/gezibash/arc/sdk/arcprogram/arcprogramtest"
)

var net *arcprogramtest.Net

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "arcprogram-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	net, err = arcprogramtest.Start(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	net.Close()
	os.RemoveAll(dir)
	os.Exit(code)
}

func citizen(t *testing.T, name string) (arcprogram.Arc, string) {
	t.Helper()
	arc, key, err := net.Citizen(name)
	if err != nil {
		t.Fatal(err)
	}
	return arc, key
}

// TestMessages sends two messages and reads them back on both sides. The
// second text starts with a dash, which arc must not read as a flag.
func TestMessages(t *testing.T) {
	ctx := context.Background()
	ada, adaKey := citizen(t, "ada")
	bob, bobKey := citizen(t, "bob")
	texts := []string{"meet at noon", "-v is not a flag"}
	for _, text := range texts {
		queued, err := ada.Send(ctx, []string{bobKey}, text)
		if err != nil {
			t.Fatal(err)
		}
		if queued.To != bobKey || !arcprogram.IsKey(queued.ID) {
			t.Fatalf("queued %+v, want a message to %s", queued, bobKey)
		}
	}
	if out, err := bob.Command(ctx, "sync").CombinedOutput(); err != nil {
		t.Fatalf("arc sync: %v\n%s", err, out)
	}

	inbox, err := bob.Inbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 2 {
		t.Fatalf("bob has %d messages, want 2: %+v", len(inbox), inbox)
	}
	for _, m := range inbox {
		if m.From != adaKey || m.Name == "" || m.At.IsZero() {
			t.Errorf("message %+v: want one from %s with a name and a time", m, adaKey)
		}
	}
	if got := []string{inbox[0].Text, inbox[1].Text}; !sameSet(got, texts) {
		t.Errorf("bob read %q, want %q", got, texts)
	}

	outbox, err := ada.Outbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(outbox) != 2 {
		t.Fatalf("ada sent %d messages, want 2: %+v", len(outbox), outbox)
	}
	for _, s := range outbox {
		if s.To != bobKey || s.Name == "" || s.At.IsZero() {
			t.Errorf("sent %+v: want one to %s with a name and a time", s, bobKey)
		}
		if s.State != "pending" && s.State != "delivered" {
			t.Errorf("sent %+v: the state is %q, want pending or delivered", s, s.State)
		}
	}
	if got := []string{outbox[0].Text, outbox[1].Text}; !sameSet(got, texts) {
		t.Errorf("ada sent %q, want %q", got, texts)
	}
	// Bob sent nothing, so his outbox is empty.
	if sent, err := bob.Outbox(ctx); err != nil || len(sent) != 0 {
		t.Errorf("bob's outbox: %+v, %v; want it empty", sent, err)
	}
}

// TestGroupMessages sends one message to two citizens. Each line names the
// three members, and the outbox has one line for each recipient.
func TestGroupMessages(t *testing.T) {
	ctx := context.Background()
	ada, adaKey := citizen(t, "ada")
	bob, bobKey := citizen(t, "bob")
	cy, cyKey := citizen(t, "cy")
	members := []string{adaKey, bobKey, cyKey}

	queued, err := ada.Send(ctx, []string{bobKey, cyKey}, "hello, you two")
	if err != nil {
		t.Fatal(err)
	}
	if queued.To != bobKey || !slices.Equal(queued.Members, members) {
		t.Fatalf("queued %+v, want a message to %s with members %v", queued, bobKey, members)
	}
	for _, member := range []arcprogram.Arc{bob, cy} {
		if out, err := member.Command(ctx, "sync").CombinedOutput(); err != nil {
			t.Fatalf("arc sync: %v\n%s", err, out)
		}
		inbox, err := member.Inbox(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(inbox) != 1 || inbox[0].ID != queued.ID || !slices.Equal(inbox[0].Members, members) {
			t.Errorf("the inbox of a member: %+v, want message %s with members %v", inbox, queued.ID, members)
		}
	}

	outbox, err := ada.Outbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var to []string
	for _, s := range outbox {
		if s.ID != queued.ID || !slices.Equal(s.Members, members) {
			t.Errorf("sent %+v, want message %s with members %v", s, queued.ID, members)
		}
		to = append(to, s.To)
	}
	if !sameSet(to, []string{bobKey, cyKey}) {
		t.Errorf("the outbox has lines to %v, want one to each of bob and cy", to)
	}
}

func TestSendRefusesANameAsTheRecipient(t *testing.T) {
	ada, _ := citizen(t, "ada")
	if _, err := ada.Send(context.Background(), []string{"bob"}, "hello"); err == nil {
		t.Fatal("Send to a name did not fail")
	}
}

// TestWatch gives a message that arrives while the watch runs, and returns
// with no error when the context ends.
func TestWatch(t *testing.T) {
	ada, adaKey := citizen(t, "ada")
	bob, bobKey := citizen(t, "bob")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	notes := &lockedBuffer{}
	got := make(chan arcprogram.Message, 4)
	done := make(chan error, 1)
	go func() {
		done <- bob.Watch(ctx, arcprogram.WatchOptions{Notes: notes}, func(m arcprogram.Message) error {
			got <- m
			return nil
		})
	}()
	if !arcprogramtest.WaitUntil(15*time.Second, func() bool { return strings.Contains(notes.String(), "watching") }) {
		t.Fatalf("the watch did not start:\n%s", notes.String())
	}
	if _, err := ada.Send(context.Background(), []string{bobKey}, "are you there"); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m.From != adaKey || m.Text != "are you there" {
			t.Fatalf("the watch gave %+v, want the message of ada", m)
		}
	case err := <-done:
		t.Fatalf("the watch stopped: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatalf("the watch gave no message in 15 s:\n%s", notes.String())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the watch returned %v after the context ended, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the watch did not return after the context ended")
	}
}

// TestWatchFailsWithABadSince returns what arc says about a bad --since.
func TestWatchFailsWithABadSince(t *testing.T) {
	bob, _ := citizen(t, "bob")
	err := bob.Watch(context.Background(), arcprogram.WatchOptions{Since: "yesterday"}, func(arcprogram.Message) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "yesterday") {
		t.Fatalf("Watch with --since yesterday returned %v, want the refusal of arc", err)
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	count := map[string]int{}
	for _, s := range a {
		count[s]++
	}
	for _, s := range b {
		count[s]--
	}
	for _, n := range count {
		if n != 0 {
			return false
		}
	}
	return true
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

// TestReplies sends a reply with SendReply. The queued line, the message
// line of the recipient and the sent line of the sender name the parent;
// the plain parent names none. SendReply refuses a parent that is not an
// id before it runs arc.
func TestReplies(t *testing.T) {
	ctx := context.Background()
	ada, adaKey := citizen(t, "ada")
	bob, bobKey := citizen(t, "bob")
	if _, err := ada.Send(ctx, []string{bobKey}, "bring the map"); err != nil {
		t.Fatal(err)
	}
	if out, err := bob.Command(ctx, "sync").CombinedOutput(); err != nil {
		t.Fatalf("arc sync: %v\n%s", err, out)
	}
	inbox, err := bob.Inbox(ctx)
	if err != nil || len(inbox) != 1 || inbox[0].ReplyTo != "" {
		t.Fatalf("bob's inbox is %+v, %v, want one plain message", inbox, err)
	}
	parent := inbox[0].ID

	if _, err := bob.SendReply(ctx, []string{adaKey}, parent[:8], "yes"); err == nil || !strings.Contains(err.Error(), "64 lowercase hex digits") {
		t.Errorf("SendReply with a short id: %v", err)
	}
	queued, err := bob.SendReply(ctx, []string{adaKey}, parent, "yes")
	if err != nil || queued.ReplyTo != parent {
		t.Fatalf("SendReply queued %+v, %v, want the parent %s", queued, err, parent)
	}
	if out, err := ada.Command(ctx, "sync").CombinedOutput(); err != nil {
		t.Fatalf("arc sync: %v\n%s", err, out)
	}
	got, err := ada.Inbox(ctx)
	if err != nil || len(got) != 1 || got[0].Text != "yes" || got[0].ReplyTo != parent {
		t.Errorf("ada's inbox is %+v, %v, want the reply to %s", got, err, parent)
	}
	sent, err := bob.Outbox(ctx)
	if err != nil || len(sent) != 1 || sent[0].ReplyTo != parent {
		t.Errorf("bob's outbox is %+v, %v, want the reply to %s", sent, err, parent)
	}
	plain, err := ada.Outbox(ctx)
	if err != nil || len(plain) != 1 || plain[0].ReplyTo != "" {
		t.Errorf("ada's outbox is %+v, %v, want one plain message", plain, err)
	}
}

// On Linux, arc dies with the thread that starts it. A goroutine that
// holds a thread and returns ends that thread, so the watch holds the
// thread that started its arc until arc ends: a healthy watch lives on
// while other threads end. Which threads end is chance, so the test runs
// four watches; with the thread let go after the start, at least one
// dies in about 97 runs of 100.
func TestAWatchOutlivesTheEndOfOtherThreads(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux kills a child when the thread that started it ends")
	}
	ada, _ := citizen(t, "ada")
	bob, bobKey := citizen(t, "bob")
	for round := range 4 {
		watchOutlivesThreads(t, ada, bob, bobKey, fmt.Sprint("still there ", round))
	}
}

func watchOutlivesThreads(t *testing.T, ada, bob arcprogram.Arc, bobKey, text string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	notes := &lockedBuffer{}
	got := make(chan arcprogram.Message, 8)
	ended := make(chan error, 1)
	go func() {
		ended <- bob.Watch(ctx, arcprogram.WatchOptions{Notes: notes}, func(m arcprogram.Message) error {
			got <- m
			return nil
		})
	}()
	if !arcprogramtest.WaitUntil(30*time.Second, func() bool { return strings.Contains(notes.String(), "watching") }) {
		t.Fatalf("the watch did not start:\n%s", notes.String())
	}
	// Each of these goroutines ends the thread that it holds.
	for range 5 {
		var wg sync.WaitGroup
		for range 500 {
			wg.Go(func() { runtime.LockOSThread() })
		}
		wg.Wait()
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := ada.Send(ctx, []string{bobKey}, text); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case m := <-got:
			if m.Text == text {
				return
			}
		case err := <-ended:
			t.Fatalf("the watch ended: %v\n%s", err, notes.String())
		case <-time.After(30 * time.Second):
			t.Fatalf("the watch gave no message:\n%s", notes.String())
		}
	}
}

// wrapper is an arc that runs script before the arc of a, in dir.
func wrapper(t *testing.T, a arcprogram.Arc, script string) arcprogram.Arc {
	t.Helper()
	path := filepath.Join(t.TempDir(), "arc")
	body := "#!/bin/sh\n" + script + "\nexec " + a.Program + " " + `"$@"` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return arcprogram.Arc{Program: path, Home: a.Home}
}

// A call returns when arc ends, even if a child of arc holds its output
// open: here a sleep that the wrapper of arc leaves behind.
func TestACallEndsWhenArcEnds(t *testing.T) {
	ada, adaKey := citizen(t, "ada")
	sleep := fmt.Sprintf("%d.%d", 300+os.Getpid()%30, time.Now().Nanosecond()%1000)
	defer func() { _ = exec.Command("pkill", "-f", "sleep "+sleep).Run() }()
	slow := wrapper(t, ada, "sleep "+sleep+" &")
	start := time.Now()
	key, err := slow.PublicKey(context.Background())
	if took := time.Since(start); err != nil || key != adaKey || took > 15*time.Second {
		t.Errorf("PublicKey gave %q, %v after %v, want %s at once", key, err, took, adaKey)
	}
}

// On Linux, arc dies with the thread that starts it. A call holds that
// thread until arc ends, so a call lives on while other threads end. With
// one P, the call waits after the start of arc while new goroutines hold
// their threads and return: the first runs on the thread of the call
// unless the call holds it, and the end of that thread kills arc.
func TestACallOutlivesTheEndOfOtherThreads(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux kills a child when the thread that started it ends")
	}
	ada, adaKey := citizen(t, "ada")
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	defer arcprogram.SetStarted(func() {
		// More than one goroutine, so that idle threads end too.
		for range 16 {
			ended := make(chan struct{})
			go func() {
				runtime.LockOSThread()
				close(ended)
			}()
			<-ended
		}
	})()
	// The first call of a process can miss, as the runtime starts its
	// threads.
	for range 3 {
		key, err := ada.PublicKey(context.Background())
		if err != nil || key != adaKey {
			t.Fatalf("PublicKey gave %q, %v after another thread ended", key, err)
		}
	}
}

// A function of a watch that panics, with a caller that recovers, or that
// ends its goroutine with runtime.Goexit, as t.FailNow does, leaves no arc
// behind.
func TestAPanicInAWatchEndsItsArc(t *testing.T) {
	for _, how := range []string{"panic", "Goexit"} {
		t.Run(how, func(t *testing.T) {
			ada, _ := citizen(t, "ada")
			bob, bobKey := citizen(t, "bob")
			notes := &lockedBuffer{}
			ended := make(chan any, 1)
			go func() {
				defer func() { ended <- recover() }()
				_ = bob.Watch(context.Background(), arcprogram.WatchOptions{Notes: notes}, func(arcprogram.Message) error {
					if how == "Goexit" {
						runtime.Goexit()
					}
					panic("the function of the watch")
				})
			}()
			if !arcprogramtest.WaitUntil(30*time.Second, func() bool { return strings.Contains(notes.String(), "watching") }) {
				t.Fatalf("the watch did not start:\n%s", notes.String())
			}
			if _, err := ada.Send(context.Background(), []string{bobKey}, how); err != nil {
				t.Fatal(err)
			}
			select {
			case r := <-ended:
				if (r == nil) != (how == "Goexit") {
					t.Fatalf("the goroutine of the watch ended with %v", r)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the function of the watch did not end it")
			}
			if out, _ := exec.Command("pgrep", "-f", bob.Home+" message watch").Output(); len(out) > 0 {
				t.Errorf("arc message watch runs on after a %s: %s", how, out)
				_ = exec.Command("pkill", "-f", bob.Home+" message watch").Run()
			}
		})
	}
}

// fakeArc is an arc program that runs script and nothing else.
func fakeArc(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "arc")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// arc warns of two homes at each command. A client gets the warning once
// in a program, and an error that follows a warning is one line, with no
// warning in it.
func TestAWarningOfArcShowsOnceAndAnErrorIsOneLine(t *testing.T) {
	// arc escaped the newline of the path itself: the client keeps that
	// escape as it is, and does not escape it twice.
	const warning = `arc: warning: both /h\narc: x/.arc and /h/.config/arc hold an identity. arc uses /h\narc: x/.arc, because it holds an identity`
	key := strings.Repeat("ab", 32)
	program := fakeArc(t, `printf '%s\n' '`+warning+`' >&2
if [ -n "$FAIL" ]; then echo "arc: no identity in /h/.arc" >&2; echo "chosen by ~/.arc" >&2; exit 1; fi
echo `+key)
	var warnings strings.Builder
	a := arcprogram.Arc{Program: program, Warnings: &warnings}
	for range 3 {
		if got, err := a.PublicKey(t.Context()); err != nil || got != key {
			t.Fatalf("PublicKey: %q, %v", got, err)
		}
	}
	if warnings.String() != warning+"\n" {
		t.Errorf("three calls warned %q, want the warning once", warnings.String())
	}
	t.Setenv("FAIL", "1")
	_, err := a.PublicKey(t.Context())
	if err == nil || err.Error() != "arc whoami: arc: no identity in /h/.arc; chosen by ~/.arc" {
		t.Errorf("the error is %q, want the lines of arc on one line, with no warning", err)
	}
}
