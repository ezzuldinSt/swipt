package device

import (
	"context"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/coredevice/accessibility"
	"github.com/swipt-automation/swipt/internal/coredevice/hid"
)

// The search bar floating at the bottom of Settings swallowed touches from
// y = 831, 59 pt above its field at y = 890, while UIKit's visible frame of
// the row above it ended at y = 870 and the accessibility hit test at y = 850
// answered the row; taps at y ≤ 830 opened it (iPhone18,2, iOS 27.0.1
// 24A446). Neither the visible frame nor the hit test sees what a bar's
// container takes, so a retried tap aims saferInset inside the edge of the
// element farthest from a bar within barReach of it, and scrolling brings an
// element scrollClearance clear of the bars.
const (
	saferInset      = 5.0
	saferDistance   = 4.0
	barReach        = 80.0
	scrollClearance = 80.0
	// A drag stays this far inside its scroll view.
	scrollMargin = 16.0
	// The drags that bring an element into view, and how long its frame is
	// watched for coming to rest after each.
	scrollDrags  = 4
	scrollSettle = 1200 * time.Millisecond
	scrollPoll   = 60 * time.Millisecond
	scrollStart  = 300 * time.Millisecond
)

// bars lists the frames of the application's bars that stay put while the
// element scrolls: the navigation bars, tab bars, toolbars and search fields
// of its window outside its scroll view and not holding it, and the
// keyboard.
func (n *PhoneNode) bars() []Rect {
	var out []Rect
	walkNodes(n.model.Root, func(it *PhoneNode) {
		if it == n || !it.Shown() || it.Contains(n) || n.Contains(it) || n.scroll != nil && n.scroll.holds(it) {
			return
		}
		keyboard := elementType(it.AX.Type) == typeKeyboard
		if !keyboard && (it.win != n.win || !slices.Contains(barRoles, roleName(it.AX))) {
			return
		}
		out = append(out, it.Visible)
	})
	return out
}

// barsNear tells whether a bar lies within barReach above or below the
// element's visible part, beside it.
func (n *PhoneNode) barsNear() (above, below bool) {
	visible := n.Visible
	middle := visible.Y + visible.Height/2
	for _, bar := range n.bars() {
		if bar.X+bar.Width <= visible.X || bar.X >= visible.X+visible.Width {
			continue
		}
		switch {
		case bar.Y+bar.Height <= middle && visible.Y-(bar.Y+bar.Height) < barReach:
			above = true
		case bar.Y >= middle && bar.Y-(visible.Y+visible.Height) < barReach:
			below = true
		}
	}
	return
}

// saferPoint is where a retried tap aims: at the activation point's x, and
// saferInset inside the edge of the element's visible part farthest from the
// bars near it, or in its middle when bars lie on both sides or neither.
func (n *PhoneNode) saferPoint() Point {
	visible := n.Visible
	if visible.Empty() {
		return n.Point
	}
	at := Point{n.Point.X, visible.Y + visible.Height/2}
	if !visible.Contains(at) {
		at.X = visible.X + visible.Width/2
	}
	inset := min(saferInset, visible.Height/4)
	switch above, below := n.barsNear(); {
	case below && !above:
		at.Y = visible.Y + inset
	case above && !below:
		at.Y = visible.Y + visible.Height - inset
	}
	return at
}

// clearArea is the part of the element's scroll view where an element lies
// scrollClearance clear of the bars above and below it, or scrollMargin
// clear of them when that leaves no room for the element.
func (n *PhoneNode) clearArea() Rect {
	screen := Rect{Width: n.space.geometry.Width, Height: n.space.geometry.Height}
	view := n.scroll.Visible.Intersect(screen)
	area := func(clearance float64) Rect {
		top, bottom := view.Y, view.Y+view.Height
		middle := view.Y + view.Height/2
		for _, bar := range n.bars() {
			if bar.X+bar.Width <= n.Frame.X || bar.X >= n.Frame.X+n.Frame.Width {
				continue
			}
			if bar.Y+bar.Height/2 < middle {
				top = max(top, bar.Y+bar.Height+clearance)
			} else {
				bottom = min(bottom, bar.Y-clearance)
			}
		}
		return Rect{view.X, top, view.Width, bottom - top}
	}
	clear := area(scrollClearance)
	if clear.Height < min(n.Frame.Height, view.Height/3) {
		clear = area(scrollMargin)
	}
	return clear
}

