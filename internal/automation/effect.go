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
	// Mark takes the newest frame as the one later frames are compared with.
	Mark(ctx context.Context) error
	// Change waits for a frame after the mark that differs from it, and
	// answers where the change shows once it has held a moment: "target"
	// when the frames still differ at target, in points, "elsewhere" when
	// they differ only below the status bar elsewhere, and ChangeTransient
	// when they went back to the mark's look. It answers "" when no frame by
	// until differs. after is the time from the mark to the first changed
	// frame.
	Change(ctx context.Context, target device.Rect, until time.Time) (where string, after time.Duration, err error)
	Close()
}

// ChangeTransient is what ScreenWatch.Change answers for a change that went
// back, as a press highlight does.
const ChangeTransient = "transient"

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
	// A point tap's target is this square around the point, the size of the
	// smallest control Apple's guidelines allow, on each side of it.
	pointReach = 44.0
)

const (
	effectTarget    = "target"
	effectElsewhere = "elsewhere"
	effectNone      = "none"
	effectMoved     = "moved"
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
	until := s.touched.Add(effectWindow)
	settle := until
	transient := ""
	if s.watch != nil {
		where, after, err := s.watch.Change(ctx, target, until)
		switch {
		case err != nil:
			s.call.engine.logger.Debug(E.Cause(err, "watch the screen after ", s.call.name))
		case where == effectTarget:
			return effect{Where: effectTarget, Detail: "the screen changed at it " + msText(after) + " after the touch, and stayed changed"}
		case where == effectElsewhere:
			return effect{Where: effectElsewhere, Detail: "the screen changed away from it " + msText(after) + " after the touch, and stayed changed"}
		case where == ChangeTransient:
			transient = "the screen changed " + msText(after) + " after the touch and went back, as a press highlight does; "
		}
		settle = time.Now().Add(pushGrace)
	}
	reaction, err := s.phone.Reaction(ctx, mark, settle)
	if err != nil && ctx.Err() == nil {
		s.call.engine.logger.Debug(E.Cause(err, "read what ", s.call.name, " changed"))
	}
	if reaction.Where != "" {
		return effect{Where: reaction.Where, Detail: transient + reaction.Detail}
	}
	if s.watch == nil {
		return effect{Where: effectNone, Detail: "the device reported no change within " + msText(time.Since(s.touched)) + ", without the live screen to watch"}
	}
	if transient != "" {
		return effect{Where: effectNone, Detail: transient + "nothing else changed"}
	}
	return effect{Where: effectNone, Detail: "nothing changed on the screen within " + msText(effectWindow)}
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
	s := o.observe(ctx, phone)
	defer s.close()
	result := tapResult{}
	add := func(method string, aim device.Aim, outcome effect) {
		result.Attempts = append(result.Attempts, tapAttempt{Method: method, Point: pointOf(aim.Point), Effect: outcome.Where, Detail: outcome.Detail})
		result.Effect, result.Detail = outcome.Where, outcome.Detail
	}
	var scrolledFirst bool
	if placement, err := phone.Placement(ctx, ref); err == nil && placement.Scrolls && (!placement.Shown || placement.Covered) {
		scrolled, err := phone.ScrollIntoView(ctx, ref)
		detail := scrolled.Detail
		if err != nil {
			detail = Message(err)
		}
		scrolledFirst = scrolled.Moved
		result.Attempts = append(result.Attempts, tapAttempt{Method: "scroll", Detail: detail})
	}
	aim, err := phone.TapElement(ctx, ref, device.TapOptions{Timeout: timeout, Ready: s.ready})
	o.record(aim, o.args[1:]...)
	if err != nil {
		return nil, err
	}
	doubt := aim.Doubt
	if doubt == "" && scrolledFirst {
		doubt = "it was scrolled into view first"
	}
	result.Element, result.Point = aim.Element, pointOf(aim.Point)
	add("tap", aim, s.effect(ctx, aim.Mark(), aim.Visible))
	switch {
	case result.Effect != effectNone:
		return compact(result), nil
	case !recover:
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
