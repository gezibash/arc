//go:build arctest

package files

import (
	"os"
	"strconv"
)

// sendLimit is the number of bytes that Send gives before end: at most
// TRANSFER_TEST_END_AFTER in a build with the tag arctest, as a sender that
// stops early.
func sendLimit(n int64) int64 {
	if m, err := strconv.ParseInt(os.Getenv("TRANSFER_TEST_END_AFTER"), 10, 64); err == nil && m >= 0 && m < n {
		return m
	}
	return n
}
