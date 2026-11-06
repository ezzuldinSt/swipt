package raster

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"math"
	"slices"
	"sync"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Arguments reads a call's JSON object as SWArguments.m does, for the calls that run outside
// this package: a value of the wrong type counts as absent.
type Arguments struct {
	values arguments
}

func ParseArguments(data json.RawMessage) (Arguments, error) {
	values, err := parseArguments(data)
	return Arguments{values: values}, err
}

func (a Arguments) Has(key string) bool {
	return a.values.has(key)
}

func (a Arguments) Number(key string) (float64, bool) {
	return a.values.number(key)
}

func (a Arguments) Float(key string, fallback float64) float64 {
	return a.values.float(key, fallback)
}

func (a Arguments) Integer(key string, fallback int) int {
	return a.values.integer(key, fallback)
}

func (a Arguments) String(key string) (string, bool) {
	return a.values.string(key)
}

func (a Arguments) Numbers(key string) ([]float64, bool) {
	return a.values.numbers(key)
}

func (a Arguments) Rect(key string) (Rect, bool) {
	return a.values.rect(key)
}

// Array answers the items of an array.
func (a Arguments) Array(key string) ([]json.RawMessage, bool) {
	return a.values.array(key)
}

// Object answers a nested object.
func (a Arguments) Object(key string) (Arguments, bool) {
	raw := a.values[key]
	if len(raw) == 0 {
		return Arguments{}, false
	}
	nested, err := ParseArguments(raw)
	return nested, err == nil
}

// Rounded keeps two decimals (SWRounded).
func Rounded(value float64) float64 {
	return rounded(value)
}

// NowMilliseconds is the time since the epoch in milliseconds.
func NowMilliseconds() float64 {
	return nowMilliseconds()
}

// rasterOf wraps pixels without copying them when they are tightly packed RGBA or gray.
func rasterOf(source image.Image) *raster {
	switch pixels := source.(type) {
	case *image.NRGBA:
		if pixels.Rect.Min == (image.Point{}) && pixels.Stride == pixels.Rect.Dx()*4 {
			return &raster{pix: pixels.Pix, width: pixels.Rect.Dx(), height: pixels.Rect.Dy(), stride: pixels.Stride}
		}
	case *image.Gray:
		return grayRaster(pixels)
	}
	return colorRaster(source)
}

// FromFrame wraps a decoded frame of a recording, in pixels a point of scale, at time into the
// recording and timestamp since the epoch (SWImageNode.m nodeWithFrame:ofVideo:). The image
// keeps the pixels, which must not change afterwards.
func FromFrame(pixels image.Image, scale, time, timestamp float64) *Image {
	r := rasterOf(pixels)
	if scale <= 0 {
		scale = 1
	}
	node := newImage(r.width, r.height, scaling(1/scale, 1/scale), r.gray, time, timestamp)
	node.cached = r
	return node
}

// NewLazyImage is a width by height image of scale whose pixels source makes when a call first
// needs them, resized to that size if they differ (SWImageNode.m nodeWithVideo:time:). The image
// keeps the pixels source answers, which must not change afterwards.
func NewLazyImage(width, height int, scale, time, timestamp float64, source func(ctx context.Context) (image.Image, error)) *Image {
	if scale <= 0 {
		scale = 1
	}
	node := newImage(max(width, 1), max(height, 1), scaling(1/scale, 1/scale), false, time, timestamp)
	node.source = func(ctx context.Context) (*raster, error) {
		pixels, err := source(ctx)
		if err != nil {
			return nil, err
		}
		return rasterOf(pixels), nil
	}
	return node
}

// NewCallSearch reads the targets of a find call whose image targets are its objects from
// first on, so a failure names the call's own arguments (SWScreenSearch.m initWithCall:firstObject:).
func NewCallSearch(method string, argumentData json.RawMessage, objects []any, first int, env Env) (*Search, error) {
	parsed, err := parseArguments(argumentData)
	if err != nil {
		return nil, err
	}
	return newSearch(&call{method: method, arguments: parsed, objects: objects, env: env}, first)
}

