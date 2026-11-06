package raster

import (
	"context"
	"image"
	"math"
	"strconv"
	"strings"
	"sync"
)

// Image is the engine side of a RasterImage (SWImageNode.m). Its geometry is known at once;
// its pixels are made when a call first needs them, from a decoder or from the image it was
// derived from, so the handle operations stay cheap on the JavaScript thread.
//
// The transform maps pixels to the image's space: points of the screen it came from, so a
// crop keeps the coordinates of the screen.
type Image struct {
	pixelWidth  int
	pixelHeight int
	transform   transform
	gray        bool
	time        float64
	timestamp   float64

	access    sync.Mutex
	source    func(ctx context.Context) (*raster, error)
	parent    *Image
	operation func(input *raster) *raster
	cached    *raster
}

func newImage(width, height int, t transform, gray bool, time, timestamp float64) *Image {
	return &Image{pixelWidth: width, pixelHeight: height, transform: t, gray: gray, time: time, timestamp: timestamp}
}

// FromImage wraps decoded pixels, a screen frame for example, as an Image of the given scale
// (pixels a point). An *image.Gray stays gray and is used in place; anything else is copied as
// color. timestamp is in milliseconds since the epoch, or NaN.
func FromImage(source image.Image, scale, timestamp float64) *Image {
	var r *raster
	if gray, isGray := source.(*image.Gray); isGray {
		r = grayRaster(gray)
	} else {
		r = colorRaster(source)
	}
	if scale <= 0 {
		scale = 1
	}
	node := newImage(r.width, r.height, scaling(1/scale, 1/scale), r.gray, math.NaN(), timestamp)
	node.cached = r
	return node
}

func (m *Image) derive(operation func(*raster) *raster, width, height int, t transform, gray bool) *Image {
	node := newImage(width, height, t, gray, m.time, m.timestamp)
	node.parent = m
	node.operation = operation
	return node
}

// PixelWidth and PixelHeight are the size of the pixels.
func (m *Image) PixelWidth() int {
	return m.pixelWidth
}

func (m *Image) PixelHeight() int {
	return m.pixelHeight
}

// Gray tells whether the pixels are one gray channel rather than red, green, blue and alpha.
func (m *Image) Gray() bool {
	return m.gray
}

func (m *Image) scale() float64 {
	return m.transform.scale()
}

func (m *Image) frame() Rect {
	return m.transform.rect(Rect{0, 0, float64(m.pixelWidth), float64(m.pixelHeight)})
}

// Width, Height, Scale and Frame are what the handle shows the JavaScript, rounded to two
// decimals (SWHandles.m). Frame is in points of the source screen.
func (m *Image) Width() float64 {
	return rounded(m.frame()[2])
}

func (m *Image) Height() float64 {
	return rounded(m.frame()[3])
}

func (m *Image) Scale() float64 {
	return rounded(m.scale())
}

func (m *Image) Frame() Rect {
	return roundedRect(m.frame())
}

// Time is the time into a recording in milliseconds, NaN for any other image.
func (m *Image) Time() float64 {
	return m.time
}

// Timestamp is when the image was captured in milliseconds since the epoch, or NaN.
func (m *Image) Timestamp() float64 {
	return m.timestamp
}

// EstimatedBytes is the memory the pixels take, which the binding reports to the garbage
// collector.
func (m *Image) EstimatedBytes() int {
	if m.gray {
		return m.pixelWidth * m.pixelHeight
	}
	return m.pixelWidth * m.pixelHeight * 4
}

// ToSpace maps a point in pixels to the image's space; ToPixels maps back.
func (m *Image) ToSpace(p Point) Point {
	return m.transform.point(p)
}

func (m *Image) ToPixels(p Point) Point {
	return m.transform.inverse().point(p)
}

// SpaceRect maps a rectangle in pixels to the bounding box of it in the image's space.
func (m *Image) SpaceRect(r Rect) Rect {
	return m.transform.rect(r)
}

