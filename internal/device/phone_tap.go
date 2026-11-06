package device

import (
	"context"
	"errors"
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
}

func (a Aim) String() string {
	if a.Point == (Point{}) {
		return a.Element
	}
	return a.Element + " at " + pointText(a.Point)
}

func (p *Phone) Tap(ctx context.Context, ref string, timeout time.Duration) (Aim, error) {
	return p.aimed(ctx, ref, timeout, "tap", func(ctx context.Context, s *axSession, _ *PhoneNode, at Point) error {
		return s.tap(ctx, at, 1, 0)
	})
}

func (p *Phone) SetText(ctx context.Context, ref, text string, timeout time.Duration) (Aim, error) {
	return p.aimed(ctx, ref, timeout, "setText", func(ctx context.Context, s *axSession, node *PhoneNode, at Point) error {
		return s.setText(ctx, node, at, text)
	})
}

// aimed runs act on the element once a tap at the point it answers would reach
// it, checking again while the element is moving, disabled or behind another
// one, until the timeout, or for coverLimit while the application's own bars
// cover it; a negative timeout waits forever and zero checks once.
func (p *Phone) aimed(ctx context.Context, ref string, timeout time.Duration, name string, act func(ctx context.Context, s *axSession, node *PhoneNode, at Point) error) (Aim, error) {
	c, ctx, done, err := p.begin(ctx)
	if err != nil {
		return Aim{}, err
	}
	defer done()
	geometry, err := p.prepare(ctx, c, true, true)
	if err != nil {
		return Aim{}, err
	}
	grant, err := p.connection.input.acquire(ctx, c)
	if err != nil {
		return Aim{}, err
	}
	defer grant.release()
	pushes, unsubscribe := c.events.subscribe(func(event Event) bool {
		return event.Kind == EventAccessibility && event.Notification == notificationAlert
	})
	defer unsubscribe()
	started := time.Now()
	s := &axSession{phone: p, conn: c, geometry: geometry, grant: grant, settled: started.Add(animationLimit)}
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
	var waited string
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
		if errors.Is(err, ErrNotAnswering) {
			pending, err = err.Error(), nil
		}
		if err != nil || pending == "" {
			detail := s.target.String() + ", attempt " + strconv.Itoa(attempt)
			if waited != "" {
				detail += ", after " + waited
			}
			p.connection.trace(started, name+".element", detail, time.Since(started))
			break
		}
		waited = pending
		alerts = append(alerts, alerted(pushes)...)
		delay = min(max(delay*2, pollFirst), pollLongest)
		if s.animating {
			delay = pollFirst
		}
		if !s.animating && !deadline.IsZero() && time.Now().Add(delay).After(deadline) {
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
			if _, err = p.read(ctx, c, s.reread); err != nil {
				return s.target, err
			}
		}
	}
	if s.acted {
		c.events.publish(Event{Kind: EventInput})
		c.front.acted()
	}
	return s.target, err
}