// shift is how far content must move toward lower coordinates for a span from
// start of length to lie within [low, high]. A span longer than that range
// needs to show minimumShown of itself in it, starting at low; a shift under
// 2 pt is none, as a drag cannot place content that finely.
func shift(start, length, low, high float64) float64 {
	var out float64
	switch {
	case length > high-low:
		if min(start+length, high)-max(start, low) >= min(minimumShown, high-low) {
			return 0
		}
		out = start - low
	case start < low:
		out = start - low
	case start+length > high:
		out = start + length - high
	}
	if math.Abs(out) < 2 {
		return 0
	}
	return out
}

const minimumShown = 44.0

// needed is how far the content must move for the frame to lie in the area:
// along y always, and along x only when the frame fits the area's width but
// reaches out of it, as in a carousel.
func needed(frame, area Rect) (dx, dy float64) {
	dy = shift(frame.Y, frame.Height, area.Y, area.Y+area.Height)
	if frame.Width <= area.Width && (frame.X < area.X-1 || frame.X+frame.Width > area.X+area.Width+1) {
		dx = shift(frame.X, frame.Width, area.X, area.X+area.Width)
	}
	return dx, dy
}

func clearOf(frame, area Rect) bool {
	dx, dy := needed(frame, area)
	return dx == 0 && dy == 0
}

// Placement is where an element of a read lies.
type Placement struct {
	// Shown tells that some of it is on the screen.
	Shown bool
	// Covered tells that the application's own bars cover it.
	Covered bool
	// Scrolls tells that it is in a scroll view, which can bring it into view.
	Scrolls bool
}

// Placement answers where the element lies, as its read had it.
func (p *Phone) Placement(ctx context.Context, ref string) (Placement, error) {
	c, _, done, err := p.begin(ctx)
	if err != nil {
		return Placement{}, err
	}
	defer done()
	node, err := p.element(c, ref)
	if err != nil {
		return Placement{}, err
	}
	return Placement{Shown: node.Shown(), Covered: node.coveredInApp() && !node.Inert, Scrolls: node.scroll != nil && node.HasFrame()}, nil
}

// Scrolled is how bringing an element into view went.
type Scrolled struct {
	// Moved tells that the element moved.
	Moved bool
	// Detail tells what was done, for the script.
	Detail string
}

