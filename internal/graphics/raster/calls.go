// Package raster runs the image calls of the ObjC graphics engine (internal/graphics/*.m) in pure
// Go, with the same arguments, results and errors, where CoreImage, Accelerate and Vision are
// missing.
package raster

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
)

const defaultMatchThreshold = 0.8

// Env is what a call gets from the engine and the Go bridge (SWEngine.m, SWGoBridge.m).
type Env interface {
	// Keep holds a result, an *Image or encoded []byte, until the JavaScript takes it back by
	// the id Keep answers.
	Keep(object any) int64
	// Authorize allows reading or writing a path, as automation.GraphicsCall.Authorize does.
	Authorize(path string, write bool) error
	// Screenshot answers PNG bytes of the screen and its scale, or 0 when unknown.
	Screenshot(ctx context.Context) ([]byte, float64, error)
	// Tracing tells whether someone watches the timeline, which Trace writes to.
	Tracing() bool
	Trace(started time.Time, name, detail string, duration time.Duration)
}

// ImageResult and BytesResult hand back a kept object by id.
type ImageResult struct {
	Image int64 `json:"image"`
}

type BytesResult struct {
	Bytes int64 `json:"bytes"`
}

type TemplateMatch struct {
	Frame  Rect    `json:"frame"`
	Center Point   `json:"center"`
	Score  float64 `json:"score"`
	Scale  float64 `json:"scale"`
}

type ConnectedComponent struct {
	Frame    Rect  `json:"frame"`
	Pixels   int   `json:"pixels"`
	Centroid Point `json:"centroid"`
}

type ImageDifference struct {
	Ratio  float64 `json:"ratio"`
	Frames []Rect  `json:"frames"`
}

type NearestImage struct {
	Index    int     `json:"index"`
	Distance float64 `json:"distance"`
}

type ScreenMatch struct {
	Frame  Rect    `json:"frame"`
	Center Point   `json:"center"`
	Score  float64 `json:"score"`
	Text   *string `json:"text"`
	Target int     `json:"target"`
	Image  int64   `json:"image"`
}

type call struct {
	ctx       context.Context
	method    string
	arguments arguments
	objects   []any
	env       Env
}

var calls = map[string]func(*call) (any, error){
	"screenshot":           callScreenshot,
	"screen.waitForChange": callWaitForChange,
	"screen.waitForStable": callWaitForStable,
	"screen.find":          callScreenFind,
	"decode":               callDecode,
	"encode":               callEncode,
	"save":                 callSave,
	"pixel":                callPixel,
	"stats":                callStats,
	"find":                 callFind,
	"colors":               callColors,
	"components":           callComponents,
	"diff":                 callDiff,
	"distance":             callDistance,
	"nearest":              callNearest,
}

// The calls of SWCallTable that need Vision, AVFoundation or the live screen feed.
var unsupported = []string{"screen.get", "text", "barcodes", "record", "recording.stop", "video.open", "video.grid", "video.diff", "video.find", "video.export"}

// Run executes a call of the ObjC call table (SWCall.m). objects are the call's *Image and
// []byte arguments; the result marshals to the JSON the ObjC answers. A call this engine
// leaves to another fails with an *UnsupportedError.
func Run(ctx context.Context, method string, argumentData json.RawMessage, objects []any, env Env) (any, error) {
	parsed, err := parseArguments(argumentData)
	if err != nil {
		return nil, err
	}
	function, found := calls[method]
	if !found {
		if slices.Contains(unsupported, method) {
			return nil, &UnsupportedError{Method: method}
		}
		return nil, failure(NameType, "graphics: there is no call ", method)
	}
	return function(&call{ctx: ctx, method: method, arguments: parsed, objects: objects, env: env})
}

func (c *call) object(index int) any {
	if index < len(c.objects) {
		return c.objects[index]
	}
	return nil
}

func (c *call) image(index int) (*Image, error) {
	image, isImage := c.object(index).(*Image)
	if !isImage || image == nil {
		return nil, failure(NameType, "graphics: argument ", index+1, " of ", c.method, " is not an Image")
	}
	return image, nil
}

func (c *call) authorized(path string, present bool, write bool) error {
	if !present {
		return failure(NameType, "graphics: the call needs a path")
	}
	return c.env.Authorize(path, write)
}

func (c *call) keep(image *Image) ImageResult {
	return ImageResult{Image: c.env.Keep(image)}
}

func callScreenshot(c *call) (any, error) {
	image, err := screenshot(c.ctx, c.env)
	if err != nil {
		return nil, err
	}
	return c.keep(image), nil
}

