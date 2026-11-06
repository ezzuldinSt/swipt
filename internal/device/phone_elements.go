package device

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/coredevice/accessibility"
)

func (p *Phone) Find(ctx context.Context, selectors []*Selector, depth int64) (*View, uint64, error) {
	return p.find(ctx, selectors, depth, true)
}

// find reads the screen for the selectors as Find does; with kept, it may
// answer the tree kept from an earlier read while that is current.
func (p *Phone) find(ctx context.Context, selectors []*Selector, depth int64, kept bool) (*View, uint64, error) {
	if len(selectors) == 0 {
		return nil, 0, E.Extend(ErrInvalid, "find needs a selector")
	}
	if err := checkDepth(depth); err != nil {
		return nil, 0, err
	}
	within := selectors[0].Within()
	for _, selector := range selectors[1:] {
		if selector.Within() != within {
			within = ""
		}
	}
	var fields, named []string
	scripted, unnamed := false, false
	for _, selector := range selectors {
		scripted = scripted || selector.Scripted()
		fields = append(fields, selector.Fields()...)
		switch app := selector.App(); {
		case app == "":
			unnamed = true
		case !slices.Contains(named, app):
			named = append(named, app)
		}
	}
	options := accessibility.ApplicationOptions{Depth: depth, Modal: slices.Contains(fields, "inert"), Focused: slices.Contains(fields, "focused")}
	if !scripted {
		options.Attributes = accessibility.SnapshotAttributes(fields)
	}
	options.Animations = true
	c, ctx, done, err := p.begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer done()
	began := time.Now()
	var base *PhoneNode
	if within != "" {
		if base, err = p.element(c, within); err != nil {
			return nil, 0, err
		}
		options.Root, options.Clip = &base.AX, &base.axClip
	}
	seen := c.events.screen.Load()
	key := "find:" + strings.Join(named, ",")
	switch {
	case !unnamed && len(named) == 1 && named[0] == springboardBundleID:
		if options.PID, err = c.springboardPID(ctx); err != nil {
			return nil, 0, err
		}
	case !unnamed:
		options.Wanted = func(bundleID string) bool { return slices.Contains(named, bundleID) }
	default:
		key = "find"
	}
	key += ":" + strconv.FormatUint(options.PID, 10) + ":" + within + ":" + strconv.FormatInt(depth, 10) + ":" + strconv.FormatBool(options.Modal) + ":" +
		strconv.FormatBool(options.Focused) + ":" + fmt.Sprint(options.Attributes)
	snapshot := SnapshotOptions{Depth: depth, Within: within}
	load := func(ctx context.Context, background bool) (*PhoneModel, error) {
		return p.loadFind(ctx, c, key, options, base, snapshot, background)
	}
	var model *PhoneModel
	cached := false
	if kept {
		model, cached, err = c.tree.read(ctx, key, load)
	} else {
		model, err = load(ctx, false)
	}
	if err != nil {
		return nil, seen, err
	}
	if cached {
		p.connection.trace(began, "find.cached", strconv.Itoa(len(model.nodes))+" nodes of "+model.BundleID, time.Since(began))
	}
	if model == nil {
		return nil, seen, nil
	}
	c.nodes.add(model)
	return NewView(model, true), seen, nil
}

func (p *Phone) loadFind(ctx context.Context, c *conn, key string, options accessibility.ApplicationOptions, base *PhoneNode, snapshot SnapshotOptions, background bool) (*PhoneModel, error) {
	geometry := c.geometryAhead()
	seen := c.events.screen.Load()
	read := c.ax.read
	if background {
		read = c.ax.readLow
	}
	queued := time.Now()
	value, err := read(ctx, key, func(ctx context.Context, client *accessibility.RemoteClient) (any, error) {
		started := time.Now()
		options := options
		if options.PID == 0 && options.Root == nil {
			options.Front = c.frontGuess()
		}
		screen, err := client.ReadApplication(ctx, options)
		read := time.Now()
		var waited time.Duration
		var placed phoneGeometry
		if err == nil && screen.Root != nil {
			placed, err = geometry(ctx)
			waited = time.Since(read)
		}
		waits := "geometry " + waited.String() + ", queued " + started.Sub(queued).String()
		if background {
			waits = "in the background, " + waits
		}
		if options.Front != nil {
			waits += ", front guessed"
		}
		if err != nil {
			p.connection.trace(started, "find.failed", waits+", "+err.Error(), time.Since(started))
			if options.PID != 0 && E.IsMulti(err, accessibility.ErrNoAnswer, accessibility.ErrNoApplication) {
				c.springboard.CompareAndSwap(options.PID, 0)
			}
			return nil, err
		}
		if screen.Root == nil {
			p.connection.trace(started, "find.skipped", waits+", "+screen.BundleID+" pid "+strconv.FormatUint(screen.App.Ref.PID, 10)+" is in front", time.Since(started))
			if base == nil {
				c.front.observed(frontApplication{pid: screen.App.Ref.PID, bundleID: screen.BundleID, at: started, via: "find"})
			}
			return (*PhoneModel)(nil), nil
		}
		model := p.model(c, placed, screen, started, base, snapshot, options.PID == 0 && base == nil)
		p.connection.trace(started, "find.read", waits+", "+strconv.Itoa(len(model.nodes))+" nodes of "+screen.BundleID+", "+strconv.Itoa(len(model.Truncated))+" cut off, "+strconv.FormatUint(c.events.screen.Load()-seen, 10)+" screen changes since", time.Since(started))
		return model, nil
	})
	if err != nil {
		return nil, err
	}
	return value.(*PhoneModel), nil
}

func (p *Phone) Changed(ctx context.Context, seen uint64, timeout time.Duration) error {
	c, ctx, done, err := p.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	changed, stop := c.events.watch(true)
	defer stop()
	if c.events.screen.Load() != seen {
		p.connection.trace(time.Now(), "changed", "already "+strconv.FormatUint(c.events.screen.Load()-seen, 10)+" screen changes", 0)
		return nil
	}
	var expire <-chan time.Time
	if timeout >= 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expire = timer.C
	}
	started := time.Now()
	select {
	case <-changed:
		p.connection.trace(started, "changed", "woken by a push", time.Since(started))
	case <-expire:
		p.connection.trace(started, "changed", "timed out", time.Since(started))
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (p *Phone) element(c *conn, ref string) (*PhoneNode, error) {
	parsed, err := parseRef(ref)
	if err != nil {
		return nil, err
	}
	node := c.nodes.find(parsed, c.ax.current())
	if node == nil {
		return nil, E.Extend(ErrNotFound, "the element is not from a current read of the screen; read the screen again")
	}
	return node, nil
}

func (p *Phone) Perform(ctx context.Context, ref, name string) (Aim, error) {
	c, ctx, done, err := p.begin(ctx)
	if err != nil {
		return Aim{}, err
	}
	defer done()
	defer c.tree.act()()
	geometry, err := c.readGeometry(ctx)
	if err != nil {
		return Aim{}, err
	}
	started := time.Now()
	s := &axSession{phone: p, conn: c, geometry: geometry}
	err = c.ax.do(ctx, true, 0, func(ctx context.Context, client *accessibility.RemoteClient) error {
		s.client = client
		node, err := p.element(c, ref)
		if err != nil {
			return err
		}
		s.target = Aim{Element: describeElement(node.AX)}
		return s.perform(ctx, node, name)
	})
	p.connection.trace(started, "perform.element", s.target.String()+", "+name, time.Since(started))
	if s.acted {
		c.events.publish(Event{Kind: EventInput})
		c.front.acted()
	}
	return s.target, err
}
