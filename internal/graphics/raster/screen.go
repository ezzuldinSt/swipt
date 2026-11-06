package raster

import (
	"context"
	"encoding/json"
	"image"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// DefaultDifferenceThreshold is the gray-level difference at which a pixel counts as changed.
const DefaultDifferenceThreshold = 24

// The change tracker always compares at the default threshold, and closes a change once the
// screen has been still this long, in milliseconds (SWChangeTracker.m).
const changeSettle = 300

// Change is a stretch of motion: Before is when the last still frame came, Start the first
// moving one and After the last moving one, all in the tracker's milliseconds. Regions are in
// pixels of the planes, which are Width by Height.
type Change struct {
	Before  float64
	Start   float64
	After   float64
	Regions []Rect
	Width   int
	Height  int
}

// ChangeRecord is a change as a recording's metadata lists it.
type ChangeRecord struct {
	Time   float64 `json:"time"`
	End    float64 `json:"end"`
	Before float64 `json:"before"`
	After  float64 `json:"after"`
	Frames []Rect  `json:"frames"`
	App    *string `json:"app"`
}

// Record scales the regions by factor over scale into points (SWVideoChangeJSON).
func (c Change) Record(factor, scale float64, app *string) ChangeRecord {
	frames := make([]Rect, len(c.Regions))
	for index, region := range c.Regions {
		frames[index] = roundedRect(Rect{region[0] * factor / scale, region[1] * factor / scale, region[2] * factor / scale, region[3] * factor / scale})
	}
	return ChangeRecord{Time: rounded(c.Start), End: rounded(c.After), Before: rounded(c.Before), After: rounded(c.After), Frames: frames, App: app}
}

// ChangeTracker finds the changes in a stream of gray planes, such as quarter-size frames of
// a recording (SWChangeTracker.m).
type ChangeTracker struct {
	changes   []Change
	reference *raster
	last      *raster
	lastTime  float64
	active    bool
	start     float64
	before    float64
	motion    float64
}

func (t *ChangeTracker) moved(a, b *raster) bool {
	if a.width != b.width || a.height != b.height {
		return true
	}
	return changedCount(a, b, DefaultDifferenceThreshold) >= minimumChange(a.width*a.height)
}

// Push adds the plane of a frame at time, in milliseconds.
func (t *ChangeTracker) Push(plane *image.Gray, time float64) {
	current := grayRaster(plane)
	switch {
	case t.reference == nil || t.last == nil:
		t.reference = current
	case t.active:
		if t.moved(t.last, current) {
			t.motion = time
		} else if time-t.motion >= changeSettle {
			t.close()
		}
	case t.moved(t.reference, current):
		t.active = true
		t.start = time
		t.before = t.lastTime
		t.motion = time
	}
	t.last = current
	t.lastTime = time
}

// Tick closes a change that has been still long enough by now, when no frame comes.
func (t *ChangeTracker) Tick(now float64) {
	if t.active && now-t.motion >= changeSettle {
		t.close()
	}
}

// Finish closes the change in progress, if any.
func (t *ChangeTracker) Finish() {
	if t.active {
		t.close()
	}
}

func (t *ChangeTracker) Changes() []Change {
	return t.changes
}

func (t *ChangeTracker) close() {
	if !t.active || t.reference == nil || t.last == nil {
		return
	}
	reference, last := t.reference, t.last
	t.active = false
	t.reference = last
	if reference.width != last.width || reference.height != last.height {
		whole := Rect{0, 0, float64(last.width), float64(last.height)}
		t.changes = append(t.changes, Change{Before: t.before, Start: t.start, After: t.motion, Regions: []Rect{whole}, Width: last.width, Height: last.height})
		return
	}
	mask, changed := changeMask(reference, last, DefaultDifferenceThreshold)
	if changed < minimumChange(last.width*last.height) {
		return
	}
	regions, _ := merged(context.Background(), mask, 2)
	t.changes = append(t.changes, Change{Before: t.before, Start: t.start, After: t.motion, Regions: regions, Width: last.width, Height: last.height})
}

// Feed delivers the screen frames of screen.waitForChange, screen.waitForStable and
// screen.find (SWScreenFeed.m).
type Feed interface {
	// Latest answers the newest frame, waiting for the first if there is none yet.
	Latest(ctx context.Context) (*Image, error)
	// Next answers a frame newer than the last one answered, or nil when none comes by until.
	Next(ctx context.Context, until time.Time) (*Image, error)
}

// Feeder is an Env with a live feed of the screen; Run takes a screenshot for each frame of an
// Env without one.
type Feeder interface {
	Feed() Feed
}

type screenshotFeed struct {
	env Env
}

func (f screenshotFeed) Latest(ctx context.Context) (*Image, error) {
	return screenshot(ctx, f.env)
}

// Next drops a screenshot that lands after until, which a live feed would not have delivered
// in time.
func (f screenshotFeed) Next(ctx context.Context, until time.Time) (*Image, error) {
	if !time.Now().Before(until) {
		return nil, nil
	}
	frame, err := screenshot(ctx, f.env)
	if err != nil || time.Now().After(until) {
		return nil, err
	}
	return frame, nil
}

func feedOf(env Env) Feed {
	if feeder, ok := env.(Feeder); ok {
		if feed := feeder.Feed(); feed != nil {
			return feed
		}
	}
	return screenshotFeed{env: env}
}

func screenshot(ctx context.Context, env Env) (*Image, error) {
	started := nowMilliseconds()
	data, scale, err := env.Screenshot(ctx)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, failure(NameUnavailable, "graphics: the device sent no screenshot")
	}
	if ctx.Err() != nil {
		return nil, aborted()
	}
	return Decode(data, scale, started)
}

