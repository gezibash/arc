//go:build !race

package rings

import "time"

// latencyBudget is the time from the publish of a wrap to the ring at a
// local URL. The relay saves the wrap, and rings in the same hook, so the
// budget holds only the WebSocket write, the TLS handshake and one POST.
const latencyBudget = 200 * time.Millisecond