var scaleSuffix = regexp.MustCompile(`@(\d+(?:\.\d+)?)x\.[^.]+$`)

func callDecode(c *call) (any, error) {
	scale, hasScale := c.arguments.number("scale")
	path, hasPath := c.arguments.string("path")
	var data []byte
	if bytes, isBytes := c.object(0).([]byte); isBytes {
		data = bytes
	} else if hasPath {
		err := c.authorized(path, true, false)
		if err != nil {
			return nil, err
		}
		if _, err = os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return nil, failure(NameNotFound, "there is no file at ", path)
		}
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, E.Cause(err, "graphics: read ", path)
		}
		if match := scaleSuffix.FindStringSubmatch(filepath.Base(path)); match != nil && !hasScale {
			scale, _ = strconv.ParseFloat(match[1], 64)
			hasScale = true
		}
	} else {
		return nil, failure(NameType, "decode needs bytes or a file")
	}
	if !hasScale {
		scale = 0
	}
	image, err := Decode(data, scale, math.NaN())
	if err != nil {
		return nil, err
	}
	return c.keep(image), nil
}

func (c *call) encodeFirst() ([]byte, error) {
	image, err := c.image(0)
	if err != nil {
		return nil, err
	}
	r, err := image.raster(c.ctx)
	if err != nil {
		return nil, err
	}
	format, hasFormat := c.arguments.string("format")
	if !hasFormat {
		format = "png"
	}
	var quality *float64
	if value, ok := c.arguments.number("quality"); ok {
		quality = &value
	}
	return encode(r, format, quality, image.scale())
}

func callEncode(c *call) (any, error) {
	data, err := c.encodeFirst()
	if err != nil {
		return nil, err
	}
	return BytesResult{Bytes: c.env.Keep(data)}, nil
}

func callSave(c *call) (any, error) {
	data, err := c.encodeFirst()
	if err != nil {
		return nil, err
	}
	path, hasPath := c.arguments.string("path")
	if err = c.authorized(path, hasPath, true); err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, E.Cause(err, "graphics: create the directory of ", path)
	}
	if err = writeAtomically(path, data); err != nil {
		return nil, E.Cause(err, "graphics: write ", path)
	}
	return struct{}{}, nil
}

func writeAtomically(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), path)
	}
	if err != nil {
		os.Remove(file.Name())
	}
	return err
}

func callPixel(c *call) (any, error) {
	image, err := c.image(0)
	if err != nil {
		return nil, err
	}
	point, ok := c.arguments.point("point")
	if !ok {
		return nil, failure(NameType, "pixel needs a point")
	}
	location := image.ToPixels(point)
	x, y := int(math.Floor(location[0])), int(math.Floor(location[1]))
	if location[0] < 0 || location[1] < 0 || x >= image.pixelWidth || y >= image.pixelHeight {
		return nil, failure(NameRange, "the point lies outside the image")
	}
	r, err := image.raster(c.ctx)
	if err != nil {
		return nil, err
	}
	return r.pixel(x, y), nil
}

func callStats(c *call) (any, error) {
	image, err := c.image(0)
	if err != nil {
		return nil, err
	}
	r, _, err := image.rasterIn(c.ctx, c.arguments.region())
	if err != nil {
		return nil, err
	}
	return statistics(r), nil
}

func scaleFactors(arguments arguments, base float64) []float64 {
	scales, ok := arguments.numbers("scales")
	if !ok {
		scales = []float64{1}
	}
	factors := make([]float64, len(scales))
	for index, scale := range scales {
		factors[index] = scale * base
	}
	return factors
}

func callFind(c *call) (any, error) {
	image, err := c.image(0)
	if err != nil {
		return nil, err
	}
	template, err := c.image(1)
	if err != nil {
		return nil, err
	}
	target, origin, err := image.rasterIn(c.ctx, c.arguments.region())
	if err != nil {
		return nil, err
	}
	pattern, err := template.raster(c.ctx)
	if err != nil {
		return nil, err
	}
	base := image.scale() / template.scale()
	matches, err := findTemplate(c.ctx, target, pattern, scaleFactors(c.arguments, base), c.arguments.float("threshold", defaultMatchThreshold),
		c.arguments.integer("limit", 100), c.arguments.float("overlap", 0.3))
	if err != nil {
		return nil, err
	}
	described := make([]TemplateMatch, len(matches))
	for index, m := range matches {
		frame := image.SpaceRect(m.rect.offset(float64(origin.X), float64(origin.Y)))
		described[index] = TemplateMatch{Frame: roundedRect(frame), Center: roundedCenter(frame), Score: math.Round(m.score*1000) / 1000, Scale: rounded(m.scale / base)}
	}
	return described, nil
}

