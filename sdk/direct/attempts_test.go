package direct

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test binary play the arc program: with
// FAKE_ARC_QUOTA set, it prints a TURN credential with that quota.
func TestMain(m *testing.M) {
	if quota := os.Getenv("FAKE_ARC_QUOTA"); quota != "" {
		fmt.Printf(`{"urls":["turn:relay.example.org:3478?transport=udp"],"username":"1:key","password":"secret","ttl":3600,"quota_left":%s}`, quota)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestRelayQuota checks that the relay attempt starts only if the quota of
// the citizen holds the bytes that are still to go, with a tenth more for
// SCTP and DTLS.
func TestRelayQuota(t *testing.T) {
	turn := func(ctx context.Context) (Credential, error) {
		return RelayTURN(exec.CommandContext(ctx, os.Args[0], "relay", "turn"))
	}

	// 10 MiB left; a file of 20 MiB needs about 22 MiB.
	t.Setenv("FAKE_ARC_QUOTA", fmt.Sprint(10<<20))
	_, err := RelayOptions(context.Background(), turn, Options{}, 20<<20)
	if err == nil {
		t.Fatal("a file of 20 MiB went to a relay that allows 10 MiB")
	}
	if want := "the relay lets this citizen relay 10.0 MiB more today, and this file needs about 22.0 MiB"; err.Error() != want {
		t.Errorf("error %q, want %q", err, want)
	}

	// 30 MiB left holds the same file.
	t.Setenv("FAKE_ARC_QUOTA", fmt.Sprint(30<<20))
	options, err := RelayOptions(context.Background(), turn, Options{STUN: "stun:relay.example.org:3478"}, 20<<20)
	if err != nil {
		t.Fatal(err)
	}
	if options.TURN == nil || options.TURN.Username != "1:key" || options.TURN.Credential != "secret" || !strings.HasPrefix(options.TURN.URLs[0], "turn:relay.example.org") {
		t.Errorf("TURN server %+v", options.TURN)
	}
}

// TestOffererSendsFirst models an offerer behind a router that blocks a
// pair when the first packet comes from outside: only an attempt with a
// hold opens. Section 3.5 puts that attempt first, so one attempt opens
// the pipe and the notes stay empty.
func TestOffererSendsFirst(t *testing.T) {
	var holds []time.Duration
	var notes strings.Builder
	err := Attempts{NoRelay: true, Notes: &notes}.Run(t.Context(), func(_ context.Context, _ Options, hold time.Duration) error {
		holds = append(holds, hold)
		if hold == 0 {
			return ErrNoPath
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 1 || holds[0] != time.Second {
		t.Errorf("holds %v, want one attempt with a hold of 1s", holds)
	}
	if notes.Len() != 0 {
		t.Errorf("notes %q, want none", notes.String())
	}
}

// TestAnswererFirst checks the swapped order of section 3.5: with
// AnswererFirst, the first attempt has no hold, and the second has a hold
// of 1 s, after the note.
func TestAnswererFirst(t *testing.T) {
	var holds []time.Duration
	var notes strings.Builder
	err := Attempts{AnswererFirst: true, NoRelay: true, Notes: &notes}.Run(t.Context(), func(_ context.Context, _ Options, hold time.Duration) error {
		holds = append(holds, hold)
		return ErrNoPath
	})
	if !errors.Is(err, ErrNoPath) {
		t.Fatalf("error %v, want ErrNoPath", err)
	}
	if want := []time.Duration{0, time.Second}; !slices.Equal(holds, want) {
		t.Errorf("holds %v, want %v", holds, want)
	}
	if want := "no direct path in the first order of the packets; trying the other order\n"; notes.String() != want {
		t.Errorf("notes %q, want %q", notes.String(), want)
	}
}

// The error of a relay attempt whose data channel does not open names the
// TURN server, and not a direct path (#427: with path turn, the error read
// "no direct path, and no path through the relay: no direct path").
func TestAFailedRelayAttemptNamesTheTURNServer(t *testing.T) {
	turn := func(context.Context) (Credential, error) {
		return Credential{URLs: []string{"turns:turn.example:443?transport=tcp"}, Username: "u", Password: "p", QuotaLeft: 1 << 30}, nil
	}
	err := Attempts{OnlyRelay: true, TURN: turn, Need: func() int64 { return 0 }}.Run(t.Context(), func(context.Context, Options, time.Duration) error {
		return ErrNoPath
	})
	want := "no direct path, and no path through the relay: the data channel did not open through the TURN server turns:turn.example:443?transport=tcp"
	if err == nil || err.Error() != want {
		t.Errorf("error %v, want %q", err, want)
	}
	if !errors.Is(err, ErrNoPath) {
		t.Errorf("error %v is not ErrNoPath", err)
	}
}

// When the TURN server gives no address, the error says why, as pion's ICE
// agent logged it, such as a dial that failed (#427: "the TURN server of the
// relay gave no address", with no reason, four times in a day).
func TestNoAddressOnTheTURNServerGivesTheReason(t *testing.T) {
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := closed.Addr().String()
	_ = closed.Close()
	turn := func(context.Context) (Credential, error) {
		return Credential{URLs: []string{"turn:" + address + "?transport=tcp"}, Username: "u", Password: "p", QuotaLeft: 1 << 30}, nil
	}
	options, err := RelayOptions(t.Context(), turn, Options{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	pc, err := NewConnection(options)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if _, err := pc.CreateDataChannel("data", nil); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	local, err := Describe(pc, offer)
	if err != nil {
		t.Fatal(err)
	}
	_, err = OfferSTUN(t.Context(), options, local)
	if err == nil || !strings.HasPrefix(err.Error(), "no direct path, and no path through the relay: the TURN server of the relay gave no address: ") || !strings.Contains(err.Error(), address) {
		t.Errorf("error %v, want the reason with the address %s", err, address)
	}
}
