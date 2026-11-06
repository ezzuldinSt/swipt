//go:build !windows

package clock

import "time"

var started = time.Now()

// Go's monotonic clock counts nanoseconds on macOS and Linux.
func ticks() int64 {
	return int64(time.Since(started))
}

func now() float64 {
	return float64(time.Now().UnixNano()) / 1e6
}
