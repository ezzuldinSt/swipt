package automation

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/jsc"
	"github.com/swipt-automation/swipt/internal/log"
)

type Graphics interface {
	Install(context *jsc.Context, native jsc.Value, engine uint64)
	Uninstall(context *jsc.Context, engine uint64)
	Run(engine uint64, call int64, context *GraphicsCall) (json.RawMessage, error)
	Watch(engine uint64, call int64, context *GraphicsCall, publish func(any)) error
}

const timelineLimit = 100_000

var (
	errNoGraphics   = E.New("graphics: this platform has no graphics")
	errScreenEnded  = E.New("graphics: the screen stream ended")
	graphicsEngines atomic.Uint64
)

var readOperations = []string{
	"device.info", "device.state", "device.installedApplications", "device.applicationIcon", "device.pasteboard",
	"accessibility.snapshot", "accessibility.find", "accessibility.changed",
}

var timelineOperations = []string{
	"device.unlock", "device.launchApplication", "input.tap", "input.swipe", "input.drag", "input.pinch", "input.touch", "input.pressKey",
	"input.button", "accessibility.tap", "accessibility.setText", "accessibility.perform",
}

type GraphicsCall struct {
	ctx  context.Context
	call *operation
}

func (c *GraphicsCall) Context() context.Context {
	return c.ctx
}

func (c *GraphicsCall) Screenshot() ([]byte, float64, error) {
	phone, err := c.call.screen(c.ctx)
	if err != nil {
		return nil, 0, err
	}
	attempted := time.Now()
	data, scale, err := phone.Capture(c.ctx)
	if c.call.dropped(c.ctx, err, attempted) {
		attempted = time.Now()
		data, scale, err = phone.Capture(c.ctx)
	}
	if c.call.dropped(c.ctx, err, attempted) {
		err = c.call.droppedError(err, "during the screenshot")
	}
	return data, scale, err
}

// The device answers its language within this, or text recognition goes on without it.
const languageLimit = 2 * time.Second

// Language is the language of the device, which text recognition reads in by default, or "" when
// there is no device or it does not answer.
func (c *GraphicsCall) Language() string {
	phone := c.call.engine.host.options.Device
	if phone == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(c.ctx, languageLimit)
	defer cancel()
	language, _ := phone.Language(ctx)
	return language
}

// Warn tells the script, in its console and log, something it should know about the call.
func (c *GraphicsCall) Warn(message string) {
	c.call.engine.logTo(c.call.exec, log.LevelWarn, message)
}

func (c *GraphicsCall) Tracing() bool {
	return c.call.engine.host.timeline.active()
}

func (c *GraphicsCall) Trace(at time.Time, name, detail string, duration time.Duration) {
	c.call.engine.host.timeline.record(TimelineEvent{
		Time: at.UnixMilli(), Type: "operation", Name: "trace." + name, Message: detail,
		DurationMs: float64(duration.Microseconds()) / 1000, Swiplet: c.call.engine.swiplet.ID,
	})
}

func (c *GraphicsCall) WatchTimeline() *TimelineWatch {
	return c.call.engine.host.watchTimeline()
}

func (c *GraphicsCall) Authorize(name string, write bool) error {
	e := c.call.engine
	roots, where := []string{filepath.Join(e.swiplet.Directory, DataFolder), e.caches}, "the data or cache directory"
	if !write {
		roots, where = append(roots, filepath.Join(e.swiplet.Directory, AssetsFolder)), "the data, cache or assets directory"
	}
	resolved, err := resolvePath(filepath.Clean(name))
	if err != nil {
		return E.Cause(err, "graphics: resolve ", strconv.Quote(name))
	}
	if filepath.IsAbs(name) {
		for _, root := range common.FilterNotDefault(roots) {
			base, err := resolvePath(root)
			if err != nil {
				continue
			}
			if relative, err := filepath.Rel(base, resolved); err == nil && relative != "." && filepath.IsLocal(relative) {
				return nil
			}
		}
	}
	return E.Cause1(ErrNotAllowed, E.New("graphics: ", strconv.Quote(name), " is outside ", where))
}

func resolvePath(name string) (string, error) {
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(name)
		if err == nil {
			slices.Reverse(missing)
			return filepath.Join(append([]string{resolved}, missing...)...), nil
		}
		parent := filepath.Dir(name)
		if _, statErr := os.Lstat(name); statErr == nil || !errors.Is(err, fs.ErrNotExist) || parent == name {
			return "", err
		}
		missing = append(missing, filepath.Base(name))
		name = parent
	}
}

