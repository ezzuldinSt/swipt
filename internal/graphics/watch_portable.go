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
	// counts only below this band. The status bar of an iPhone with the
	// Dynamic Island is 54 pt tall (iPhone18,2, iOS 27.0.1 24A446).
	statusBand = 54.0
	// A change at the target counts this far around it, where a control's
	// pressed look spreads past its frame.
	targetMargin = 8.0
	// A change counts once it still shows this long after it first does: a
	// press highlight, or the bounce of a Home Screen icon tapped on its label,
	// goes back sooner, while a pushed view or a launch stays.
	persistHold = 350 * time.Millisecond
	// A change of at most ignoredSpots spots, each changing no more than an
	// ignoreSize square of points and spanning no more than spotSpan times
	// that, is no effect: the Clock icon's second hand ticked inside the
	// target of a tap on its label, which opened nothing (iPhone18,2, iOS
	// 27.0.1 24A446), as a caret blinks and the time's digits change. These
	// are the values of waitForStable's ignoreSize; changed pixels within
	// spotGap points join one spot.
	ignoreSize   = 12.0
	ignoredSpots = 4
	spotSpan     = 4.0
	spotGap      = 4.0
	// Pixels that changed on their own between the marks, as an animation
	// in the target does, do not count after the next mark; a change between
	// marks over more than this share of the screen was a scroll or a
	// transition rather than such an animation, and is no baseline.
	baselineShare = 0.25
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

// screenWatch compares the frames of a lease with the one marked last,
// leaving out the pixels that changed on their own before the mark.
type screenWatch struct {
	frames    raster.Feed
	release   func()
	reference *raster.Image
	// earlier is the frame before the reference, from the watch's start or
	// the mark before, which the baseline compares the reference with.
	earlier  *raster.Image
	marked   time.Time
	baseline []bool
}

func (w *screenWatch) Mark(ctx context.Context) error {
	frame, err := w.frames.Latest(ctx)
	if err != nil {
		return err
	}
	w.earlier, w.reference, w.marked = w.reference, frame, time.Now()
	return nil
}

func (w *screenWatch) Close() {
	w.release()
}

// screenPlane compares gray planes of the screen below the status band.
type screenPlane struct {
	comparison *raster.Comparison
	area       raster.Rect
	width      int
	height     int
	// points is the size of a plane pixel in points.
	points float64
	cell   int
}

func newScreenPlane(reference *raster.Image) (*screenPlane, error) {
	screen := reference.Frame()
	area := raster.Rect{screen[0], screen[1] + statusBand, screen[2], screen[3] - statusBand}
	comparison, err := raster.NewComparison(reference, &area, raster.DefaultDifferenceThreshold)
	if err != nil {
		return nil, err
	}
	plane := &screenPlane{comparison: comparison, area: area}
	return plane, nil
}

func (p *screenPlane) of(ctx context.Context, frame *raster.Image) (*image.Gray, error) {
	gray, err := p.comparison.Plane(ctx, frame)
	if err != nil {
		return nil, err
	}
	if p.width == 0 {
		p.width, p.height = gray.Rect.Dx(), gray.Rect.Dy()
		p.points = p.area[2] / float64(p.width)
		p.cell = max(int(math.Round(spotGap/p.points)), 1)
	}
	return gray, nil
}

// spot is a patch of changed pixels, in points.
type spot struct {
	frame  device.Rect
	pixels int
}

// changes lists the spots where after differs from before, leaving out the
// pixels skip marks.
func (p *screenPlane) changes(before, after *image.Gray, skip []bool) []spot {
	if before.Rect != after.Rect || before.Rect.Dx() != p.width || before.Rect.Dy() != p.height {
		return []spot{{frame: device.Rect{X: p.area[0], Y: p.area[1], Width: p.area[2], Height: p.area[3]}, pixels: p.width * p.height}}
	}
	columns, rows := (p.width+p.cell-1)/p.cell, (p.height+p.cell-1)/p.cell
	counts := make([]int, columns*rows)
	total := 0
	for y := range p.height {
		first, second := before.Pix[y*before.Stride:], after.Pix[y*after.Stride:]
		for x := range p.width {
			if skip != nil && skip[y*p.width+x] {
				continue
			}
			difference := int(first[x]) - int(second[x])
			if difference >= raster.DefaultDifferenceThreshold || -difference >= raster.DefaultDifferenceThreshold {
				counts[(y/p.cell)*columns+x/p.cell]++
				total++
			}
		}
	}
	// As raster's change counting: fewer changed pixels than this are none.
	if total < max(4, p.width*p.height/5000) {
		return nil
	}
	seen := make([]bool, len(counts))
	var out []spot
	var stack []int
	for start, count := range counts {
		if count == 0 || seen[start] {
			continue
		}
		left, right, top, bottom, pixels := columns, -1, rows, -1, 0
		seen[start] = true
		stack = append(stack[:0], start)
		for len(stack) > 0 {
			at := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			x, y := at%columns, at/columns
			left, right, top, bottom = min(left, x), max(right, x), min(top, y), max(bottom, y)
			pixels += counts[at]
			// Cells up to two apart join, across one without a change.
			for ny := max(y-2, 0); ny <= min(y+2, rows-1); ny++ {
				for nx := max(x-2, 0); nx <= min(x+2, columns-1); nx++ {
					if next := ny*columns + nx; counts[next] != 0 && !seen[next] {
						seen[next] = true
						stack = append(stack, next)
					}
				}
			}
		}
		size := float64(p.cell) * p.points
		out = append(out, spot{pixels: pixels, frame: device.Rect{
			X: p.area[0] + float64(left)*size, Y: p.area[1] + float64(top)*size,
			Width: float64(right-left+1) * size, Height: float64(bottom-top+1) * size,
		}})
	}
	return out
}