func describeRect(r Rect) string {
	parts := make([]string, 4)
	for index, value := range roundedRect(r) {
		parts[index] = strconv.FormatFloat(value, 'g', 6, 64)
	}
	return strings.Join(parts, ", ")
}

// PixelRect answers the whole pixels a rectangle of the image's space covers, or a
// RangeError when it does not overlap the image.
func (m *Image) PixelRect(r Rect) (image.Rectangle, error) {
	mapped, overlaps := intersect(m.transform.inverse().rect(r), Rect{0, 0, float64(m.pixelWidth), float64(m.pixelHeight)})
	left := math.Floor(mapped[0] + 1e-6)
	top := math.Floor(mapped[1] + 1e-6)
	right := math.Ceil(mapped.maxX() - 1e-6)
	bottom := math.Ceil(mapped.maxY() - 1e-6)
	if !overlaps || right-left < 1 || bottom-top < 1 {
		return image.Rectangle{}, failure(NameRange, "[", describeRect(r), "] does not overlap the image [", describeRect(m.frame()), "]")
	}
	return image.Rect(int(left), int(top), int(right), int(bottom)), nil
}

// Crop cuts a rectangle of the image's space; the result shares the pixels.
func (m *Image) Crop(r Rect) (*Image, error) {
	pixels, err := m.PixelRect(r)
	if err != nil {
		return nil, err
	}
	return m.derive(func(input *raster) *raster {
		return input.view(pixels.Min.X, pixels.Min.Y, pixels.Dx(), pixels.Dy())
	}, pixels.Dx(), pixels.Dy(), translation(float64(pixels.Min.X), float64(pixels.Min.Y)).then(m.transform), m.gray), nil
}

// Resize scales the pixels to width by height; the image keeps its frame.
func (m *Image) Resize(width, height int) *Image {
	width, height = max(width, 1), max(height, 1)
	return m.derive(func(input *raster) *raster {
		return resized(input, width, height)
	}, width, height, scaling(float64(m.pixelWidth)/float64(width), float64(m.pixelHeight)/float64(height)).then(m.transform), m.gray)
}

// Rotate turns the image clockwise; the corners a turn uncovers are transparent, or black
// when the image is gray.
func (m *Image) Rotate(degrees float64) *Image {
	normalized := math.Mod(degrees, 360)
	if normalized < 0 {
		normalized += 360
	}
	if normalized == 0 {
		return m
	}
	angle := normalized * math.Pi / 180
	quarter := math.Mod(normalized, 90) == 0
	turns := int(normalized) / 90
	var width, height int
	switch {
	case quarter && turns%2 == 0:
		width, height = m.pixelWidth, m.pixelHeight
	case quarter:
		width, height = m.pixelHeight, m.pixelWidth
	default:
		w, h := float64(m.pixelWidth), float64(m.pixelHeight)
		width = int(math.Ceil(math.Abs(w*math.Cos(angle)) + math.Abs(h*math.Sin(angle)) - 1e-6))
		height = int(math.Ceil(math.Abs(w*math.Sin(angle)) + math.Abs(h*math.Cos(angle)) - 1e-6))
	}
	toParent := translation(-float64(width)/2, -float64(height)/2).then(rotation(-angle)).then(translation(float64(m.pixelWidth)/2, float64(m.pixelHeight)/2))
	outWidth, outHeight := max(width, 1), max(height, 1)
	return m.derive(func(input *raster) *raster {
		if quarter {
			return rotatedQuarters(input, turns)
		}
		return rotatedDegrees(input, normalized, outWidth, outHeight)
	}, outWidth, outHeight, toParent.then(m.transform), m.gray)
}

func (m *Image) Grayscale() *Image {
	return m.derive(grayscale, m.pixelWidth, m.pixelHeight, m.transform, true)
}

// Threshold makes a black and white image split at level; a negative level picks Otsu's.
func (m *Image) Threshold(level int) *Image {
	return m.derive(func(input *raster) *raster {
		return thresholded(input, level)
	}, m.pixelWidth, m.pixelHeight, m.transform, true)
}

