package raster

import (
	"context"
	"image"
	"math"
	"slices"
	"sync"
)

// Source makes the pixels of an image as calls read them: a frame of the live screen feed
// keeps its decoded picture and converts only the part a call reads, when it reads it.
// Rectangles are in the source's pixels, upright.
type Source interface {
	// Pixels writes the straight R, G, B, A pixels of rect into out, rect.Dx()·4 bytes a row.
	Pixels(ctx context.Context, rect image.Rectangle, out []byte) error
	// Gray writes rect shrunk to width by height by averaging blocks of it, in gray, into out,
	// width bytes a row: the gray of Pixels averaged the same way, up to rounding.
	Gray(ctx context.Context, rect image.Rectangle, width, height int, out []byte) error
	// Bytes is the memory the source keeps, all of it whatever part an image shows.
	Bytes() int
	// Share answers another hold on the source, which an image cropped from this one keeps.
	Share() Source
	// Release gives up this hold; the source is not read through it afterwards.
	Release()
}

// FromSource is a width by height image of scale, stamped with timestamp in milliseconds since
// the epoch, whose pixels source makes as calls read them. The image holds source until
// Release lets it go; it keeps the pixels it made only while few other such images made theirs
// since (renderedLimit), so a script's frames keep about their pictures' memory each.
func FromSource(width, height int, scale, timestamp float64, source Source) *Image {
	if scale <= 0 {
		scale = 1
	}
	node := newImage(width, height, scaling(1/scale, 1/scale), false, math.NaN(), timestamp)
	node.window, node.windowRect = source, image.Rect(0, 0, width, height)
	return node
}

// renderedLimit is how many images of a source keep the whole pixels they made: a frame's are
// 15 MB at the device's size (1320x2868), and a call that read them all once, such as text
// recognition, rarely reads them again.
const renderedLimit = 2

var rendered struct {
	access sync.Mutex
	images []*Image
}

// remember keeps an image's pixels among the latest ones made, and lets the oldest go.
func remember(m *Image) {
	rendered.access.Lock()
	rendered.images = slices.DeleteFunc(rendered.images, func(it *Image) bool { return it == m })
	rendered.images = append(rendered.images, m)
	var evicted []*Image
	if count := len(rendered.images); count > renderedLimit {
		evicted = slices.Clone(rendered.images[:count-renderedLimit])
		rendered.images = append(rendered.images[:0], rendered.images[count-renderedLimit:]...)
	}
	rendered.access.Unlock()
	// The caller holds m's lock, so the others are let go on their own.
	if len(evicted) > 0 {
		go func() {
			for _, it := range evicted {
				it.access.Lock()
				if it.window != nil {
					it.cached = nil
				}
				it.access.Unlock()
			}
		}()
	}
}

func forget(m *Image) {
	rendered.access.Lock()
	defer rendered.access.Unlock()
	rendered.images = slices.DeleteFunc(rendered.images, func(it *Image) bool { return it == m })
}

// Hold takes another hold on the image, which a Release gives up; it answers the image.
func (m *Image) Hold() *Image {
	m.holds.Add(1)
	return m
}

// Own takes the image for a handle that releases it once the script lets it go: the first
// handle takes over the hold the image was made with, and any other takes one more.
func (m *Image) Own() *Image {
	if !m.owned.CompareAndSwap(false, true) {
		m.Hold()
	}
	return m
}

// Release gives up a hold on the image. Once none is left, it lets go of what it keeps to make
// its pixels, such as a frame's picture, which goes back to the feed's pool; reading it fails
// afterwards. A frame a screen wait passed over, and an image the script let go, are released.
func (m *Image) Release() {
	if m.holds.Add(-1) >= 0 {
		return
	}
	m.access.Lock()
	window, parent := m.window, m.parent
	m.released = true
	m.window, m.parent, m.cached, m.source, m.operation = nil, nil, nil, nil, nil
	m.access.Unlock()
	forget(m)
	if window != nil {
		window.Release()
	}
	if parent != nil {
		parent.Release()
	}
}

func (m *Image) gone() error {
	return failure(NameState, "graphics: the image was released")
}

// windowPixels converts the part of the window under pixels, of the image's pixels.
func (m *Image) windowPixels(ctx context.Context, pixels image.Rectangle) (*raster, error) {
	out := newRaster(pixels.Dx(), pixels.Dy(), false)
	if err := m.window.Pixels(ctx, pixels.Add(m.windowRect.Min), out.pix); err != nil {
		return nil, err
	}
	return out, nil
}

