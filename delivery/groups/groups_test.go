package groups_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/keyer"
	"fiatjaf.com/nostr/khatru"
	"fiatjaf.com/nostr/nip11"
	"fiatjaf.com/nostr/nip29"
	"github.com/gezibash/arc/delivery/groups"
	"github.com/gezibash/arc/delivery/testrelay"
	"github.com/gezibash/arc/delivery/transport/relay"
)

var ctx = context.Background()

type citizen struct {
	key   nostr.SecretKey
	relay relay.Relay
}

func newCitizen(url string) citizen {
	k := nostr.Generate()
	return citizen{key: k, relay: relay.Relay{URL: url, Signer: keyer.NewPlainKeySigner(k)}}
}

func (c citizen) send(t *testing.T, kind nostr.Kind, content string, tags ...nostr.Tag) (nostr.Event, error) {
	t.Helper()
	event := nostr.Event{Kind: kind, CreatedAt: nostr.Now(), Content: content, Tags: append(nostr.Tags{{"-"}}, tags...)}
	if err := event.Sign(c.key); err != nil {
		t.Fatal(err)
	}
	return event, c.relay.Send(ctx, event)
}

// query reads the relay the way any Nostr client does, with the library alone.
func query(t *testing.T, url string, filter nostr.Filter) []nostr.Event {
	t.Helper()
	conn, err := nostr.RelayConnect(ctx, url, nostr.RelayOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sub, err := conn.Subscribe(c, filter, nostr.SubscriptionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsub()
	var out []nostr.Event
	for {
		select {
		case event := <-sub.Events:
			out = append(out, event)
		case <-sub.EndOfStoredEvents:
			return out
		case <-c.Done():
			t.Fatal("the relay did not end the query")
		}
	}
}

func TestNIP29APostOpensInAClientAndAnAdminRemovesIt(t *testing.T) {
	admin := nostr.Generate()
	url, relayKey := testrelay.StartGroups(t, "agora", admin.Public())
	alice := newCitizen(url)
	moderator := citizen{key: admin, relay: relay.Relay{URL: url, Signer: keyer.NewPlainKeySigner(admin)}}

	// A client finds the group: its metadata, signed by the relay itself.
	info, err := nip11.Fetch(ctx, url)
	if err != nil || info.Self == nil || *info.Self != relayKey {
		t.Fatalf("the relay does not name its key: %v %+v", err, info.Self)
	}
	metadata := query(t, url, nostr.Filter{Kinds: []nostr.Kind{nostr.KindSimpleGroupMetadata}, Tags: nostr.TagMap{"d": {"agora"}}})
	if len(metadata) != 1 || metadata[0].PubKey != relayKey || !metadata[0].VerifySignature() {
		t.Fatalf("the group metadata is %+v", metadata)
	}
	group, err := nip29.NewGroupFromMetadataEvent(url, &metadata[0])
	if err != nil || group.Address.ID != "agora" || group.Restricted {
		t.Fatalf("the client read the group as %v %v", group, err)
	}
	admins := query(t, url, nostr.Filter{Kinds: []nostr.Kind{nostr.KindSimpleGroupAdmins}, Tags: nostr.TagMap{"d": {"agora"}}})
	if len(admins) != 1 || group.MergeInAdminsEvent(&admins[0]) != nil || len(group.Members[admin.Public()]) == 0 {
		t.Fatalf("the client does not see the admin: %+v", admins)
	}

	// Anyone posts to an open group.
	post, err := alice.send(t, nostr.KindSimpleGroupThread, "first post", nostr.Tag{"h", "agora"}, nostr.Tag{"title", "Hello"})
	if err != nil {
		t.Fatal(err)
	}
	threads := query(t, url, nostr.Filter{Kinds: []nostr.Kind{nostr.KindSimpleGroupThread}, Tags: nostr.TagMap{"h": {"agora"}}})
	if len(threads) != 1 || threads[0].ID != post.ID {
		t.Fatalf("the client read %d threads", len(threads))
	}

	// Only an admin removes it.
	if _, err := alice.send(t, nostr.KindSimpleGroupDeleteEvent, "", nostr.Tag{"h", "agora"}, nostr.Tag{"e", post.ID.Hex()}); err == nil ||
		!strings.Contains(err.Error(), "only an admin") {
		t.Errorf("a citizen who is not an admin removed a post: %v", err)
	}
	if _, err := moderator.send(t, nostr.KindSimpleGroupDeleteEvent, "", nostr.Tag{"h", "agora"}, nostr.Tag{"e", post.ID.Hex()}); err != nil {
		t.Fatal(err)
	}
	if got := query(t, url, nostr.Filter{IDs: []nostr.ID{post.ID}}); len(got) != 0 {
		t.Error("the removed post is still there")
	}
}

func TestNIP29TheRelayRefusesWhatAGroupDoesNotAllow(t *testing.T) {
	url, _ := testrelay.StartGroups(t, "agora")
	alice := newCitizen(url)

	cases := map[string]struct {
		kind nostr.Kind
		tags nostr.Tags
		want string
	}{
		"a group that does not exist": {nostr.KindSimpleGroupThread, nostr.Tags{{"h", "nowhere"}}, "no group"},
		"group state from a citizen":  {nostr.KindSimpleGroupMetadata, nostr.Tags{{"d", "agora"}}, "only the relay"},
		"moderation with no group":    {nostr.KindSimpleGroupPutUser, nostr.Tags{{"p", alice.key.Public().Hex()}}, "h tag"},
		"a new group":                 {nostr.KindSimpleGroupCreateGroup, nostr.Tags{{"h", "agora"}}, "exists"},
	}
	for name, c := range cases {
		if _, err := alice.send(t, c.kind, "", c.tags...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}

func TestNIP29GroupsSurviveARestart(t *testing.T) {
	db := &boltdb.BoltBackend{Path: filepath.Join(t.TempDir(), "relay.db")}
	if err := db.Init(); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key, admin := nostr.Generate(), nostr.Generate().Public()

	first := khatru.NewRelay()
	first.UseEventstore(db, 500)
	g, _ := groups.Attach(first, db, key)
	if err := g.Create("agora", "Agora", false, []nostr.PubKey{admin}); err != nil {
		t.Fatal(err)
	}

	second := khatru.NewRelay()
	second.UseEventstore(db, 500)
	again, err := groups.Attach(second, db, key)
	if err != nil {
		t.Fatal(err)
	}
	admins, err := again.Admins("agora")
	if err != nil || len(admins) != 1 || admins[0] != admin {
		t.Errorf("after a restart the admins are %v %v", admins, err)
	}
}
