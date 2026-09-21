package mail_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/mail"
	"github.com/gezibash/arc/delivery/node"
	"github.com/gezibash/arc/delivery/private"
	"github.com/gezibash/arc/delivery/store"
	"github.com/gezibash/arc/delivery/testrelay"
	"github.com/gezibash/arc/delivery/transport"
	"github.com/gezibash/arc/delivery/transport/file"
	"github.com/gezibash/arc/delivery/transport/relay"
)

// citizen is one node with its own key, store and mail.
type citizen struct {
	key  keys.Key
	node *node.Node
	mail *mail.Mail
}

func newCitizen(t *testing.T, relays ...transport.Transport) citizen {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	k := keys.Generate()
	n := &node.Node{Store: s}
	m, err := mail.Open(dir, k, n, relays)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return citizen{key: k, node: n, mail: m}
}

func (c citizen) sync(t *testing.T, tr transport.Transport) mail.Report {
	t.Helper()
	report, err := c.mail.Sync(context.Background(), tr)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func (c citizen) send(t *testing.T, to citizen, text string) {
	t.Helper()
	if _, err := c.mail.Send(context.Background(), to.key.Public, text); err != nil {
		t.Fatal(err)
	}
}

func inboxText(c citizen) []string {
	var out []string
	for _, m := range c.mail.Inbox() {
		out = append(out, m.Text)
	}
	return out
}

func stick(t *testing.T) file.Dir { return file.Dir{Path: t.TempDir()} }

func TestAMessageCrossesARelayAndIsAcknowledged(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t, r), newCitizen(t, r)

	alice.send(t, bob, "hello over the relay")

	got := bob.sync(t, r)
	if got.Received != 1 {
		t.Fatalf("bob received %d messages", got.Received)
	}
	if msgs := bob.mail.Inbox(); len(msgs) != 1 || msgs[0].Text != "hello over the relay" || msgs[0].From != alice.key.Public {
		t.Fatalf("bob's inbox: %+v", msgs)
	}

	back := alice.sync(t, r)
	if back.Delivered != 1 {
		t.Errorf("alice saw %d acknowledgements, want 1", back.Delivered)
	}
	if out := alice.mail.Outbox(); len(out) != 1 || out[0].State(time.Now()) != "delivered" || out[0].Text != "hello over the relay" {
		t.Errorf("alice's outbox: %+v", out)
	}
}

// The proof of phase 2: no relay, and a third machine carries the mail.
func TestAMessageReachesAnOfflineRecipientThroughACourier(t *testing.T) {
	alice, carol, bob := newCitizen(t), newCitizen(t), newCitizen(t)
	first, second := stick(t), stick(t)

	alice.send(t, bob, "carried by hand")
	alice.sync(t, first)

	if got := carol.sync(t, first); got.Carried != 1 {
		t.Fatalf("carol carried %d wraps, want 1", got.Carried)
	}
	if len(carol.mail.Inbox()) != 0 {
		t.Error("carol could read mail that was not for her")
	}
	carol.sync(t, second)

	if got := bob.sync(t, second); got.Received != 1 {
		t.Fatalf("bob received %d messages", got.Received)
	}
	if got := inboxText(bob); len(got) != 1 || got[0] != "carried by hand" {
		t.Fatalf("bob's inbox: %v", got)
	}

	// The acknowledgement goes back the same way.
	bob.sync(t, second)
	carol.sync(t, second)
	carol.sync(t, first)
	if got := alice.sync(t, first); got.Delivered != 1 {
		t.Errorf("alice saw %d acknowledgements, want 1", got.Delivered)
	}
	if out := alice.mail.Outbox(); out[0].State(time.Now()) != "delivered" {
		t.Errorf("the message is %s", out[0].State(time.Now()))
	}
}

