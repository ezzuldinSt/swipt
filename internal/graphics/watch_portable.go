//go:build !darwin || !cgo

package graphics

import (
	"context"
	"image"
	"math"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/automation"
	"github.com/swipt-automation/swipt/internal/device"
	"github.com/swipt-automation/swipt/internal/graphics/raster"
)

const (
	// The status bar's clock and indicators change on their own; a change
	// counts elsewhere only below this band. The status bar of an iPhone with
	// the Dynamic Island is 54 pt tall (iPhone18,2, iOS 27.0.1 24A446).
	statusBand = 54.0
	// A change at the target counts this far around it, where a control's
	// pressed look spreads past its frame.
	targetMargin = 8.0
	// After a change elsewhere, the target has this long to change too: a
	// pushed view and the highlight of the row that pushed it come in the same
	// few frames.
	targetGrace = 250 * time.Millisecond
)

var _ automation.ScreenWatcher = (*Graphics)(nil)

// WatchScreen holds the live feed for an action that tells what it changed,
// waiting for its first frame.
func (g *Graphics) WatchScreen(ctx context.Context) (automation.ScreenWatch, error) {
	if g.feed == nil {
		return nil, E.New("the screen has no live feed here")
	}
	lease, err := g.feed.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	return watchFeed(ctx, lease, lease.Release)
}

func watchFeed(ctx context.Context, frames raster.Feed, release func()) (*screenWatch, error) {
	reference, err := frames.Latest(ctx)
	if err != nil {
		release()
		return nil, err
	}
	return &screenWatch{frames: frames, release: release, reference: reference, marked: time.Now()}, nil
}

// screenWatch compares the frames of a lease with the one marked last.
type screenWatch struct {
	frames    raster.Feed
	release   func()
	reference *raster.Image
	marked    time.Time
}

func (w *screenWatch) Mark(ctx context.Context) error {
	frame, err := w.frames.Latest(ctx)
	if err != nil {
		return err
	}
	w.reference, w.marked = frame, time.Now()
	return nil
}

func (w *screenWatch) Close() {
	w.release()
}

// comparisonOf compares the region of the reference, clipped to it, or answers
// nil when the region lies outside it.
func comparisonOf(ctx context.Context, reference *raster.Image, region raster.Rect) (*raster.Comparison, *image.Gray, error) {
	screen := reference.Frame()
	left, top := math.Max(region[0], screen[0]), math.Max(region[1], screen[1])
	right, bottom := math.Min(region[0]+region[2], screen[0]+screen[2]), math.Min(region[1]+region[3], screen[1]+screen[3])
	if right-left < 1 || bottom-top < 1 {
		return nil, nil, nil
	}
	clipped := raster.Rect{left, top, right - left, bottom - top}
	comparison, err := raster.NewComparison(reference, &clipped, raster.DefaultDifferenceThreshold)
	if err != nil {
		return nil, nil, err
	}
	plane, err := comparison.Plane(ctx, reference)
	if err != nil {
		return nil, nil, err
	}
	return comparison, plane, nil
}

func (w *screenWatch) Change(ctx context.Context, target device.Rect, until time.Time) (string, time.Duration, error) {
	screen := w.reference.Frame()
	near, nearBefore, err := comparisonOf(ctx, w.reference, raster.Rect{target.X - targetMargin, target.Y - targetMargin, target.Width + 2*targetMargin, target.Height + 2*targetMargin})
	if err != nil {
		return "", 0, err
	}
	whole, wholeBefore, err := comparisonOf(ctx, w.reference, raster.Rect{screen[0], screen[1] + statusBand, screen[2], screen[3] - statusBand})
	if err != nil {
		return "", 0, err
	}
	var elsewhere time.Duration
	var deadline time.Time
	for {
		wait := until
		if !deadline.IsZero() && deadline.Before(wait) {
			wait = deadline
		}
		frame, err := w.frames.Next(ctx, wait)
		if err != nil {
			return "", 0, err
		}
		if frame == nil {
			if elsewhere != 0 {
				return automationElsewhere, elsewhere, nil
			}
			return "", 0, nil
		}
		after := w.since(frame)
		if near != nil {
			plane, err := near.Plane(ctx, frame)
			if err != nil {
				return "", 0, err
			}
			if near.Changed(nearBefore, plane) {
				return automationTarget, after, nil
			}
		}
		if elsewhere == 0 && whole != nil {
			plane, err := whole.Plane(ctx, frame)
			if err != nil {
				return "", 0, err
			}
			if whole.Changed(wholeBefore, plane) {
				elsewhere, deadline = max(after, time.Millisecond), time.Now().Add(targetGrace)
				if near == nil {
					return automationElsewhere, elsewhere, nil
				}
			}
		}
	}
}

// since is the time from the mark to the frame's arrival.
func (w *screenWatch) since(frame *raster.Image) time.Duration {
	if arrived := frame.Timestamp(); !math.IsNaN(arrived) {
		return max(time.UnixMicro(int64(arrived*1000)).Sub(w.marked), 0)
	}
	return time.Since(w.marked)
}

const (
	automationTarget    = "target"
	automationElsewhere = "elsewhere"
)
