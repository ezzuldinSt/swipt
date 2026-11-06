package raster

import (
	"context"
	"image"
	"math"
	"time"
)

// The screen waits of this engine, which reads the live feed's frames as they come and gray
// planes of them made straight from the decoded pictures.
const (
	// waitForStable's quiet time when the call gives no durationMs, and the frames that must
	// come in it. The device sends some 42 frames a second of a still screen and 60 of a moving
	// one (iPhone18,2, iOS 27.0.1), so 100 ms takes four to six frames; three keep a burst of
	// frames the network held back from passing for a quiet stretch.
	stableQuiet  = 100.0
	stableFrames = 3
	// waitForStable's ignoreSize when the call gives none, in points. A blinking caret (2 by
	// 22 points), the Clock icon's second hand and a digit of the status bar's time change less
	// than 12 by 12 points, counted in the plane's pixels of 1.33 points; a 24-point knob that
	// slides 2 points a frame changes 96 square points a frame, which add up against the first
	// frame of a quiet stretch.
	stableIgnore = 12.0
	// When no frame comes, the screen counts as still once its quiet time and this much more
	// have gone by. The device sends frames of a still screen too, so a pause in them is more
	// likely the network or a decoder waiting for a keyframe, which a second outlasts.
	stableSilence = 1000.0
	// A quiet stretch in which changes too small to count came in three frames running lasts
	// this much longer, so a small thing that moves adds up to a change before the stretch ends:
	// the fixture's 24-point square moving 2 points a frame at 30 fps changes too little against
	// a stretch's first frame in 100 ms, and enough in 300. The last small step of a transition
	// does not lengthen it.
	ignoredQuiet = 200.0
	// After input, waitForStable waits this long for the screen to answer before it takes a
	// screen that did not change as settled: a tap's first changed frame reached the PC some
	// 270 ms after the tap, and a swipe's 335 ms after it began (iPhone18,2, iOS 27.0.1 over
	// Wi-Fi).
	inputResponse = 400 * time.Millisecond
	// A difference of this many gray levels over a tenth of the plane is a fade under way,
	// which waitForStable takes as a change though no pixel reaches the threshold yet.
	faintLevel = 6
	// Changed pixels within this many points join one spot for ignoreSize; at most ignoredSpots
	// spots may be ignored, and each may span spotSpan times ignoreSize.
	spotGap      = 4.0
	ignoredSpots = 4
	spotSpan     = 4.0
)

// InputClock is an Env that knows when the device last took input; waitForStable right after
// input gives the screen time to answer it.
type InputClock interface {
	LastInput() time.Time
}

// RepeatTeller is a Feed that can tell, from the stream's own bytes, that the frame it answered
// last repeats the one it answered before it pixel for pixel; a wait then need not compare it.
type RepeatTeller interface {
	Repeated() bool
}

// Comparison tells whether successive frames changed in a region (SWScreenComparison.m).
// Regions at least 64 pixels on their short side compare at a quarter of their size, by
// averaging blocks.
type Comparison struct {
	region    Rect
	width     int
	height    int
	factor    int
	threshold int
	// points is the size of a plane pixel in points of the screen.
	points float64
	// ignore, in points, makes a change of at most ignoredSpots spots, each changing no more
	// than an ignore by ignore square and spanning spotSpan times that, not count; 0 counts
	// every change.
	ignore float64
	// fades counts a faint difference over a large part of the plane as a change.
	fades        bool
	cells, marks []int32
}

// levels is the gray plane of a frame's region that a Comparison compares.
type levels struct {
	pix           []byte
	width, height int
}

// NewComparison sets up a comparison of region, or the whole image when region is nil, in
// the space of frame.
func NewComparison(frame *Image, region *Rect, threshold int) (*Comparison, error) {
	area := frame.frame()
	if region != nil {
		area = *region
	}
	pixels, err := frame.PixelRect(area)
	if err != nil {
		return nil, err
	}
	factor := 1
	if min(pixels.Dx(), pixels.Dy()) >= 64 {
		factor = 4
	}
	return &Comparison{
		region: area, width: max(pixels.Dx()/factor, 1), height: max(pixels.Dy()/factor, 1), factor: factor, threshold: threshold,
		points: float64(factor) / frame.scale(),
	}, nil
}

