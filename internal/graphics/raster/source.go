package raster

import (
	"context"
	"image"
	"math"
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
	// Share answers another hold on the source, which an image cropped from this one keeps.
	Share() Source
	// Release gives up this hold; the source is not read through it afterwards.
	Release()
}

// FromSource is a width by height image of scale, stamped with timestamp in milliseconds since
// the epoch, whose pixels source makes as calls read them. The image holds source until it
// makes all its pixels or Release lets it go.
func FromSource(width, height int, scale, timestamp float64, source Source) *Image {
	if scale <= 0 {
		scale = 1
	}
	node := newImage(width, height, scaling(1/scale, 1/scale), false, math.NaN(), timestamp)
	node.window, node.windowRect = source, image.Rect(0, 0, width, height)
	return node
}

// Release lets go of what an image holds to make its pixels, for an image no call reads again,
// such as a frame a screen wait looked at and passed over; reading it afterwards fails.
func (m *Image) Release() {
	m.access.Lock()
	defer m.access.Unlock()
	if m.window != nil {
		m.window.Release()
		m.window = nil
	}
	m.released = true
	m.cached, m.source, m.parent, m.operation = nil, nil, nil, nil
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