// Recognizer is an Env that reads text, for the text targets of screen.find; another engine
// provides it (SWRecognition.m). Without one, a search that reaches a text target fails with
// an UnsupportedError for "text". Its errors reach the caller unchanged.
type Recognizer interface {
	// RecognizeText reads the frame as the text call would with these arguments: a JSON object
	// of level "accurate", correction false, and the region, languages and minimumHeight of the
	// screen.find call when it has them. The region is in the frame's space.
	RecognizeText(ctx context.Context, frame *Image, arguments json.RawMessage) (Text, error)
}

// Text is what a recognizer read, in the space of the frame it read (points of the screen).
type Text interface {
	// Lines are the lines in reading order, as the text call lists them.
	Lines() []TextLine
	// BoundingBox answers the frame of the UTF-16 units [start, end) of a candidate of a line,
	// as the text handle's boundingBox does, or false where that answers null.
	BoundingBox(line, candidate, start, end int) (Rect, bool, error)
}

// TextLine is a line's best candidate: its text, its confidence rounded as the text call
// rounds it, and the line's frame.
type TextLine struct {
	Text       string
	Confidence float64
	Frame      Rect
}

// Hit is what a search found in a frame: Text is the matched text of a text target.
type Hit struct {
	Frame  Rect
	Score  float64
	Text   *string
	Target int
}

type textTarget struct {
	pattern    string
	expression *regexp.Regexp
}

// Search looks for screen.find's targets in successive frames (SWScreenSearch.m). It probes a
// frame only when it differs from the last probed one, and while the screen moves only as often
// as probing allows.
type Search struct {
	targets    []any
	arguments  arguments
	env        Env
	comparison *Comparison
	// The planes of the frame checked last and of the one probed last, and one to fill.
	previous, probed, spare *levels
	probedAt                float64
	hasProbed               bool
	resting                 bool
	frames                  int
	probes                  int
	probing                 float64
	comparing               float64
	last                    float64
}

// NewSearch reads the targets of screen.find's arguments; the image targets take templates in
// order.
func NewSearch(argumentData json.RawMessage, templates []*Image, env Env) (*Search, error) {
	parsed, err := parseArguments(argumentData)
	if err != nil {
		return nil, err
	}
	objects := make([]any, len(templates))
	for index, template := range templates {
		objects[index] = template
	}
	return newSearch(&call{method: "screen.find", arguments: parsed, objects: objects, env: env}, 0)
}