func (c *Comparison) newPlane() *levels {
	return &levels{pix: make([]byte, c.width*c.height), width: c.width, height: c.height}
}

// fill makes the plane of frame's region into p, reusing its bytes. A frame of another size
// makes a plane of its size, which compares as changed.
func (c *Comparison) fill(ctx context.Context, frame *Image, p *levels) error {
	pixels, err := frame.PixelRect(c.region)
	if err != nil {
		return err
	}
	width, height := max(pixels.Dx()/c.factor, 1), max(pixels.Dy()/c.factor, 1)
	if cap(p.pix) < width*height {
		p.pix = make([]byte, width*height)
	}
	p.pix, p.width, p.height = p.pix[:width*height], width, height
	return frame.grayPlane(ctx, pixels, width, height, p.pix)
}

// Plane is the gray plane of the region that Changed compares.
func (c *Comparison) Plane(ctx context.Context, frame *Image) (*image.Gray, error) {
	p := c.newPlane()
	if err := c.fill(ctx, frame, p); err != nil {
		return nil, err
	}
	return &image.Gray{Pix: p.pix, Stride: p.width, Rect: image.Rect(0, 0, p.width, p.height)}, nil
}

// Changed tells whether enough pixels differ between two planes of this comparison.
func (c *Comparison) Changed(first, second *image.Gray) bool {
	a, b := grayRaster(first), grayRaster(second)
	return c.changed(&levels{pix: a.pix, width: a.width, height: a.height}, &levels{pix: b.pix, width: b.width, height: b.height})
}

// changed tells whether second differs from first: in enough pixels at the threshold, unless
// they lie in a few spots small enough to ignore, or, when the comparison counts fades, by a
// faint difference over a tenth of the plane.
func (c *Comparison) changed(first, second *levels) bool {
	changed, _ := c.difference(first, second)
	return changed
}

// difference is changed, and tells too whether the pixels differed but only in spots small
// enough to ignore.
func (c *Comparison) difference(first, second *levels) (changed, ignored bool) {
	if first.width != second.width || first.height != second.height {
		return true, false
	}
	count, faint := 0, 0
	threshold := c.threshold
	for index, value := range first.pix {
		difference := int(value) - int(second.pix[index])
		if difference < 0 {
			difference = -difference
		}
		if difference >= threshold {
			count++
		}
		if difference >= faintLevel {
			faint++
		}
	}
	total := first.width * first.height
	if count >= minimumChange(total) {
		if c.ignore <= 0 || !c.small(first, second) {
			return true, false
		}
		ignored = true
	}
	return c.fades && faint*10 >= total, ignored
}

// small tells whether the pixels that differ at the threshold lie in at most ignoredSpots
// spots, each of no more changed pixels than an ignore by ignore square holds and spanning no
// more than spotSpan times ignore; pixels within spotGap points join a spot.
func (c *Comparison) small(first, second *levels) bool {
	cell := max(int(math.Round(spotGap/c.points)), 1)
	columns, rows := (first.width+cell-1)/cell, (first.height+cell-1)/cell
	if cap(c.cells) < columns*rows {
		c.cells, c.marks = make([]int32, columns*rows), make([]int32, columns*rows)
	}
	counts, marks := c.cells[:columns*rows], c.marks[:columns*rows]
	clear(counts)
	clear(marks)
	for y := range first.height {
		row := y * first.width
		for x := range first.width {
			difference := int(first.pix[row+x]) - int(second.pix[row+x])
			if difference >= c.threshold || -difference >= c.threshold {
				counts[(y/cell)*columns+x/cell]++
			}
		}
	}
	area := c.ignore * c.ignore / (c.points * c.points)
	span := spotSpan * c.ignore / (float64(cell) * c.points)
	spots := int32(0)
	var stack []int
	for start, count := range counts {
		if count == 0 || marks[start] != 0 {
			continue
		}
		spots++
		if spots > ignoredSpots {
			return false
		}
		left, right, top, bottom := columns, -1, rows, -1
		changed := 0.0
		marks[start] = spots
		stack = append(stack[:0], start)
		for len(stack) > 0 {
			at := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			x, y := at%columns, at/columns
			left, right, top, bottom = min(left, x), max(right, x), min(top, y), max(bottom, y)
			changed += float64(counts[at])
			if changed > area || float64(right-left) > span || float64(bottom-top) > span {
				return false
			}
			// Neighbors up to two cells off join, across one cell without a change.
			for ny := max(y-2, 0); ny <= min(y+2, rows-1); ny++ {
				for nx := max(x-2, 0); nx <= min(x+2, columns-1); nx++ {
					if next := ny*columns + nx; counts[next] != 0 && marks[next] == 0 {
						marks[next] = spots
						stack = append(stack, next)
					}
				}
			}
		}
	}
	return true
}

