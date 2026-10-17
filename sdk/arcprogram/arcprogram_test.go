package arcprogram_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
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
