package relay

import "time"

// SetReuseWait changes the wait on a used connection, and returns a function
// that puts the old wait back.
func SetReuseWait(d time.Duration) func() {
	old := reuseWait
	reuseWait = d
	return func() { reuseWait = old }
}