func (c *call) comparison(reference *Image, ignore float64) (*Comparison, error) {
	comparison, err := NewComparison(reference, c.arguments.region(), c.threshold())
	if err != nil {
		return nil, err
	}
	comparison.ignore = c.arguments.float("ignoreSize", ignore)
	return comparison, nil
}

// repeated tells whether the feed knows that the frame it answered last repeats the one before.
func repeated(feed Feed) bool {
	teller, ok := feed.(RepeatTeller)
	return ok && teller.Repeated()
}

// callWaitForChange answers the first frame that differs from the given image, or else from
// the first frame (SWScreenCalls.m). It compares a frame as soon as it comes, and only one the
// stream does not tell repeats the last.
func callWaitForChange(c *call) (any, error) {
	feed := feedOf(c.env)
	deadline := deadlineOf(c.arguments.timeout())
	frame, err := feed.Latest(c.ctx)
	if err != nil {
		return nil, err
	}
	reference := frame
	given, isImage := c.object(0).(*Image)
	if isImage && given != nil {
		reference = given
	}
	comparison, err := c.comparison(reference, 0)
	if err != nil {
		frame.Release()
		return nil, err
	}
	before, current := comparison.newPlane(), comparison.newPlane()
	if err = comparison.fill(c.ctx, reference, before); err != nil {
		frame.Release()
		return nil, err
	}
	check := reference != frame
	started := time.Now()
	looked, compared := 0, 0
	for {
		if !check {
			next, err := feed.Next(c.ctx, deadline)
			frame.Release()
			if err != nil {
				return nil, err
			}
			if next == nil {
				return nil, failure(NameTimeout, "the screen did not change within ", truncated(c.arguments.timeout()), " ms")
			}
			frame = next
			looked++
			if repeated(feed) {
				continue
			}
		}
		check = false
		compared++
		if err = comparison.fill(c.ctx, frame, current); err != nil {
			frame.Release()
			return nil, err
		}
		if comparison.changed(before, current) {
			if c.env.Tracing() {
				c.env.Trace(started, "screen.waitForChange", "compared "+truncated(float64(compared))+" of "+truncated(float64(looked))+
					" frames; answered "+truncated(nowMilliseconds()-frameTime(frame))+" ms after the changed frame arrived", time.Since(started))
			}
			return c.keep(frame), nil
		}
	}
}

// frameTime is when a frame arrived, in milliseconds on the clock of nowMilliseconds.
func frameTime(frame *Image) float64 {
	if math.IsNaN(frame.timestamp) {
		return nowMilliseconds()
	}
	return frame.timestamp
}

// untilTime turns a time on the clock of nowMilliseconds into a time.Time.
func untilTime(milliseconds float64) time.Time {
	return time.Now().Add(time.Duration((milliseconds - nowMilliseconds()) * float64(time.Millisecond)))
}

