package relay_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sort"
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

// listen starts one relay with the partners that it approves.
func listen(t *testing.T, me *identity.Identity, transit bool, peers ...relay.Peer) *relay.Relay {
	t.Helper()

	server, err := relay.Listen(context.Background(), relay.Options{
		Identity: me,
		Address:  "127.0.0.1:0",
		Transit:  transit,
		Peers:    peers,
		Log:      testLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	return server
}

// chain starts relays that each approve the one before and the one after,
// and returns them in order. Only the relays in the middle pass traffic on.
func chain(t *testing.T, count int) []*relay.Relay {
	t.Helper()

	// The relay with the lower key dials, so the keys rise along the chain
	// and each relay holds the address of the one it dials.
	identities := make([]*identity.Identity, count)
	for index := range identities {
		me, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		identities[index] = me
	}
	sort.Slice(identities, func(left, right int) bool {
		return string(identities[left].PublicKey) < string(identities[right].PublicKey)
	})

	// A relay dials a partner whose key stands below its own, so every relay
	// needs the address of the one it dials. The relays start from the end,
	// and a partner without an address waits to be dialled.
	servers := make([]*relay.Relay, count)

	for index := count - 1; index >= 0; index-- {
		var peers []relay.Peer

		for _, at := range []int{index - 1, index + 1} {
			if at < 0 || at >= count {
				continue
			}
			peer := relay.Peer{PublicKey: identities[at].PublicKey}
			if servers[at] != nil {
				peer.Address = servers[at].Addr().String()
			}
			peers = append(peers, peer)
		}

		servers[index] = listen(t, identities[index], index > 0 && index < count-1, peers...)
	}
	return servers
}

// serving joins a relay and announces one capability with the reach given.
func serving(t *testing.T, server *relay.Relay, reach announce.Federation) (*identity.Identity, *client.Client) {
	t.Helper()

	me, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	connection := join(t, server, me)

	record, err := announce.Create(me, []announce.Capability{{
		ID: "exec", Kind: "compute", Scheme: "exec", Title: "Run a command",
	}}, announce.Options{Federation: reach, RelayPublicKey: relayKeyFor(reach, server)})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := connection.Announce(ctx, record); err != nil {
		t.Fatalf("announce: %v", err)
	}
	return me, connection
}

func relayKeyFor(reach announce.Federation, server *relay.Relay) []byte {
	if reach == announce.Local {
		return nil
	}
	return server.PublicKey()
}

// waitFor runs a check until it passes, or the time runs out.
func waitFor(t *testing.T, what string, check func() bool) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s did not happen", what)
}

func TestTwoRelaysFederate(t *testing.T) {
	relays := chain(t, 2)
	provider, _ := serving(t, relays[1], announce.Direct)

	_, caller := citizen(t, relays[0])
	ctx := context.Background()

	// The catalog of the partner reaches this relay, and a search finds the
	// citizen of the other relay.
	waitFor(t, "the search finds the citizen of the partner", func() bool {
		page, err := caller.Search(ctx, "run a command", 10, "")
		return err == nil && len(page.Entries) == 1 &&
			page.Entries[0]["public_key"] == provider.EncodePublicKey()
	})

	entries, err := caller.Resolve(ctx, provider.ShortName())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("resolve found %v", entries)
	}
}

