package relay_test

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/announce"
	"github.com/gezibash/arc/client"
	"github.com/gezibash/arc/frame"
	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/packet"
	"github.com/gezibash/arc/relay"
	"github.com/gezibash/arc/session"
)

func lookupRequest(kind, query, id string, budget int, path ...[]byte) map[string]any {
	hops := make([]any, 0, len(path))
	for _, hop := range path {
		hops = append(hops, hex.EncodeToString(hop))
	}
	return map[string]any{
		"type":    kind,
		"query":   query,
		"network": map[string]any{"id": id, "path": hops, "budget": budget},
	}
}

func lookupID(n byte) string { return strings.Repeat(hex.EncodeToString([]byte{n}), 16) }

func keyOf(t *testing.T) []byte {
	t.Helper()
	me, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return me.PublicKey
}

// A name travels the chain as a live lookup, and the path that it finds
// carries a packet at once.
func TestALiveLookupFindsANameAcrossTheChain(t *testing.T) {
	relays := chain(t, 3)
	provider, waiting := serving(t, relays[2], announce.Network)
	caller, from := citizen(t, relays[0])
	ctx := context.Background()

	// One try: the catalog syncs every two seconds, so a retry could find
	// the name without a live lookup.
	entries, err := from.Resolve(ctx, provider.Name())
	if err != nil || len(entries) != 1 || entries[0]["public_key"] != provider.EncodePublicKey() {
		t.Fatalf("the name gave %v, %v", entries, err)
	}

	talk, err := session.Establish(caller, provider.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := frame.Encode(frame.Request, frame.NewRequestID(), map[string]any{"path": "/"}, []byte("found live"))
	nonce, ciphertext, seq, err := talk.Encrypt(body)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := packet.Encode(caller, provider.PublicKey, talk.ID, seq, nonce, ciphertext,
		packet.WithEphemeralKey(talk.EphemeralPublic))
	if err != nil {
		t.Fatal(err)
	}
	if err := from.SendPacket(raw); err != nil {
		t.Fatal(err)
	}

	select {
	case <-waiting.Packets():
	case <-time.After(15 * time.Second):
		t.Fatal("the packet did not follow the path of the lookup")
	}
}

// A search of a cold catalog asks the partners, and finds a publisher that
// is two relays away.
func TestASearchAsksThePartnersOfAColdCatalog(t *testing.T) {
	relays := chain(t, 3)
	provider, _ := serving(t, relays[2], announce.Network)
	_, from := citizen(t, relays[0])
	ctx := context.Background()

	// One try, before the catalog of the first relay syncs.
	page, err := from.Search(ctx, "exec", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range page.Entries {
		if entry["public_key"] == provider.EncodePublicKey() {
			return
		}
	}
	t.Fatalf("the search gave %v", page.Entries)
}

// A partner that does not answer could hold a citizen of the same name, so
// a lookup by name fails. A full key that the relay holds still answers.
func TestAPartialLookupByNameFails(t *testing.T) {
	me, _ := identity.Generate()
	ghost, _ := identity.Generate()
	server := listen(t, me, false, relay.Peer{PublicKey: ghost.PublicKey, Address: "127.0.0.1:1"})

	local, _ := serving(t, server, announce.Local)
	_, asking := citizen(t, server)
	ctx := context.Background()

	_, err := asking.Resolve(ctx, local.Name())
	var refused *client.DirectoryError
	if !errors.As(err, &refused) || refused.Reason != "federation_unavailable" {
		t.Fatalf("a lookup by name with a partner down gave %v", err)
	}

	entries, err := asking.Resolve(ctx, local.EncodePublicKey())
	if err != nil || len(entries) != 1 {
		t.Fatalf("the full key of a citizen here gave %v, %v", entries, err)
	}
}

// A path that is not one is refused: one that names the relay itself, one
// that does not end at the partner that sent it, and a budget out of range.
func TestALookupRefusesAPathThatIsNotOne(t *testing.T) {
	server := start(t)
	peer, other := keyOf(t), keyOf(t)

	for name, request := range map[string]map[string]any{
		"the relay itself":  lookupRequest("resolve", "x", lookupID(1), 4, server.PublicKey(), peer),
		"another sender":    lookupRequest("resolve", "x", lookupID(2), 4, other),
		"no path":           lookupRequest("resolve", "x", lookupID(3), 4),
		"a budget of none":  lookupRequest("resolve", "x", lookupID(4), 0, peer),
		"a budget too high": lookupRequest("resolve", "x", lookupID(5), relay.LookupBudget+1, peer),
		"a short id":        lookupRequest("resolve", "x", "abc", 4, peer),
	} {
		if answer := server.AnswerLookup(peer, request); answer["ok"] != false {
			t.Errorf("%s: %v", name, answer)
		}
	}
}

// An id that comes back is answered as partial, and with nothing.
func TestALookupIDThatComesBackIsPartial(t *testing.T) {
	server := start(t)
	peer := keyOf(t)

	first := server.AnswerLookup(peer, lookupRequest("resolve", "x", lookupID(9), 4, peer))
	if first["ok"] != true || first["partial"] != false {
		t.Fatalf("first = %v", first)
	}
	again := server.AnswerLookup(peer, lookupRequest("resolve", "x", lookupID(9), 4, peer))
	if again["ok"] != true || again["partial"] != true {
		t.Fatalf("again = %v", again)
	}
}

// A relay that passes traffic on spends one of the budget on itself. With
// nothing left for its partner, the answer is partial.
func TestABudgetThatRunsOutIsPartial(t *testing.T) {
	relays := chain(t, 3)
	origin := relays[0].PublicKey()

	waitFor(t, "the middle relay links both ends", func() bool {
		return relays[1].LinkTo(relays[0].PublicKey()) != nil && relays[1].LinkTo(relays[2].PublicKey()) != nil
	})

	spent := relays[1].AnswerLookup(origin, lookupRequest("search", "", lookupID(20), 1, origin))
	if spent["partial"] != true {
		t.Errorf("a budget of one gave %v", spent)
	}

	enough := relays[1].AnswerLookup(origin, lookupRequest("search", "", lookupID(21), 2, origin))
	if enough["partial"] != false {
		t.Errorf("a budget of two gave %v", enough)
	}
}

// A record for direct partners answers the partner that asked first, and
// never a lookup that crossed another relay.
func TestADirectRecordAnswersOnlyTheOrigin(t *testing.T) {
	server := start(t)
	provider, _ := serving(t, server, announce.Direct)
	partner, far := keyOf(t), keyOf(t)

	near := server.AnswerLookup(partner, lookupRequest("resolve", provider.EncodePublicKey(), lookupID(30), 4, partner))
	if entries, _ := near["entries"].([]any); len(entries) != 1 {
		t.Errorf("the direct partner got %v", near)
	}

	further := server.AnswerLookup(partner, lookupRequest("resolve", provider.EncodePublicKey(), lookupID(31), 4, far, partner))
	if entries, _ := further["entries"].([]any); len(entries) != 0 {
		t.Errorf("a lookup from two relays away got %v", further)
	}
}