// graphicsPrewarmer is a Graphics that can open the screen stream before a
// script's first screen call needs it.
type graphicsPrewarmer interface {
	Prewarm()
}

// prewarmGraphics starts opening the screen stream as a script starts, where
// the graphics can, so its first screen call does not wait for it.
func (h *Host) prewarmGraphics() {
	if prewarmer, ok := h.options.Graphics.(graphicsPrewarmer); ok {
		prewarmer.Prewarm()
	}
}

func (e *engine) installGraphics(native jsc.Value) {
	graphics := e.host.options.Graphics
	if graphics == nil {
		return
	}
	e.graphics = graphicsEngines.Add(1)
	graphics.Install(e.context, native, e.graphics)
}

func (e *engine) uninstallGraphics() {
	graphics := e.host.options.Graphics
	if graphics == nil || e.graphics == 0 {
		return
	}
	graphics.Uninstall(e.context, e.graphics)
	e.graphics = 0
}

func graphicsRun(ctx context.Context, call *operation) (any, error) {
	var id int64
	if err := call.decode(1, &id); err != nil {
		return nil, err
	}
	graphics := call.engine.host.options.Graphics
	if graphics == nil {
		return nil, errNoGraphics
	}
	return graphics.Run(call.engine.graphics, id, &GraphicsCall{ctx: ctx, call: call})
}

func (e *engine) watchGraphics(ctx context.Context, call *operation, publish func(any, error), ended func(error)) error {
	var id int64
	if err := call.decode(1, &id); err != nil {
		return err
	}
	graphics := e.host.options.Graphics
	if graphics == nil {
		return errNoGraphics
	}
	go func() {
		err := graphics.Watch(e.graphics, id, &GraphicsCall{ctx: ctx, call: call}, func(value any) { publish(value, nil) })
		if err == nil {
			err = errScreenEnded
		}
		ended(err)
	}()
	return nil
}

type TimelineEvent struct {
	Time       int64             `json:"time"`
	Type       string            `json:"type"`
	Name       string            `json:"name,omitempty"`
	Arguments  []json.RawMessage `json:"arguments,omitempty"`
	DurationMs float64           `json:"durationMs,omitempty"`
	Error      string            `json:"error,omitempty"`
	Swiplet    string            `json:"swiplet,omitempty"`
	Action     string            `json:"action,omitempty"`
	Caller     string            `json:"caller,omitempty"`
	App        string            `json:"app,omitempty"`
	Level      string            `json:"level,omitempty"`
	Message    string            `json:"message,omitempty"`
}

type timeline struct {
	access  sync.Mutex
	watches map[*TimelineWatch]struct{}
}

type TimelineWatch struct {
	timeline *timeline
	cancel   context.CancelFunc
	done     chan struct{}
	access   sync.Mutex
	events   []TimelineEvent
}

func (t *timeline) record(event TimelineEvent) {
	t.access.Lock()
	defer t.access.Unlock()
	for watch := range t.watches {
		watch.add(event)
	}
}

func (t *timeline) active() bool {
	t.access.Lock()
	defer t.access.Unlock()
	return len(t.watches) > 0
}

func (h *Host) watchTimeline() *TimelineWatch {
	ctx, cancel := context.WithCancel(h.ctx)
	watch := &TimelineWatch{timeline: &h.timeline, cancel: cancel, done: make(chan struct{})}
	h.timeline.access.Lock()
	h.timeline.watches[watch] = struct{}{}
	h.timeline.access.Unlock()
	go func() {
		defer close(watch.done)
		if phone := h.options.Device; phone != nil {
			_ = phone.WatchForeground(ctx, func(app string) {
				watch.add(TimelineEvent{Time: time.Now().UnixMilli(), Type: "foreground", App: app})
			})
		}
	}()
	return watch
}

func (w *TimelineWatch) add(event TimelineEvent) {
	w.access.Lock()
	defer w.access.Unlock()
	if len(w.events) < timelineLimit {
		w.events = append(w.events, event)
	}
}

func (w *TimelineWatch) Stop() []TimelineEvent {
	w.timeline.access.Lock()
	delete(w.timeline.watches, w)
	w.timeline.access.Unlock()
	w.cancel()
	<-w.done
	w.access.Lock()
	defer w.access.Unlock()
	events := slices.Clone(w.events)
	common.SortBy(events, func(it TimelineEvent) int64 { return it.Time })
	return events
}
