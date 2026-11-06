package device

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	"github.com/swipt-automation/swipt/internal/coredevice"
	"github.com/swipt-automation/swipt/internal/coredevice/accessibility"
	"github.com/swipt-automation/swipt/internal/coredevice/apps"
	"github.com/swipt-automation/swipt/internal/coredevice/display"
	"github.com/swipt-automation/swipt/internal/coredevice/hid"
	"github.com/swipt-automation/swipt/internal/coredevice/instruments"
	"github.com/swipt-automation/swipt/internal/coredevice/pasteboard"
)

const keptModels = 16

type conn struct {
	connection *Connection
	logger     logger.ContextLogger
	device     *coredevice.Device
	display    *display.Client
	pasteboard *pasteboard.Client
	ctx        context.Context
	cancel     context.CancelFunc
	owner      context.Context
	events     *events
	ax         *axWorker
	front      *foreground
	darwin     *darwinNotifications
	nodes      nodeIndex
	lockState  lockStateReader

	backlightCache backlightCache

	geometryAccess sync.Mutex
	geometry       phoneGeometry
	geometryAt     time.Time
	springboard    atomic.Uint64

	locationAccess sync.Mutex
	location       *instruments.Location

	software *hid.Client
}

func newConn(s *Connection, session context.Context, device *coredevice.Device, displayClient *display.Client, owner context.Context) *conn {
	ctx, cancel := context.WithCancel(session)
	c := &conn{
		connection: s, logger: s.logger, device: device, display: displayClient, ctx: ctx, cancel: cancel, owner: owner,
		events: newEvents(),
	}
	c.pasteboard = pasteboard.New(ctx, device, s.logger)
	c.lockState.owner = c
	c.ax = newAXWorker(c)
	c.front = newForeground(c)
	c.darwin = newDarwinNotifications(c)
	go c.ax.run()
	go func() { _, _ = c.lockState.recent(ctx) }()
	go c.watchBacklight()
	return c
}

func (c *conn) operation(ctx context.Context) (context.Context, context.CancelFunc) {
	operation, cancel := context.WithCancel(c.ctx)
	stop := context.AfterFunc(ctx, cancel)
	if ctx.Err() != nil {
		cancel()
	}
	return operation, func() {
		stop()
		cancel()
	}
}

func (c *conn) close() error {
	<-c.ax.done
	c.front.close()
	c.darwin.close()
	c.locationAccess.Lock()
	c.closeLocation()
	c.locationAccess.Unlock()
	return c.lockState.close()
}

func (c *conn) pushed(push accessibility.Push) {
	event := Event{Kind: EventAccessibility, PID: push.PID, Notification: push.Notification}
	if push.Notification == 0 {
		event = Event{Kind: EventTree}
		if push.Frontmost != nil {
			event.PID = push.Frontmost.Ref.PID
		}
	}
	c.events.publish(event)
	c.front.pushed(push)
}

func (c *conn) springboardPID(ctx context.Context) (uint64, error) {
	if pid := c.springboard.Load(); pid != 0 {
		return pid, nil
	}
	processes, err := apps.Processes(ctx, c.device)
	if err != nil {
		return 0, err
	}
	springboard := common.Find(processes, func(it apps.Process) bool { return strings.HasSuffix(it.Executable, "/SpringBoard.app/SpringBoard") })
	if springboard.PID == 0 {
		return 0, E.New("SpringBoard is not running")
	}
	c.springboard.Store(springboard.PID)
	return springboard.PID, nil
}

type nodeIndex struct {
	access sync.Mutex
	models []*PhoneModel
}

func (n *nodeIndex) add(model *PhoneModel) {
	n.access.Lock()
	defer n.access.Unlock()
	if slices.Contains(n.models, model) {
		return
	}
	n.models = append(n.models, model)
	if len(n.models) > keptModels {
		n.models = slices.Delete(n.models, 0, len(n.models)-keptModels)
	}
}

func (n *nodeIndex) find(ref accessibility.ElementRef, generation uint64) *PhoneNode {
	n.access.Lock()
	defer n.access.Unlock()
	for _, model := range slices.Backward(n.models) {
		if node := model.Node(ref); node != nil && model.generation == generation {
			return node
		}
	}
	return nil
}

func (n *nodeIndex) clear() {
	n.access.Lock()
	n.models = nil
	n.access.Unlock()
}
