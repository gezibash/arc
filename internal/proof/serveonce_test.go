package proof

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/keys"
)

// TestOneHomeRunsEachRequestOnce proves that one home serves a capability
// with one arc serve at a time (#447). A second serve waits for the first,
// and says so, and takes over when the first gets SIGKILL. Then, as in
// Cato's restart case, a new serve starts every 5 s while the old one gets
// SIGTERM or SIGKILL in turn, and a caller calls all the time: no request
// runs twice.
func TestOneHomeRunsEachRequestOnce(t *testing.T) {
	work := t.TempDir()
	url := startRelay(t, filepath.Join(work, "relay"))
	provider := arc(t, filepath.Join(work, "provider")).with("ECHO_LOG=" + filepath.Join(work, "runs"))
	provider.run("keys", "gen")
	provider.run("relay", "add", url)
	public, err := hex.DecodeString(provider.key())
	if err != nil {
		t.Fatal(err)
	}
	address := "exec://" + program("echo") + "?manifest=" + filepath.Join(repo, "apps/exec/manifest.json")

	first := provider.serve(address)
	second := provider.start("serve", address)
	if !second.waitFor("waiting for another arc serve of exec in this home", 5*time.Second) {
		t.Fatalf("a second serve of the home did not wait:\n%s", second.output())
	}
	if strings.Contains(second.output(), "serves") {
		t.Fatalf("a second serve of the home serves too:\n%s", second.output())
	}
	_ = syscall.Kill(-first.command.Process.Pid, syscall.SIGKILL)
	if !second.waitFor("serves", 15*time.Second) {
		t.Fatalf("the second serve did not take over after SIGKILL of the first:\n%s", second.output())
	}
	t.Log("ok: a second serve of the home waits, and takes over after SIGKILL of the first")

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	var sent int
	wg.Go(func() {
		for i := 0; ctx.Err() == nil; i++ {
			callCtx, stop := context.WithTimeout(ctx, 3*time.Second)
			_, _, _ = call.Live(callCtx, keys.Generate(), nostr.PubKey(public), call.Request{Capability: "exec", Method: "ECHO", Path: "/", Body: fmt.Sprintf("run-%d", i)}, relay.Relay{URL: url})
			stop()
			sent = i + 1
		}
	})
	current := second
	for i := range 4 {
		time.Sleep(5 * time.Second)
		// The new serve either waits for the old one, or, with no lock,
		// serves beside it; both then run for a second, as in a restart
		// that overlaps.
		next := provider.start("serve", address)
		deadline := time.Now().Add(20 * time.Second)
		for !strings.Contains(next.output(), "waiting for another arc serve") && !strings.Contains(next.output(), "serves") && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		time.Sleep(time.Second)
		signal := syscall.SIGTERM
		if i%2 == 1 {
			signal = syscall.SIGKILL
		}
		_ = syscall.Kill(-current.command.Process.Pid, signal)
		if !next.waitFor("serves", 20*time.Second) {
			t.Fatalf("serve %d did not take over after %s:\n%s", i+3, signal, next.output())
		}
		current = next
	}
	cancel()
	wg.Wait()

	body, err := os.ReadFile(filepath.Join(work, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	runs := map[string]int{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(body)), "\n") {
		runs[line]++
	}
	twice := 0
	for line, n := range runs {
		if n > 1 && strings.HasPrefix(line, "run-") {
			twice++
		}
	}
	if twice > 0 || len(runs) == 0 {
		t.Fatalf("%d of %d calls ran twice, of %d sent", twice, len(runs), sent)
	}
	t.Logf("ok: %d calls ran, of %d sent, through 4 restarts; none ran twice", len(runs), sent)
}