// checkColor tells whether the pixel under a check's point is within its tolerance, 16 unless
// it names one, of its color on red, green and blue.
func checkColor(check arguments, image *Image, r *raster) bool {
	point, hasPoint := check.point("point")
	color, hasColor := check.numbers("color")
	if !hasPoint || !hasColor || len(color) != 3 {
		return false
	}
	location := image.ToPixels(point)
	x, y := int(math.Floor(location[0])), int(math.Floor(location[1]))
	if location[0] < 0 || location[1] < 0 || x >= r.width || y >= r.height {
		return false
	}
	allowed := check.integer("tolerance", 16)
	pixel := r.pixel(x, y)
	for channel := range 3 {
		if abs(pixel[channel]-integerOf(color[channel])) > allowed {
			return false
		}
	}
	return true
}

func callColors(c *call) (any, error) {
	image, err := c.image(0)
	if err != nil {
		return nil, err
	}
	r, err := image.raster(c.ctx)
	if err != nil {
		return nil, err
	}
	checks := c.arguments.objects("checks")
	results := make([]bool, len(checks))
	for index, check := range checks {
		results[index] = checkColor(check, image, r)
	}
	return results, nil
}

func callComponents(c *call) (any, error) {
	image, err := c.image(0)
	if err != nil {
		return nil, err
	}
	r, err := image.raster(c.ctx)
	if err != nil {
		return nil, err
	}
	parts, err := components(c.ctx, binarized(r), c.arguments.integer("minimumPixels", 1))
	if err != nil {
		return nil, err
	}
	described := make([]ConnectedComponent, len(parts))
	for index, part := range parts {
		described[index] = ConnectedComponent{Frame: roundedRect(image.SpaceRect(part.rect())), Pixels: part.count, Centroid: roundedPoint(image.ToSpace(part.centroid()))}
	}
	return described, nil
}

// Difference compares two images at a gray-level threshold: the share of changed pixels and
// the frames of the changed areas, nearby ones merged, in the first image's space. The second
// image is resized to the first.
func Difference(ctx context.Context, first, second *Image, threshold int) (*ImageDifference, error) {
	a, err := first.raster(ctx)
	if err != nil {
		return nil, err
	}
	other, err := second.raster(ctx)
	if err != nil {
		return nil, err
	}
	mask, changed := changeMask(a, resized(other, a.width, a.height), threshold)
	regions, err := merged(ctx, mask, max(2, a.width/200))
	if err != nil {
		return nil, err
	}
	frames := make([]Rect, len(regions))
	for index, region := range regions {
		frames[index] = roundedRect(first.SpaceRect(region))
	}
	return &ImageDifference{Ratio: math.Round(float64(changed)/float64(max(a.width*a.height, 1))*10000) / 10000, Frames: frames}, nil
}

func callDiff(c *call) (any, error) {
	first, err := c.image(0)
	if err != nil {
		return nil, err
	}
	second, err := c.image(1)
	if err != nil {
		return nil, err
	}
	return Difference(c.ctx, first, second, c.arguments.integer("threshold", DefaultDifferenceThreshold))
}

// distanceJSON keeps five decimals, as the ObjC's SWRounded(distance·1000)/1000 does.
func distanceJSON(distance float64) float64 {
	return rounded(distance*1000) / 1000
}

func callDistance(c *call) (any, error) {
	first, err := c.image(0)
	if err != nil {
		return nil, err
	}
	reference, err := printOf(c.ctx, first)
	if err != nil {
		return nil, err
	}
	second, err := c.image(1)
	if err != nil {
		return nil, err
	}
	other, err := printOf(c.ctx, second)
	if err != nil {
		return nil, err
	}
	return distanceJSON(reference.Distance(other)), nil
}

func callNearest(c *call) (any, error) {
	first, err := c.image(0)
	if err != nil {
		return nil, err
	}
	reference, err := printOf(c.ctx, first)
	if err != nil {
		return nil, err
	}
	bestIndex, bestDistance := -1, 0.0
	for index := 1; index < len(c.objects); index++ {
		candidate, err := c.image(index)
		if err != nil {
			return nil, err
		}
		other, err := printOf(c.ctx, candidate)
		if err != nil {
			return nil, err
		}
		if distance := reference.Distance(other); bestIndex < 0 || distance < bestDistance {
			bestIndex, bestDistance = index-1, distance
		}
	}
	if bestIndex < 0 {
		return nil, failure(NameType, "nearest needs candidates")
	}
	return NearestImage{Index: bestIndex, Distance: distanceJSON(bestDistance)}, nil
}

