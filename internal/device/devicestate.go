package device

import (
	"context"
	"sync"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
)

// The remote notification proxy relays any Darwin notification it is asked to
// observe, such as com.apple.system.config.network_change (iPhone18,2, iOS
// 27.2 24B115).
const notificationProxyService = "com.apple.mobile.notification_proxy.shim.remote"

var stateNotifications = []string{
	"com.apple.springboard.lockstate",
	"com.apple.springboard.hasBlankedScreen",
	"com.apple.iokit.hid.displayStatus",
}

const (
	lockedPoll  = 5 * time.Second
	displayPoll = time.Second

	relayRetryFirst   = time.Second
	relayRetryLongest = 30 * time.Second
	relayStable       = time.Minute
)

type darwinNotifications struct {
	conn *conn

	access   sync.Mutex
	watchers map[chan struct{}]struct{}
	relayed  bool
	runs     uint64
	stop     context.CancelFunc
	stopped  chan struct{}
}

func newDarwinNotifications(c *conn) *darwinNotifications {
	return &darwinNotifications{conn: c, watchers: make(map[chan struct{}]struct{})}
}

// watch signals each relayed notification. relaying reports which run of the
// relay this is and whether it relays now: a state read while it relays stays
// current until a notification arrives, as long as the run is the same, since
// a new run may have missed notifications before it started.
func (d *darwinNotifications) watch() (changed <-chan struct{}, relaying func() (uint64, bool), cancel func()) {
	signal := make(chan struct{}, 1)
	d.access.Lock()
	d.watchers[signal] = struct{}{}
	if d.stop == nil {
		ctx, stop := context.WithCancel(d.conn.ctx)
		d.stop, d.stopped = stop, make(chan struct{})
		go d.observe(ctx, d.stopped)
	}
	d.access.Unlock()
	var once sync.Once
	return signal, d.relaying, func() {
		once.Do(func() {
			d.access.Lock()
			defer d.access.Unlock()
			delete(d.watchers, signal)
			if len(d.watchers) == 0 && d.stop != nil {
				d.stop()
				d.stop, d.relayed = nil, false
			}
		})
	}
}

func (d *darwinNotifications) relaying() (uint64, bool) {
	d.access.Lock()
	defer d.access.Unlock()
	return d.runs, d.relayed
}

// observe relays the notifications until stopped, starting the relay again
// after it fails.
func (d *darwinNotifications) observe(ctx context.Context, stopped chan struct{}) {
	defer close(stopped)
	delay := relayRetryFirst
	for {
		started := time.Now()
		err := d.relay(ctx)
		d.access.Lock()
		if ctx.Err() == nil {
			d.relayed = false
		}
		d.access.Unlock()
		if ctx.Err() != nil {
			return
		}
		d.conn.logger.Debug(E.Cause(err, "observe the device's lock and display notifications"))
		if time.Since(started) >= relayStable {
			delay = relayRetryFirst
		}
		if sleep(ctx, delay) != nil {
			return
		}
		delay = min(delay*2, relayRetryLongest)
	}
}

func (d *darwinNotifications) relay(ctx context.Context) error {
	conn, err := d.conn.device.DialShim(ctx, notificationProxyService)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	defer conn.Close()
	for _, name := range stateNotifications {
		if err = conn.Send(map[string]any{"Command": "ObserveNotification", "Name": name}); err != nil {
			return E.Cause(err, "observe ", name)
		}
	}
	d.access.Lock()
	if ctx.Err() == nil {
		d.relayed = true
		d.runs++
	}
	d.access.Unlock()
	for {
		message, err := conn.Receive()
		if err != nil {
			return err
		}
		switch message["Command"] {
		case "RelayNotification":
			name, _ := message["Name"].(string)
			d.conn.events.publish(Event{Kind: EventDevice, Name: name})
			d.access.Lock()
			for signal := range d.watchers {
				select {
				case signal <- struct{}{}:
				default:
				}
			}
			d.access.Unlock()
		case "ProxyDeath":
			return E.New("the notification proxy stopped")
		}
	}
}

func (d *darwinNotifications) close() {
	d.access.Lock()
	stop, stopped := d.stop, d.stopped
	d.stop = nil
	d.access.Unlock()
	if stop != nil {
		stop()
		<-stopped
	}
}

