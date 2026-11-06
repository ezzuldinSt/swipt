package device

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	"github.com/swipt-automation/swipt/internal/coredevice/accessibility"
	"github.com/swipt-automation/swipt/internal/coredevice/display"
	"github.com/swipt-automation/swipt/internal/coredevice/hid"
	"github.com/swipt-automation/swipt/internal/coredevice/pasteboard"
)

const (
	phoneGeometryLife = time.Second
	phoneEdgeRegion   = 4.0
)

type Point struct {
	X, Y float64
}

type Rect struct {
	X, Y, Width, Height float64
}

func (r Rect) Center() Point {
	return Point{r.X + r.Width/2, r.Y + r.Height/2}
}

func (r Rect) Contains(at Point) bool {
	return at.X >= r.X && at.X <= r.X+r.Width && at.Y >= r.Y && at.Y <= r.Y+r.Height
}

func (r Rect) Intersect(other Rect) Rect {
	x, y := max(r.X, other.X), max(r.Y, other.Y)
	return Rect{x, y, max(min(r.X+r.Width, other.X+other.Width)-x, 0), max(min(r.Y+r.Height, other.Y+other.Height)-y, 0)}
}

func (r Rect) Empty() bool {
	return r.Width <= 0 || r.Height <= 0
}

type PhoneScreen struct {
	Width, Height float64
	Scale         float64
	Orientation   string
}

type Phone struct {
	connection *Connection
	logger     logger.ContextLogger

	captureAccess sync.Mutex

	locationAccess sync.Mutex
	location       *simulatedLocation

	languageAccess sync.Mutex
	language       string
	languageAt     time.Time
}

type simulatedLocation struct {
	latitude, longitude float64
	release             func()
}

func (s *Connection) Phone() *Phone {
	return s.phone
}