// small tells whether a spot is too small to be an effect.
func (p *screenPlane) small(it spot) bool {
	area := float64(it.pixels) * p.points * p.points
	return area <= ignoreSize*ignoreSize && max(it.frame.Width, it.frame.Height) <= spotSpan*ignoreSize+float64(p.cell)*p.points
}

// where tells where spots show an effect: "target" when one that counts
// overlaps target, "elsewhere" when those that count lie away from it, and ""
// when none counts, which a few small spots do not.
func (p *screenPlane) where(spots []spot, target device.Rect) string {
	counted := spots
	if len(spots) <= ignoredSpots {
		counted = nil
		for _, it := range spots {
			if !p.small(it) {
				counted = append(counted, it)
			}
		}
	}
	if len(counted) == 0 {
		return ""
	}
	for _, it := range counted {
		if !it.frame.Intersect(target).Empty() {
			return automationTarget
		}
	}
	return automationElsewhere
}

// baselineOf marks the pixels that changed between the frame before the
// reference and the reference, grown by a cell, unless they cover more than
// baselineShare of the screen.
func (p *screenPlane) baselineOf(before, after *image.Gray) []bool {
	if before.Rect != after.Rect || before.Rect.Dx() != p.width || before.Rect.Dy() != p.height {
		return nil
	}
	out := make([]bool, p.width*p.height)
	changed := 0
	for y := range p.height {
		first, second := before.Pix[y*before.Stride:], after.Pix[y*after.Stride:]
		for x := range p.width {
			difference := int(first[x]) - int(second[x])
			if difference < raster.DefaultDifferenceThreshold && -difference < raster.DefaultDifferenceThreshold {
				continue
			}
			changed++
			for ny := max(y-p.cell, 0); ny <= min(y+p.cell, p.height-1); ny++ {
				for nx := max(x-p.cell, 0); nx <= min(x+p.cell, p.width-1); nx++ {
					out[ny*p.width+nx] = true
				}
			}
		}
	}
	if changed == 0 || float64(changed) > baselineShare*float64(p.width*p.height) {
		return nil
	}
	return out
}

// Change answers where the frames first changed from the mark's, once the
// change has held for persistHold: a press highlight or an icon's bounce that
// goes back to the marked look by then is transient, not an effect. Changes
// of a few small spots, and pixels that changed on their own before the mark,
// do not count.
func (w *screenWatch) Change(ctx context.Context, target device.Rect, until time.Time) (string, time.Duration, error) {
	plane, err := newScreenPlane(w.reference)
	if err != nil {
		return "", 0, err
	}
	before, err := plane.of(ctx, w.reference)
	if err != nil {
		return "", 0, err
	}
	if w.earlier != nil {
		if earlier, err := plane.of(ctx, w.earlier); err == nil {
			if fresh := plane.baselineOf(earlier, before); fresh != nil {
				if len(w.baseline) != len(fresh) {
					w.baseline = fresh
				} else {
					for index, on := range fresh {
						w.baseline[index] = w.baseline[index] || on
					}
				}
			}
		}
	}
	reach := device.Rect{X: target.X - targetMargin, Y: target.Y - targetMargin, Width: target.Width + 2*targetMargin, Height: target.Height + 2*targetMargin}
	classify := func(frame *raster.Image) (string, error) {
		after, err := plane.of(ctx, frame)
		if err != nil {
			return "", err
		}
		return plane.where(plane.changes(before, after, w.baseline), reach), nil
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
