package device

import (
	"context"
	"math"
	"strconv"
	"time"

	"github.com/swipt-automation/swipt/internal/coredevice/accessibility"
)

// Settings loaded its list about 640 ms after a relaunch brought it to the
// front: the first read had General at y = 715 and no VPN row, and 140 ms
// later General had moved to y = 823 and VPN lay at y = 734, so a tap aimed
// from that read opened VPN (iPhone18,2, iOS 27.0.1 24A446). An element of an
// application that came to the front less than arrivalSettle ago, or whose
// frame differs from the read's, is tapped only once two reads of its frame
// steadyGap apart agree, waiting up to steadyLimit for that.
const (
	arrivalSettle = 1500 * time.Millisecond
	steadyGap     = 100 * time.Millisecond
	steadyLimit   = time.Second
	// The reads of the screen again an element that moved since its read
	// may take while the tap waits for it to settle.
	steadyRereads = 2
)

// steadiness answers why the tap must wait for the element to settle, or "".
// It marks the wait as settling, which the tap's time limit does not cut
// short, until steadyLimit; past it, a still moving element waits as any
// other, and one that may still be settling is tapped in doubt.
func (s *axSession) steadiness(target *PhoneNode, frame, previous *accessibility.LayoutFrame, previousAt time.Time) string {
	if frame == nil {
		return ""
	}
	fresh := target.space.rect(*frame)
	moving := previous != nil && !nearFrame(frame, previous)
	shifted := !nearRect(fresh, target.Frame)
	arrived := s.conn.front.arrival(target.model.Root.AX.Ref.PID)
	young := !arrived.IsZero() && time.Since(arrived) < arrivalSettle
	if previous != nil && !moving && time.Since(previousAt) >= steadyGap {
		s.steady = true
	}
	settling := time.Now().Before(s.steadyUntil)
	if moving || (young || shifted) && !s.steady {
		s.moved = true
		switch {
		case settling:
			s.settling = true
			return " is still moving into place"
		case moving:
			return " is moving"
		}
		s.target.Doubt = "it may still have been moving into place"
	}
	if shifted {
		// The point, the visible part and the covers of the read are stale.
		s.moved, s.stale = true, true
		s.rereads++
		s.settling = s.rereads <= steadyRereads
		return " moved since the screen was read, from " + rectText(target.Frame) + " to " + rectText(fresh)
	}
	return ""
}

func rectText(r Rect) string {
	return "[" + trimmed(r.X) + ", " + trimmed(r.Y) + ", " + trimmed(r.Width) + ", " + trimmed(r.Height) + "]"
}

func trimmed(value float64) string {
	return strconv.FormatFloat(math.Round(value*10)/10, 'f', -1, 64)
}

// afterTouch reads the element's frame and the element at the touch right
// after the touch, so a tap whose element moved away from it is told apart
// from one that reached it: only a move across the touch's y counts, as the
// view a tap pushes slides its predecessor sideways.
func (s *axSession) afterTouch(ctx context.Context, target *PhoneNode, at Point) {
	before := s.target.mark.element
	if before == nil || before.Frame == nil {
		return
	}
	batch := &accessibility.Batch{}
	attributes := batch.Attributes(target.AX)
	hit := batch.HitTest(s.geometry.digitizer(at))
	if s.client.Run(ctx, batch) != nil || attributes.Code != 0 || attributes.Value.Frame == nil {
		return
	}
	was, now := target.space.rect(*before.Frame), target.space.rect(*attributes.Value.Frame)
	within := func(r Rect) bool { return at.Y >= r.Y && at.Y <= r.Y+r.Height }
	if !within(was) || within(now) {
		return
	}
	displaced := "it moved from " + rectText(was) + " to " + rectText(now) + " as the touch went at " + pointText(at)
	if hit.Code == 0 && hit.Value.Ref != target.AX.Ref {
		reached := hit.Value
		if node := target.model.Node(reached.Ref); node != nil {
			reached = node.AX
		} else {
			read := &accessibility.Batch{}
			element := read.Attributes(reached)
			if s.client.Run(ctx, read) == nil && element.Code == 0 {
				reached = element.Value
			}
		}
		displaced += "; a hit test there just after answered " + describeElement(reached)
	}
	s.target.mark.displaced = displaced
}

// Displaced tells, when the element moved away from the touch as it went,
// where from and to, and what a hit test at the touch answered; "" otherwise.
func (m Mark) Displaced() string {
	return m.displaced
}
