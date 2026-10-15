package arcprogram_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
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
		queued, err := ada.Send(ctx, bobKey, text)
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

func TestSendRefusesANameAsTheRecipient(t *testing.T) {
	ada, _ := citizen(t, "ada")
	if _, err := ada.Send(context.Background(), "bob", "hello"); err == nil {
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
	if _, err := ada.Send(context.Background(), bobKey, "are you there"); err != nil {
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
