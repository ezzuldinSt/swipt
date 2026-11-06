package device

import (
	"context"
	"math"
	"slices"
	"strconv"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/coredevice/accessibility"
)

// UIKit drops touches during a presentation while the accessibility hit test
// already reaches the element: an alert took them 450 to 500 ms after the tap
// that presented it, and the animation count reached zero about 100 ms later;
// after a launch it reached zero as late as 1.23 s (iPad17,1, iPadOS 27.3
// 24C5037i). The count covers every animation of the process, so a repeating
// one must not hold a tap back for long.
const animationLimit = 1500 * time.Millisecond

type Aim struct {
	Element string
	Point   Point
	Secure  bool
	// Visible is the part of the element on the screen, where a tap's effect
	// shows first.
	Visible Rect
	// Risky is the word of the element's label or identifier that makes acting
	// on it irreversible or outward, such as "Delete" or "Send"; nothing acts on
	// such an element again on its own.
	Risky string
	// Selected tells that the element was selected as the tap went, so the tap
	// may rightly change nothing.
	Selected bool
	// Doubt tells why the tap's point may have missed the element: part of
	// it was cut off, a cover or bar lay by it, it moved or animated before
	// the tap, or the hit test answered another element that it accepted. It
	// is "" for an element shown whole, clear and steady.
	Doubt string

	mark Mark
}

func (a Aim) String() string {
	if a.Point == (Point{}) {
		return a.Element
	}
	return a.Element + " at " + pointText(a.Point)
}

// Mark is what the device had reported when the input went, for Reaction.
func (a Aim) Mark() Mark {
	return a.mark
}

// TapOptions are the options of TapElement.
type TapOptions struct {
	// Timeout is how long the tap waits for the element, as Tap takes it.
	Timeout time.Duration
	// Retry is a point a tap on the element already tried: the tap aims at the
	// element's safest point instead (saferPoint), and gives up with
	// ErrSamePoint, tapping nothing, when that is the same one.
	Retry *Point
	// Ready runs as the touch is about to go, once the element is aimed at.
	Ready func(ctx context.Context)
}

// ErrSamePoint tells that an element has no safer point than the one a tap
// already tried.
var ErrSamePoint = E.New("the element has no safer point than the one tried")

func (p *Phone) Tap(ctx context.Context, ref string, timeout time.Duration) (Aim, error) {
	return p.TapElement(ctx, ref, TapOptions{Timeout: timeout})
}

// TapElement taps the element once a tap at the point it answers would reach
// it, as Tap does.
func (p *Phone) TapElement(ctx context.Context, ref string, options TapOptions) (Aim, error) {
	name := "tap"
	if options.Retry != nil {
		name = "tap.safer"
	}
	return p.aimed(ctx, ref, options.Timeout, name, options.Retry, func(ctx context.Context, s *axSession, node *PhoneNode, at Point) error {
		if options.Ready != nil {
			options.Ready(ctx)
		}
		if err := s.tap(ctx, at, 1, 0); err != nil {
			return err
		}
		s.afterTouch(ctx, node, at)
		return nil
	})
}

func (p *Phone) SetText(ctx context.Context, ref, text string, timeout time.Duration) (Aim, error) {
	return p.aimed(ctx, ref, timeout, "setText", nil, func(ctx context.Context, s *axSession, node *PhoneNode, at Point) error {
		return s.setText(ctx, node, at, text)
	})
}

