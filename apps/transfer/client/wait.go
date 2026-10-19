package client

import "time"

// confirmWait is base, and one second more for each rate bytes of size.
func confirmWait(base time.Duration, rate, size int64) time.Duration {
	return base + time.Duration(size)*time.Second/time.Duration(rate)
}
