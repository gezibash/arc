package relay

import "time"

// SetReuseWait changes the wait on a used connection, and returns a function
// that puts the old wait back.
func SetReuseWait(d time.Duration) func() {
	old := reuseWait
	reuseWait = d
	return func() { reuseWait = old }
}

// SetStallLimit changes the limit of the first attempt to reach a relay, and
// forgets the address family of each host. It returns a function that puts
// the old limit back.
func SetStallLimit(d time.Duration) func() {
	old := stallLimit
	stallLimit = d
	forget := func() {
		families.Lock()
		defer families.Unlock()
		clear(families.of)
	}
	forget()
	return func() {
		stallLimit = old
		forget()
	}
}

// SetIdleCheck changes how long a kept connection can be idle before a call
// checks it, and returns a function that puts the old value back.
func SetIdleCheck(d time.Duration) func() {
	old := idleCheck
	idleCheck = d
	return func() { idleCheck = old }
}
