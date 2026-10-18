package arcprogram

// SetStarted makes a call run f between the start of arc and the wait for
// it, until restore runs.
func SetStarted(f func()) (restore func()) {
	old := started
	started = f
	return func() { started = old }
}