func (p *Phone) begin(ctx context.Context) (*conn, context.Context, func(), error) {
	c, release, err := p.connection.acquire(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	operation, cancel := c.operation(ctx)
	return c, operation, func() {
		cancel()
		release()
	}, nil
}

func (p *Phone) Interrupted(since time.Time) bool {
	s := p.connection
	s.access.Lock()
	defer s.access.Unlock()
	return s.interruptedLocked(since)
}

func (p *Phone) DropCause() error {
	s := p.connection
	s.access.Lock()
	defer s.access.Unlock()
	if s.attached != nil && s.attached.device.Err() != nil {
		return s.attached.device.Err()
	}
	return s.detachCause
}

func (p *Phone) Disconnected(since time.Time) bool {
	s := p.connection
	s.access.Lock()
	defer s.access.Unlock()
	if s.closed || s.unavailable != nil || s.failure != nil {
		return false
	}
	return s.attached == nil || s.interruptedLocked(since)
}

func (s *Connection) interruptedLocked(since time.Time) bool {
	return s.detachedAt.After(since) || s.attached != nil && s.attached.device.Closed()
}

func (p *Phone) WaitConnected(ctx context.Context) error {
	release := p.connection.hold()
	defer release()
	_, err := p.connection.waitConn(ctx, true)
	return err
}

type phoneGeometry struct {
	PhoneScreen
	turns int
}

func (c *conn) readGeometry(ctx context.Context) (phoneGeometry, error) {
	c.geometryAccess.Lock()
	defer c.geometryAccess.Unlock()
	if time.Since(c.geometryAt) < phoneGeometryLife || c.connection.feeds.steadySince(c.geometryAt) {
		return c.geometry, nil
	}
	asked := time.Now()
	info, err := c.readDisplays(ctx)
	if err != nil {
		return phoneGeometry{}, E.Cause(err, "read the device's displays")
	}
	return c.keepGeometryLocked(info, asked)
}

// readDisplays asks for the displays, keeping the backlight state the answer
// reports.
func (c *conn) readDisplays(ctx context.Context) (*display.Info, error) {
	run, _ := c.darwin.relaying()
	read := c.backlightCache.begin(run, time.Now())
	info, err := c.display.Info(ctx)
	if err != nil {
		return nil, err
	}
	if state, err := info.Backlight(); err == nil {
		c.backlightCache.store(read, state)
	}
	return info, nil
}

func (c *conn) keepGeometryLocked(info *display.Info, asked time.Time) (phoneGeometry, error) {
	primary, found := primaryDisplay(info.Displays)
	if !found {
		return phoneGeometry{}, E.New("the device lists no usable screen")
	}
	c.connection.updateDisplay(primary)
	scale := float64(max(primary.PointScale, 1))
	turns := orientationTurns(primary.CurrentOrientation)
	width, height := primary.Width, primary.Height
	if turns%2 == 1 {
		width, height = height, width
	}
	touch := touchTurns(geometryOf(primary), width, height)
	orientation := "portrait"
	switch {
	case width > height:
		orientation = "landscape"
	case turns == 2:
		orientation = "portraitUpsideDown"
	}
	c.geometry = phoneGeometry{
		PhoneScreen: PhoneScreen{Width: float64(width) / scale, Height: float64(height) / scale, Scale: scale, Orientation: orientation},
		turns:       touch,
	}
	c.geometryAt = asked
	return c.geometry, nil
}

func (g phoneGeometry) check(at Point) error {
	if at.X < 0 || at.X > g.Width || at.Y < 0 || at.Y > g.Height {
		return E.New("the point [", at.X, ", ", at.Y, "] is outside the ", g.Width, "x", g.Height, " screen")
	}
	return nil
}

func (g phoneGeometry) touch(at Point) hid.Touch {
	u := clamp(at.X/g.Width, 0, 1)
	v := clamp(at.Y/g.Height, 0, 1)
	for range g.turns {
		u, v = v, 1-u
	}
	return hid.Touch{X: u, Y: v}
}

// The system-wide hit test takes a point in the digitizer's orientation:
// (834 − y, x) in landscape (iPad17,1, iPadOS 27.2 24B115).
func (g phoneGeometry) digitizer(at Point) accessibility.LayoutPoint {
	touch := g.touch(at)
	width, height := g.Width, g.Height
	if g.turns%2 == 1 {
		width, height = height, width
	}
	return accessibility.LayoutPoint{X: touch.X * width, Y: touch.Y * height}
}

func (g phoneGeometry) edge(at Point) hid.Edge {
	distances := [4]float64{at.Y, g.Width - at.X, g.Height - at.Y, at.X}
	nearest := -1
	for side, distance := range distances {
		if distance <= phoneEdgeRegion && (nearest < 0 || distance < distances[nearest]) {
			nearest = side
		}
	}
	if nearest < 0 {
		return hid.EdgeNone
	}
	edges := [4]hid.Edge{hid.EdgeTop, hid.EdgeRight, hid.EdgeBottom, hid.EdgeLeft}
	return edges[(nearest-g.turns)&3]
}

// An application that keeps the interface orientation reports its frames in
// it; SpringBoard's windows report theirs in the digitizer's space whatever
// the orientation, and that space is portrait on an iPad whose display is
// landscape native (iPad17,1, iPadOS 27.2 24B115; iPhone18,2, iOS 27.2
// 24B5110a). Some of SpringBoard's windows report their frames and those of
// their contents in pixels (iPhone18,2, iOS 27.2 24B115).
type axSpace struct {
	geometry      phoneGeometry
	rotated       bool
	width, height float64
	pid, context  uint64
}

func spaceOf(geometry phoneGeometry, app accessibility.Element) axSpace {
	return axSpace{geometry: geometry, pid: app.Ref.PID, context: app.Context}.window(app.Frame)
}

func (s axSpace) window(frame *accessibility.LayoutFrame) axSpace {
	out := s
	out.rotated, out.width, out.height = false, 0, 0
	if frame == nil || frame.Width <= 0 || frame.Height <= 0 {
		return out
	}
	switch {
	case (frame.Width > frame.Height) != (s.geometry.Width > s.geometry.Height):
		out.rotated, out.width, out.height = true, frame.Width, frame.Height
	case math.Abs(frame.Width-s.geometry.Width*s.geometry.Scale) < 1 && math.Abs(frame.Height-s.geometry.Height*s.geometry.Scale) < 1:
		out.width, out.height = frame.Width, frame.Height
	}
	return out
}

func (s axSpace) point(at accessibility.LayoutPoint) Point {
	if s.width == 0 {
		return Point{at.X, at.Y}
	}
	u, v := at.X/s.width, at.Y/s.height
	if s.rotated {
		for range s.geometry.turns {
			u, v = 1-v, u
		}
	}
	return Point{u * s.geometry.Width, v * s.geometry.Height}
}

func (s axSpace) rect(frame accessibility.LayoutFrame) Rect {
	first := s.point(accessibility.LayoutPoint{X: frame.X, Y: frame.Y})
	second := s.point(accessibility.LayoutPoint{X: frame.X + frame.Width, Y: frame.Y + frame.Height})
	return Rect{X: min(first.X, second.X), Y: min(first.Y, second.Y), Width: math.Abs(second.X - first.X), Height: math.Abs(second.Y - first.Y)}
}

// A widget or Live Activity has a render context of its own, placed on the
// screen at an offset the translator does not report; web content reports
// none and is in the application's space (iPhone18,2, iOS 27.2 24B5110a).
func (s axSpace) remote(element accessibility.Element) bool {
	return s.context != 0 && element.Context != 0 && element.Ref.PID != s.pid && element.Context != s.context
}

type PhoneNode struct {
	AX        accessibility.Element
	Frame     Rect
	Visible   Rect
	Point     Point
	Covered   bool
	Inert     bool
	Remote    bool
	Web       bool
	Focused   bool
	Truncated bool
	Clip      Rect
	Children  []*PhoneNode
	Parent    *PhoneNode

	model  *PhoneModel
	space  axSpace
	axClip accessibility.LayoutFrame

	order, end  int
	scroll, win *PhoneNode
	cover       *PhoneNode
	shown       float64
	// overlapped tells that a cover lies over or right by the activation point.
	overlapped bool
}

func (n *PhoneNode) HasFrame() bool {
	return n.AX.Frame != nil && !n.Remote
}

func (n *PhoneNode) Shown() bool {
	return n.HasFrame() && !n.Visible.Empty()
}

func (n *PhoneNode) Contains(other *PhoneNode) bool {
	for ; other != nil; other = other.Parent {
		if other == n {
			return true
		}
	}
	return false
}

type PhoneModel struct {
	BundleID  string
	Root      *PhoneNode
	System    *PhoneNode
	Truncated []*PhoneNode

	app        accessibility.Element
	nodes      map[accessibility.ElementRef]*PhoneNode
	generation uint64
	read       SnapshotOptions
	// animating is set when UIView animations ran as the tree was read.
	animating bool
}

func (m *PhoneModel) Node(ref accessibility.ElementRef) *PhoneNode {
	return m.nodes[ref]
}

func walkNodes(root *PhoneNode, visit func(node *PhoneNode)) {
	visit(root)
	for _, child := range root.Children {
		walkNodes(child, visit)
	}
}

// Automation types that XCTest converts to XCUIElementTypeWindow,
// XCUIElementTypeKeyboard and XCUIElementTypeStatusBar (Xcode 27.1 27A9275
// XCTAutomationSupport _XCTElementTypeFromAXAutomationType 0x1e444, table
// 0x46e28).
const (
	windowAutomationType    = 2
	keyboardAutomationType  = 19
	statusBarAutomationType = 25
)

func modelOf(geometry phoneGeometry, screen *accessibility.Screen, base *PhoneNode) *PhoneModel {
	model := &PhoneModel{BundleID: screen.BundleID, app: screen.App, nodes: make(map[accessibility.ElementRef]*PhoneNode)}
	space := spaceOf(geometry, screen.Root.Element)
	if base != nil {
		space = base.space
	}
	model.Root = model.convert(space, screen.Root, nil, screen.Focused)
	if base != nil && base.Inert {
		walkNodes(model.Root, func(node *PhoneNode) { node.Inert = true })
	}
	if screen.System != nil {
		model.System = model.convert(spaceOf(geometry, screen.System.Element), screen.System, nil, nil)
	}
	var keyboard, input *PhoneNode
	walkNodes(model.Root, func(node *PhoneNode) {
		if keyboard == nil && node.AX.Type == keyboardAutomationType && node.Shown() {
			keyboard = node
		}
	})
	var over []cover
	if keyboard != nil {
		over = append(over, cover{node: keyboard, rect: keyboard.Visible})
		input = keyboard.window()
	}
	if model.System != nil {
		walkNodes(model.System, func(node *PhoneNode) {
			if node.Shown() && node.AX.AccessibilityElement && len(node.Children) == 0 && !node.inStatusBar() {
				over = append(over, cover{node: node, rect: node.Visible})
			}
		})
	}
	model.uncover(over, input)
	return model
}

// The keyboard has a window of its own, which also holds the candidate bar
// outside the keyboard (iPhone18,2, iOS 27.2 24B115).
func (n *PhoneNode) window() *PhoneNode {
	for node := n; node != nil; node = node.Parent {
		if node.AX.Type == windowAutomationType {
			return node
		}
	}
	return n
}

func (n *PhoneNode) context() uint64 {
	for node := n; node != nil; node = node.Parent {
		if node.AX.Context != 0 {
			return node.AX.Context
		}
	}
	return 0
}

func (n *PhoneNode) inStatusBar() bool {
	for node := n; node != nil; node = node.Parent {
		if node.AX.Type == statusBarAutomationType {
			return true
		}
	}
	return false
}

func (m *PhoneModel) convert(space axSpace, node *accessibility.TreeNode, parent *PhoneNode, focused *accessibility.ElementRef) *PhoneNode {
	if node.Type == windowAutomationType && node.Frame != nil {
		space = space.window(node.Frame)
	}
	out := &PhoneNode{
		AX: node.Element, Inert: node.Inert, Web: node.Web, Truncated: node.Truncated, Parent: parent, Remote: space.remote(node.Element),
		model: m, space: space, axClip: node.Clip,
	}
	m.nodes[node.Ref] = out
	if out.Truncated {
		m.Truncated = append(m.Truncated, out)
	}
	if out.HasFrame() {
		out.Frame = space.rect(*node.Frame)
		out.Clip = space.rect(node.Clip)
		out.Visible = out.Frame.Intersect(out.Clip).Intersect(Rect{Width: space.geometry.Width, Height: space.geometry.Height})
		out.Point = out.Visible.Center()
		if activation := node.Activation; activation != nil {
			if point := space.point(*activation); out.Visible.Contains(point) {
				out.Point = point
			}
		}
	}
	out.Focused = focused != nil && *focused == node.Ref && node.TextEntry()
	for _, child := range node.Children {
		out.Children = append(out.Children, m.convert(space, child, out, focused))
	}
	return out
}

type SnapshotOptions struct {
	Front  bool
	Raw    bool
	Depth  int64
	Within string
}

func (o SnapshotOptions) key() string {
	depth := strconv.FormatInt(o.Depth, 10)
	switch {
	case o.Within != "":
		return "within:" + o.Within + ":" + depth
	case o.Front:
		return "front:" + depth
	}
	return "screen:" + depth
}

// read answers the tree a snapshot reads, the kept one while it is current.
func (p *Phone) read(ctx context.Context, c *conn, options SnapshotOptions) (*PhoneModel, bool, error) {
	options.Raw = false
	model, cached, err := c.tree.read(ctx, options.key(), func(ctx context.Context, background bool) (*PhoneModel, error) {
		return p.load(ctx, c, options, background)
	})
	if err != nil {
		return nil, false, err
	}
	c.nodes.add(model)
	return model, cached, nil
}

func (p *Phone) load(ctx context.Context, c *conn, options SnapshotOptions, background bool) (*PhoneModel, error) {
	geometry := c.geometryAhead()
	var base *PhoneNode
	var springboard uint64
	var err error
	switch {
	case options.Within != "":
		if base, err = p.element(c, options.Within); err != nil {
			return nil, err
		}
	case !options.Front:
		if springboard, err = c.springboardPID(ctx); err != nil {
			return nil, err
		}
	}
	read := c.ax.read
	if background {
		read = c.ax.readLow
	}
	value, err := read(ctx, options.key(), func(ctx context.Context, client *accessibility.RemoteClient) (any, error) {
		return p.readScreen(ctx, c, client, geometry, springboard, base, options)
	})
	if err != nil {
		return nil, err
	}
	return value.(*PhoneModel), nil
}

func (p *Phone) readScreen(ctx context.Context, c *conn, client *accessibility.RemoteClient, geometryAhead func(ctx context.Context) (phoneGeometry, error), springboard uint64, base *PhoneNode, options SnapshotOptions) (*PhoneModel, error) {
	started := time.Now()
	var screen *accessibility.Screen
	var err error
	switch {
	case base != nil:
		screen, err = client.ReadApplication(ctx, accessibility.ApplicationOptions{
			Root: &base.AX, Clip: &base.axClip, Depth: options.Depth, Modal: true, Focused: true, Expand: true, Animations: true,
		})
	case options.Front:
		screen, err = client.ReadApplication(ctx, accessibility.ApplicationOptions{Depth: options.Depth, Focused: true, Expand: true, Animations: true, Front: c.frontGuess()})
	default:
		screen, err = client.ReadScreen(ctx, springboard, options.Depth, nil)
		if errors.Is(err, accessibility.ErrNoApplication) {
			c.springboard.Store(0)
		}
	}
	if err != nil {
		return nil, err
	}
	geometry, err := geometryAhead(ctx)
	if err != nil {
		return nil, err
	}
	return p.model(c, geometry, screen, started, base, options, base == nil), nil
}

func (p *Phone) model(c *conn, geometry phoneGeometry, screen *accessibility.Screen, started time.Time, base *PhoneNode, read SnapshotOptions, front bool) *PhoneModel {
	model := modelOf(geometry, screen, base)
	model.generation = c.ax.current()
	model.animating = screen.Animating
	model.read = read
	root := "the application"
	if base != nil {
		root = "element " + refText(base.AX.Ref)
	}
	p.logger.Debug("read ", len(model.nodes), " nodes of ", model.BundleID, " from ", root, " to depth ", read.Depth, " in ", time.Since(started),
		", rounds ", fmt.Sprint(screen.Rounds))
	if len(model.Truncated) > 0 {
		p.logger.Info("the read of ", model.BundleID, " from ", root, " stopped at depth ", read.Depth, " above the children of ", len(model.Truncated), " elements: ",
			strings.Join(common.Map(model.Truncated, func(node *PhoneNode) string { return refText(node.AX.Ref) + " " + describeElement(node.AX) }), ", "))
	}
	c.ax.answered(screen.App.Ref.PID)
	if front {
		c.front.observed(frontApplication{pid: screen.App.Ref.PID, bundleID: screen.BundleID, at: started, via: "tree"})
		if screen.BundleID != "" {
			c.lastFront.Store(&accessibility.Front{App: screen.App, BundleID: screen.BundleID})
		}
	}
	return model
}

// frontGuess answers the application last read as the frontmost one while
// the app in front is known to be it, so that a read can take it for the
// frontmost one in the same round.
func (c *conn) frontGuess() *accessibility.Front {
	guess := c.lastFront.Load()
	if guess == nil {
		return nil
	}
	if pid, known := c.front.known(); !known || pid != guess.App.Ref.PID {
		return nil
	}
	return guess
}

// geometryLimit bounds a geometry read that a read of the tree waits for.
const geometryLimit = 10 * time.Second

// geometryAhead reads the geometry alongside a read of the tree, which waits
// for it only to place what it read: a geometry read took up to 240 ms over
// Wi-Fi once its cache had expired (iPhone18,2, iOS 27.0.1 24A446).
func (c *conn) geometryAhead() func(ctx context.Context) (phoneGeometry, error) {
	done := make(chan struct{})
	var geometry phoneGeometry
	var err error
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(c.ctx, geometryLimit)
		defer cancel()
		geometry, err = c.readGeometry(ctx)
	}()
	return func(ctx context.Context) (phoneGeometry, error) {
		select {
		case <-done:
			return geometry, err
		case <-ctx.Done():
			return phoneGeometry{}, ctx.Err()
		}
	}
}