// ScrollIntoView brings the element scrollClearance clear of the
// application's bars: first with the accessibility action scrollToVisible,
// which stops at UIKit's visible frame, then with eased drags of its scroll
// view. It reads the screen again after moving it, so that the next action
// on the element aims at where it is.
func (p *Phone) ScrollIntoView(ctx context.Context, ref string) (Scrolled, error) {
	c, ctx, done, err := p.begin(ctx)
	if err != nil {
		return Scrolled{}, err
	}
	defer done()
	geometry, err := p.prepare(ctx, c, true, true)
	if err != nil {
		return Scrolled{}, err
	}
	started := time.Now()
	node, err := p.element(c, ref)
	if err != nil {
		return Scrolled{}, err
	}
	name := describeElement(node.AX)
	if !node.HasFrame() || node.scroll == nil {
		return Scrolled{}, E.New(name, " is in no scroll view")
	}
	area := node.clearArea()
	frame := node.Frame
	if node.Shown() && clearOf(frame, area) {
		return Scrolled{Detail: name + " is already clear of the bars"}, nil
	}
	s := &axSession{phone: p, conn: c, geometry: geometry}
	var steps []string
	acted := false
	defer func() {
		if acted {
			c.events.publish(Event{Kind: EventInput})
			c.front.acted()
		}
	}()
	err = c.ax.do(ctx, true, node.model.generation, func(ctx context.Context, client *accessibility.RemoteClient) error {
		s.client = client
		return s.perform(ctx, node, "scrollToVisible")
	})
	acted = acted || s.acted
	if err == nil {
		steps = append(steps, "scrollToVisible")
		if frame, err = p.restingFrame(ctx, c, node, frame); err != nil {
			return Scrolled{Moved: true}, err
		}
	} else if ctx.Err() != nil {
		return Scrolled{}, err
	}
	for range scrollDrags {
		dx, dy := needed(frame, area)
		if dx == 0 && dy == 0 {
			break
		}
		from, to := dragAcross(area, dx, dy)
		fingers, err := DragPath(from, to, dragTime(from, to), 0)
		if err != nil {
			return Scrolled{}, err
		}
		acted = true
		if err = p.gesture(ctx, c, func(input *hid.Client) error {
			return play(ctx, geometry, input, fingers)
		}); err != nil {
			return Scrolled{}, err
		}
		steps = append(steps, "a drag from "+pointText(from)+" to "+pointText(to))
		moved, err := p.restingFrame(ctx, c, node, frame)
		if err != nil {
			return Scrolled{Moved: true}, err
		}
		if nearRect(moved, frame) {
			frame = moved
			break
		}
		frame = moved
	}
	result := Scrolled{Moved: !nearRect(frame, node.Frame), Detail: name + " was brought into view with " + strings.Join(steps, ", then ")}
	if !clearOf(frame, area) {
		result.Detail += "; it is still not " + strconv.Itoa(int(scrollClearance)) + " pt clear of the bars"
	}
	p.connection.trace(started, "scrollIntoView", result.Detail, time.Since(started))
	if result.Moved {
		if _, _, err = p.read(ctx, c, SnapshotOptions{}); err != nil {
			return result, err
		}
	}
	return result, nil
}

// dragAcross is a drag within area that moves its content dx and dy toward
// lower coordinates, as far as the area allows.
func dragAcross(area Rect, dx, dy float64) (Point, Point) {
	reach := func(delta, length float64) float64 {
		if delta == 0 {
			return 0
		}
		return math.Copysign(min(math.Abs(delta)+touchSlop, math.Max(length-2*scrollMargin, 0)*0.9), delta)
	}
	moveX, moveY := reach(dx, area.Width), reach(dy, area.Height)
	start := func(origin, length, move float64) float64 {
		switch {
		case move > 0:
			return origin + length - scrollMargin
		case move < 0:
			return origin + scrollMargin
		}
		return origin + length/3
	}
	from := Point{start(area.X, area.Width, moveX), start(area.Y, area.Height, moveY)}
	return from, Point{from.X - moveX, from.Y - moveY}
}

// dragTime gives a drag 1.5 ms a point, from 300 to 800 ms, slow enough that
// the scroll view keeps no speed when it ends.
func dragTime(from, to Point) time.Duration {
	length := math.Hypot(to.X-from.X, to.Y-from.Y)
	return min(max(time.Duration(length*1.5*float64(time.Millisecond)), 300*time.Millisecond), 800*time.Millisecond)
}

func nearRect(first, second Rect) bool {
	return math.Abs(first.X-second.X) < 1 && math.Abs(first.Y-second.Y) < 1 && math.Abs(first.Width-second.Width) < 1 && math.Abs(first.Height-second.Height) < 1
}

// restingFrame reads the element's frame until it stops moving, for up to
// scrollSettle; a frame still where it was before, at from, has scrollStart
// to begin moving, as an animated scroll may start after the action answers.
func (p *Phone) restingFrame(ctx context.Context, c *conn, node *PhoneNode, from Rect) (Rect, error) {
	started := time.Now()
	var last *Rect
	for {
		frames, err := p.frames(ctx, c, []*PhoneNode{node})
		if err != nil {
			return Rect{}, err
		}
		if frames[0] == nil {
			return Rect{}, E.New(describeElement(node.AX), " went away as it scrolled")
		}
		elapsed := time.Since(started)
		if last != nil && nearRect(*last, *frames[0]) && (!nearRect(from, *frames[0]) || elapsed >= scrollStart) || elapsed >= scrollSettle {
			return *frames[0], nil
		}
		last = frames[0]
		if err = sleep(ctx, scrollPoll); err != nil {
			return Rect{}, err
		}
	}
}

