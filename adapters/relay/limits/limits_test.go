package limits_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/keyer"
	"fiatjaf.com/nostr/khatru"
	"fiatjaf.com/nostr/nip13"
	"github.com/gezibash/arc/adapters/relay/limits"
	"github.com/gezibash/arc/adapters/relay/sealed"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/core/store"
)

var ctx = context.Background()

// start runs a relay as arc relay serve runs it, with the policy.
func start(t *testing.T, p limits.Policy) string {
	t.Helper()
	db := &boltdb.BoltBackend{Path: filepath.Join(t.TempDir(), "relay.db")}
	if err := db.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	rl := khatru.NewRelay()
	rl.UseEventstore(db, 500)
	sealed.Protect(rl)
	limits.Apply(rl, db.DB, p)
	server := httptest.NewServer(rl)
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

// publish sends one event on a new connection with the given headers, and
// no authentication.
func publish(t *testing.T, url string, header http.Header, event nostr.Event) error {
	t.Helper()
	conn, err := nostr.RelayConnect(ctx, url, nostr.RelayOptions{RequestHeader: header, NoticeHandler: func(*nostr.Relay, string) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	return conn.Publish(ctx, event)
}

func note(content string) nostr.Event {
	e := nostr.Event{Kind: 1, CreatedAt: nostr.Now(), Content: content}
	e.Sign(nostr.Generate())
	return e
}

// The deploy sets a cap of 256 KiB, and the protocol limit of 64 KiB
// applies. Journal content travels in parts of 32 KiB, docs/interface/SPEC.md
// section 3.9.2, so a full part and a full draft must pass.
func TestTheSizeCapTakesAFullPartAndAFullDraft(t *testing.T) {
	url := start(t, limits.Policy{MaxEventBytes: 256 * 1024})
	k := keyer.NewPlainKeySigner(nostr.Generate())
	mine := relay.Relay{URL: url, Signer: k}

	line := "a line of a journal page, with \"quotes\" and a tab\t.\n"
	text := strings.Repeat(line, draft.PartSize/len(line))
	part, err := draft.Part(ctx, k, "page", text, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	wrap, err := draft.Wrap(ctx, k, "page", nostr.Event{Kind: 30023, CreatedAt: 1, Content: text}, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []nostr.Event{part, wrap} {
		if err := mine.Send(ctx, e); err != nil {
			t.Errorf("a full event of kind %d holds %d bytes, and the relay refused it: %v", e.Kind, len(e.String()), err)
		}
	}

	if err := publish(t, url, nil, note(strings.Repeat("x", 300*1024))); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("an event of 300 KiB: %v", err)
	}
}

// largestWrap seals and wraps the largest rumor that mail takes: 24 KiB of
// content, with quotes that make its JSON MaxRumor bytes. The real Bolt store
// of the relay must keep it.
func largestWrap(t *testing.T) nostr.Event {
	t.Helper()
	alice, bob := keys.Generate(), keys.Generate()
	tags := nostr.Tags{{"p", bob.Public.Hex()}}
	now := time.Now()
	plain := private.Rumor(alice, 14, strings.Repeat("x", private.MaxMailContent), tags, now)
	quotes := private.MaxRumor - len(plain.String())
	rumor := private.Rumor(alice, 14, strings.Repeat(`"`, quotes)+strings.Repeat("x", private.MaxMailContent-quotes), tags, now)
	if err := private.CheckMail(rumor); err != nil {
		t.Fatal(err)
	}
	w, err := private.Wrap(ctx, alice, bob.Public, rumor, private.RelayForm, private.WrapKind, now.Add(private.MaxAge))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(w.String()); n != private.MaxWrap {
		t.Fatalf("the largest wrap holds %d bytes, not %d", n, private.MaxWrap)
	}
	return w
}

// resized is a wrap whose content has extra bytes more, signed again by a
// new one-time key. Real sealing cannot make it: NIP-44 pads in steps.
func resized(w nostr.Event, extra int) nostr.Event {
	w.Content += strings.Repeat("A", extra)
	w.Sign(nostr.Generate())
	return w
}

// The wrap limit is a protocol limit, docs/delivery/SPEC.md section 3.4.
// The relay keeps it with no flag set, and a flag can only make it smaller.
func TestTheRelayRefusesAWrapLargerThanMail(t *testing.T) {
	largest := largestWrap(t)
	for _, p := range []limits.Policy{{}, {MaxEventBytes: 256 * 1024}} {
		url := start(t, p)
		if err := publish(t, url, nil, largest); err != nil {
			t.Errorf("cap %d: the largest wrap: %v", p.MaxEventBytes, err)
		}
		batch, err := relay.Relay{URL: url}.Fetch(ctx, nostr.Filter{IDs: []nostr.ID{largest.ID}})
		if err != nil || len(batch.Events) != 1 || batch.Events[0].String() != largest.String() {
			t.Errorf("cap %d: the relay does not serve the largest wrap whole: %v", p.MaxEventBytes, err)
		}
		if err := publish(t, url, nil, resized(largest, 1)); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Errorf("cap %d: a wrap of %d bytes: %v", p.MaxEventBytes, private.MaxWrap+1, err)
		}
		// Other kinds have the larger limit of an event.
		if err := publish(t, url, nil, note(strings.Repeat("x", private.MaxWrap))); err != nil {
			t.Errorf("cap %d: a note larger than a wrap: %v", p.MaxEventBytes, err)
		}
	}

	url := start(t, limits.Policy{MaxEventBytes: 50_000})
	if err := publish(t, url, nil, largest); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("cap 50,000: the largest wrap: %v", err)
	}
}

func TestAGiftWrapNeedsAuthenticationOrWork(t *testing.T) {
	url := start(t, limits.Policy{WrapAuth: true, WrapPoW: 8})
	recipient := nostr.Generate().Public()
	wrap := func(kind nostr.Kind, pow int) nostr.Event {
		one := nostr.Generate()
		e := nostr.Event{Kind: kind, CreatedAt: nostr.Now(), PubKey: one.Public(), Content: "sealed",
			Tags: nostr.Tags{{"p", recipient.Hex()}}}
		if pow > 0 {
			tag, err := nip13.DoWork(ctx, e, pow)
			if err != nil {
				t.Fatal(err)
			}
			e.Tags = append(e.Tags, tag)
		}
		e.Sign(one)
		return e
	}

	if err := publish(t, url, nil, wrap(1059, 0)); err == nil || !strings.Contains(err.Error(), "auth-required") {
		t.Errorf("a gift wrap without authentication: %v", err)
	}
	if err := publish(t, url, nil, wrap(1059, 8)); err != nil {
		t.Errorf("a gift wrap with work of 8 bits: %v", err)
	}
	if err := publish(t, url, nil, note("public")); err != nil {
		t.Errorf("a public note: %v", err)
	}

	// The relay client answers the challenge and sends again. A one-time
	// key is enough, as a sender uses for the inbox relay of a recipient.
	once := relay.Relay{URL: url, Signer: keyer.NewPlainKeySigner(nostr.Generate())}
	if err := once.Send(ctx, wrap(1059, 0)); err != nil {
		t.Errorf("a gift wrap after authentication: %v", err)
	}

	// A live call sends a wrap of kind 21059 on the connection that waits
	// for the answer. Here the answer is the wrap itself.
	live := wrap(21059, 0)
	answers := nostr.Filter{Kinds: []nostr.Kind{21059}, Tags: nostr.TagMap{"p": {recipient.Hex()}}}
	got, err := once.Exchange(ctx, live, answers, func(e nostr.Event) bool { return e.ID == live.ID })
	if err != nil || got.ID != live.ID {
		t.Errorf("a live wrap after authentication: %v", err)
	}
}

func TestTheRateLimitHoldsOneAddressBehindTheProxy(t *testing.T) {
	url := start(t, limits.Policy{Rate: 60, Burst: 3, IPHeader: "Fly-Client-IP"})
	from := func(ip, forwarded string) http.Header {
		return http.Header{"Fly-Client-IP": {ip}, "X-Forwarded-For": {forwarded}}
	}

	// A client can write any X-Forwarded-For. The relay reads only the
	// header that the proxy sets.
	for i, forwarded := range []string{"9.9.9.1", "9.9.9.2", "9.9.9.3"} {
		if err := publish(t, url, from("1.1.1.1", forwarded), note("burst")); err != nil {
			t.Fatalf("event %d of the burst: %v", i+1, err)
		}
	}
	if err := publish(t, url, from("1.1.1.1", "9.9.9.4"), note("one more")); err == nil || !strings.Contains(err.Error(), "rate-limited") {
		t.Errorf("the event after the burst: %v", err)
	}
	if err := publish(t, url, from("2.2.2.2", "9.9.9.4"), note("another address")); err != nil {
		t.Errorf("another address: %v", err)
	}
}

func TestAFullStoreRefusesNewEventsButNotDeletions(t *testing.T) {
	url := start(t, limits.Policy{MaxStoreBytes: 1 << 20})
	full := false
	for range 100 {
		err := publish(t, url, nil, note(strings.Repeat("x", 30*1024)))
		if err != nil {
			if !strings.Contains(err.Error(), "full") {
				t.Fatal(err)
			}
			full = true
			break
		}
	}
	if !full {
		t.Fatal("the relay took 3 MB into a store of 1 MiB")
	}

	author := nostr.Generate()
	deletion := nostr.Event{Kind: nostr.KindDeletion, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"e", note("x").ID.Hex()}}}
	deletion.Sign(author)
	if err := publish(t, url, nil, deletion); err != nil {
		t.Errorf("a deletion into a full store: %v", err)
	}
}

