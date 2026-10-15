package mail_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/mailbox"
	boltstore "github.com/gezibash/arc/adapters/store/bolt"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/mail"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/core/transport"
	"github.com/gezibash/arc/internal/testrelay"
	"github.com/gezibash/arc/internal/testutil"
)

func (c citizen) sendGroup(t *testing.T, text string, to ...citizen) []mail.Outgoing {
	t.Helper()
	var keys []nostr.PubKey
	for _, member := range to {
		keys = append(keys, member.key.Public)
	}
	out, err := c.mail.Send(context.Background(), keys, text)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// states maps each recipient of the outbox to the state of its entry.
func states(t *testing.T, c citizen) map[nostr.PubKey]string {
	t.Helper()
	got := map[nostr.PubKey]string{}
	for _, o := range testutil.Must(c.mail.Outbox(context.Background())) {
		got[testutil.Must(nostr.PubKeyFromHex(o.To))] = o.State(time.Now())
	}
	return got
}

// A group message reaches two members over a relay. Each member sees the
// three keys, and a reply from one member reaches the other two.
func TestAGroupMessageReachesEachMember(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob, carol := newCitizen(t, r), newCitizen(t, r), newCitizen(t, r)

	out := alice.sendGroup(t, "hello, both of you", bob, carol)
	if len(out) != 2 || out[0].Rumor != out[1].Rumor {
		t.Fatalf("the outbox has %+v, want one rumor with two entries", out)
	}

	want := []nostr.PubKey{alice.key.Public, bob.key.Public, carol.key.Public}
	for _, member := range []citizen{bob, carol} {
		if got := member.sync(t, r); got.Received != 1 {
			t.Fatalf("a member received %d messages, want 1", got.Received)
		}
		msgs := testutil.Must(member.mail.Inbox(context.Background()))
		if len(msgs) != 1 || msgs[0].Text != "hello, both of you" || msgs[0].From != alice.key.Public {
			t.Fatalf("the inbox of a member: %+v", msgs)
		}
		if !slices.Equal(msgs[0].Members, want) {
			t.Errorf("the members are %v, want %v", msgs[0].Members, want)
		}
	}

	bob.sendGroup(t, "a reply to all", alice, carol)
	for _, member := range []citizen{alice, carol} {
		member.sync(t, r)
		if got := inboxText(member); !slices.Contains(got, "a reply to all") {
			t.Errorf("a member did not get the reply: %v", got)
		}
	}
	var reply mail.Message
	for _, m := range testutil.Must(carol.mail.Inbox(context.Background())) {
		if m.Text == "a reply to all" {
			reply = m
		}
	}
	if reply.From != bob.key.Public || !slices.Equal(reply.Members, []nostr.PubKey{bob.key.Public, alice.key.Public, carol.key.Public}) {
		t.Errorf("the reply: %+v", reply)
	}
}

// An acknowledgement marks the entry of its author only.
func TestAnAcknowledgementMarksOnlyItsAuthor(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob, carol := newCitizen(t, r), newCitizen(t, r), newCitizen(t)

	alice.sendGroup(t, "who is there", bob, carol)
	bob.sync(t, r)
	if got := alice.sync(t, r); got.Delivered != 1 {
		t.Fatalf("alice saw %d acknowledgements, want 1", got.Delivered)
	}
	got := states(t, alice)
	if got[bob.key.Public] != "delivered" || got[carol.key.Public] != "pending" {
		t.Errorf("the outbox states: %v, want bob delivered and carol pending", got)
	}
}

// A seal to carol that holds a rumor with a p tag for bob only does not
// make carol a member. Carol refuses it and does not acknowledge it.
func TestAMessageThatDoesNotNameTheRecipientIsRefused(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob, carol := newCitizen(t, r), newCitizen(t), newCitizen(t, r)

	ctx := context.Background()
	rumor := private.Rumor(alice.key, mail.MessageKind, "not for carol", nostr.Tags{{"p", bob.key.Public.Hex()}}, time.Now())
	wrap, err := private.Wrap(ctx, alice.key, carol.key.Public, rumor, private.RelayForm, private.WrapKind, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Send(ctx, wrap); err != nil {
		t.Fatal(err)
	}

	got := carol.sync(t, r)
	if got.Received != 0 || len(got.Refused) != 1 || !strings.Contains(got.Refused[0], "no p tag") {
		t.Errorf("carol's report: %+v", got)
	}
	if msgs := inboxText(carol); len(msgs) != 0 {
		t.Errorf("carol's inbox: %v", msgs)
	}
	if acks, err := r.Fetch(ctx, nostr.Filter{Kinds: []nostr.Kind{private.WrapKind}, Tags: nostr.TagMap{"p": {alice.key.Public.Hex()}}}); err != nil || len(acks.Events) != 0 {
		t.Errorf("carol acknowledged it: %d wraps for alice, %v", len(acks.Events), err)
	}
}

func TestAGroupHasAtMostTenParticipants(t *testing.T) {
	alice := newCitizen(t)
	ctx := context.Background()
	var nine []nostr.PubKey
	for range 9 {
		nine = append(nine, keys.Generate().Public)
	}
	if out, err := alice.mail.Send(ctx, nine, "ten with me"); err != nil || len(out) != 9 {
		t.Fatalf("a group of ten: %d entries, %v", len(out), err)
	}

	for name, to := range map[string][]nostr.PubKey{
		"eleven":      append(slices.Clone(nine), keys.Generate().Public),
		"a key twice": {nine[0], nine[0]},
		"yourself":    {nine[0], alice.key.Public},
		"nobody":      nil,
	} {
		if _, err := alice.mail.Send(ctx, to, "refused"); err == nil {
			t.Errorf("a send to %s did not fail", name)
		}
	}
	if out := testutil.Must(alice.mail.Outbox(ctx)); len(out) != 9 {
		t.Errorf("a refused send left entries: the outbox has %d, want 9", len(out))
	}
}

// testdata/outbox-v0.31.0 is the mail of a sender of arc v0.31.0, which
// named each outbox entry by its rumor id. The message to bob was
// delivered; the message to carol was pending. Its clock was 2100, so its
// wraps do not expire.
func TestAnOldOutboxMovesToTheNewKeys(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"events.db", "mail.db"} {
		unzip(t, filepath.Join("testdata", "outbox-v0.31.0", name+".gz"), filepath.Join(dir, name))
	}
	var secrets map[string]string
	if err := json.Unmarshal(testutil.Must(os.ReadFile(filepath.Join("testdata", "outbox-v0.31.0", "keys.json"))), &secrets); err != nil {
		t.Fatal(err)
	}
	aliceKey, bobKey, carolKey := mustKey(t, secrets["alice"]), mustKey(t, secrets["bob"]), mustKey(t, secrets["carol"])

	r := relay.Relay{URL: testrelay.Start(t)}
	s, err := boltstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	n := &node.Node{Store: s}
	openAlice := func() *mail.Mail {
		m, err := mailbox.Open(dir, aliceKey, n, []transport.Transport{r})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	type entry struct {
		text, state string
		attempts    int
	}
	entries := func(m *mail.Mail) map[nostr.PubKey]entry {
		got := map[nostr.PubKey]entry{}
		for _, o := range testutil.Must(m.Outbox(context.Background())) {
			to := testutil.Must(nostr.PubKeyFromHex(o.To))
			if _, twice := got[to]; twice {
				t.Errorf("the outbox holds two entries to %s", o.To)
			}
			got[to] = entry{o.Text, o.State(time.Now()), o.Attempts}
		}
		return got
	}

	// The first open moves the entries. A second open finds nothing to move.
	m := openAlice()
	first := entries(m)
	m.Close()
	m = openAlice()
	t.Cleanup(func() { m.Close() })
	want := map[nostr.PubKey]entry{
		bobKey.Public:   {"delivered before the update", "delivered", 2},
		carolKey.Public: {"pending before the update", "pending", 2},
	}
	for name, got := range map[string]map[nostr.PubKey]entry{"first": first, "second": entries(m)} {
		if !maps.Equal(got, want) {
			t.Errorf("the outbox after the %s open: %v, want %v", name, got, want)
		}
	}

	// The pending message still goes out, and its acknowledgement still
	// finds its entry.
	alice := citizen{dir: dir, key: aliceKey, node: n, mail: m}
	carol := newCitizenWithKey(t, carolKey, r)
	alice.sync(t, r)
	// A sync counts an attempt for each wrap that it gives.
	if got := entries(m)[carolKey.Public].attempts; got <= 2 {
		t.Errorf("the pending message has %d attempts after a sync, want more than 2", got)
	}
	if got := carol.sync(t, r); got.Received != 1 {
		t.Fatalf("carol received %d messages, want 1", got.Received)
	}
	if msgs := testutil.Must(carol.mail.Inbox(context.Background())); len(msgs) != 1 || !slices.Equal(msgs[0].Members, []nostr.PubKey{aliceKey.Public, carolKey.Public}) {
		t.Errorf("carol's inbox: %+v", msgs)
	}
	if got := alice.sync(t, r); got.Delivered != 1 {
		t.Errorf("alice saw %d acknowledgements, want 1", got.Delivered)
	}
	if got := entries(m)[carolKey.Public].state; got != "delivered" {
		t.Errorf("the message to carol is %s, want delivered", got)
	}
}

// olderInbox reads the inbox as arc v0.31.0 did (core/mail/mail.go:899 at
// dba5a6a): each received seal of kind 14, from the author of the rumor.
// It reads no p tag.
func olderInbox(t *testing.T, c citizen) []mail.Message {
	t.Helper()
	var out []mail.Message
	for _, seal := range testutil.Must(c.node.Store.Query(nostr.Filter{Kinds: []nostr.Kind{private.SealKind}})) {
		if seal.PubKey == c.key.Public {
			continue
		}
		rumor := testutil.Must(private.OpenSeal(context.Background(), c.key, seal))
		if rumor.Kind == mail.MessageKind {
			out = append(out, mail.Message{ID: rumor.ID.Hex(), From: rumor.PubKey, Text: rumor.Content})
		}
	}
	return out
}

// Each member gets a NIP-59 wrap of a kind 14 rumor, as for a direct
// message. The reader of an older arc shows it as a direct message from
// the sender.
func TestAnOlderArcReadsAGroupMessageAsADirectMessage(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob, carol := newCitizen(t, r), newCitizen(t, r), newCitizen(t, r)
	alice.sendGroup(t, "to the group", bob, carol)

	ctx := context.Background()
	batch, err := r.Fetch(ctx, nostr.Filter{Kinds: []nostr.Kind{private.WrapKind}, Tags: nostr.TagMap{"p": {bob.key.Public.Hex()}}})
	if err != nil || len(batch.Events) != 1 {
		t.Fatalf("the relay holds %d wraps for bob, want 1 (%v)", len(batch.Events), err)
	}
	opened, err := private.Unwrap(ctx, bob.key, batch.Events[0])
	if err != nil || opened.Rumor.Kind != mail.MessageKind || opened.Author() != alice.key.Public {
		t.Fatalf("bob opened %+v, %v", opened.Rumor, err)
	}

	bob.sync(t, r)
	if got := olderInbox(t, bob); len(got) != 1 || got[0].From != alice.key.Public || got[0].Text != "to the group" {
		t.Errorf("the older inbox of bob: %+v", got)
	}
}

func unzip(t *testing.T, from, to string) {
	t.Helper()
	in := testutil.Must(os.Open(from))
	defer in.Close()
	z := testutil.Must(gzip.NewReader(in))
	out := testutil.Must(os.Create(to))
	defer out.Close()
	if _, err := io.Copy(out, z); err != nil {
		t.Fatal(err)
	}
}
