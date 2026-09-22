package direct_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gezibash/arc/direct"
	"github.com/gezibash/arc/identity"
)

// two citizens that carry each other's control messages, as a relay would.
type citizens struct {
	alice, bob    *identity.Identity
	first, second *direct.Manager
	sessionID     []byte

	// down stops the relay: no control message passes.
	down atomic.Bool
}

var errRelayDown = errors.New("the relay is down")

// meet builds two citizens whose owners allow one conversation to leave the
// relay. The control messages pass straight between them, as the relay
// carries them.
func meet(t *testing.T, callerListens, providerListens bool) *citizens {
	t.Helper()
	return meetWithLease(t, callerListens, providerListens, 30000)
}

// meetWithLease builds the two citizens with a lease of their choice.
func meetWithLease(t *testing.T, callerListens, providerListens bool, lease int) *citizens {
	t.Helper()

	alice, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	bob, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	sessionID := make([]byte, 16)
	if _, err := rand.Read(sessionID); err != nil {
		t.Fatal(err)
	}

	held := &citizens{alice: alice, bob: bob, sessionID: sessionID}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("ARC_TEST_LOG") != "" {
		quiet = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	held.first = direct.NewManager(alice, rules(t, bob.PublicKey, callerListens, lease),
		func(peer, body []byte) error {
			if held.down.Load() {
				return errRelayDown
			}
			go held.second.Control(alice.PublicKey, sessionID, body)
			return nil
		}, quiet)

	held.second = direct.NewManager(bob, rules(t, alice.PublicKey, providerListens, lease),
		func(peer, body []byte) error {
			if held.down.Load() {
				return errRelayDown
			}
			go held.first.Control(bob.PublicKey, sessionID, body)
			return nil
		}, quiet)

	t.Cleanup(func() { held.first.Close(); held.second.Close() })
	return held
}

// rules writes the policy of one owner: it dials the other machine, and
// listens when it is asked to.
func rules(t *testing.T, peer []byte, listens bool, lease int) []direct.Rule {
	t.Helper()

	document := map[string]any{
		"version": 1,
		"rules": []any{map[string]any{
			"peer":       hex.EncodeToString(peer),
			"capability": "primary",
			"scheme":     "exec",
			"path":       "/",
			"lease_ms":   lease,
			"dial":       []any{"127.0.0.1"},
		}},
	}

	if listens {
		rule := document["rules"].([]any)[0].(map[string]any)
		rule["listen"] = map[string]any{"bind": "127.0.0.1", "address": "127.0.0.1", "port": 0}
	}

	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}

	held, err := direct.DecodePolicy(encoded)
	if err != nil {
		t.Fatalf("the policy did not hold: %v", err)
	}
	return held
}

func scope() map[string]any {
	return map[string]any{
		"version": 1, "capability": "primary", "scheme": "exec", "path": "/",
		"caller": "ab", "provider": "cd", "package_hash": "ef",
		"method": "EXEC", "session": "01", "ek": "02",
	}
}