func newSearch(c *call, first int) (*Search, error) {
	var targets []any
	images := first
	for _, entry := range c.arguments.objects("targets") {
		if kind, _ := entry.string("kind"); kind == "text" {
			pattern, ok := entry.string("pattern")
			if !ok {
				return nil, failure(NameType, "graphics: a text target needs a pattern")
			}
			source := pattern
			if flags, _ := entry.string("flags"); strings.Contains(flags, "i") {
				source = "(?i)" + pattern
			}
			expression, err := regexp.Compile(source)
			if err != nil {
				return nil, failureCause(NameError, err, "graphics: compile the pattern ", strconv.Quote(pattern))
			}
			targets = append(targets, &textTarget{pattern: pattern, expression: expression})
			continue
		}
		template, err := c.image(images)
		if err != nil {
			return nil, err
		}
		targets = append(targets, template)
		images++
	}
	if len(targets) == 0 {
		return nil, failure(NameType, "graphics: find needs a target")
	}
	return &Search{targets: targets, arguments: c.arguments, env: c.env, resting: true}, nil
}

// Check probes a frame when it is due and answers what it found, or nil.
func (s *Search) Check(ctx context.Context, frame *Image) (*Hit, error) {
	if s.comparison == nil {
		comparison, err := NewComparison(frame, s.arguments.region(), DefaultDifferenceThreshold)
		if err != nil {
			return nil, err
		}
		s.comparison = comparison
	}
	s.frames++
	compared := nowMilliseconds()
	plane := s.spare
	if plane == nil {
		plane = s.comparison.newPlane()
	}
	if err := s.comparison.fill(ctx, frame, plane); err != nil {
		return nil, err
	}
	fresh := s.probed == nil || s.comparison.changed(s.probed, plane)
	settled := true
	if fresh && s.previous != nil {
		settled = !s.comparison.changed(s.previous, plane)
	}
	s.previous, s.spare = plane, s.previous
	s.comparing += nowMilliseconds() - compared
	if !fresh {
		s.resting = true
		return nil, nil
	}
	now := frame.time
	if math.IsNaN(now) {
		now = frame.timestamp
	}
	if math.IsNaN(now) {
		now = nowMilliseconds()
	}
	due := !s.hasProbed || now-s.probedAt >= 2*s.last
	rested := s.resting
	s.resting = false
	if !settled && !rested && !due {
		return nil, nil
	}
	if s.probed == nil {
		s.probed = s.comparison.newPlane()
	}
	s.probed.pix, s.probed.width, s.probed.height = append(s.probed.pix[:0], plane.pix...), plane.width, plane.height
	s.hasProbed = true
	s.probedAt = now
	started := nowMilliseconds()
	hit, err := s.probe(ctx, frame)
	s.last = nowMilliseconds() - started
	s.probing += s.last
	s.probes++
	return hit, err
}

func (s *Search) probe(ctx context.Context, frame *Image) (*Hit, error) {
	target, origin, err := frame.rasterIn(ctx, s.arguments.region())
	if err != nil {
		return nil, err
	}
	threshold := s.arguments.float("threshold", defaultMatchThreshold)
	binarize, hasBinarize := s.arguments.number("binarize")
	var binaryTarget *raster
	var recognized Text
	for index, entry := range s.targets {
		if ctx.Err() != nil {
			return nil, aborted()
		}
		if text, isText := entry.(*textTarget); isText {
			if recognized == nil {
				recognized, err = s.recognize(ctx, frame)
				if err != nil {
					return nil, err
				}
			}
			if hit := s.matchText(text, recognized, index); hit != nil {
				return hit, nil
			}
			continue
		}
		template := entry.(*Image)
		base := frame.scale() / template.scale()
		factors := []float64{base}
		if scales, ok := s.arguments.numbers("scales"); ok {
			factors = make([]float64, len(scales))
			for position, scale := range scales {
				factors[position] = scale * base
			}
		}
		searched := target
		pattern, err := template.raster(ctx)
		if err != nil {
			return nil, err
		}
		if hasBinarize {
			if binaryTarget == nil {
				binaryTarget = thresholded(target, integerOf(binarize))
			}
			searched = binaryTarget
			pattern = thresholded(pattern, integerOf(binarize))
		}
		matches, err := findTemplate(ctx, searched, pattern, factors, threshold, 1, 0.3)
		if err != nil {
			return nil, err
		}
		if len(matches) > 0 {
			return &Hit{Frame: frame.SpaceRect(matches[0].rect.offset(float64(origin.X), float64(origin.Y))), Score: matches[0].score, Target: index}, nil
		}
	}
	return nil, nil
}

