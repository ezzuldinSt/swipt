package device

import (
	"context"
	"errors"
	"sync"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/coredevice/accessibility"
)

const screenViewingWindow = 10 * time.Second

// The banner is at depth 6 below SpringBoard's application element on iOS
// 27.3 24C5037i; depth 5 omitted it. Its pushed AX tree omitted the banner.
const screenViewingDepth = 6

// remotepairingdeviced waits for keybagd.lock_status after the final media
// stream ends, then schedules the audit notification (24C5037i, 0x100068fa8,
// 0x100069220, 0x1000172a0). The relay's State was 0 on both lock and unlock.
const keybagLockStatus = "com.apple.mobile.keybagd.lock_status"

type screenViewing struct {
	connection *Connection
	access     sync.Mutex
	pending    bool
	generation uint64
	after      time.Time
	stopped    time.Time
	release    func()
	cancel     context.CancelFunc
	running    *conn
}

func (v *screenViewing) ended(c *conn, stopping time.Time) {
	if v.connection.ctx.Err() != nil {
		return
	}
	if c.ctx.Err() == nil {
		if err := c.darwin.waitReady(c.ctx); err != nil {
			c.logger.Warn(E.Cause(err, "observe the next unlock for the screen-viewing notification"))
			return
		}
	}
	v.access.Lock()
	if v.connection.ctx.Err() != nil {
		v.access.Unlock()
		return
	}
	v.pending = true
	v.generation++
	v.stopped = time.Now()
	if stopping.After(v.after) {
		v.after = stopping
	}
	if v.release == nil {
		v.release = v.connection.hold()
	}
	v.access.Unlock()
	v.connection.logger.Debug("waiting for the next unlock to dismiss the screen-viewing banner")
	if current := v.connection.currentConn(); current != nil {
		select {
		case current.viewingWake <- struct{}{}:
		default:
		}
	}
}

func (v *screenViewing) close() {
	v.access.Lock()
	if v.cancel != nil {
		v.cancel()
	}
	release := v.release
	v.release, v.pending = nil, false
	v.access.Unlock()
	if release != nil {
		release()
	}
}

func (v *screenViewing) locked(c *conn) {
	v.access.Lock()
	if v.running == c {
		v.cancel()
	}
	v.access.Unlock()
}

func (v *screenViewing) unlocked(c *conn, at time.Time) {
	v.access.Lock()
	if !v.pending || v.cancel != nil || !at.After(v.after) || c.ctx.Err() != nil {
		v.access.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, screenViewingWindow)
	generation, stopped := v.generation, v.stopped
	v.after = at
	v.cancel, v.running = cancel, c
	v.access.Unlock()
	go func() {
		started := time.Now()
		err := c.dismissScreenViewingBanner(ctx)
		interrupted := errors.Is(ctx.Err(), context.Canceled)
		consume := !interrupted && (err == nil || !at.Before(stopped))
		cancel()
		v.access.Lock()
		v.cancel, v.running = nil, nil
		var release func()
		if generation == v.generation && consume {
			v.pending = false
			release, v.release = v.release, nil
		}
		v.access.Unlock()
		if release != nil {
			release()
		}
		switch {
		case err == nil:
			c.logger.Debug("dismissed the screen-viewing banner in ", time.Since(started))
		case !interrupted && c.ctx.Err() == nil:
			c.logger.Warn(E.Cause(err, "dismiss the screen-viewing banner"))
		}
		select {
		case c.viewingWake <- struct{}{}:
		default:
		}
	}()
}

func (c *conn) watchScreenViewing(received <-chan Event, stop func(), relayDone <-chan struct{}) {
	defer stop()
	var unlocked time.Time
	for {
		select {
		case event := <-received:
			if event.Kind != EventDevice || event.Name != keybagLockStatus {
				continue
			}
			locked, err := c.lockState.locked(c.ctx, c.device)
			if err != nil {
				if c.ctx.Err() == nil {
					c.logger.Warn(E.Cause(err, "read the lock state for the screen-viewing notification"))
				}
				continue
			}
			if locked {
				unlocked = time.Time{}
				c.connection.viewing.locked(c)
				continue
			}
			if unlocked.IsZero() {
				unlocked = event.at
			}
		case <-c.viewingWake:
		case <-relayDone:
			if c.ctx.Err() == nil {
				c.connection.viewing.close()
			}
			return
		case <-c.ctx.Done():
			return
		}
		c.connection.viewing.unlocked(c, unlocked)
	}
}

func (c *conn) dismissScreenViewingBanner(ctx context.Context) error {
	selector, err := ParseSelector([]byte(`{"app":"com.apple.springboard","identifier":"NotificationShortLookView","label":{"$regex":"^DEVICE HUB, .*This device was viewed remotely from Mac$","$flags":"i"}}`))
	if err != nil {
		return err
	}
	dismissed := false
	for ctx.Err() == nil {
		// Each read goes to the device: the banner shows in SpringBoard's tree
		// without a push, so a kept tree would hide it.
		view, _, err := c.connection.phone.find(ctx, []*Selector{selector}, screenViewingDepth, false)
		if err != nil {
			return err
		}
		if view == nil {
			continue
		}
		banner, err := view.Find("", selector, nil)
		if err != nil {
			return err
		}
		if banner == nil {
			if dismissed {
				return nil
			}
			continue
		}
		if dismissed {
			continue
		}
		err = c.ax.do(ctx, true, view.model.generation, func(ctx context.Context, client *accessibility.RemoteClient) error {
			var err error
			dismissed, err = client.DismissScreenViewingBanner(ctx, banner.node.AX)
			return err
		})
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}