// frames reads the nodes' frames on the screen, nil for those the device no
// longer has.
func (p *Phone) frames(ctx context.Context, c *conn, nodes []*PhoneNode) ([]*Rect, error) {
	out := make([]*Rect, len(nodes))
	err := c.ax.do(ctx, false, nodes[0].model.generation, func(ctx context.Context, client *accessibility.RemoteClient) error {
		batch := &accessibility.Batch{}
		mapped := make([]*accessibility.Result[bool], len(nodes))
		attributes := make([]*accessibility.Result[accessibility.Element], len(nodes))
		for index, node := range nodes {
			mapped[index], attributes[index] = batch.Mapped(node.AX), batch.Attributes(node.AX)
		}
		if err := client.Run(ctx, batch); err != nil {
			return err
		}
		for index, node := range nodes {
			if mapped[index].Code == 0 && mapped[index].Value && attributes[index].Code == 0 && attributes[index].Value.Frame != nil {
				out[index] = new(node.space.rect(*attributes[index].Value.Frame))
			}
		}
		return nil
	})
	return out, err
}

// Paged is how a page scroll went.
type Paged struct {
	// Moved tells that the content moved.
	Moved bool
	// Container names the scroll view.
	Container string
}

var pageActions = map[string]string{"down": "scrollDown", "up": "scrollUp", "left": "scrollLeft", "right": "scrollRight"}

// pageReferences is how many elements of a scroll view show whether it moved.
const pageReferences = 3

// ScrollPage scrolls the largest scroll view of the read the element belongs
// to by a page, toward content further down, up, left or right: with the
// accessibility action, or with an eased drag across most of it where the
// action does not move it.
func (p *Phone) ScrollPage(ctx context.Context, ref, direction string) (Paged, error) {
	action, known := pageActions[direction]
	if !known {
		return Paged{}, E.Extend(ErrInvalid, "the direction must be down, up, left or right")
	}
	c, ctx, done, err := p.begin(ctx)
	if err != nil {
		return Paged{}, err
	}
	defer done()
	geometry, err := p.prepare(ctx, c, true, true)
	if err != nil {
		return Paged{}, err
	}
	started := time.Now()
	element, err := p.element(c, ref)
	if err != nil {
		return Paged{}, err
	}
	scroll := element.model.largestScroll()
	if scroll == nil {
		return Paged{}, E.Extend(ErrNotFound, element.model.BundleID, " shows no scroll view")
	}
	paged := Paged{Container: describeNode(scroll) + " of " + element.model.BundleID}
	references := scroll.references()
	if len(references) == 0 {
		return paged, E.New(paged.Container, " shows nothing that could tell whether it scrolled")
	}
	before, err := p.frames(ctx, c, references)
	if err != nil {
		return paged, err
	}
	s := &axSession{phone: p, conn: c, geometry: geometry}
	defer func() {
		if s.acted {
			c.events.publish(Event{Kind: EventInput})
			c.front.acted()
		}
	}()
	err = c.ax.do(ctx, true, scroll.model.generation, func(ctx context.Context, client *accessibility.RemoteClient) error {
		s.client = client
		return s.perform(ctx, scroll, action)
	})
	if err != nil && ctx.Err() != nil {
		return paged, err
	}
	how := "the action " + action
	if err == nil {
		if paged.Moved, err = p.pageMoved(ctx, c, references, before); err != nil {
			return paged, err
		}
	}
	if !paged.Moved {
		s.acted = true
		from, to := pageDrag(scroll, direction)
		fingers, err := DragPath(from, to, dragTime(from, to), 0)
		if err != nil {
			return paged, err
		}
		if err = p.gesture(ctx, c, func(input *hid.Client) error {
			return play(ctx, geometry, input, fingers)
		}); err != nil {
			return paged, err
		}
		how = "a drag from " + pointText(from) + " to " + pointText(to)
		if paged.Moved, err = p.pageMoved(ctx, c, references, before); err != nil {
			return paged, err
		}
	}
	p.connection.trace(started, "scrollPage", paged.Container+" "+direction+" with "+how+", moved "+strconv.FormatBool(paged.Moved), time.Since(started))
	return paged, nil
}

