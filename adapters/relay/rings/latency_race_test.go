//go:build race

package rings

import "time"

// latencyBudget is larger under the race detector, which makes the code
// several times slower on a shared CI runner. It still refuses the old
// design, which waited a settle of 1 s and then up to 1 s for its loop.
const latencyBudget = time.Second