// Mail lives for 7 days, set in the expiration tag of its wraps,
// docs/delivery/SPEC.md section 4.2. The relay keeps a wrap until then.
func TestTheRelayDropsAWrapWhenItExpires(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	now := func() time.Time { return time.Unix(clock.Load(), 0) }
	db := &boltdb.BoltBackend{Path: filepath.Join(t.TempDir(), "relay.db")}
	if err := db.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	rl := khatru.NewRelay()
	rl.UseEventstore(db, 500)
	sealed.Protect(rl)
	limits.Apply(rl, db.DB, limits.Policy{Now: now})
	server := httptest.NewServer(rl)
	t.Cleanup(server.Close)
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	mine := relay.Relay{URL: url}

	alice, bob := keys.Generate(), keys.Generate()
	mail := func(expires time.Time) nostr.Event {
		rumor := private.Rumor(alice, 14, "hello bob", nostr.Tags{{"p", bob.Public.Hex()}}, now())
		w, err := private.Wrap(ctx, alice, bob.Public, rumor, private.RelayForm, private.WrapKind, expires)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	served := func(w nostr.Event) bool {
		t.Helper()
		batch, err := mine.Fetch(ctx, nostr.Filter{IDs: []nostr.ID{w.ID}})
		if err != nil {
			t.Fatal(err)
		}
		return len(batch.Events) == 1
	}

	if err := publish(t, url, nil, mail(now().Add(-time.Second))); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("a wrap that has already expired: %v", err)
	}

	w := mail(now().Add(private.MaxAge))
	if err := publish(t, url, nil, w); err != nil {
		t.Fatalf("a wrap that expires in 7 days: %v", err)
	}
	clock.Add(int64((private.MaxAge - time.Minute) / time.Second))
	if !served(w) {
		t.Fatal("the relay does not serve the wrap one minute before it expires")
	}

	clock.Add(60)
	if served(w) {
		t.Error("the relay serves the wrap after it expires")
	}
	for range db.QueryEvents(nostr.Filter{IDs: []nostr.ID{w.ID}}, 1) {
		t.Error("the store still holds the wrap after it expires")
	}
}

