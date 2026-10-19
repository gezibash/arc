//go:build !arctest

package files

// sendLimit is the number of bytes that Send gives before end; a test build
// can give fewer, as a sender that stops early.
func sendLimit(n int64) int64 { return n }