// aimed runs act on the element once a tap at the point it answers would reach
// it, checking again while the element is moving, disabled or behind another
// one, until the timeout, or for coverLimit while the application's own bars
// cover it; a negative timeout waits forever and zero checks once. Given
// retry, the point a tap tried before, it aims at the element's safest point.
func (p *Phone) aimed(ctx context.Context, ref string, timeout time.Duration, name string, retry *Point, act func(ctx context.Context, s *axSession, node *PhoneNode, at Point) error) (Aim, error) {
	c, ctx, done, err := p.begin(ctx)
	if err != nil {
		return Aim{}, err
	}
	defer done()
	defer c.tree.act()()
	geometry, err := p.prepare(ctx, c, true, true)
	if err != nil {
		return Aim{}, err
	}
	grant, err := p.connection.input.acquire(ctx, c)
	if err != nil {
		return Aim{}, err
	}
	defer grant.release()
	pushes, unsubscribe := c.events.subscribe()
	defer unsubscribe()
	started := time.Now()
	s := &axSession{phone: p, conn: c, geometry: geometry, grant: grant, settled: started.Add(animationLimit), steadyUntil: started.Add(steadyLimit), retry: retry}
	var deadline time.Time
	if timeout >= 0 {
		deadline = started.Add(timeout)
	}
	var alerts []uint64
	refuse := func(pending, after string, cause error) (Aim, error) {
		p.connection.trace(started, name+".refused", pending, time.Since(started))
		if text := p.interrupted(ctx, c, geometry, s.blocker, append(alerts, alerted(pushes)...)); text != "" {
			pending += "; in the way: " + text
		}
		if cause == nil {
			return s.target, E.New(pending, after)
		}
		return s.target, E.Extend(cause, pending, after)
	}
	var delay time.Duration
	var waited, unsteady string
	var covered time.Time
	for attempt := 1; ; attempt++ {
		var pending string
		err = c.ax.do(ctx, true, 0, func(ctx context.Context, client *accessibility.RemoteClient) error {
			s.client = client
			node, err := p.element(c, ref)
			if err != nil {
				return err
			}
			at, reason, err := s.aim(ctx, node)
			if err != nil || reason != "" {
				pending = reason
				return err
			}
			s.target.Point = at
			return act(ctx, s, node, at)
		})
		if err != nil || pending == "" {
			detail := s.target.String() + ", attempt " + strconv.Itoa(attempt)
			if waited != "" {
				detail += ", after " + waited
			}
			p.connection.trace(started, name+".element", detail, time.Since(started))
			break
		}
		waited = pending
		if s.animating || s.moved || s.blocker != nil {
			unsteady = pending
		}
		alerts = append(alerts, alerted(pushes)...)
		delay = min(max(delay*2, pollFirst), pollLongest)
		switch {
		case s.settling:
			delay = steadyGap
		case s.animating:
			delay = pollFirst
		}
		if !s.animating && !s.settling && !deadline.IsZero() && time.Now().Add(delay).After(deadline) {
			if timeout == 0 {
				return refuse(pending, "", ErrTimeout)
			}
			return refuse(pending, "; gave up after "+timeout.String(), ErrTimeout)
		}
		switch {
		case !s.fixed:
			covered = time.Time{}
		case covered.IsZero():
			covered = time.Now()
		case time.Since(covered) >= coverLimit:
			return refuse(pending, "", nil)
		}
		if err = sleep(ctx, delay); err != nil {
			return s.target, err
		}
		if s.stale {
			if _, _, err = p.read(ctx, c, SnapshotOptions{}); err != nil {
				return s.target, err
			}
		}
	}
	if s.acted {
		c.events.publish(Event{Kind: EventInput})
		c.front.acted()
		s.target.mark.conn, s.target.mark.screens = c, c.events.screen.Load()
		if s.target.Doubt == "" && unsteady != "" {
			s.target.Doubt = "before the tap, " + unsteady
		}
	}
	return s.target, err
}