// Trace records how the search went on the timeline under name, when someone watches it.
func (s *Search) Trace(name string, started float64, hit *Hit, frameTime float64) {
	s.trace(name, started, hit, frameTime)
}

// Match is what the find calls answer for a hit in a kept image (SWFindCalls.m SWMatchJSON).
func (h *Hit) Match(image int64) ScreenMatch {
	return ScreenMatch{
		Frame: roundedRect(h.Frame), Center: roundedCenter(h.Frame), Score: math.Round(h.Score*1000) / 1000,
		Text: h.Text, Target: h.Target, Image: image,
	}
}

// The grid's label strip above each cell and the gap around cells, in pixels (SWVideoGrid.m).
const (
	gridLabel = 30
	gridGap   = 4
)

var (
	labelFace     font.Face
	labelFaceOnce sync.Once
)

// face is Go Regular at 20 pixels, standing in for the system font CoreText draws.
func face() font.Face {
	labelFaceOnce.Do(func() {
		parsed, err := opentype.Parse(goregular.TTF)
		if err == nil {
			labelFace, _ = opentype.NewFace(parsed, &opentype.FaceOptions{Size: 20, DPI: 72, Hinting: font.HintingFull})
		}
	})
	return labelFace
}

// GridLabel is a time in milliseconds as the grid labels it, m:ss.mmm.
func GridLabel(time float64) string {
	milliseconds := int64(math.Round(time))
	return fmt.Sprintf("%d:%02d.%03d", milliseconds/60000, milliseconds/1000%60, milliseconds%1000)
}

// VideoGrid lays out the frames of a recording at times, sorted, in columns of cellWidth
// pixels on a dark canvas, each under a label of its time (SWVideoGrid.m). Every cell is as
// tall as the first frame scaled to cellWidth, or shorter. The image has a scale of 1.
func VideoGrid(ctx context.Context, times []float64, columns, cellWidth int, frameAt func(ctx context.Context, time float64) (image.Image, error)) (*Image, error) {
	sorted := slices.Clone(times)
	slices.SortStableFunc(sorted, cmp.Compare[float64])
	columns, cellWidth = max(columns, 1), max(cellWidth, 1)
	var cells []*raster
	cellHeight := 0
	for index, time := range sorted {
		if ctx.Err() != nil {
			return nil, aborted()
		}
		pixels, err := frameAt(ctx, time)
		if err != nil {
			return nil, err
		}
		cell := rasterOf(pixels)
		if cell.gray {
			cell = colorRaster(cell.image())
		}
		height := max(int(math.Round(float64(cell.height)*float64(cellWidth)/float64(cell.width))), 1)
		if index == 0 {
			cellHeight = height
		}
		cells = append(cells, resized(cell, cellWidth, min(height, cellHeight)))
	}
	if len(cells) == 0 {
		return nil, failure(NameRange, "graphics: the grid has no frames")
	}
	count := len(cells)
	shown := min(columns, count)
	rows := (count + shown - 1) / shown
	canvas := newRaster(shown*cellWidth+(shown+1)*gridGap, rows*(cellHeight+gridLabel+gridGap)+gridGap, false)
	for index := 0; index < len(canvas.pix); index += 4 {
		canvas.pix[index], canvas.pix[index+1], canvas.pix[index+2], canvas.pix[index+3] = 24, 24, 24, 255
	}
	labels := &font.Drawer{Dst: canvas.image().(*image.NRGBA), Src: image.White, Face: face()}
	for index, cell := range cells {
		x := gridGap + index%shown*(cellWidth+gridGap)
		y := gridGap + index/shown*(cellHeight+gridLabel+gridGap)
		for row := range cell.height {
			copy(canvas.pix[(y+gridLabel+row)*canvas.stride+x*4:], cell.row(row))
		}
		if labels.Face != nil {
			labels.Dot = fixed.P(x+6, y+gridLabel-8)
			labels.DrawString(GridLabel(sorted[index]))
		}
	}
	node := newImage(canvas.width, canvas.height, scaling(1, 1), false, math.NaN(), math.NaN())
	node.cached = canvas
	return node, nil
}
