package automation

import (
	"context"
	"errors"
	"math"
	"strconv"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/device"
)

// ScreenWatcher is a Graphics that can watch the live screen around an
// action, to tell what the action changed.
type ScreenWatcher interface {
	WatchScreen(ctx context.Context) (ScreenWatch, error)
}

// ScreenWatch holds the live screen for one action and its retries.
type ScreenWatch interface {
	// Mark takes the newest frame as the screen before the touch.
	Mark(ctx context.Context) error
	// Change tells where the frames the device captured after the touch changed from the
	// screen before it, and how (ScreenChange).
	Change(ctx context.Context, query ChangeQuery) (ScreenChange, error)
	// Settle waits, for up to limit, until the region and then the whole screen have not
	// changed for quiet.
	Settle(ctx context.Context, region device.Rect, quiet, limit time.Duration) error
	Close()
}

// ChangeQuery is what a watch looks for after a touch.
type ChangeQuery struct {
	// Target is where, in points, the touch's own effect shows.
	Target device.Rect
	// Touched is when the touch went, on the PC's clock; the frames the device captured
	// before Touched plus Lead show the screen before it could answer.
	Touched time.Time
	Lead    time.Duration
	// Window is how long after the touch a change may begin; Stall is how much longer the
	// watch waits for the frames to tell, after which the change is unknown.
	Window, Stall time.Duration
}

// ScreenChange is what a watch saw after a touch.
type ScreenChange struct {
	// Where is "target" or "elsewhere" for a change that lasted, ChangeTransient for one
	// that went back, ChangeUnknown when the frames did not come in time to tell, and ""
	// for none.
	Where string
	// After is the time from the touch to the first changed frame's capture.
	After time.Duration
	// ByArrival tells that the stream did not tell when the device captured its frames, so
	// they were ordered by when they came.
	ByArrival bool
}

// ChangeTransient and ChangeUnknown are what ScreenChange.Where answers for a change that
// went back, as a press highlight does, and for frames that did not come in time.
const (
	ChangeTransient = "transient"
	ChangeUnknown   = "unknown"
)

const (
	// The first changed frame of a tap reached the PC 105 ms after the touch
	// at the soonest and about 270 ms after it typically, and Settings pushed
	// a view within 350 ms (iPhone18,2, iOS 27.0.1 24A446): a tap that shows
	// nothing in this long most likely missed.
	effectWindow = 800 * time.Millisecond
	// After the frames show nothing, the device's screen-change push, which
	// comes 0.75 s after a change at the soonest, has this long to arrive.
	pushGrace = 200 * time.Millisecond
	// How long a retried tap waits for its element.
	retryLimit = time.Second
	// How much longer than the window a watch waits for a frame the device captured after
	// it, over a link that holds frames back, before the effect is unknown.
	stallLimit = 1500 * time.Millisecond
	// A frame the device captured within renderLead plus the touch's trip to the device
	// shows the screen before the touch: UIKit draws a touch's answer a frame or two after
	// it comes. The trip is the round of accessibility requests around the touch, which
	// bounds the round trip, within shortestTrip and longestTrip, or defaultTrip when none
	// was timed: round trips over Wi-Fi took 11 to 25 ms (iPhone18,2, iOS 27.0.1 24A446).
	renderLead   = 30 * time.Millisecond
	defaultTrip  = 25 * time.Millisecond
	shortestTrip = 10 * time.Millisecond
	longestTrip  = 200 * time.Millisecond
	// A tap on an application that came to the front less than arrivalSettle ago waits up
	// to arrivalLimit for the element's part of the screen, and then the whole screen, to stay
	// still for settleQuiet first: Settings finished loading its list after the first read
	// of it (iPhone18,2, iOS 27.0.1 24A446).
	arrivalSettle = 1500 * time.Millisecond
	settleQuiet   = 150 * time.Millisecond
	arrivalLimit  = 1500 * time.Millisecond
	// A point tap's target is this square around the point, the size of the
	// smallest control Apple's guidelines allow, on each side of it.
	pointReach = 44.0
)

const (
	effectTarget    = "target"
	effectElsewhere = "elsewhere"
	effectNone      = "none"
	effectMoved     = "moved"
	effectUnknown   = ChangeUnknown
)

