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
	ok(t, caller, "", "call", "--later", "echo+arc://"+providerKey+"/", "carried call")
	answered := func() bool {
		ok(t, caller, "", "sync")
		return strings.Contains(ok(t, caller, "", "call", "results"), "carried call")
	}
	if !within(10*time.Second, answered) {
		t.Fatalf("the carried call was not answered: %s", ok(t, caller, "", "call", "results"))
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
