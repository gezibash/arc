// Package testrelay runs a Nostr relay inside a test.
package testrelay

import (
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"fiatjaf.com/nostr/eventstore/slicestore"
	"fiatjaf.com/nostr/khatru"
)

// Start runs a relay that keeps events in memory and supports Negentropy, and
// returns its URL. The relay stops when the test ends.
func Start(t *testing.T) string {
	return start(t, true)
}

// StartPlain runs a relay that does not support Negentropy.
func StartPlain(t *testing.T) string {
	return start(t, false)
}

func start(t *testing.T, negentropy bool) string {
	t.Helper()

	db := &slicestore.SliceStore{}
	if err := db.Init(); err != nil {
		t.Fatal(err)
	}

	relay := khatru.NewRelay()
	relay.Log = log.New(io.Discard, "", 0)
	relay.UseEventstore(db, 500)
	if negentropy {
		relay.Negentropy = true
		relay.Info.SupportedNIPs = append(relay.Info.SupportedNIPs, 77)
	}

	server := httptest.NewServer(relay)
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}
