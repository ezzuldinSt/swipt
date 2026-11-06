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
	// A change counts once it still shows this long after it first does: a
	// press highlight, or the bounce of a Home Screen icon tapped on its label,
	// goes back sooner, while a pushed view or a launch stays.
	persistHold = 350 * time.Millisecond
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

// Change answers where the frames first changed from the mark's, once the
// change has held for persistHold: a press highlight or an icon's bounce that
// goes back to the marked look by then is "transient", not an effect.
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
	classify := func(frame *raster.Image) (string, error) {
		for _, it := range []struct {
			comparison *raster.Comparison
			before     *image.Gray
			where      string
		}{{near, nearBefore, automationTarget}, {whole, wholeBefore, automationElsewhere}} {
			if it.comparison == nil {
				continue
			}
			plane, err := it.comparison.Plane(ctx, frame)
			if err != nil {
				return "", err
			}
			if it.comparison.Changed(it.before, plane) {
				return it.where, nil
			}
		}
		return "", nil
	}
	var first time.Duration
	var judge time.Time
	var last *raster.Image
	for {
		wait := until
		if !judge.IsZero() {
			wait = judge
		}
		frame, err := w.frames.Next(ctx, wait)
		if err != nil {
			return "", 0, err
		}
		if frame != nil {
			last = frame
			if judge.IsZero() {
				where, err := classify(frame)
				if err != nil {
					return "", 0, err
				}
				if where != "" {
					first, judge = max(w.since(frame), time.Millisecond), time.Now().Add(persistHold)
				}
				continue
			}
			if time.Now().Before(judge) {
				continue
			}
		}
		if judge.IsZero() {
			return "", 0, nil
		}
		where, err := classify(last)
		if err != nil {
			return "", 0, err
		}
		if where == "" {
			return automation.ChangeTransient, first, nil
		}
		return where, first, nil
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