func TestAStickHoldsNoRecipientAndNoText(t *testing.T) {
	alice, bob := newCitizen(t), newCitizen(t)
	s := stick(t)

	alice.send(t, bob, "a private word")
	alice.sync(t, s)

	entries, err := os.ReadDir(filepath.Join(s.Path, "events"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("nothing reached the stick")
	}
	for _, entry := range entries {
		body, _ := os.ReadFile(filepath.Join(s.Path, "events", entry.Name()))
		for _, secret := range []string{bob.key.Public.Hex(), alice.key.Public.Hex(), "a private word"} {
			if strings.Contains(string(body), secret) {
				t.Errorf("%s holds %s", entry.Name(), secret)
			}
		}
	}
}

func TestTheHopLimitBoundsTheSpread(t *testing.T) {
	alice, bob := newCitizen(t), newCitizen(t)
	alice.send(t, bob, "far away")

	sticks := []file.Dir{stick(t)}
	alice.sync(t, sticks[0])

	// Each courier reads the last stick and writes a new one.
	carried := []int{}
	for i := 0; i < 4; i++ {
		c := newCitizen(t)
		carried = append(carried, c.sync(t, sticks[len(sticks)-1]).Carried)
		next := stick(t)
		c.sync(t, next)
		sticks = append(sticks, next)
	}

	if fmt.Sprint(carried) != "[1 1 1 0]" {
		t.Errorf("couriers carried %v, want [1 1 1 0]", carried)
	}

	// The last courier that carried still delivers to the recipient.
	if got := bob.sync(t, sticks[3]); got.Received != 1 {
		t.Errorf("bob received %d from the third courier's stick", got.Received)
	}
}

func TestAForgedHopLimitDoesNotSpreadFurther(t *testing.T) {
	alice, bob, carol := newCitizen(t), newCitizen(t), newCitizen(t)
	s := stick(t)
	alice.send(t, bob, "x")
	alice.sync(t, s)

	matches, _ := filepath.Glob(filepath.Join(s.Path, "events", "*.hops"))
	for _, path := range matches {
		if err := os.WriteFile(path, []byte("1000\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	carol.sync(t, s)
	out := stick(t)
	carol.sync(t, out)

	matches, _ = filepath.Glob(filepath.Join(out.Path, "events", "*.hops"))
	for _, path := range matches {
		body, _ := os.ReadFile(path)
		if strings.TrimSpace(string(body)) != fmt.Sprint(mail.StartHops-1) {
			t.Errorf("carol wrote a hop limit of %s, want %d", strings.TrimSpace(string(body)), mail.StartHops-1)
		}
	}
}

func TestAReplayedWrapIsHandledOnce(t *testing.T) {
	alice, bob := newCitizen(t), newCitizen(t)
	s := stick(t)
	alice.send(t, bob, "once")
	alice.sync(t, s)

	first := bob.sync(t, s)
	second := bob.sync(t, s)
	if first.Received != 1 || second.Received != 0 {
		t.Errorf("received %d then %d, want 1 then 0", first.Received, second.Received)
	}
	if len(bob.mail.Inbox()) != 1 {
		t.Errorf("the inbox holds %d messages, want 1", len(bob.mail.Inbox()))
	}

	// One acknowledgement, in its two forms, and no more.
	acks := bob.node.Store.Query(nostr.Filter{Kinds: []nostr.Kind{private.WrapKind}})
	if len(acks) != 2 {
		t.Errorf("bob holds %d wraps, want the two forms of one acknowledgement", len(acks))
	}
}

// Carol meets more senders than she has room for, each with their own stick.
func TestACourierCarriesAtMostItsShare(t *testing.T) {
	carol := newCitizen(t)
	recipient := newCitizen(t)

	for i := 0; i < mail.MaxCarried+5; i++ {
		sender := newCitizen(t)
		sender.send(t, recipient, fmt.Sprintf("message %d", i))
		s := stick(t)
		sender.sync(t, s)
		carol.sync(t, s)
	}

	if got := carol.mail.Carrying(); got != mail.MaxCarried {
		t.Errorf("carol carries %d wraps, want %d", got, mail.MaxCarried)
	}
}

func TestAChangedWrapIsRefused(t *testing.T) {
	alice, bob := newCitizen(t), newCitizen(t)
	s := stick(t)
	alice.send(t, bob, "x")
	alice.sync(t, s)

	matches, _ := filepath.Glob(filepath.Join(s.Path, "events", "*.json"))
	for _, path := range matches {
		body, _ := os.ReadFile(path)
		changed := strings.Replace(string(body), `"content":"`, `"content":"A`, 1)
		if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got := bob.sync(t, s)
	if got.Received != 0 || len(got.Refused) == 0 {
		t.Errorf("received %d and refused %d, want 0 and some", got.Received, len(got.Refused))
	}
}
