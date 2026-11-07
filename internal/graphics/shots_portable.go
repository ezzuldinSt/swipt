//go:build !darwin || !cgo

package graphics

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/swipt-automation/swipt/internal/coredevice"
	"github.com/swipt-automation/swipt/internal/graphics/clock"
	"github.com/swipt-automation/swipt/internal/graphics/feed"
)

// The screenshot service's PNG takes 3.3 to 3.5 s over Wi-Fi (iPhone18,2, iOS 27.0.1): some
// 0.4 s for the device to render it and the rest to send its 4.45 MB. The live stream shows
// every change of the screen within a frame, so while it runs a PNG stays exact for as long as
// no frame differs from the one before by a single sample: screenshot answers a PNG it took
// earlier, or one it took in the background once the screen stood still, when the frames since
// the PNG began prove the screen the same up to the call.
const (
	// The screen must stand still this long, and the last action or input be this old, before
	// a PNG is taken in the background.
	shotSettle = 500 * time.Millisecond
	// A background PNG the screen changed under before anything read it doubles the wait before
	// the next, from twice shotSettle up to shotPatience.
	shotPatience = 8 * time.Second
	// A frame proves the screen at the call once its time on the device's clock, put on the
	// PC's by the frames that came soonest, is this much past the call: the device's
	// encoding and the network take at least that.
	shotProofLag = 25.0
	// The frame that proves a PNG must come within this; a still screen streams some 42
	// frames a second (iPhone18,2, iOS 27.0.1).
	shotProofLimit = 150 * time.Millisecond
	// Frames this far apart, in milliseconds, leave a gap in which the screen was not watched,
	// as when the stream closed and opened again; so do units that gave no frame, as when the
	// decoder waited for a keyframe. A shorter pause with every unit decoded is not one: every
	// picture the device sent was compared, and a PNG's own transfer holds the stream's packets
	// back 150-250 ms on Wi-Fi (iPhone18,2, iOS 27.0.1 24A446), so a 250 ms limit made the PNG
	// stale by the time it landed.
	shotGap = 1000.0
	// After the device declined a capture as another was still in progress (CoreDeviceError
	// 25003), a fresh one is asked for again this often, for up to shotBusyLimit.
	shotBusyRetry     = 100 * time.Millisecond
	shotBusyLimit     = 2 * time.Second
	captureInProgress = 25003
)

// shotFrame is a frame of the live feed, as the cache compares them.
type shotFrame interface {
	Info() feed.FrameInfo
	// Same tells whether the frame shows the same samples as other.
	Same(other shotFrame) bool
	Release()
}

type feedShotFrame struct {
	frame *feed.Frame
}

func (f feedShotFrame) Info() feed.FrameInfo { return f.frame.Info() }
func (f feedShotFrame) Release()             { f.frame.Release() }

func (f feedShotFrame) Same(other shotFrame) bool {
	previous, ok := other.(feedShotFrame)
	return ok && f.frame.Same(previous.frame)
}

type capturer func(ctx context.Context) ([]byte, float64, error)

// shot is a PNG of the screen, taken while the watch was at generation.
type shot struct {
	data       []byte
	scale      float64
	generation uint64
	started    time.Time
	background bool
	served     bool
}

// shooting is a capture under way.
type shooting struct {
	generation uint64
	background bool
	started    time.Time
	cancel     context.CancelFunc
	done       chan struct{}
}

// ShotStats counts what the cache did: the screenshots it answered from a PNG it held or one
// under way, the ones it took fresh, and its background captures, the ones of those the
// screen changed under before anything read them, and their bytes.
type ShotStats struct {
	Hits, Joined, Fresh          int
	Background, Wasted, Canceled int
	BackgroundBytes              int64
}

