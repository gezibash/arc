package client

import "time"

// maxExtraWait caps what the size adds to a wait.
const maxExtraWait = 6 * time.Hour

// confirmWait is base, and one second more for each rate bytes of size, at
// most maxExtraWait more. It divides first, so a large size cannot
// overflow.
func confirmWait(base time.Duration, rate, size int64) time.Duration {
	if size <= 0 || rate <= 0 {
		return base
	}
	if size/rate >= int64(maxExtraWait/time.Second) {
		return base + maxExtraWait
	}
	whole := time.Duration(size/rate) * time.Second
	rest := time.Duration(float64(size%rate) / float64(rate) * float64(time.Second))
	return base + whole + rest
}
