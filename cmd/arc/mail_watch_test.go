package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/internal/testrelay"
)

// A provider takes its mail from a watch on the relay. With a tick of one
// hour, no sync runs during the test, and the mail still arrives at once.
func TestServeTakesMailFromAWatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	relay := testrelay.Start(t)
	echo := providerBinary(t, "../../adapters/provider/host/testdata/echo")
	provider, providerKey := providerHome(t, relay)
	dir := app(t, "echo", fmt.Sprintf("version = 2\n[serve]\ncommand = %q\nmanifest = \"./manifest.json\"\n", echo))
	serveDir(t, ctx, provider, dir, "--interval", "1h")
	caller, _ := providerHome(t, relay)
	ok(t, caller, "", "install", providerKey, "--yes")

	// A message: the provider acknowledges it from its watch.
	ok(t, caller, "", "message", "send", providerKey, "a message while the tick sleeps")
	delivered := func() bool {
		ok(t, caller, "", "sync")
		return strings.Contains(ok(t, caller, "", "message", "outbox"), "delivered")
	}
	if !within(10*time.Second, delivered) {
		t.Fatalf("the message was not delivered: %s", ok(t, caller, "", "message", "outbox"))
	}

	// A store-and-forward call: the provider answers it from its watch.
	// Its text is hostile, and the echo provider sends it back, so the
	// reply reaches the terminal escaped too.
	ok(t, caller, "", "call", "--later", "echo+arc://"+providerKey+"/", "carried call "+hostile)
	answered := func() bool {
		ok(t, caller, "", "sync")
		return strings.Contains(ok(t, caller, "", "call", "results"), "reply: ")
	}
	if !within(10*time.Second, answered) {
		t.Fatalf("the carried call was not answered: %s", ok(t, caller, "", "call", "results"))
	}
	results := ok(t, caller, "", "call", "results")
	if strings.ContainsAny(results, "\x1b\u2028\U000e0068") || !strings.Contains(results, "reply: ") || strings.Count(results, shownHostile) != 2 {
		t.Errorf("arc call results printed %q, want the text and the reply, each as %q", results, shownHostile)
	}
}

// within checks a condition each 200 ms until it holds, for at most limit.
func within(limit time.Duration, check func() bool) bool {
	for deadline := time.Now().Add(limit); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		if check() {
			return true
		}
	}
	return false
}