func (p *Phone) Snapshot(ctx context.Context, options SnapshotOptions) (*View, error) {
	if err := checkDepth(options.Depth); err != nil {
		return nil, err
	}
	c, ctx, done, err := p.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	started := time.Now()
	model, cached, err := p.read(ctx, c, options)
	if err != nil {
		return nil, err
	}
	name := "snapshot.read"
	if cached {
		name = "snapshot.cached"
	}
	p.connection.trace(started, name, strconv.Itoa(len(model.nodes))+" nodes of "+model.BundleID+", "+strconv.Itoa(len(model.Truncated))+" cut off", time.Since(started))
	return NewView(model, options.Raw), nil
}

func checkDepth(depth int64) error {
	if depth < 0 || depth > accessibility.MaxDepth {
		return E.Extend(ErrInvalid, "depth must be an integer from 0 to ", accessibility.MaxDepth, ", the most one read of the device carries")
	}
	return nil
}

type PhoneInfo struct {
	Name, Model, OSVersion, Build string
	Language                      string
	Screen                        PhoneScreen
}

func (p *Phone) Info(ctx context.Context) (PhoneInfo, error) {
	c, ctx, done, err := p.begin(ctx)
	if err != nil {
		return PhoneInfo{}, err
	}
	defer done()
	info := c.device.Info()
	out := PhoneInfo{Name: *p.connection.name.Load(), Model: info.ProductType, OSVersion: info.OSVersion, Build: info.BuildVersion}
	var geometry phoneGeometry
	var geometryErr error
	var read sync.WaitGroup
	read.Go(func() { geometry, geometryErr = c.readGeometry(ctx) })
	out.Language, err = c.lockState.language(ctx, c.device)
	read.Wait()
	if geometryErr != nil {
		return PhoneInfo{}, geometryErr
	}
	out.Screen = geometry.PhoneScreen
	return out, err
}

