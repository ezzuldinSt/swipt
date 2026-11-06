package feed

import (
	"sync"
	"time"

	"github.com/swipt-automation/swipt/internal/graphics/clock"
)

// feedClock stamps the feed's stages: ticks reads a precise monotonic clock in nanoseconds and
// wall answers the wall-clock time of a reading, in milliseconds since the epoch.
type feedClock struct {
	ticks func() int64
	wall  func(ticks int64) float64
}

var preciseClock = feedClock{ticks: clock.Ticks, wall: clock.Milliseconds}

// Timing sums the durations of a stage of the feed, read on the precise clock.
type Timing struct {
	Count           int64
	Last, Mean, Max time.Duration
}

type timing struct {
	access              sync.Mutex
	count               int64
	total, last, oldest time.Duration
}

func (t *timing) add(duration time.Duration) {
	t.access.Lock()
	t.count++
	t.total += duration
	t.last = duration
	t.oldest = max(t.oldest, duration)
	t.access.Unlock()
}

func (t *timing) read() Timing {
	t.access.Lock()
	defer t.access.Unlock()
	read := Timing{Count: t.count, Last: t.last, Max: t.oldest}
	if t.count > 0 {
		read.Mean = t.total / time.Duration(t.count)
	}
	return read
}
