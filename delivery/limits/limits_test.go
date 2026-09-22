package limits_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/keyer"
	"fiatjaf.com/nostr/khatru"
	"fiatjaf.com/nostr/nip13"
	"github.com/gezibash/arc/delivery/draft"
	"github.com/gezibash/arc/delivery/limits"
	"github.com/gezibash/arc/delivery/sealed"
	"github.com/gezibash/arc/delivery/transport/relay"
)

var ctx = context.Background()

// start runs a relay as arcn relay serve runs it, with the policy.
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

// The deploy caps an event at 256 KiB. Journal content travels in parts of
// 32 KiB, docs/delivery/SPEC.md section 16.1, so a full part and a full draft
// must pass.
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
