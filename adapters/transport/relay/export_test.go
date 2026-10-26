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

// SetDownFor changes how long a fetch fails at once after a connect failed,
// and returns a function that puts the old value back.
func SetDownFor(d time.Duration) func() {
	old := downFor
	downFor = d
	return func() { downFor = old }
}

// Reach runs reach, for a test that gives it an attempt of its own.
var Reach = reach

// SetCallTimeout changes the time that a call gives the relay, and returns
// a function that puts the old one back.
func SetCallTimeout(d time.Duration) func() {
	old := callTimeout
	callTimeout = d
	return func() { callTimeout = old }
}

// SetQuickFor changes how long this process remembers a connect that
// failed at once, and returns a function that puts the old value back.
func SetQuickFor(d time.Duration) func() {
	old := quickFor
	quickFor = d
	return func() { quickFor = old }
}

// HostClosed runs hostClosed.
var HostClosed = hostClosed