// A packet for a citizen of a partner travels the link between the relays.
func TestAPacketCrossesOneLink(t *testing.T) {
	relays := chain(t, 2)
	provider, waiting := serving(t, relays[1], announce.Direct)
	caller, from := citizen(t, relays[0])

	waitFor(t, "the catalog of the partner arrives", func() bool {
		entries, err := from.Resolve(context.Background(), provider.EncodePublicKey())
		return err == nil && len(entries) == 1
	})

	talk, err := session.Establish(caller, provider.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	body, err := frame.Encode(frame.Request, frame.NewRequestID(), map[string]any{"path": "/"}, []byte("across the network"))
	if err != nil {
		t.Fatal(err)
	}

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
	case arrived := <-waiting.Packets():
		got, err := packet.Decode(arrived)
		if err != nil {
			t.Fatal(err)
		}

		joined, err := session.Accept(provider, got.Src, got.EphemeralPublic, got.SessionID)
		if err != nil {
			t.Fatal(err)
		}

		plaintext, err := joined.Decrypt(got.Nonce, got.Ciphertext)
		if err != nil {
			t.Fatal(err)
		}

		answer, err := frame.Decode(plaintext)
		if err != nil {
			t.Fatal(err)
		}
		if string(answer.Body) != "across the network" {
			t.Errorf("body = %q", answer.Body)
		}

		// The answer travels back the same way.
		reply, err := frame.Encode(frame.Response, answer.RequestID, nil, []byte("and back"))
		if err != nil {
			t.Fatal(err)
		}

		nonce, ciphertext, seq, err := joined.Encrypt(reply)
		if err != nil {
			t.Fatal(err)
		}

		back, err := packet.Encode(provider, caller.PublicKey, joined.ID, seq, nonce, ciphertext,
			packet.WithEphemeralKey(joined.EphemeralPublic))
		if err != nil {
			t.Fatal(err)
		}
		if err := waiting.SendPacket(back); err != nil {
			t.Fatal(err)
		}

	case <-time.After(15 * time.Second):
		t.Fatal("the packet did not cross the link")
	}

	select {
	case arrived := <-from.Packets():
		got, err := packet.Decode(arrived)
		if err != nil {
			t.Fatal(err)
		}
		plaintext, err := talk.Decrypt(got.Nonce, got.Ciphertext)
		if err != nil {
			t.Fatal(err)
		}
		answer, err := frame.Decode(plaintext)
		if err != nil {
			t.Fatal(err)
		}
		if string(answer.Body) != "and back" {
			t.Errorf("the answer is %q", answer.Body)
		}

	case <-time.After(15 * time.Second):
		t.Fatal("the answer did not come back")
	}
}

// A partner opens its link again after the link breaks.
func TestALinkComesBackAfterItBreaks(t *testing.T) {
	relays := chain(t, 2)
	dialer, answerer := relays[0], relays[1]

	waitFor(t, "the link stands", func() bool {
		return dialer.LinkTo(answerer.PublicKey()) != nil &&
			answerer.LinkTo(dialer.PublicKey()) != nil
	})

	broken := dialer.LinkTo(answerer.PublicKey())
	broken.Close()

	waitFor(t, "the link stands again", func() bool {
		held := dialer.LinkTo(answerer.PublicKey())
		return held != nil && held != broken
	})
}

// A citizen that shares only with its own relay stays there.
func TestALocalCitizenStaysHome(t *testing.T) {
	relays := chain(t, 2)
	provider, _ := serving(t, relays[1], announce.Local)
	_, caller := citizen(t, relays[0])

	// Give the catalog time to carry it, if it were going to.
	time.Sleep(3 * time.Second)

	entries, err := caller.Resolve(context.Background(), provider.EncodePublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a local citizen reached another relay: %v", entries)
	}
}

// A citizen that shares with direct partners reaches them, and goes no
// further.
func TestADirectCitizenReachesOnePartnerOnly(t *testing.T) {
	relays := chain(t, 3)
	provider, _ := serving(t, relays[2], announce.Direct)

	_, middle := citizen(t, relays[1])
	_, far := citizen(t, relays[0])
	ctx := context.Background()

	waitFor(t, "the partner in the middle sees the citizen", func() bool {
		entries, err := middle.Resolve(ctx, provider.EncodePublicKey())
		return err == nil && len(entries) == 1
	})

	time.Sleep(3 * time.Second)
	entries, err := far.Resolve(ctx, provider.EncodePublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a direct citizen crossed two relays: %v", entries)
	}
}