func (s *axSession) aim(ctx context.Context, target *PhoneNode) (Point, string, error) {
	name := describeElement(target.AX)
	s.target = Aim{Element: name, Point: target.Point, Secure: target.AX.Secure(), Visible: target.Visible, Risky: riskyWord(target.AX), Doubt: target.doubt()}
	s.animating, s.moved, s.settling, s.stale, s.fixed, s.blocker = false, false, false, false, false, nil
	if !target.HasFrame() {
		return Point{}, "", E.New(name, " has no position on the screen; perform one of its actions instead")
	}
	s.stale = true
	switch {
	case !target.AX.Enabled:
		return Point{}, name + " is disabled", nil
	case !target.Shown():
		return Point{}, name + " is off the screen", nil
	case target.coveredInApp() && !target.Inert:
		s.fixed = true
		return Point{}, name + " is covered by " + target.coverText() + ", which leaves " + strconv.Itoa(int(target.shown*100)) +
			"% of it showing; scroll it clear of that rather than tapping it blind", nil
	case target.Covered && !target.Inert:
		return Point{}, name + " is covered by " + target.coverText(), nil
	}
	s.stale = false
	at := target.Point
	if s.retry != nil {
		if at = target.saferPoint(); math.Hypot(at.X-s.retry.X, at.Y-s.retry.Y) < saferDistance {
			return Point{}, "", ErrSamePoint
		}
	}
	batch := &accessibility.Batch{}
	mapped := batch.Mapped(target.AX)
	attributes := batch.Attributes(target.AX)
	hit := batch.HitTest(s.geometry.digitizer(at))
	animating := batch.AnimationsInProgress(target.AX)
	if err := s.client.Run(ctx, batch); err != nil {
		return Point{}, "", err
	}
	previous, previousAt := s.previous, s.previousAt
	s.previous, s.previousAt = attributes.Value.Frame, time.Now()
	switch {
	case mapped.Code == 0 && !mapped.Value:
		return Point{}, name + " is gone", nil
	case mapped.Code != 0 || attributes.Code != 0:
		return Point{}, name + " did not answer", nil
	case !attributes.Value.Enabled:
		return Point{}, name + " is disabled", nil
	}
	name = describeElement(attributes.Value)
	s.target.Element, s.target.Secure, s.target.Selected = name, attributes.Value.Secure(), attributes.Value.Selected
	if risky := riskyWord(attributes.Value); risky != "" {
		s.target.Risky = risky
	}
	s.target.mark = Mark{element: &attributes.Value, front: target.model.Root.AX.Ref.PID, generation: target.model.generation}
	if animating.Code == 0 && animating.Value && time.Now().Before(s.settled) {
		s.animating = true
		return Point{}, name + " is animating", nil
	}
	if reason := s.steadiness(target, attributes.Value.Frame, previous, previousAt); reason != "" {
		return Point{}, name + reason, nil
	}
	if hit.Code != 0 {
		return Point{}, "the device does not tell which element is at " + pointText(at), nil
	}
	node := target.model.Node(hit.Value.Ref)
	switch {
	case hit.Value.Ref == target.AX.Ref, node != nil && target.Contains(node):
		return at, "", nil
	case node != nil && hit.Value.AccessibilityElement && node.Contains(target):
		s.target.Doubt = "the hit test answered " + describeElement(node.AX) + ", which holds it"
		return at, "", nil
	case node == nil:
		read := &accessibility.Batch{}
		element := read.Attributes(hit.Value)
		if err := s.client.Run(ctx, read); err != nil {
			return Point{}, "", err
		}
		if element.Code == 0 && sameWindow(target, element.Value) {
			s.target.Doubt = "the hit test answered " + describeElement(element.Value) + ", which the read did not list"
			return at, "", nil
		}
		hit.Value = element.Value
	}
	if target.Inert {
		s.blocker = &blocker{pid: target.AX.Ref.PID, modal: true}
		return Point{}, name + " is behind a modal view, as a sheet or an alert", nil
	}
	s.blocker = &blocker{pid: hit.Value.Ref.PID, ref: &hit.Value.Ref}
	return Point{}, "a tap at " + pointText(at) + " would reach " + describeElement(hit.Value) + ", not " + name, nil
}

// An element covered by the application's own bars stays covered until the
// script scrolls it, so a tap gives up on it once a transition would have
// ended.
const coverLimit = animationLimit

// alerted lists the processes that posted an alert notification among the
// events received.
func alerted(pushes <-chan Event) []uint64 {
	var out []uint64
	for {
		select {
		case event := <-pushes:
			if event.Kind == EventAccessibility && event.Notification == notificationAlert && event.PID != 0 && !slices.Contains(out, event.PID) {
				out = append(out, event.PID)
			}
		default:
			return out
		}
	}
}

func sameWindow(target *PhoneNode, hit accessibility.Element) bool {
	context := hit.Context
	if node := target.model.Node(hit.Ref); node != nil {
		context = node.context()
	}
	if own := target.context(); context != 0 && own != 0 {
		return context == own
	}
	return hit.Ref.PID == target.AX.Ref.PID
}

func nearFrame(first, second *accessibility.LayoutFrame) bool {
	if first == nil || second == nil {
		return first == second
	}
	return math.Abs(first.X-second.X) < 1 && math.Abs(first.Y-second.Y) < 1 && math.Abs(first.Width-second.Width) < 1 && math.Abs(first.Height-second.Height) < 1
}