// pageMoved watches the references come to rest, telling whether any moved
// from where it was or went; references still where they were have
// scrollStart to begin moving.
func (p *Phone) pageMoved(ctx context.Context, c *conn, references []*PhoneNode, before []*Rect) (bool, error) {
	started := time.Now()
	var last []*Rect
	for {
		now, err := p.frames(ctx, c, references)
		if err != nil {
			return false, err
		}
		resting := last != nil && slices.EqualFunc(now, last, func(a, b *Rect) bool {
			return a == nil && b == nil || a != nil && b != nil && nearRect(*a, *b)
		})
		moved := !slices.EqualFunc(now, before, func(a, b *Rect) bool {
			return a != nil && b != nil && nearRect(*a, *b)
		})
		if elapsed := time.Since(started); resting && (moved || elapsed >= scrollStart) || elapsed >= scrollSettle {
			return moved, nil
		}
		last = now
		if err = sleep(ctx, scrollPoll); err != nil {
			return false, err
		}
	}
}

// pageDrag crosses most of the scroll view's part clear of the bars, against
// the direction of the content wanted.
func pageDrag(scroll *PhoneNode, direction string) (Point, Point) {
	area := scroll.Visible.Intersect(Rect{Width: scroll.space.geometry.Width, Height: scroll.space.geometry.Height})
	top, bottom := area.Y, area.Y+area.Height
	middle := area.Y + area.Height/2
	for _, bar := range scroll.bars() {
		if bar.Y+bar.Height/2 < middle {
			top = max(top, bar.Y+bar.Height+scrollMargin)
		} else {
			bottom = min(bottom, bar.Y-scrollClearance)
		}
	}
	if bottom-top < 2*scrollMargin {
		top, bottom = area.Y+scrollMargin, area.Y+area.Height-scrollMargin
	}
	x := area.X + area.Width/3
	left, right := area.X+scrollMargin, area.X+area.Width-scrollMargin
	y := (top + bottom) / 2
	switch direction {
	case "down":
		return Point{x, bottom}, Point{x, bottom - (bottom-top)*0.75}
	case "up":
		return Point{x, top}, Point{x, top + (bottom-top)*0.75}
	case "right":
		return Point{right, y}, Point{right - (right-left)*0.75, y}
	}
	return Point{left, y}, Point{left + (right-left)*0.75, y}
}

// largestScroll is the shown scroll view of the application that shows the
// most.
func (m *PhoneModel) largestScroll() *PhoneNode {
	var out *PhoneNode
	walkNodes(m.Root, func(node *PhoneNode) {
		if scrolls(node.AX) && node.Shown() && !node.Inert && (out == nil || node.Visible.area() > out.Visible.area()) {
			out = node
		}
	})
	return out
}

// references picks pageReferences shown elements of the scroll view, spread
// through it, whose frames tell whether it moved.
func (n *PhoneNode) references() []*PhoneNode {
	var shown []*PhoneNode
	walkNodes(n, func(node *PhoneNode) {
		if node != n && node.AX.AccessibilityElement && node.Shown() {
			shown = append(shown, node)
		}
	})
	if len(shown) <= pageReferences {
		return shown
	}
	out := make([]*PhoneNode, 0, pageReferences)
	for index := range pageReferences {
		out = append(out, shown[index*(len(shown)-1)/(pageReferences-1)])
	}
	return out
}