// A citizen that shares with the network crosses a relay that passes traffic
// on, and its packets travel the path.
func TestANetworkCitizenCrossesTheChain(t *testing.T) {
	relays := chain(t, 3)
	provider, waiting := serving(t, relays[2], announce.Network)
	caller, from := citizen(t, relays[0])
	ctx := context.Background()

	waitFor(t, "the far relay learns the citizen", func() bool {
		entries, err := from.Resolve(ctx, provider.EncodePublicKey())
		return err == nil && len(entries) == 1
	})

	talk, err := session.Establish(caller, provider.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	body, err := frame.Encode(frame.Request, frame.NewRequestID(), map[string]any{"path": "/"}, []byte("three relays"))
	if err != nil {
		t.Fatal(err)
	}

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

	var arrived []byte
	select {
	case arrived = <-waiting.Packets():
	case <-time.After(15 * time.Second):
		t.Fatal("the packet did not cross the chain")
	}

	got, err := packet.Decode(arrived)
	if err != nil {
		t.Fatal(err)
	}

	joined, err := session.Accept(provider, got.Src, got.EphemeralPublic, got.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := joined.Decrypt(got.Nonce, got.Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := frame.Decode(plaintext)
	if string(answer.Body) != "three relays" {
		t.Errorf("body = %q", answer.Body)
	}

	// The answer travels the path backwards.
	reply, _ := frame.Encode(frame.Response, answer.RequestID, nil, []byte("and back again"))
	nonce, ciphertext, seq, err = joined.Encrypt(reply)
	if err != nil {
		t.Fatal(err)
	}

	back, err := packet.Encode(provider, caller.PublicKey, joined.ID, seq, nonce, ciphertext,
		packet.WithEphemeralKey(joined.EphemeralPublic))
	if err != nil {
		t.Fatal(err)
	}
	if err := waiting.SendPacket(back); err != nil {
		t.Fatal(err)
	}

	select {
	case arrived := <-from.Packets():
		got, err := packet.Decode(arrived)
		if err != nil {
			t.Fatal(err)
		}
		plaintext, err := talk.Decrypt(got.Nonce, got.Ciphertext)
		if err != nil {
			t.Fatal(err)
		}
		answer, _ := frame.Decode(plaintext)
		if string(answer.Body) != "and back again" {
			t.Errorf("the answer is %q", answer.Body)
		}

	case <-time.After(15 * time.Second):
		t.Fatal("the answer did not come back over the chain")
	}
}

// A relay that does not pass traffic on is a boundary.
func TestARelayWithoutTransitStopsTheChain(t *testing.T) {
	identities := make([]*identity.Identity, 3)
	for index := range identities {
		identities[index], _ = identity.Generate()
	}
	sort.Slice(identities, func(left, right int) bool {
		return string(identities[left].PublicKey) < string(identities[right].PublicKey)
	})

	third := listen(t, identities[2], false, relay.Peer{PublicKey: identities[1].PublicKey})
	second := listen(t, identities[1], false,
		relay.Peer{PublicKey: identities[0].PublicKey},
		relay.Peer{PublicKey: identities[2].PublicKey, Address: third.Addr().String()})
	first := listen(t, identities[0], false, relay.Peer{PublicKey: identities[1].PublicKey, Address: second.Addr().String()})

	provider, _ := serving(t, third, announce.Network)
	_, middle := citizen(t, second)
	_, far := citizen(t, first)
	ctx := context.Background()

	waitFor(t, "the relay in the middle sees the citizen", func() bool {
		entries, err := middle.Resolve(ctx, provider.EncodePublicKey())
		return err == nil && len(entries) == 1
	})

	time.Sleep(3 * time.Second)
	entries, err := far.Resolve(ctx, provider.EncodePublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a relay without transit passed a record on: %v", entries)
	}
}

// testLog writes the relay log when ARC_TEST_LOG is set.
func testLog() *slog.Logger {
	if os.Getenv("ARC_TEST_LOG") == "" {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