// Text recognition reads in the device's language, which a read serves for
// this long.
const languageLife = time.Minute

// Language reads the device's language for text recognition, answering the
// last one read while the device is not connected rather than waiting for it.
func (p *Phone) Language(ctx context.Context) (string, error) {
	p.languageAccess.Lock()
	defer p.languageAccess.Unlock()
	if p.language != "" && time.Since(p.languageAt) < languageLife {
		return p.language, nil
	}
	if p.Disconnected(time.Now()) {
		if p.language != "" {
			return p.language, nil
		}
		return "", ErrDisconnected
	}
	c, ctx, done, err := p.begin(ctx)
	if err != nil {
		return p.language, err
	}
	defer done()
	language, err := c.device.Language(ctx)
	if err != nil {
		return p.language, err
	}
	p.language, p.languageAt = language, time.Now()
	return language, nil
}

func (p *Phone) Locked(ctx context.Context) (bool, error) {
	c, ctx, done, err := p.begin(ctx)
	if err != nil {
		return false, err
	}
	defer done()
	locked, err := c.lockState.locked(ctx, c.device)
	if err != nil {
		return false, E.Cause(err, "read the lock state")
	}
	return locked, nil
}

func (p *Phone) Display(ctx context.Context) (string, error) {
	c, ctx, done, err := p.begin(ctx)
	if err != nil {
		return "", err
	}
	defer done()
	return c.recentBacklight(ctx)
}

