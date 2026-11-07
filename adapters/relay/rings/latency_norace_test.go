//go:build !race

package rings

import (
	"os"
	"time"
)

// latencyBudget is the time from the publish of a wrap to the ring at a
// local URL. The relay saves the wrap, and rings in the same hook, so the
// budget holds only the WebSocket write, the TLS handshake and one POST.
// On a CI runner, whose disk other jobs share, one sync of the store can
// take longer than the whole budget, so CI gives 1 s. That still refuses
// a ring that waits for a poll, or a lost ring.
var latencyBudget = 200 * time.Millisecond

func init() {
	if os.Getenv("CI") != "" {
		latencyBudget = time.Second
	}
}
