package mail_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/mail"
	"github.com/gezibash/arc/internal/testrelay"
	"github.com/gezibash/arc/internal/testutil"
)

// A reply crosses a relay with one e tag that names its parent. The inbox
// of the recipient and the outbox of the sender give the parent id.
func TestAReplyNamesItsParent(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t, r), newCitizen(t, r)
	ctx := context.Background()

	alice.send(t, bob, "bring the map")
	bob.sync(t, r)
	parent := testutil.Must(bob.mail.Inbox(ctx))[0]
	if parent.ReplyTo != "" {
		t.Fatalf("a plain message has the parent %q", parent.ReplyTo)
	}
	out := testutil.Must(bob.mail.SendReply(ctx, []nostr.PubKey{alice.key.Public}, testutil.Must(nostr.IDFromHex(parent.ID)), "yes"))
	alice.sync(t, r)

	got := testutil.Must(alice.mail.Inbox(ctx))
	if len(got) != 1 || got[0].Text != "yes" || got[0].ReplyTo != parent.ID {
		t.Fatalf("alice's inbox is %+v, want the reply to %s", got, parent.ID)
	}
	rumors := testutil.Must(alice.mail.Rumors(ctx, []nostr.Kind{mail.MessageKind}))
	for _, rumor := range rumors {
		if rumor.ID.Hex() == out[0].Rumor {
			if e := slices.Collect(rumor.Tags.FindAll("e")); len(e) != 1 {
				t.Errorf("the reply holds the tags %v, want one e tag", rumor.Tags)
			}
		}
	}
	sent := testutil.Must(bob.mail.Outbox(ctx))
	if len(sent) != 1 || sent[0].ReplyTo != parent.ID {
		t.Errorf("bob's outbox is %+v, want the reply to %s", sent, parent.ID)
	}
}

// A sender refuses a reply to a message that it does not hold, and a reply
// to other members than the members of the parent. It seals nothing.
func TestASendReplyChecksItsParent(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob, carol := newCitizen(t, r), newCitizen(t, r), newCitizen(t, r)
	ctx := context.Background()
	alice.send(t, bob, "just for bob")
	bob.sync(t, r)
	parent := testutil.Must(nostr.IDFromHex(testutil.Must(bob.mail.Inbox(ctx))[0].ID))

	cases := []struct {
		name   string
		to     []nostr.PubKey
		parent nostr.ID
		want   string
	}{
		{"an id that bob does not hold", []nostr.PubKey{alice.key.Public}, nostr.ID{1}, "no message 0100"},
		{"the parent, to carol", []nostr.PubKey{carol.key.Public}, parent, "the members of its parent"},
		{"the parent, to alice and carol", []nostr.PubKey{alice.key.Public, carol.key.Public}, parent, "the members of its parent"},
	}
	for _, c := range cases {
		if _, err := bob.mail.SendReply(ctx, c.to, c.parent, "hi"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	if out := testutil.Must(bob.mail.Outbox(ctx)); len(out) != 0 {
		t.Errorf("a refused reply left %d outbox entries", len(out))
	}
}

// A reader takes the parent from exactly one e tag of 64 lowercase hex
// digits, and ignores the elements after the id. Each other message is a
// plain message.
func TestReplyToReadsOneETag(t *testing.T) {
	id := strings.Repeat("ab", 32)
	other := strings.Repeat("cd", 32)
	cases := []struct {
		name string
		tags nostr.Tags
		want string
	}{
		{"one e tag", nostr.Tags{{"p", other}, {"e", id}}, id},
		{"a relay hint and a marker", nostr.Tags{{"e", id, "wss://relay.example", "reply"}}, id},
		{"no e tag", nostr.Tags{{"p", other}}, ""},
		{"two e tags", nostr.Tags{{"e", id}, {"e", other}}, ""},
		{"an e tag with no value, then one", nostr.Tags{{"e"}, {"e", id}}, ""},
		{"capital hex", nostr.Tags{{"e", strings.ToUpper(id)}}, ""},
		{"63 digits", nostr.Tags{{"e", id[1:]}}, ""},
		{"not hex", nostr.Tags{{"e", strings.Repeat("zz", 32)}}, ""},
	}
	for _, c := range cases {
		if got := mail.ReplyTo(nostr.Event{Kind: mail.MessageKind, Tags: c.tags}); got != c.want {
			t.Errorf("%s: ReplyTo is %q, want %q", c.name, got, c.want)
		}
	}
}

// An arc older than replies reads no e tag. The inbox of v0.34.0
// (core/mail/mail.go:1112 at a615c70) reads the author, the p tags, the
// time and the content; olderInbox, the rule of v0.31.0, reads less. Both
// show a reply as a plain message from its author.
func TestAnOlderArcReadsAReplyAsAMessage(t *testing.T) {
	r := relay.Relay{URL: testrelay.Start(t)}
	alice, bob := newCitizen(t, r), newCitizen(t, r)
	ctx := context.Background()
	alice.send(t, bob, "bring the map")
	bob.sync(t, r)
	parent := testutil.Must(nostr.IDFromHex(testutil.Must(bob.mail.Inbox(ctx))[0].ID))
	testutil.Must(bob.mail.SendReply(ctx, []nostr.PubKey{alice.key.Public}, parent, "yes"))
	alice.sync(t, r)

	if got := olderInbox(t, alice); len(got) != 1 || got[0].From != bob.key.Public || got[0].Text != "yes" {
		t.Errorf("the older inbox of alice: %+v", got)
	}
}
