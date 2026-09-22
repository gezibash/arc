package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/catalog"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/testrelay"
	"github.com/gezibash/arc/delivery/transport/relay"
)

// A provider that stopped leaves its last announcement on the relay. Without
// a wake hook, a live call needs an announcement that is at most 5 minutes
// old (docs/delivery/SPEC.md, section 15.1, step 3). Otherwise the call stops
// at once with peer_offline, and does not wait for its timeout.
func TestALiveCallNeedsACurrentAnnouncementOrAWakeHook(t *testing.T) {
	url := testrelay.Start(t)
	manifest, err := os.ReadFile(filepath.Join("..", "exec-provider", "interface.json"))
	if err != nil {
		t.Fatal(err)
	}
	provider := keys.Generate()
	announce := func(at time.Time) {
		t.Helper()
		event, err := catalog.AnnounceManifest(provider, manifest, nostr.Timestamp(at.Unix()))
		if err != nil {
			t.Fatal(err)
		}
		if err := (relay.Relay{URL: url}).Send(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	announce(time.Now().Add(-10 * time.Minute))

	home := t.TempDir()
	for _, args := range [][]string{{"keys", "gen"}, {"relay", "add", url}, {"install", provider.Public.Hex(), "--yes"}} {
		if err := arc(t, home, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	call := func() (error, time.Duration) {
		start := time.Now()
		err := arc(t, home, "call", provider.Public.Hex(), `{"argv":["true"]}`, "--timeout", "3s")
		return err, time.Since(start)
	}

	err, took := call()
	if err == nil || !strings.Contains(err.Error(), "peer_offline") || took > 2*time.Second {
		t.Fatalf("a call to a provider announced 10 minutes ago returned %v after %s, want peer_offline at once", err, took)
	}

	// A wake hook runs instead of the check. This one fails.
	hook := "[wake.\"" + provider.Public.Hex() + "\"]\nkind = \"command\"\nargv = [\"sh\", \"-c\", \"echo 'the machine is gone' >&2; exit 3\"]\n"
	if err := os.WriteFile(filepath.Join(home, "wake.toml"), []byte(hook), 0o600); err != nil {
		t.Fatal(err)
	}
	if err, _ := call(); err == nil || !strings.Contains(err.Error(), "wake_failed") || !strings.Contains(err.Error(), "the machine is gone") {
		t.Fatalf("a call whose wake hook failed returned %v, want wake_failed with the hook's error", err)
	}
	if err := os.Remove(filepath.Join(home, "wake.toml")); err != nil {
		t.Fatal(err)
	}

	// A current announcement lets the call go out. Nobody serves, so the
	// relay says that no one listened.
	announce(time.Now())
	if err, _ := call(); err == nil || !strings.Contains(err.Error(), "no one was listening") {
		t.Fatalf("a call to a provider announced now returned %v, want the relay's answer", err)
	}
}