func (p *Phone) WatchState(ctx context.Context, name string, emit func(any)) error {
	read := func(ctx context.Context, c *conn) (any, error) {
		if name == "locked" {
			locked, err := c.lockState.locked(ctx, c.device)
			if err != nil {
				return nil, E.Cause(err, "read the lock state")
			}
			return locked, nil
		}
		return c.backlight(ctx)
	}
	poll := displayPoll
	if name == "locked" {
		poll = lockedPoll
	}
	var last any
	first := true
	for {
		c, release, err := p.connection.acquire(ctx)
		if err != nil {
			return err
		}
		changed, relaying, cancel := c.darwin.watch()
		operation, stop := c.operation(ctx)
		for operation.Err() == nil {
			value, err := read(operation, c)
			switch {
			case err != nil && operation.Err() == nil:
				p.logger.Debug(E.Cause(err, "read ", name))
			case err == nil && (first || value != last):
				first, last = false, value
				emit(value)
			}
			wait := poll
			if _, relayed := relaying(); !relayed {
				wait = displayPoll
			}
			timer := time.NewTimer(wait)
			select {
			case <-changed:
			case <-timer.C:
			case <-operation.Done():
			}
			timer.Stop()
		}
		stop()
		cancel()
		release()
		if err = ctx.Err(); err != nil {
			return err
		}
	}
}

// The backlight state is kept between actions and read again after a relayed
// notification, a button press, or input that reaches a display that was not
// activeOn, which wakes or brightens it. The display may dim without a
// notification, so a state older than backlightRefresh is read again in the
// background as it is served.
const (
	backlightRefresh       = time.Second
	backlightRefreshLimit  = 5 * time.Second
	unrelayedBacklightLife = time.Second
)

type backlightCache struct {
	access     sync.Mutex
	value      string
	readAt     time.Time
	run        uint64
	version    uint64
	refreshing bool
}

type backlightRead struct {
	version, run uint64
	at           time.Time
}

func (b *backlightCache) begin(run uint64, at time.Time) backlightRead {
	b.access.Lock()
	defer b.access.Unlock()
	return backlightRead{version: b.version, run: run, at: at}
}

// store keeps a state read since begin, unless something that changes it
// happened meanwhile or a later read is kept already.
func (b *backlightCache) store(read backlightRead, value string) {
	b.access.Lock()
	defer b.access.Unlock()
	if read.version != b.version || read.at.Before(b.readAt) {
		return
	}
	b.value, b.readAt, b.run = value, read.at, read.run
}

// cached answers the kept state while it is current: read during the relay's
// current run with no notification since, or within unrelayedBacklightLife
// without the relay. due tells that it is old enough to read again.
func (b *backlightCache) cached(now time.Time, run uint64, relaying bool) (value string, current, due bool) {
	b.access.Lock()
	defer b.access.Unlock()
	if b.readAt.IsZero() {
		return "", false, false
	}
	age := now.Sub(b.readAt)
	if (!relaying || run != b.run) && age >= unrelayedBacklightLife {
		return "", false, false
	}
	return b.value, true, age >= backlightRefresh
}

func (b *backlightCache) invalidate() {
	b.access.Lock()
	defer b.access.Unlock()
	b.version++
	b.readAt = time.Time{}
}

func (b *backlightCache) inputSent() {
	b.access.Lock()
	defer b.access.Unlock()
	if b.value != "activeOn" {
		b.version++
		b.readAt = time.Time{}
	}
}

func (b *backlightCache) startRefresh() bool {
	b.access.Lock()
	defer b.access.Unlock()
	if b.refreshing {
		return false
	}
	b.refreshing = true
	return true
}

func (b *backlightCache) endRefresh() {
	b.access.Lock()
	b.refreshing = false
	b.access.Unlock()
}

func (c *conn) watchBacklight() {
	changed, _, cancel := c.darwin.watch()
	defer cancel()
	received, unsubscribe := c.events.subscribe()
	defer unsubscribe()
	for {
		select {
		case <-changed:
			c.backlightCache.invalidate()
		case event := <-received:
			if event.Kind == EventInput {
				c.backlightCache.inputSent()
			}
		case <-c.ctx.Done():
			return
		}
	}
}