// shotCache watches the frames of the live feed for any change of the screen, and keeps the
// newest PNG while none came since it began.
type shotCache struct {
	// background takes the PNGs the cache takes on its own; lastInput tells when the device
	// last took input.
	background capturer
	lastInput  func() time.Time
	now        func() float64

	// previous is the newest frame, which idle lets go once frames stop coming.
	frameAccess sync.Mutex
	previous    shotFrame
	idle        *time.Timer

	access sync.Mutex
	// changed is closed and made again at every frame.
	changed chan struct{}
	// generation counts the changes, gaps and actions the watch saw; a PNG is exact while it
	// stays the one its capture began in.
	generation  uint64
	stillSince  float64
	lastArrival float64
	// offset is the least arrival less media time of the frames since the watch began, and
	// proven the newest frame's media time put on the PC's clock with it.
	offset, proven float64
	watching       bool
	acting         int
	actedAt        time.Time
	cached         *shot
	flight         *shooting
	patience       time.Duration
	notBefore      time.Time
	stats          ShotStats
}

func newShotCache(background capturer, lastInput func() time.Time) *shotCache {
	return &shotCache{background: background, lastInput: lastInput, now: clock.Now, changed: make(chan struct{})}
}

// frame looks at a frame of the feed, on its decoding goroutine, and takes it over.
func (c *shotCache) frame(f shotFrame) {
	info := f.Info()
	c.frameAccess.Lock()
	defer c.frameAccess.Unlock()
	previous := c.previous
	c.previous = f
	if c.idle == nil {
		c.idle = time.AfterFunc(time.Second, c.release)
	} else {
		c.idle.Reset(time.Second)
	}
	c.access.Lock()
	// The frames of a tiled stream each stand for up to one unit of every band.
	gap := previous == nil || !c.watching || info.Arrival-c.lastArrival > shotGap ||
		info.SincePrevious.Units > int64(max(info.Bands, 1))
	c.access.Unlock()
	// A tiled stream sends no unit for a band that did not change.
	still := !gap && (info.SincePrevious.Repeats || info.Bands > 1 && info.SincePrevious.ChangedBands == 0 || f.Same(previous))
	if previous != nil {
		previous.Release()
	}
	c.access.Lock()
	defer c.access.Unlock()
	if gap || info.Arrival-info.Media < c.offset {
		c.offset = info.Arrival - info.Media
	}
	c.proven = info.Media + c.offset
	c.watching = true
	if !still {
		c.changeLocked(info.Arrival)
	}
	c.lastArrival = info.Arrival
	close(c.changed)
	c.changed = make(chan struct{})
	c.startLocked(info.Arrival)
}

// changeLocked makes every PNG so far stale, and stops a background capture: the screen it
// shows is gone.
func (c *shotCache) changeLocked(at float64) {
	c.generation++
	c.stillSince = at
	if f := c.flight; f != nil && f.background {
		f.cancel()
		if c.acting == 0 && c.watching {
			c.wastedLocked()
		}
	}
	if s := c.cached; s != nil {
		if s.background && !s.served {
			c.wastedLocked()
		}
		c.cached = nil
	}
}

// wastedLocked waits longer before the next background capture.
func (c *shotCache) wastedLocked() {
	c.stats.Wasted++
	c.patience = min(max(c.patience*2, 2*shotSettle), shotPatience)
	c.notBefore = time.Now().Add(c.patience)
}

// release lets the newest frame go when no frame came for a second, as when the stream closed;
// the next frame then starts the watch again.
func (c *shotCache) release() {
	c.frameAccess.Lock()
	defer c.frameAccess.Unlock()
	if c.previous != nil {
		c.previous.Release()
		c.previous = nil
	}
}

// startLocked starts a background capture once the screen stood still long enough, with no
// PNG of it yet, nothing being taken and no action or input lately.
func (c *shotCache) startLocked(at float64) {
	if c.background == nil || c.flight != nil || c.acting > 0 || c.cached != nil && c.cached.generation == c.generation {
		return
	}
	now := time.Now()
	if at-c.stillSince < float64(shotSettle/time.Millisecond) || now.Before(c.notBefore) || now.Sub(c.actedAt) < shotSettle {
		return
	}
	if input := c.lastInput(); !input.IsZero() && now.Sub(input) < shotSettle {
		return
	}
	c.stats.Background++
	ctx, cancel := context.WithCancel(context.Background())
	flight := &shooting{generation: c.generation, background: true, started: now, cancel: cancel, done: make(chan struct{})}
	c.flight = flight
	go func() {
		defer cancel()
		data, scale, err := c.background(ctx)
		c.access.Lock()
		defer c.access.Unlock()
		c.stats.BackgroundBytes += int64(len(data))
		c.landLocked(flight, data, scale, err)
		if err != nil && ctx.Err() != nil {
			c.stats.Canceled++
		}
	}()
}