// callWaitForStable answers the latest frame once the screen has not changed for durationMs
// (SWScreenCalls.m). Each frame is compared with the first of the quiet stretch, so a slow fade
// or scroll adds up to a change; a change of a few small spots, ignoreSize points at most, does
// not count, though ones in frames running lengthen the stretch by ignoredQuiet. The stretch needs stableFrames frames,
// or no frame for stableSilence more; right after input it also waits up to inputResponse for
// the screen to answer.
func callWaitForStable(c *call) (any, error) {
	feed := feedOf(c.env)
	deadline := deadlineOf(c.arguments.timeout())
	quiet := c.arguments.float("durationMs", stableQuiet)
	frame, err := feed.Latest(c.ctx)
	if err != nil {
		return nil, err
	}
	comparison, err := c.comparison(frame, stableIgnore)
	if err != nil {
		frame.Release()
		return nil, err
	}
	comparison.fades = true
	anchor, previous, current := comparison.newPlane(), comparison.newPlane(), comparison.newPlane()
	if err = comparison.fill(c.ctx, frame, anchor); err != nil {
		frame.Release()
		return nil, err
	}
	copyLevels(previous, anchor)
	started := time.Now()
	changedAt, last := nowMilliseconds(), frameTime(frame)
	frames, changes, compared, looked := 0, 0, 1, 0
	// needed is the quiet time, which small changes in frame after frame lengthen; moving
	// counts those frames.
	needed, moving := quiet, 0
	var input time.Time
	if clock, ok := c.env.(InputClock); ok {
		input = clock.LastInput()
	}
	answered := input.IsZero() || started.Sub(input) >= inputResponse
	for {
		if !answered && time.Since(input) >= inputResponse {
			answered = true
		}
		if answered && frames >= stableFrames && last-changedAt >= needed {
			c.traceStable(started, frames, changes, compared, looked, last-changedAt)
			return c.keep(frame), nil
		}
		if !time.Now().Before(deadline) {
			frame.Release()
			return nil, failure(NameTimeout, "the screen did not settle within ", truncated(c.arguments.timeout()), " ms")
		}
		wake := untilTime(changedAt + quiet + stableSilence)
		if !answered {
			wake = later(wake, input.Add(inputResponse))
		}
		next, err := feed.Next(c.ctx, earlier(wake, deadline))
		if err != nil {
			frame.Release()
			return nil, err
		}
		if next == nil {
			if answered && nowMilliseconds()-changedAt >= quiet+stableSilence {
				c.traceStable(started, frames, changes, compared, looked, nowMilliseconds()-changedAt)
				return c.keep(frame), nil
			}
			continue
		}
		frame.Release()
		frame = next
		last = frameTime(frame)
		looked++
		if repeated(feed) {
			frames++
			moving = 0
			continue
		}
		compared++
		if err = comparison.fill(c.ctx, frame, current); err != nil {
			frame.Release()
			return nil, err
		}
		changed, ignored := comparison.difference(anchor, current)
		if !changed {
			// A small change that goes on frame after frame may be a small thing moving.
			if ignored && comparison.moved(previous, current) {
				if moving++; moving >= 3 {
					needed = quiet + ignoredQuiet
				}
			} else {
				moving = 0
			}
			previous, current = current, previous
			frames++
			continue
		}
		anchor, current = current, anchor
		copyLevels(previous, anchor)
		changedAt, frames, needed, moving = last, 0, quiet, 0
		changes++
		if !input.IsZero() && last >= float64(input.UnixMicro())/1000 {
			answered = true
		}
	}
}

func (c *call) traceStable(started time.Time, frames, changes, compared, looked int, quiet float64) {
	if !c.env.Tracing() {
		return
	}
	c.env.Trace(started, "screen.waitForStable", truncated(float64(changes))+" changes; compared "+truncated(float64(compared))+" of "+truncated(float64(looked+1))+
		" frames; settled "+truncated(quiet)+" ms after the last change, over "+truncated(float64(frames))+" quiet frames", time.Since(started))
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// moved tells whether enough pixels differ faintly between two frames for something to have
// moved between them.
func (c *Comparison) moved(first, second *levels) bool {
	if first.width != second.width || first.height != second.height {
		return true
	}
	count := 0
	for index, value := range first.pix {
		if difference := int(value) - int(second.pix[index]); difference >= faintLevel || -difference >= faintLevel {
			count++
		}
	}
	return count >= minimumChange(first.width*first.height)
}

func copyLevels(into, from *levels) {
	into.pix, into.width, into.height = append(into.pix[:0], from.pix...), from.width, from.height
}