// sized is an event of the kind whose JSON holds exactly size bytes. Its
// content is plain text, so JSON adds no escapes.
func sized(t *testing.T, kind nostr.Kind, tags nostr.Tags, size int) nostr.Event {
	t.Helper()
	e := nostr.Event{Kind: kind, CreatedAt: nostr.Now(), Tags: tags}
	e.Sign(nostr.Generate())
	e.Content = strings.Repeat("x", size-len(e.String()))
	e.Sign(nostr.Generate())
	if n := len(e.String()); n != size {
		t.Fatalf("the event holds %d bytes, not %d", n, size)
	}
	return e
}

// The limit of an event is a protocol limit, docs/delivery/SPEC.md section
// 3.4. It holds for a regular, a replaceable and an addressable event, with
// no flag set, and a flag can only make it smaller. The real Bolt store of
// the relay keeps the largest event.
func TestTheRelayRefusesAnEventAboveTheLimit(t *testing.T) {
	kinds := []struct {
		kind nostr.Kind
		tags nostr.Tags
	}{
		{1, nil},
		{10002, nostr.Tags{{"r", "wss://relay.example"}}},
		{30272, nostr.Tags{{"d", "echo"}, {"t", "echo"}}},
	}
	for _, p := range []limits.Policy{{}, {MaxEventBytes: 256 * 1024}} {
		url := start(t, p)
		for _, k := range kinds {
			largest := sized(t, k.kind, k.tags, store.MaxEvent)
			if err := publish(t, url, nil, largest); err != nil {
				t.Errorf("cap %d: kind %d of %d bytes: %v", p.MaxEventBytes, k.kind, store.MaxEvent, err)
			}
			batch, err := relay.Relay{URL: url}.Fetch(ctx, nostr.Filter{IDs: []nostr.ID{largest.ID}})
			if err != nil || len(batch.Events) != 1 || batch.Events[0].String() != largest.String() {
				t.Errorf("cap %d: the relay does not serve kind %d of %d bytes whole: %v", p.MaxEventBytes, k.kind, store.MaxEvent, err)
			}
			over := sized(t, k.kind, k.tags, store.MaxEvent+1)
			if err := publish(t, url, nil, over); err == nil || !strings.Contains(err.Error(), "at most 65536") {
				t.Errorf("cap %d: kind %d of %d bytes: %v", p.MaxEventBytes, k.kind, store.MaxEvent+1, err)
			}
		}
	}

	url := start(t, limits.Policy{MaxEventBytes: 50_000})
	if err := publish(t, url, nil, sized(t, 30272, kinds[2].tags, 50_001)); err == nil || !strings.Contains(err.Error(), "at most 50000") {
		t.Errorf("cap 50,000: an announcement of 50,001 bytes: %v", err)
	}
}

// No store keeps an ephemeral event, so only the flag caps it. A live reply
// travels in one ephemeral gift wrap, and can be larger than 64 KiB.
func TestAnEphemeralEventHasOnlyTheFlag(t *testing.T) {
	big := sized(t, 20001, nil, 4*store.MaxEvent)
	// The relay answers "mute" when it took the event and no one listens.
	if err := publish(t, start(t, limits.Policy{}), nil, big); err != nil && !strings.Contains(err.Error(), "mute:") {
		t.Errorf("no cap: an ephemeral event of %d bytes: %v", 4*store.MaxEvent, err)
	}
	if err := publish(t, start(t, limits.Policy{MaxEventBytes: 3 * store.MaxEvent}), nil, big); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("cap %d: an ephemeral event of %d bytes: %v", 3*store.MaxEvent, 4*store.MaxEvent, err)
	}
}
