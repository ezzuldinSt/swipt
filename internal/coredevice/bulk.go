package coredevice

import (
	"context"
	"sync"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
)

// A tunnel is one TCP connection, which carries every connection to the
// device and its datagrams in order, so a transfer queues the requests behind
// it. A second tunnel carries the display stream and screenshots instead:
// with screenshots transferring at 1.2-1.9 MB/s, getdisplayinfo took 104 ms at
// p90 and up to 167 ms on the same tunnel, against 64 ms and up to 104 ms with
// them on a second one (the medians of 11 runs each), and 15-24 ms at p90 with
// nothing transferring; a stream of up to 1.1 MB/s made no difference
// (iPhone18,2, iOS 27.0.1 24A446, connected straight through a Wi-Fi access
// point).
const (
	// The second tunnel opened in 0.52-0.62 s over Wi-Fi (iPhone18,2, iOS
	// 27.0.1 24A446). A transfer that waits for it takes the first after
	// bulkWait, while it goes on opening for up to bulkPatience.
	bulkWait     = 1500 * time.Millisecond
	bulkPatience = 5 * time.Second
	// bulkIdle is how long the second tunnel stays open once nothing uses it,
	// as long as the display stream stays warm after a script.
	bulkIdle = 30 * time.Second
	// After the second tunnel failed to open, transfers take the first one
	// for this long.
	bulkRetry = time.Minute
)

type bulkTunnel struct {
	owner   *Device
	connect func(ctx context.Context) (*Device, error)
	// waitFor, idleFor and retryAfter are bulkWait, bulkIdle and bulkRetry,
	// but in tests.
	waitFor, idleFor, retryAfter time.Duration

	access   sync.Mutex
	device   *Device
	leases   int
	idle     *time.Timer
	opening  chan struct{}
	failedAt time.Time
	closed   bool
}

// Bulk answers the connection to carry a transfer of bulk data, the display
// stream or a screenshot, and a function to call when the transfer is done:
// a second tunnel to the device, made on the first such transfer, or this
// connection, when there is no second one. With wait, a transfer waits for a
// second tunnel being made, for up to bulkWait; without, it takes this
// connection meanwhile.
func (d *Device) Bulk(ctx context.Context, wait bool) (*Device, func()) {
	b := d.bulk
	if b == nil {
		return d, func() {}
	}
	b.access.Lock()
	if device := b.healthy(); device != nil {
		defer b.access.Unlock()
		return device, b.lease(device)
	}
	if b.closed || time.Since(b.failedAt) < b.retryAfter {
		b.access.Unlock()
		return d, func() {}
	}
	opening := b.opening
	if opening == nil {
		opening = b.open()
	}
	b.access.Unlock()
	if !wait {
		return d, func() {}
	}
	timer := time.NewTimer(b.waitFor)
	defer timer.Stop()
	select {
	case <-opening:
	case <-timer.C:
		return d, func() {}
	case <-ctx.Done():
		return d, func() {}
	}
	b.access.Lock()
	defer b.access.Unlock()
	if device := b.healthy(); device != nil {
		return device, b.lease(device)
	}
	return d, func() {}
}

// healthy answers the second tunnel, unless it has ended.
func (b *bulkTunnel) healthy() *Device {
	if b.device != nil && b.device.Err() != nil {
		b.owner.logger.Debug("the second tunnel for transfers ended: ", b.device.Err())
		b.device, b.leases = nil, 0
		if b.idle != nil {
			b.idle.Stop()
			b.idle = nil
		}
	}
	return b.device
}

func (b *bulkTunnel) open() chan struct{} {
	opening := make(chan struct{})
	b.opening = opening
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), bulkPatience)
		go func() {
			select {
			case <-b.owner.Done():
				cancel()
			case <-ctx.Done():
			}
		}()
		began := time.Now()
		device, err := b.connect(ctx)
		cancel()
		b.access.Lock()
		b.opening = nil
		switch {
		case err != nil:
			b.failedAt = time.Now()
			b.owner.logger.Debug(E.Cause(err, "open a second tunnel for transfers; they take the first for ", b.retryAfter))
		case b.closed:
			device.Close()
		default:
			b.owner.logger.Debug("opened a second tunnel for transfers in ", time.Since(began).Round(time.Millisecond))
			b.device, b.leases = device, 0
			b.rest(device)
		}
		b.access.Unlock()
		close(opening)
	}()
	return opening
}

func (b *bulkTunnel) lease(device *Device) func() {
	b.leases++
	if b.idle != nil {
		b.idle.Stop()
		b.idle = nil
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			b.access.Lock()
			defer b.access.Unlock()
			if b.device != device {
				return
			}
			b.leases--
			if b.leases == 0 {
				b.rest(device)
			}
		})
	}
}

// rest closes the second tunnel once it has gone unused for idleFor.
func (b *bulkTunnel) rest(device *Device) {
	var timer *time.Timer
	timer = time.AfterFunc(b.idleFor, func() {
		b.access.Lock()
		if b.idle != timer || b.device != device || b.leases != 0 {
			b.access.Unlock()
			return
		}
		b.idle, b.device = nil, nil
		b.access.Unlock()
		b.owner.logger.Debug("closing the second tunnel, unused for ", b.idleFor)
		device.Close()
	})
	if b.idle != nil {
		b.idle.Stop()
	}
	b.idle = timer
}

func (b *bulkTunnel) close() {
	b.access.Lock()
	b.closed = true
	device := b.device
	b.device = nil
	if b.idle != nil {
		b.idle.Stop()
		b.idle = nil
	}
	b.access.Unlock()
	if device != nil {
		device.Close()
	}
}