// One citizen offers, the other accepts, and the conversation leaves the
// relay.
func TestARouteLeavesTheRelay(t *testing.T) {
	held := meet(t, true, true)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	route, err := held.first.Offer(ctx, held.bob.PublicKey, scope(), held.sessionID)
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	if route.Role != "caller" {
		t.Errorf("role = %s", route.Role)
	}

	from := held.first.Carrier(held.bob.PublicKey, "primary", "exec", "/")
	if from == nil {
		t.Fatal("the citizen that offered holds no carrier")
	}

	var to *direct.Conn
	for attempt := 0; attempt < 50 && to == nil; attempt++ {
		to = held.second.Carrier(held.alice.PublicKey, "primary", "exec", "/")
		if to == nil {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if to == nil {
		t.Fatal("the citizen that accepted holds no carrier")
	}

	if err := from.Send([]byte("off the relay")); err != nil {
		t.Fatal(err)
	}

	select {
	case arrived := <-to.Packets():
		if string(arrived) != "off the relay" {
			t.Errorf("packet = %q", arrived)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the packet did not travel the carrier")
	}
}

// A route stands when only one side can listen.
func TestARouteStandsWithOneListener(t *testing.T) {
	for _, listens := range []struct{ caller, provider bool }{
		{caller: true, provider: false},
		{caller: false, provider: true},
	} {
		held := meet(t, listens.caller, listens.provider)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		route, err := held.first.Offer(ctx, held.bob.PublicKey, scope(), held.sessionID)
		cancel()

		if err != nil {
			t.Fatalf("caller listens %v, provider listens %v: %v", listens.caller, listens.provider, err)
		}
		if route == nil {
			t.Fatal("no route")
		}
	}
}

// Neither side listens, so nothing stands.
func TestARouteNeedsSomewhereToMeet(t *testing.T) {
	held := meet(t, false, false)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := held.first.Offer(ctx, held.bob.PublicKey, scope(), held.sessionID); err == nil {
		t.Error("a route stood with nowhere to meet")
	}
}

// The owner of the other side allows nothing, so the offer goes nowhere.
func TestAnOfferWithoutAPolicyIsRefused(t *testing.T) {
	held := meet(t, true, true)

	// The citizen that answers holds no rule for this capability.
	other := scope()
	other["capability"] = "other"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := held.first.Offer(ctx, held.bob.PublicKey, other, held.sessionID); err == nil {
		t.Error("a route stood without a rule")
	}
}

func TestAWithdrawalEndsARoute(t *testing.T) {
	held := meet(t, true, true)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	route, err := held.first.Offer(ctx, held.bob.PublicKey, scope(), held.sessionID)
	if err != nil {
		t.Fatal(err)
	}

	held.first.Withdraw(route.ID)

	if held.first.Carrier(held.bob.PublicKey, "primary", "exec", "/") != nil {
		t.Error("the route stands after the withdrawal")
	}

	for attempt := 0; attempt < 50; attempt++ {
		if held.second.Carrier(held.alice.PublicKey, "primary", "exec", "/") == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("the other side still holds the route")
}

func TestAllowed(t *testing.T) {
	held := meet(t, true, true)

	if !held.first.Allowed(held.bob.PublicKey, "primary", "exec", "/") {
		t.Error("the rule of the owner does not answer")
	}
	if held.first.Allowed(held.bob.PublicKey, "other", "exec", "/") {
		t.Error("a capability without a rule answers")
	}
	if held.first.Allowed(held.alice.PublicKey, "primary", "exec", "/") {
		t.Error("a peer without a rule answers")
	}
}

// both returns the carriers of the two sides, once both stand.
func (held *citizens) both(t *testing.T) (*direct.Conn, *direct.Conn) {
	t.Helper()

	from := held.first.Carrier(held.bob.PublicKey, "primary", "exec", "/")
	var to *direct.Conn
	for attempt := 0; attempt < 50 && to == nil; attempt++ {
		to = held.second.CarrierFor(held.alice.PublicKey)
		if to == nil {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if from == nil || to == nil {
		t.Fatalf("the carriers do not stand: %v, %v", from, to)
	}
	return from, to
}

// stands says whether both sides still carry the conversation.
func (held *citizens) stands() bool {
	return held.first.Carrier(held.bob.PublicKey, "primary", "exec", "/") != nil &&
		held.second.CarrierFor(held.alice.PublicKey) != nil
}

func offer(t *testing.T, held *citizens) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := held.first.Offer(ctx, held.bob.PublicKey, scope(), held.sessionID); err != nil {
		t.Fatal(err)
	}
}

// While the relay carries the renewals, a route outlives its lease many times.
func TestARouteRenewsItsLease(t *testing.T) {
	held := meetWithLease(t, true, true, direct.MinLeaseMS)
	offer(t, held)
	from, to := held.both(t)

	time.Sleep(3 * time.Second)

	if !held.stands() {
		t.Fatal("the route ended after three leases, with the relay up")
	}
	if err := from.Send([]byte("still direct")); err != nil {
		t.Fatal(err)
	}
	select {
	case arrived := <-to.Packets():
		if string(arrived) != "still direct" {
			t.Errorf("packet = %q", arrived)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the packet did not travel the carrier")
	}
}

// Without the relay no renewal passes. The route serves until its lease ends,
// and then both sides close the carrier, so the conversation goes back to
// the relay.
func TestARouteEndsWithItsLeaseWithoutTheRelay(t *testing.T) {
	held := meetWithLease(t, true, true, direct.MinLeaseMS)
	offer(t, held)
	from, to := held.both(t)

	held.down.Store(true)

	time.Sleep(300 * time.Millisecond)
	if !held.stands() {
		t.Fatal("the route ended before its lease")
	}

	for name, carrier := range map[string]*direct.Conn{"the side that offered": from, "the side that answered": to} {
		select {
		case <-carrier.Done():
		case <-time.After(2 * time.Second):
			t.Errorf("%s still holds the carrier after the lease", name)
		}
	}
	if held.stands() {
		t.Error("a side still names the route after the lease")
	}
}

// When the relay comes back before the lease ends, the renewals pass again.
func TestARouteRenewsWhenTheRelayComesBack(t *testing.T) {
	held := meetWithLease(t, true, true, direct.MinLeaseMS)
	offer(t, held)
	held.both(t)

	held.down.Store(true)
	time.Sleep(600 * time.Millisecond)
	held.down.Store(false)

	time.Sleep(2 * time.Second)
	if !held.stands() {
		t.Fatal("the route ended, although the relay came back before its lease ended")
	}
}