// effect is what an input changed, as a script sees it.
type effect struct {
	Where  string `json:"effect"`
	Detail string `json:"detail,omitempty"`
}

// tapAttempt is one way a tap tried to reach its element.
type tapAttempt struct {
	Method string      `json:"method"`
	Point  *[2]float64 `json:"point,omitempty"`
	Effect string      `json:"effect,omitempty"`
	Detail string      `json:"detail,omitempty"`
}

// tapResult is what accessibility.tap and input.tap answer.
type tapResult struct {
	Element  string       `json:"element,omitempty"`
	Point    *[2]float64  `json:"point,omitempty"`
	Effect   string       `json:"effect,omitempty"`
	Detail   string       `json:"detail,omitempty"`
	Attempts []tapAttempt `json:"attempts,omitempty"`
}

func pointOf(at device.Point) *[2]float64 {
	if at == (device.Point{}) {
		return nil
	}
	return &[2]float64{tenth(at.X), tenth(at.Y)}
}

func tenth(value float64) float64 {
	return math.Round(value*10) / 10
}

// observer watches what one action and its retries change: the live frames
// where the graphics have them, and the device's reports.
type observer struct {
	call    *operation
	phone   *device.Phone
	watch   ScreenWatch
	touched time.Time
}

func (o *operation) observe(ctx context.Context, phone *device.Phone) *observer {
	out := &observer{call: o, phone: phone}
	if watcher, ok := o.engine.host.options.Graphics.(ScreenWatcher); ok {
		watch, err := watcher.WatchScreen(ctx)
		if err != nil {
			o.engine.logger.Debug(E.Cause(err, "watch the screen for the effect of ", o.name))
		} else {
			out.watch = watch
		}
	}
	return out
}

func (s *observer) close() {
	if s.watch != nil {
		s.watch.Close()
	}
}

// ready marks the screen as the input goes.
func (s *observer) ready(ctx context.Context) {
	s.touched = time.Now()
	if s.watch != nil {
		if err := s.watch.Mark(ctx); err != nil {
			s.call.engine.logger.Debug(E.Cause(err, "mark the screen before ", s.call.name))
			s.watch.Close()
			s.watch = nil
		}
	}
}

// effect tells what the input marked last changed at target and elsewhere:
// whether the element moved away from the touch as it went, then the frames,
// then what the device reports of the element and the screen. A change that
// went back, as a press highlight does, is no effect.
func (s *observer) effect(ctx context.Context, mark device.Mark, target device.Rect) effect {
	if displaced := mark.Displaced(); displaced != "" {
		return effect{Where: effectMoved, Detail: displaced + ", so the touch most likely reached another element"}
	}
	settle := s.touched.Add(effectWindow)
	transient, unknown, timing := "", "", ""
	if s.watch != nil {
		change, err := s.watch.Change(ctx, ChangeQuery{Target: target, Touched: s.touched, Lead: leadOf(mark), Window: effectWindow, Stall: stallLimit})
		if change.ByArrival {
			timing = ", by when the frames came, as the stream did not yet tell when they were captured"
		}
		switch {
		case err != nil:
			s.call.engine.logger.Debug(E.Cause(err, "watch the screen after ", s.call.name))
		case change.Where == effectTarget:
			return effect{Where: effectTarget, Detail: "the screen changed at it " + msText(change.After) + " after the touch, and stayed changed" + timing}
		case change.Where == effectElsewhere:
			return effect{Where: effectElsewhere, Detail: "the screen changed away from it " + msText(change.After) + " after the touch, and stayed changed" + timing}
		case change.Where == ChangeTransient:
			transient = "the screen changed " + msText(change.After) + " after the touch and went back, as a press highlight does; "
		case change.Where == ChangeUnknown:
			unknown = "the stream sent no frame the device captured " + msText(effectWindow) + " after the touch within " + msText(effectWindow+stallLimit) + "; "
		}
		settle = time.Now().Add(pushGrace)
	}
	reaction, err := s.phone.Reaction(ctx, mark, settle)
	if err != nil && ctx.Err() == nil {
		s.call.engine.logger.Debug(E.Cause(err, "read what ", s.call.name, " changed"))
	}
	switch {
	case reaction.Where != "":
		return effect{Where: reaction.Where, Detail: transient + unknown + reaction.Detail}
	case unknown != "":
		return effect{Where: effectUnknown, Detail: unknown + "nothing tells whether the tap changed the screen"}
	case s.watch == nil:
		return effect{Where: effectNone, Detail: "the device reported no change within " + msText(time.Since(s.touched)) + ", without the live screen to watch"}
	case transient != "":
		return effect{Where: effectNone, Detail: transient + "nothing else changed"}
	}
	return effect{Where: effectNone, Detail: "nothing changed on the screen within " + msText(effectWindow) + timing}
}