// landLocked ends a capture; its PNG is kept while the screen did not change since it began.
func (c *shotCache) landLocked(flight *shooting, data []byte, scale float64, err error) {
	if c.flight == flight {
		c.flight = nil
	}
	close(flight.done)
	if err == nil && len(data) > 0 && flight.generation == c.generation && !c.inputSince(flight.started) {
		c.cached = &shot{data: data, scale: scale, generation: flight.generation, started: flight.started, background: flight.background, served: !flight.background}
	} else if flight.background && err == nil {
		c.wastedLocked()
	}
}

func (c *shotCache) inputSince(started time.Time) bool {
	input := c.lastInput()
	return !input.IsZero() && !input.Before(started)
}

// act tells the cache an action starts; the answer tells it the action ended. A PNG taken
// before an action ends may be stale, and a background capture stops at once.
func (c *shotCache) act() func() {
	c.access.Lock()
	c.acting++
	c.changeLocked(c.now())
	c.access.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.access.Lock()
			defer c.access.Unlock()
			c.acting--
			c.actedAt = time.Now()
			c.changeLocked(c.now())
		})
	}
}

// provedLocked waits, briefly, for a frame of the screen as it is past requested: it proves
// the screen the same as when the PNG of generation began, unless it changed meanwhile.
func (c *shotCache) provedLocked(ctx context.Context, requested float64, generation uint64) bool {
	deadline := time.Now().Add(shotProofLimit)
	for {
		if c.generation != generation || !c.watching {
			return false
		}
		if c.proven >= requested+shotProofLag {
			return true
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			return false
		}
		changed := c.changed
		c.access.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-changed:
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
		c.access.Lock()
		if ctx.Err() != nil {
			return false
		}
	}
}

// screenshot answers a PNG of the screen as it is at the call: the one the cache holds, or the
// one under way, when the stream proves the screen did not change since it began; else a fresh
// one from fresh, which the cache then keeps.
func (c *shotCache) screenshot(ctx context.Context, fresh capturer) ([]byte, float64, error) {
	requested := c.now()
	c.access.Lock()
	joined := false
	for {
		if s := c.cached; s != nil && s.generation == c.generation && c.acting == 0 && !c.inputSince(s.started) {
			generation := s.generation
			if c.provedLocked(ctx, requested, generation) && c.cached == s {
				s.served = true
				c.patience = 0
				if joined {
					c.stats.Joined++
				} else {
					c.stats.Hits++
				}
				c.access.Unlock()
				return s.data, s.scale, nil
			}
			if ctx.Err() != nil {
				c.access.Unlock()
				return nil, 0, ctx.Err()
			}
			break
		}
		flight := c.flight
		if flight == nil {
			break
		}
		switch {
		case flight.generation == c.generation && c.acting == 0:
			joined = true
		case flight.background:
			// It shows a screen that is gone: stop it, and wait for the device to be free.
			flight.cancel()
		}
		c.access.Unlock()
		select {
		case <-flight.done:
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
		c.access.Lock()
	}
	c.stats.Fresh++
	flight := &shooting{generation: c.generation, started: time.Now(), cancel: func() {}, done: make(chan struct{})}
	c.flight = flight
	c.access.Unlock()
	data, scale, err := freshCapture(ctx, fresh)
	c.access.Lock()
	c.landLocked(flight, data, scale, err)
	c.access.Unlock()
	return data, scale, err
}

// freshCapture takes a PNG, asking again while the device is still busy with one the cache
// stopped.
func freshCapture(ctx context.Context, fresh capturer) ([]byte, float64, error) {
	deadline := time.Now().Add(shotBusyLimit)
	for {
		data, scale, err := fresh(ctx)
		var declined *coredevice.FeatureError
		if err == nil || !errors.As(err, &declined) || declined.Code != captureInProgress || time.Now().After(deadline) {
			return data, scale, err
		}
		select {
		case <-time.After(shotBusyRetry):
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
}

// Stats answers the cache's counts.
func (c *shotCache) Stats() ShotStats {
	c.access.Lock()
	defer c.access.Unlock()
	return c.stats
}
