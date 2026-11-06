package raster

import (
	"image"
	"image/color"
	"runtime"
	"sync"
)

// raster is a pixel buffer, either 4 bytes a pixel in R, G, B, A order with straight alpha
// (the ObjC keeps B, G, R, A; SWRasterCodec.m) or 1 byte of gray. A view shares the pixels of
// the raster it was cut from.
type raster struct {
	pix    []byte
	width  int
	height int
	stride int
	gray   bool
}

func newRaster(width, height int, gray bool) *raster {
	r := &raster{width: width, height: height, gray: gray}
	r.stride = width * r.channels()
	r.pix = make([]byte, r.stride*height)
	return r
}

func (r *raster) channels() int {
	if r.gray {
		return 1
	}
	return 4
}

func (r *raster) row(y int) []byte {
	start := y * r.stride
	return r.pix[start : start+r.width*r.channels()]
}

func (r *raster) view(x, y, width, height int) *raster {
	channels := r.channels()
	start := y*r.stride + x*channels
	end := start + (height-1)*r.stride + width*channels
	return &raster{pix: r.pix[start:end:end], width: width, height: height, stride: r.stride, gray: r.gray}
}

// pixel reads red, green, blue and alpha; gray reads as an opaque gray color (SWRaster.m).
func (r *raster) pixel(x, y int) [4]int {
	if r.gray {
		value := int(r.pix[y*r.stride+x])
		return [4]int{value, value, value, 255}
	}
	p := r.pix[y*r.stride+x*4:]
	return [4]int{int(p[0]), int(p[1]), int(p[2]), int(p[3])}
}

func (r *raster) image() image.Image {
	bounds := image.Rect(0, 0, r.width, r.height)
	if r.gray {
		return &image.Gray{Pix: r.pix, Stride: r.stride, Rect: bounds}
	}
	return &image.NRGBA{Pix: r.pix, Stride: r.stride, Rect: bounds}
}

func (r *raster) opaque() bool {
	if r.gray {
		return true
	}
	for y := range r.height {
		row := r.row(y)
		for x := 3; x < len(row); x += 4 {
			if row[x] != 255 {
				return false
			}
		}
	}
	return true
}

// grayRaster wraps an *image.Gray without copying it.
func grayRaster(plane *image.Gray) *raster {
	bounds := plane.Bounds()
	r := &raster{width: bounds.Dx(), height: bounds.Dy(), stride: plane.Stride, gray: true}
	if r.width == 0 || r.height == 0 {
		r.width, r.height = max(r.width, 0), max(r.height, 0)
		return r
	}
	start := plane.PixOffset(bounds.Min.X, bounds.Min.Y)
	r.pix = plane.Pix[start : start+(r.height-1)*r.stride+r.width]
	return r
}

// colorRaster converts any decoded image to straight R, G, B, A bytes. The common decoder
// outputs take a path without per-pixel interface calls.
func colorRaster(source image.Image) *raster {
	bounds := source.Bounds()
	out := newRaster(bounds.Dx(), bounds.Dy(), false)
	width := out.width
	switch src := source.(type) {
	case *image.NRGBA:
		parallel(out.height, width, func(start, end int) {
			for y := start; y < end; y++ {
				offset := src.PixOffset(bounds.Min.X, bounds.Min.Y+y)
				copy(out.row(y), src.Pix[offset:offset+width*4])
			}
		})
	case *image.RGBA:
		parallel(out.height, width, func(start, end int) {
			for y := start; y < end; y++ {
				offset := src.PixOffset(bounds.Min.X, bounds.Min.Y+y)
				unpremultiply(out.row(y), src.Pix[offset:offset+width*4])
			}
		})
	case *image.Gray:
		parallel(out.height, width, func(start, end int) {
			for y := start; y < end; y++ {
				offset := src.PixOffset(bounds.Min.X, bounds.Min.Y+y)
				line := src.Pix[offset : offset+width]
				row := out.row(y)
				for x, value := range line {
					row[x*4], row[x*4+1], row[x*4+2], row[x*4+3] = value, value, value, 255
				}
			}
		})
	case *image.YCbCr:
		parallel(out.height, width, func(start, end int) {
			for y := start; y < end; y++ {
				row := out.row(y)
				for x := range width {
					luma := src.Y[src.YOffset(bounds.Min.X+x, bounds.Min.Y+y)]
					chroma := src.COffset(bounds.Min.X+x, bounds.Min.Y+y)
					red, green, blue := color.YCbCrToRGB(luma, src.Cb[chroma], src.Cr[chroma])
					row[x*4], row[x*4+1], row[x*4+2], row[x*4+3] = red, green, blue, 255
				}
			}
		})
	case *image.Paletted:
		palette := make([][4]byte, 256)
		for index, entry := range src.Palette {
			converted := color.NRGBAModel.Convert(entry).(color.NRGBA)
			palette[index] = [4]byte{converted.R, converted.G, converted.B, converted.A}
		}
		parallel(out.height, width, func(start, end int) {
			for y := start; y < end; y++ {
				offset := src.PixOffset(bounds.Min.X, bounds.Min.Y+y)
				line := src.Pix[offset : offset+width]
				row := out.row(y)
				for x, index := range line {
					copy(row[x*4:x*4+4], palette[index][:])
				}
			}
		})
	default:
		parallel(out.height, width*4, func(start, end int) {
			for y := start; y < end; y++ {
				row := out.row(y)
				for x := range width {
					converted := color.NRGBAModel.Convert(source.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.NRGBA)
					row[x*4], row[x*4+1], row[x*4+2], row[x*4+3] = converted.R, converted.G, converted.B, converted.A
				}
			}
		})
	}
	return out
}

func unpremultiply(dst, src []byte) {
	for x := 0; x+3 < len(src); x += 4 {
		alpha := uint32(src[x+3])
		switch alpha {
		case 255:
			copy(dst[x:x+4], src[x:x+4])
		case 0:
			dst[x], dst[x+1], dst[x+2], dst[x+3] = 0, 0, 0, 0
		default:
			dst[x] = byte((uint32(src[x])*255 + alpha/2) / alpha)
			dst[x+1] = byte((uint32(src[x+1])*255 + alpha/2) / alpha)
			dst[x+2] = byte((uint32(src[x+2])*255 + alpha/2) / alpha)
			dst[x+3] = byte(alpha)
		}
	}
}

// parallel runs work over [0, count) in bands, one per processor, when the job is large
// enough to pay for the goroutines; cost is a rough amount of work per item.
func parallel(count, cost int, work func(start, end int)) {
	workers := min(runtime.GOMAXPROCS(0), count)
	if workers <= 1 || count*max(cost, 1) < 1<<15 {
		if count > 0 {
			work(0, count)
		}
		return
	}
	band := (count + workers - 1) / workers
	var group sync.WaitGroup
	for start := 0; start < count; start += band {
		end := min(start+band, count)
		group.Go(func() {
			work(start, end)
		})
	}
	group.Wait()
}