func (s *axSession) aim(ctx context.Context, target *PhoneNode) (Point, string, error) {
	name := describeElement(target.AX)
	s.target = Aim{Element: name, Point: target.Point, Secure: target.AX.Secure()}
	s.animating, s.stale, s.fixed, s.blocker = false, false, false, nil
	s.reread = SnapshotOptions{Depth: target.model.read.Depth, Within: target.model.read.Within}
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
	batch := &accessibility.Batch{}
	mapped := batch.Mapped(target.AX)
	attributes := batch.Attributes(target.AX)
	hit := batch.HitTest(s.geometry.digitizer(at))
	animating := batch.AnimationsInProgress(target.AX)
	if err := s.client.Run(ctx, batch); err != nil {
		return Point{}, "", err
	}
	previous := s.previous
	s.previous = attributes.Value.Frame
	switch {
	case mapped.Code == 0 && !mapped.Value:
		return Point{}, name + " is gone", nil
	case mapped.Code != 0 || attributes.Code != 0:
		return Point{}, name + " did not answer", nil
	case !attributes.Value.Enabled:
		return Point{}, name + " is disabled", nil
	case previous != nil && !nearFrame(attributes.Value.Frame, previous):
		return Point{}, name + " is moving", nil
	}
	if fresh := attributes.Value; fresh.Label != "" || fresh.Identifier != "" || fresh.Value != "" {
		name = describeElement(fresh)
	}
	s.target.Element, s.target.Secure = name, attributes.Value.Secure()
	if animating.Code == 0 && animating.Value && time.Now().Before(s.settled) {
		s.animating = true
		return Point{}, name + " is animating", nil
	}
	if fresh := attributes.Value.Frame; fresh != nil {
		rect := target.space.rect(*fresh)
		shown := rect.Intersect(Rect{Width: s.geometry.Width, Height: s.geometry.Height})
		if !rect.Empty() && shown.Empty() {
			s.stale = true
			return Point{}, name + " is off the screen, at " + pointText(Point{rect.X, rect.Y}) + " sized " + pointText(Point{rect.Width, rect.Height}), nil
		}
		if !rect.Empty() && !rect.Contains(at) {
			at = shown.Center()
			again := &accessibility.Batch{}
			hit = again.HitTest(s.geometry.digitizer(at))
			if err := s.client.Run(ctx, again); err != nil {
				return Point{}, "", err
			}
		}
	}
	if hit.Code != 0 {
		return Point{}, "the device does not tell which element is at " + pointText(at), nil
	}
	var reached accessibility.Element
	node := target.model.Node(hit.Value.Ref)
	switch {
	case hit.Value.Ref == target.AX.Ref, node != nil && (target.Contains(node) || node.AX.AccessibilityElement && node.Contains(target)):
		return at, "", nil
	case node != nil:
		reached = node.AX
	default:
		inside, known, err := s.inside(ctx, target, hit.Value)
		if err != nil {
			return Point{}, "", err
		}
		if inside {
			return at, "", nil
		}
		read := &accessibility.Batch{}
		element := read.Attributes(hit.Value)
		if err = s.client.Run(ctx, read); err != nil {
			return Point{}, "", err
		}
		if !known && target.model.read.Within == "" && element.Code == 0 && sameWindow(target, element.Value) {
			return at, "", nil
		}
		reached = element.Value
	}
	if target.Inert {
		s.blocker = &blocker{pid: target.AX.Ref.PID, modal: true}
		return Point{}, name + " is behind a modal view, as a sheet or an alert", nil
	}
	s.blocker = &blocker{pid: reached.Ref.PID, ref: &reached.Ref}
	reason := "a tap at " + pointText(at) + " would reach " + describeElement(reached) + " of " + s.process(ctx, reached.Ref.PID) + ", not " + name +
		" of " + s.process(ctx, target.AX.Ref.PID)
	if reached.Ref.PID == target.AX.Ref.PID {
		reason += "; input.tap at the element's point taps there regardless"
	}
	return Point{}, reason, nil
}

func (s *axSession) process(ctx context.Context, pid uint64) string {
	text := "process " + strconv.FormatUint(pid, 10)
	bundleID := s.conn.front.bundleOf(pid)
	if springboard, err := s.conn.springboardPID(ctx); bundleID == "" && err == nil && pid == springboard {
		bundleID = springboardBundleID
	}
	if bundleID == "" {
		return text
	}
	return bundleID + " (" + text + ")"
}

func (s *axSession) inside(ctx context.Context, target *PhoneNode, hit accessibility.Element) (inside, known bool, err error) {
	current := hit
	for range accessibility.MaxDepth + 1 {
		if node := target.model.Node(current.Ref); node != nil {
			return target.Contains(node) || node.AX.AccessibilityElement && node.Contains(target), true, nil
		}
		batch := &accessibility.Batch{}
		parent := batch.Parent(current)
		if err = s.client.Run(ctx, batch); err != nil {
			return false, false, err
		}
		if parent.Code != 0 || parent.Value.Application() {
			return false, false, nil
		}
		current = parent.Value
	}
	return false, false, nil
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