func (p *Phone) Frontmost(ctx context.Context) (string, error) {
	c, ctx, done, err := p.begin(ctx)
	if err != nil {
		return "", err
	}
	defer done()
	front, err := c.front.get(ctx)
	return front.bundleID, err
}

func (p *Phone) WatchForeground(ctx context.Context, emit func(string)) error {
	var last string
	for {
		c, operation, done, err := p.begin(ctx)
		if err != nil {
			return err
		}
		changed := make(chan struct{}, 1)
		cancel := c.front.watch(func(ForegroundApp) {
			select {
			case changed <- struct{}{}:
			default:
			}
		})
		for operation.Err() == nil {
			wait := time.Duration(-1)
			if front, err := c.front.get(operation); err == nil {
				if front.bundleID != last {
					last = front.bundleID
					emit(last)
				}
			} else if operation.Err() == nil {
				p.logger.Debug(E.Cause(err, "read the frontmost application"))
				wait = time.Second
			}
			var retry <-chan time.Time
			if wait > 0 {
				retry = time.After(wait)
			}
			select {
			case <-changed:
			case <-retry:
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

// backlight reads the backlight state from the device.
func (c *conn) backlight(ctx context.Context) (string, error) {
	if state, hooked := c.hookedBacklight(); hooked {
		return state, nil
	}
	return c.readBacklight(ctx)
}

// recentBacklight answers the backlight state kept from the device's last
// report while it is current, and reads it otherwise.
func (c *conn) recentBacklight(ctx context.Context) (string, error) {
	if state, hooked := c.hookedBacklight(); hooked {
		return state, nil
	}
	run, relaying := c.darwin.relaying()
	state, current, due := c.backlightCache.cached(time.Now(), run, relaying)
	if !current {
		return c.readBacklight(ctx)
	}
	if due {
		c.refreshBacklight()
	}
	return state, nil
}

func (c *conn) hookedBacklight() (string, bool) {
	read := c.connection.options.BacklightState
	if read == nil {
		return "", false
	}
	switch read() {
	case 0:
		return "off", true
	case 1:
		return "inactiveOn", true
	case 2:
		return "activeOn", true
	case 3:
		return "dimmed", true
	}
	return "", false
}

func (c *conn) readBacklight(ctx context.Context) (string, error) {
	asked := time.Now()
	info, err := c.readDisplays(ctx)
	if err != nil {
		return "", E.Cause(err, "read the backlight state")
	}
	state, err := info.Backlight()
	if err != nil {
		return "", E.Cause(err, "read the backlight state")
	}
	if c.geometryAccess.TryLock() {
		if asked.After(c.geometryAt) {
			_, _ = c.keepGeometryLocked(info, asked)
		}
		c.geometryAccess.Unlock()
	}
	return state, nil
}

func (c *conn) refreshBacklight() {
	if !c.backlightCache.startRefresh() {
		return
	}
	go func() {
		defer c.backlightCache.endRefresh()
		ctx, cancel := context.WithTimeout(c.ctx, backlightRefreshLimit)
		defer cancel()
		if _, err := c.readBacklight(ctx); err != nil && c.ctx.Err() == nil {
			c.logger.Debug(E.Cause(err, "refresh the backlight state"))
		}
	}()
}

func (p *Phone) Clipboard(ctx context.Context) (string, bool, error) {
	c, ctx, done, err := p.begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer done()
	snapshot, err := c.pasteboard.Read(ctx)
	if errors.Is(err, pasteboard.ErrRefused) {
		return "", false, E.New("the clipboard holds an item the device does not hand out, such as a copied password")
	}
	if err != nil {
		return "", false, err
	}
	text, ok := snapshot.Text()
	return text, ok, nil
}

func (p *Phone) SetClipboard(ctx context.Context, text *string) error {
	c, ctx, done, err := p.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	written := make(chan error, 1)
	if text == nil {
		err = c.pasteboard.Clear(ctx, func(err error) { written <- err })
	} else {
		err = c.pasteboard.WriteText(ctx, *text, func(err error) { written <- err })
	}
	if err != nil {
		return err
	}
	select {
	case err = <-written:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Safari's tab bar reports its address after a U+200E (iPhone18,2, iOS 27.2
// 24B115); times put U+202F before AM and PM, and phone numbers U+00A0
// between groups (iPhone18,2, iOS 27.0.1 24A446).
func PlainText(text string) string {
	return strings.Map(plainRune, text)
}

func plainRune(r rune) rune {
	switch {
	case unicode.Is(unicode.Bidi_Control, r):
		return -1
	case r == '\u00a0', r == '\u2007', r == '\u2009', r == '\u202f':
		return ' '
	}
	return r
}
