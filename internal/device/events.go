package device

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

const (
	pollFirst   = 50 * time.Millisecond
	pollLongest = 500 * time.Millisecond
	eventBuffer = 64
)

type EventKind int

const (
	EventForeground EventKind = iota
	EventAccessibility
	EventTree
	EventAppState
	EventDevice
	EventInput
)

type Event struct {
	Kind         EventKind
	PID          uint64
	BundleID     string
	Notification uint64
	Name         string
}

type events struct {
	screen      atomic.Uint64
	access      sync.Mutex
	watchers    map[chan struct{}]bool
	subscribers map[chan Event]struct{}
	// inputAt is when input was last sent, in nanoseconds since the epoch.
	inputAt atomic.Int64
}

func newEvents() *events {
	return &events{watchers: make(map[chan struct{}]bool), subscribers: make(map[chan Event]struct{})}
}

func (e *events) watch(screen bool) (<-chan struct{}, func()) {
	changed := make(chan struct{}, 1)
	e.access.Lock()
	e.watchers[changed] = screen
	e.access.Unlock()
	return changed, func() {
		e.access.Lock()
		delete(e.watchers, changed)
		e.access.Unlock()
	}
}

func (e *events) subscribe() (<-chan Event, func()) {
	received := make(chan Event, eventBuffer)
	e.access.Lock()
	e.subscribers[received] = struct{}{}
	e.access.Unlock()
	return received, func() {
		e.access.Lock()
		delete(e.subscribers, received)
		e.access.Unlock()
	}
}

func (e *events) publish(event Event) {
	if event.Kind == EventInput {
		e.inputAt.Store(time.Now().UnixNano())
	}
	screen := event.Kind != EventAppState
	if screen {
		e.screen.Add(1)
	}
	e.access.Lock()
	defer e.access.Unlock()
	for changed, onlyScreen := range e.watchers {
		if onlyScreen && !screen {
			continue
		}
		select {
		case changed <- struct{}{}:
		default:
		}
	}
	for received := range e.subscribers {
		select {
		case received <- event:
		default:
		}
	}
}

func (c *conn) wait(ctx context.Context, deadline time.Time, check func(ctx context.Context) (bool, error)) (bool, error) {
	changed, stop := c.events.watch(false)
	defer stop()
	delay := pollFirst
	for {
		done, err := check(ctx)
		if err != nil || done {
			return done, err
		}
		wait := delay
		if !deadline.IsZero() {
			left := time.Until(deadline)
			if left <= 0 {
				return false, nil
			}
			wait = min(wait, left)
		}
		timer := time.NewTimer(wait)
		select {
		case <-changed:
			delay = pollFirst
		case <-timer.C:
			delay = min(delay*2, pollLongest)
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		}
		timer.Stop()
	}
}

func (p *Phone) Events(ctx context.Context, emit func(Event)) error {
	for {
		c, operation, done, err := p.begin(ctx)
		if err != nil {
			return err
		}
		received, cancel := c.events.subscribe()
		for operation.Err() == nil {
			select {
			case event := <-received:
				emit(event)
			case <-operation.Done():
			}
		}
		cancel()
		done()
		if err = ctx.Err(); err != nil {
			return err
		}
	}
}

// LastInput is when this server last sent the device input, a script's or a viewer's, or the
// zero time when it has sent none since the device connected. Screen waits right after input
// give the screen time to answer it.
func (p *Phone) LastInput() time.Time {
	s := p.connection
	s.access.Lock()
	attached := s.attached
	s.access.Unlock()
	if attached == nil {
		return time.Time{}
	}
	if at := attached.events.inputAt.Load(); at != 0 {
		return time.Unix(0, at)
	}
	return time.Time{}
}
