// Package testutil contains helpers for checking successful test setup reads.
package testutil

// Must fails the test by panicking if setup cannot produce its value. It keeps
// existing behavioral assertions from accidentally discarding storage errors.
func Must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}
