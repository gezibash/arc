package relaylog

import (
	"testing"
	"time"
)

// The duration of a connection counts from its accept, not from the
// upgrade, which comes after the reply that the client reads: else a
// connection open 50 ms for its client could log less (#454).
func TestADurationCountsFromTheAccept(t *testing.T) {
	c := &conn{accepted: time.Now().Add(-50 * time.Millisecond)}
	// The upgrade starts the record of errors 30 ms after the accept.
	c.start()
	if d, _ := c.stop(); d < 50*time.Millisecond {
		t.Errorf("a connection accepted 50 ms ago lasted %s, want at least 50ms", d)
	}
}