// recognize reads the frame accurately and without language correction, keeping the region,
// languages and minimum height of the search.
func (s *Search) recognize(ctx context.Context, frame *Image) (Text, error) {
	recognizer, ok := s.env.(Recognizer)
	if !ok {
		return nil, &UnsupportedError{Method: "text"}
	}
	values := map[string]json.RawMessage{"level": json.RawMessage(`"accurate"`), "correction": json.RawMessage("false")}
	for _, key := range []string{"region", "languages", "minimumHeight"} {
		if value, present := s.arguments[key]; present {
			values[key] = value
		}
	}
	encoded, _ := json.Marshal(values)
	return recognizer.RecognizeText(ctx, frame, encoded)
}

func utf16Length(text string) int {
	length := 0
	for _, character := range text {
		length += utf16.RuneLen(character)
	}
	return length
}

func (s *Search) matchText(target *textTarget, recognized Text, index int) *Hit {
	for line, entry := range recognized.Lines() {
		found := target.expression.FindStringIndex(entry.Text)
		if found == nil {
			continue
		}
		matched := entry.Text[found[0]:found[1]]
		start := utf16Length(entry.Text[:found[0]])
		box, ok, err := recognized.BoundingBox(line, 0, start, start+utf16Length(matched))
		frame := entry.Frame
		if ok {
			frame = box
		} else if s.env.Tracing() {
			reason := "the range is outside the line"
			if err != nil {
				reason = err.Error()
			}
			s.env.Trace(time.Now(), "screen.find.box", "the whole line stands in for "+matched+": "+reason, 0)
		}
		return &Hit{Frame: frame, Score: entry.Confidence, Text: &matched, Target: index}
	}
	return nil
}

func (s *Search) describeTargets() string {
	described := make([]string, len(s.targets))
	for index, target := range s.targets {
		if text, isText := target.(*textTarget); isText {
			described[index] = "text /" + text.pattern + "/"
		} else {
			template := target.(*Image)
			described[index] = "image " + strconv.Itoa(template.pixelWidth) + "x" + strconv.Itoa(template.pixelHeight)
		}
	}
	return strings.Join(described, ", ")
}

func truncated(value float64) string {
	return strconv.FormatInt(int64(value), 10)
}

// trace records how a search went on the timeline, when someone watches it.
func (s *Search) trace(name string, started float64, hit *Hit, frameTime float64) {
	if !s.env.Tracing() {
		return
	}
	now := nowMilliseconds()
	where := "the screen"
	if region := s.arguments.region(); region != nil {
		r := roundedRect(*region)
		where = "[" + truncated(r[0]) + ", " + truncated(r[1]) + ", " + truncated(r[2]) + ", " + truncated(r[3]) + "]"
	}
	result := "no match"
	if hit != nil {
		result = "matched target " + strconv.Itoa(hit.Target)
	}
	detail := s.describeTargets() + " in " + where + ", " + result + "; compared " + strconv.Itoa(s.frames) + " frames in " + truncated(s.comparing) +
		" ms, probed " + strconv.Itoa(s.probes) + " in " + truncated(s.probing) + " ms, last " + truncated(s.last) + " ms"
	if hit != nil && !math.IsNaN(frameTime) {
		detail += ", the matched frame is " + truncated(now-frameTime) + " ms old"
	}
	s.env.Trace(timeOf(started), name, detail, time.Duration((now-started)*float64(time.Millisecond)))
}

func timeOf(milliseconds float64) time.Time {
	return time.UnixMicro(int64(milliseconds * 1000))
}