// Mask makes the pixels whose red, green and blue lie within [low, high] white and the rest
// black. The bounds are clamped to bytes.
func (m *Image) Mask(low, high [3]int) *Image {
	var lower, upper [3]byte
	for channel := range 3 {
		lower[channel] = byte(min(max(low[channel], 0), 255))
		upper[channel] = byte(min(max(high[channel], 0), 255))
	}
	return m.derive(func(input *raster) *raster {
		return masked(input, lower, upper)
	}, m.pixelWidth, m.pixelHeight, m.transform, true)
}

func (m *Image) Invert() *Image {
	return m.derive(inverted, m.pixelWidth, m.pixelHeight, m.transform, m.gray)
}

func (m *Image) Dilate(radius int) *Image {
	return m.derive(func(input *raster) *raster {
		return morphed(input, radius, true)
	}, m.pixelWidth, m.pixelHeight, m.transform, m.gray)
}

func (m *Image) Erode(radius int) *Image {
	return m.derive(func(input *raster) *raster {
		return morphed(input, radius, false)
	}, m.pixelWidth, m.pixelHeight, m.transform, m.gray)
}

// ShowBytes encodes the image as PNG, which the handle's showBytes returns as a Uint8Array.
func (m *Image) ShowBytes(ctx context.Context) ([]byte, error) {
	r, err := m.raster(ctx)
	if err != nil {
		return nil, err
	}
	return encode(r, "png", nil, m.scale())
}

// Pixels answers the pixels as an *image.NRGBA or an *image.Gray, shared with the image:
// read them, never write them.
func (m *Image) Pixels(ctx context.Context) (image.Image, error) {
	r, err := m.raster(ctx)
	if err != nil {
		return nil, err
	}
	return r.image(), nil
}

// PixelsIn answers the pixels under a region of the image's space, or all of them when region
// is nil, and where they start.
func (m *Image) PixelsIn(ctx context.Context, region *Rect) (image.Image, image.Point, error) {
	r, origin, err := m.rasterIn(ctx, region)
	if err != nil {
		return nil, image.Point{}, err
	}
	return r.image(), origin, nil
}

// raster renders the pixels once and keeps them; the chain that made them is let go.
func (m *Image) raster(ctx context.Context) (*raster, error) {
	m.access.Lock()
	defer m.access.Unlock()
	if m.cached != nil {
		return m.cached, nil
	}
	r, err := m.renderLocked(ctx)
	if err != nil {
		return nil, err
	}
	m.cached, m.source, m.parent, m.operation = r, nil, nil, nil
	return r, nil
}

// render makes the pixels for a derived image without keeping those of this one, unless it
// decodes them; only the image a call asks for keeps its pixels.
func (m *Image) render(ctx context.Context) (*raster, error) {
	m.access.Lock()
	defer m.access.Unlock()
	if m.cached != nil {
		return m.cached, nil
	}
	return m.renderLocked(ctx)
}

func (m *Image) renderLocked(ctx context.Context) (*raster, error) {
	if m.source != nil {
		r, err := m.source(ctx)
		if err != nil {
			return nil, err
		}
		if r.width != m.pixelWidth || r.height != m.pixelHeight {
			r = resized(r, m.pixelWidth, m.pixelHeight)
		}
		m.cached, m.source = r, nil
		return r, nil
	}
	input, err := m.parent.render(ctx)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, aborted()
	}
	return m.operation(input), nil
}

func (m *Image) rasterIn(ctx context.Context, region *Rect) (*raster, image.Point, error) {
	r, err := m.raster(ctx)
	if err != nil || region == nil {
		return r, image.Point{}, err
	}
	pixels, err := m.PixelRect(*region)
	if err != nil {
		return nil, image.Point{}, err
	}
	return r.view(pixels.Min.X, pixels.Min.Y, pixels.Dx(), pixels.Dy()), pixels.Min, nil
}
