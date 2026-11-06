// Package clock reads time finer than time.Now does on Windows, where time.Now and Go's
// monotonic clock advance with the system timer, in steps of 1 to 15.6 ms. The screen feed
// stamps its stages with it, and the images it answers carry its wall-clock time.
package clock

// Ticks reads a monotonic clock in nanoseconds; only the differences of readings mean anything.
func Ticks() int64 {
	return ticks()
}

// Now is the wall-clock time, in milliseconds since the epoch.
func Now() float64 {
	return now()
}

// Milliseconds is the wall-clock time of an earlier reading of Ticks, in milliseconds since
// the epoch.
func Milliseconds(reading int64) float64 {
	return Now() - float64(Ticks()-reading)/1e6
}
