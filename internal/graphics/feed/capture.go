package feed

import "math"

// A picture's arrival less its media time is the offset of the device's clock from the PC's
// plus the delay the network and the decoder put on it; the least of it over the session's
// frames is the offset plus the least such delay, so media time plus it estimates when the
// device captured a picture, on the PC's clock, a few milliseconds late. The least is kept
// over two windows of captureWindow, so the clocks' drift does not build up.
const captureWindow = 15_000.0

// captureClock estimates when the device captured pictures (FrameInfo.Captured).
type captureClock struct {
	least, before float64
	since         float64
	started       bool
}

// captured takes a picture's media time and arrival, in milliseconds, and answers its capture
// time on the PC's clock in milliseconds since the epoch.
func (c *captureClock) captured(media, arrival float64) float64 {
	if math.IsNaN(media) || math.IsNaN(arrival) || arrival <= 0 {
		return math.NaN()
	}
	delay := arrival - media
	switch {
	case !c.started:
		c.started, c.least, c.before, c.since = true, delay, math.Inf(1), arrival
	case arrival-c.since >= captureWindow:
		c.before, c.least, c.since = c.least, delay, arrival
	default:
		c.least = min(c.least, delay)
	}
	return media + min(c.least, c.before)
}