// leadOf is how long after a touch the frames the device captured may show it: its trip to the
// device, at most the round of accessibility requests around it, and the render.
func leadOf(mark device.Mark) time.Duration {
	trip := mark.Latency()
	if trip <= 0 {
		trip = defaultTrip
	}
	return min(max(trip, shortestTrip), longestTrip) + renderLead
}

func msText(duration time.Duration) string {
	return strconv.FormatInt(duration.Milliseconds(), 10) + " ms"
}

// pointTarget is the square a point tap's effect shows in first.
func pointTarget(at device.Point) device.Rect {
	return device.Rect{X: at.X - pointReach, Y: at.Y - pointReach, Width: 2 * pointReach, Height: 2 * pointReach}
}

// tapElement taps the element, scrolling it into view first when it is off
// the screen or under the application's bars, and tells what the tap changed.
// When nothing changed and the tap's point was in doubt (Aim.Doubt), it tries
// again, in turn: at a safer point of the element, after scrolling it clear
// of the bars, and with its press action. It never tries again on a clear,
// whole and steady element, whose tap may rightly show nothing or show it
// only after the window, nor on an element whose label makes acting on it
// irreversible or outward, nor when recover is false.
func (o *operation) tapElement(ctx context.Context, phone *device.Phone, ref string, timeout time.Duration, recover bool) (any, error) {
	started := time.Now()
	s := o.observe(ctx, phone)
	defer s.close()
	watched := time.Now()
	result := tapResult{}
	add := func(method string, aim device.Aim, outcome effect) {
		result.Attempts = append(result.Attempts, tapAttempt{Method: method, Point: pointOf(aim.Point), Effect: outcome.Where, Detail: outcome.Detail})
		result.Effect, result.Detail = outcome.Where, outcome.Detail
	}
	var scrolledFirst bool
	placement, placed := phone.Placement(ctx, ref)
	if placed == nil && placement.Scrolls && (!placement.Shown || placement.Covered) {
		scrolled, err := phone.ScrollIntoView(ctx, ref)
		detail := scrolled.Detail
		if err != nil {
			detail = Message(err)
		}
		scrolledFirst = scrolled.Moved
		result.Attempts = append(result.Attempts, tapAttempt{Method: "scroll", Detail: detail})
	}
	if placed == nil && s.watch != nil && !placement.Arrived.IsZero() && time.Since(placement.Arrived) < arrivalSettle {
		if err := s.watch.Settle(ctx, placement.Visible, settleQuiet, arrivalLimit); err != nil && ctx.Err() == nil {
			o.engine.logger.Debug(E.Cause(err, "wait for the screen to settle before ", o.name))
		}
	}
	aiming := time.Now()
	aim, err := phone.TapElement(ctx, ref, device.TapOptions{Timeout: timeout, Ready: s.ready})
	o.record(aim, o.args[1:]...)
	if err != nil {
		return nil, err
	}
	tapped := time.Now()
	doubt := aim.Doubt
	if doubt == "" && scrolledFirst {
		doubt = "it was scrolled into view first"
	}
	result.Element, result.Point = aim.Element, pointOf(aim.Point)
	add("tap", aim, s.effect(ctx, aim.Mark(), aim.Visible))
	o.trace(started, "tap.phases", "watching the screen "+msText(watched.Sub(started))+", placing "+msText(aiming.Sub(watched))+
		", aiming and touching "+msText(tapped.Sub(aiming))+" (the touch "+msText(s.touched.Sub(aiming))+" in), the effect "+msText(time.Since(tapped))+": "+result.Effect)
	switch {
	case result.Effect != effectNone:
		return compact(result), nil
	case !recover:
		return compact(result), nil
	case s.watch == nil:
		result.Detail += "; not tried again, as without the live screen nothing tells a missed tap from one that rightly changed nothing"
		return compact(result), nil
	case aim.Risky != "":
		result.Detail += "; not tried again, as " + strconv.Quote(aim.Risky) + " may act irreversibly or outside the device"
		return compact(result), nil
	case aim.Selected:
		result.Detail += "; not tried again, as it was already selected"
		return compact(result), nil
	case doubt == "":
		result.Detail += "; not tried again, as it was shown whole, clear of bars and covers and steady, and a tap that rightly changes nothing, or shows it later, looks the same"
		return compact(result), nil
	}
	result.Attempts[len(result.Attempts)-1].Detail += "; tried again, as " + doubt
	first := aim.Point
	retried, err := phone.TapElement(ctx, ref, device.TapOptions{Timeout: retryLimit, Retry: &first, Ready: s.ready})
	switch {
	case errors.Is(err, device.ErrSamePoint):
	case err != nil:
		result.Attempts = append(result.Attempts, tapAttempt{Method: "saferTap", Detail: Message(err)})
	default:
		if add("saferTap", retried, s.effect(ctx, retried.Mark(), retried.Visible)); result.Effect != effectNone {
			return result, nil
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	scrolled, err := phone.ScrollIntoView(ctx, ref)
	if err != nil {
		result.Attempts = append(result.Attempts, tapAttempt{Method: "scroll", Detail: Message(err)})
	} else if scrolled.Moved {
		result.Attempts = append(result.Attempts, tapAttempt{Method: "scroll", Detail: scrolled.Detail})
		again, err := phone.TapElement(ctx, ref, device.TapOptions{Timeout: retryLimit, Ready: s.ready})
		if err != nil {
			result.Attempts = append(result.Attempts, tapAttempt{Method: "tap", Detail: Message(err)})
		} else if add("tap", again, s.effect(ctx, again.Mark(), again.Visible)); result.Effect != effectNone {
			return result, nil
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	pressed, err := phone.PerformAction(ctx, ref, "press", s.ready)
	if err != nil {
		result.Attempts = append(result.Attempts, tapAttempt{Method: "press", Detail: Message(err)})
	} else if add("press", pressed, s.effect(ctx, pressed.Mark(), pressed.Visible)); result.Effect != effectNone {
		return result, nil
	}
	tries := 0
	for _, attempt := range result.Attempts {
		if attempt.Effect != "" {
			tries++
		}
	}
	result.Effect = effectNone
	result.Detail = "nothing changed on the screen within " + msText(effectWindow) + " of the tap at " + pointText(aim.Point) +
		" or of " + strconv.Itoa(tries-1) + " more tries; a tap that rightly changes nothing looks the same"
	return result, nil
}

// compact leaves out the attempts of a tap that needed no other.
func compact(result tapResult) tapResult {
	if len(result.Attempts) == 1 {
		result.Attempts = nil
	}
	return result
}

func pointText(at device.Point) string {
	return "[" + strconv.FormatFloat(tenth(at.X), 'f', -1, 64) + ", " + strconv.FormatFloat(tenth(at.Y), 'f', -1, 64) + "]"
}

// tapPoint taps the point, telling what the tap changed when verify.
func (o *operation) tapPoint(ctx context.Context, phone *device.Phone, at device.Point, count int, verify bool) (any, error) {
	if !verify {
		return tapResult{Point: pointOf(at)}, phone.TapPoint(ctx, at, count)
	}
	s := o.observe(ctx, phone)
	defer s.close()
	s.ready(ctx)
	if err := phone.TapPoint(ctx, at, count); err != nil {
		return nil, err
	}
	mark, err := phone.MarkScreen(ctx)
	if err != nil {
		return nil, err
	}
	outcome := s.effect(ctx, mark, pointTarget(at))
	return tapResult{Point: pointOf(at), Effect: outcome.Where, Detail: outcome.Detail}, nil
}

// trace records a step of the operation on the timeline, when someone watches
// it.
func (o *operation) trace(at time.Time, name, detail string) {
	timeline := &o.engine.host.timeline
	if !timeline.active() {
		return
	}
	timeline.record(TimelineEvent{
		Time: at.UnixMilli(), Type: "operation", Name: "trace." + name, Message: detail,
		DurationMs: float64(time.Since(at).Microseconds()) / 1000, Swiplet: o.engine.swiplet.ID,
	})
}