// deadlineOf turns a timeout in milliseconds into a deadline; Infinity never comes.
func deadlineOf(timeout float64) time.Time {
	if math.IsInf(timeout, 1) || timeout > float64(math.MaxInt64/int64(time.Millisecond)) {
		return time.Now().AddDate(100, 0, 0)
	}
	return time.Now().Add(time.Duration(timeout * float64(time.Millisecond)))
}

func (c *call) threshold() int {
	return c.arguments.integer("threshold", DefaultDifferenceThreshold)
}

// callWaitForChange answers the first frame that differs from the given image, or else from
// the first frame (SWScreenCalls.m).
func callWaitForChange(c *call) (any, error) {
	feed := feedOf(c.env)
	deadline := deadlineOf(c.arguments.timeout())
	frame, err := feed.Latest(c.ctx)
	if err != nil {
		return nil, err
	}
	reference := frame
	if given, isImage := c.object(0).(*Image); isImage && given != nil {
		reference = given
	}
	comparison, err := NewComparison(reference, c.arguments.region(), c.threshold())
	if err != nil {
		return nil, err
	}
	before, err := comparison.plane(c.ctx, reference)
	if err != nil {
		return nil, err
	}
	check := c.object(0) != nil
	for {
		if !check {
			frame, err = feed.Next(c.ctx, deadline)
			if err != nil {
				return nil, err
			}
			if frame == nil {
				return nil, failure(NameTimeout, "the screen did not change within ", truncated(c.arguments.timeout()), " ms")
			}
		}
		check = false
		plane, err := comparison.plane(c.ctx, frame)
		if err != nil {
			return nil, err
		}
		if comparison.changed(before, plane) {
			return c.keep(frame), nil
		}
	}
}

// callWaitForStable answers the latest frame once the screen has not changed for durationMs.
func callWaitForStable(c *call) (any, error) {
	feed := feedOf(c.env)
	deadline := deadlineOf(c.arguments.timeout())
	quiet := time.Duration(c.arguments.float("durationMs", 500) * float64(time.Millisecond))
	frame, err := feed.Latest(c.ctx)
	if err != nil {
		return nil, err
	}
	comparison, err := NewComparison(frame, c.arguments.region(), c.threshold())
	if err != nil {
		return nil, err
	}
	last, err := comparison.plane(c.ctx, frame)
	if err != nil {
		return nil, err
	}
	changedAt := time.Now()
	for {
		remaining := quiet - time.Since(changedAt)
		if remaining <= 0 {
			return c.keep(frame), nil
		}
		if !time.Now().Before(deadline) {
			return nil, failure(NameTimeout, "the screen did not settle within ", truncated(c.arguments.timeout()), " ms")
		}
		until := time.Now().Add(remaining)
		if deadline.Before(until) {
			until = deadline
		}
		next, err := feed.Next(c.ctx, until)
		if err != nil {
			return nil, err
		}
		if next == nil {
			continue
		}
		frame = next
		current, err := comparison.plane(c.ctx, frame)
		if err != nil {
			return nil, err
		}
		if comparison.changed(last, current) {
			changedAt = time.Now()
		}
		last = current
	}
}

// callScreenFind looks for the targets in the latest frame and, with a timeout, in the frames
// after it until one matches (SWFindCalls.m).
func callScreenFind(c *call) (any, error) {
	started := nowMilliseconds()
	search, err := newSearch(c, 0)
	if err != nil {
		return nil, err
	}
	timeout := 0.0
	if c.arguments.has("timeoutMs") {
		timeout = c.arguments.timeout()
	}
	deadline := deadlineOf(timeout)
	feed := feedOf(c.env)
	frame, err := feed.Latest(c.ctx)
	if err != nil {
		return nil, err
	}
	for {
		if c.ctx.Err() != nil {
			return nil, aborted()
		}
		hit, err := search.Check(c.ctx, frame)
		if err != nil {
			return nil, err
		}
		if hit != nil {
			search.trace("screen.find", started, hit, frame.timestamp)
			return ScreenMatch{
				Frame: roundedRect(hit.Frame), Center: roundedCenter(hit.Frame), Score: math.Round(hit.Score*1000) / 1000,
				Text: hit.Text, Target: hit.Target, Image: c.env.Keep(frame),
			}, nil
		}
		var next *Image
		if timeout != 0 {
			next, err = feed.Next(c.ctx, deadline)
			if err != nil {
				return nil, err
			}
		}
		if next == nil {
			search.trace("screen.find", started, nil, frame.timestamp)
			return nil, nil
		}
		frame = next
	}
}
