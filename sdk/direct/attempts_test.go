package direct

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
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