// grayPlane writes the pixels of the image's pixels rectangle shrunk to width by height by
// averaging blocks, in gray, into out, from the window while the image has one.
func (m *Image) grayPlane(ctx context.Context, pixels image.Rectangle, width, height int, out []byte) error {
	m.access.Lock()
	if m.cached == nil && m.window != nil {
		defer m.access.Unlock()
		return m.window.Gray(ctx, pixels.Add(m.windowRect.Min), width, height, out)
	}
	m.access.Unlock()
	r, err := m.raster(ctx)
	if err != nil {
		return err
	}
	averagedGray(r.view(pixels.Min.X, pixels.Min.Y, pixels.Dx(), pixels.Dy()), width, height, out)
	return nil
}

// averagedGray shrinks a raster to width by height by averaging blocks of it, then grays the
// averages, into out; the blocks split the raster at whole pixels.
func averagedGray(r *raster, width, height int, out []byte) {
	parallel(height, r.width*r.height/max(height, 1), func(start, end int) {
		sums := make([]int32, width*3)
		for y := start; y < end; y++ {
			clear(sums)
			top, bottom := y*r.height/height, (y+1)*r.height/height
			for row := top; row < bottom; row++ {
				line := r.row(row)
				for x := range width {
					left, right := x*r.width/width, (x+1)*r.width/width
					if r.gray {
						var sum int32
						for _, value := range line[left:right] {
							sum += int32(value)
						}
						sums[x*3] += sum
						continue
					}
					var red, green, blue int32
					for at := left * 4; at < right*4; at += 4 {
						red += int32(line[at])
						green += int32(line[at+1])
						blue += int32(line[at+2])
					}
					sums[x*3] += red
					sums[x*3+1] += green
					sums[x*3+2] += blue
				}
			}
			target := out[y*width : (y+1)*width]
			for x := range width {
				count := int32((bottom - top) * ((x+1)*r.width/width - x*r.width/width))
				if count == 0 {
					continue
				}
				if r.gray {
					target[x] = byte((sums[x*3] + count/2) / count)
					continue
				}
				red, green, blue := (sums[x*3]+count/2)/count, (sums[x*3+1]+count/2)/count, (sums[x*3+2]+count/2)/count
				target[x] = byte((77*uint32(red) + 150*uint32(green) + 29*uint32(blue)) >> 8)
			}
		}
	})
}

// AveragedGray shrinks pixels to width by height by averaging blocks of them, then grays the
// averages, into out, width bytes a row: the planes the screen waits compare.
func AveragedGray(pixels *image.NRGBA, width, height int, out []byte) {
	averagedGray(rasterOf(pixels), width, height, out)
}

// lentLimit is how many buffers of lent pixels are kept for the next loans.
const lentLimit = 2

var lent struct {
	access  sync.Mutex
	buffers [][]byte
}

func borrowPixels(size int) []byte {
	lent.access.Lock()
	defer lent.access.Unlock()
	for index, buffer := range lent.buffers {
		if cap(buffer) >= size && cap(buffer) <= size+size/4 {
			lent.buffers = slices.Delete(lent.buffers, index, index+1)
			return buffer[:size]
		}
	}
	return make([]byte, size)
}

func returnPixels(buffer []byte) {
	lent.access.Lock()
	defer lent.access.Unlock()
	if len(lent.buffers) >= lentLimit {
		lent.buffers = lent.buffers[1:]
	}
	lent.buffers = append(lent.buffers, buffer)
}

// UsePixels lends the image's pixels, an *image.NRGBA or an *image.Gray, to use, which must not
// keep them: an image of a source that has not made them converts them into a buffer that it
// takes back afterwards, so a call that reads a frame once, such as text recognition, leaves
// nothing of it behind. Other images lend the pixels Pixels answers.
func (m *Image) UsePixels(ctx context.Context, use func(image.Image) error) error {
	m.access.Lock()
	if m.cached != nil || m.window == nil {
		m.access.Unlock()
		pixels, err := m.Pixels(ctx)
		if err != nil {
			return err
		}
		return use(pixels)
	}
	r := &raster{pix: borrowPixels(m.pixelWidth * m.pixelHeight * 4), width: m.pixelWidth, height: m.pixelHeight, stride: m.pixelWidth * 4}
	err := m.window.Pixels(ctx, m.windowRect, r.pix)
	m.access.Unlock()
	if err == nil {
		err = use(r.image())
	}
	returnPixels(r.pix)
	return err
}
