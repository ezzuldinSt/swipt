package feed

import (
	"image"
	"runtime"
	"sync"
)

// colorMatrix is the YCbCr to RGB matrix of a stream (MFVideoTransferMatrix).
type colorMatrix int

const (
	matrixBT709 colorMatrix = iota
	matrixBT601
	matrixBT2020
)

// pixelLayout is how a decoded picture keeps its samples.
type pixelLayout int

const (
	// layoutNV12 is 4:2:0: a luma plane, then a plane of interleaved Cb and Cr at half the size.
	layoutNV12 pixelLayout = iota
	// layoutAYUV is 4:4:4 with V, U, Y and A bytes for each pixel (MFVideoFormat_AYUV), which
	// the device's remote desktop stream decodes to.
	layoutAYUV
)

// picture is a decoded picture copied out of the decoder; every row of its planes is stride
// bytes. An AYUV picture keeps its pixels in luma and has no chroma plane.
type picture struct {
	layout        pixelLayout
	width, height int
	stride        int
	luma          []byte
	chroma        []byte
	fullRange     bool
	matrix        colorMatrix
	// time is the sample time the decoder gave the picture, in 100 ns units.
	time int64

	buffer *[]byte
}

// picturePool recycles the bytes of pictures, which come at up to 60 a second.
var picturePool sync.Pool

// newPicture allocates a width by height picture whose planes the decoder fills; rows is the
// rows of the luma plane. An NV12 stride covers every chroma pair, so it is even.
func newPicture(layout pixelLayout, width, height, stride, rows int) *picture {
	var size int
	if layout == layoutAYUV {
		stride = max(stride, width*4)
		size = stride * rows
	} else {
		stride = max(stride, (width+1)&^1)
		size = stride*rows + stride*((rows+1)/2)
	}
	var buffer *[]byte
	if pooled, ok := picturePool.Get().(*[]byte); ok && cap(*pooled) >= size {
		buffer = pooled
		*buffer = (*buffer)[:size]
	} else {
		made := make([]byte, size)
		buffer = &made
	}
	p := &picture{layout: layout, width: width, height: height, stride: stride, luma: *buffer, buffer: buffer}
	if layout == layoutNV12 {
		p.luma, p.chroma = (*buffer)[:stride*rows], (*buffer)[stride*rows:]
	}
	return p
}

// recycle returns the bytes to the pool; the picture must not be read afterwards.
func (p *picture) recycle() {
	if p.buffer != nil {
		picturePool.Put(p.buffer)
		p.buffer, p.luma, p.chroma = nil, nil, nil
	}
}

// coefficients are the matrix in 16.16 fixed point with the range folded in: R = Y·y + Cr·rv,
// G = Y·y − Cb·gu − Cr·gv, B = Y·y + Cb·bu, after removing the luma and chroma offsets.
type coefficients struct {
	lumaOffset     int32
	y              int32
	rv, gu, gv, bu int32
}

func coefficientsOf(matrix colorMatrix, fullRange bool) coefficients {
	var kr, kb float64
	switch matrix {
	case matrixBT601:
		kr, kb = 0.299, 0.114
	case matrixBT2020:
		kr, kb = 0.2627, 0.0593
	default:
		kr, kb = 0.2126, 0.0722
	}
	kg := 1 - kr - kb
	lumaScale, chromaScale := 1.0, 1.0
	offset := int32(0)
	if !fullRange {
		lumaScale, chromaScale = 255.0/219, 255.0/224
		offset = 16
	}
	fixed := func(value float64) int32 {
		return int32(value*65536 + 0.5)
	}
	return coefficients{
		lumaOffset: offset,
		y:          fixed(lumaScale),
		rv:         fixed(chromaScale * 2 * (1 - kr)),
		gu:         fixed(chromaScale * 2 * (1 - kb) * kb / kg),
		gv:         fixed(chromaScale * 2 * (1 - kr) * kr / kg),
		bu:         fixed(chromaScale * 2 * (1 - kb)),
	}
}

func clamp(value int32) byte {
	value >>= 16
	if value < 0 {
		return 0
	}
	if value > 255 {
		return 255
	}
	return byte(value)
}

// put writes one opaque pixel from Y, Cb and Cr samples.
func (c *coefficients) put(pixel []byte, luma, cb, cr int32) {
	scaled := (luma-c.lumaOffset)*c.y + 1<<15
	cb -= 128
	cr -= 128
	pixel[0] = clamp(scaled + cr*c.rv)
	pixel[1] = clamp(scaled - cb*c.gu - cr*c.gv)
	pixel[2] = clamp(scaled + cb*c.bu)
	pixel[3] = 255
}

// sample reads the Y, Cb and Cr of the pixel at (x, y).
func (p *picture) sample(x, y int) (luma, cb, cr int32) {
	if p.layout == layoutAYUV {
		at := y*p.stride + x*4
		return int32(p.luma[at+2]), int32(p.luma[at+1]), int32(p.luma[at])
	}
	pair := (y/2)*p.stride + x&^1
	return int32(p.luma[y*p.stride+x]), int32(p.chroma[pair]), int32(p.chroma[pair+1])
}

// rgba converts the top-left width by height of the picture to opaque RGBA turned clockwise by
// quarter turns, as SWFrame.m crops the pixel buffer and rotates the raster. The rows are
// shared among the processors.
func (p *picture) rgba(width, height, turns int) *image.NRGBA {
	width, height = min(width, p.width), min(height, p.height)
	turns &= 3
	outWidth, outHeight := width, height
	if turns%2 == 1 {
		outWidth, outHeight = height, width
	}
	out := newRGBA(outWidth, outHeight)
	c := coefficientsOf(p.matrix, p.fullRange)
	// The source pixel of (x, y) in the output is origin + x·across + y·down (raster.remapped).
	var origin, across, down [2]int
	switch turns {
	case 0:
		origin, across, down = [2]int{0, 0}, [2]int{1, 0}, [2]int{0, 1}
	case 1:
		origin, across, down = [2]int{0, height - 1}, [2]int{0, -1}, [2]int{1, 0}
	case 2:
		origin, across, down = [2]int{width - 1, height - 1}, [2]int{-1, 0}, [2]int{0, -1}
	case 3:
		origin, across, down = [2]int{width - 1, 0}, [2]int{0, 1}, [2]int{-1, 0}
	}
	parallel(outHeight, func(start, end int) {
		for y := start; y < end; y++ {
			row := out.Pix[y*out.Stride : y*out.Stride+outWidth*4]
			switch {
			case turns == 0 && p.layout == layoutAYUV:
				convertAYUVRow(row, p.luma[y*p.stride:y*p.stride+outWidth*4], &c)
			case turns == 0:
				p.convertRow(row, y, &c)
			default:
				sx, sy := origin[0]+y*down[0], origin[1]+y*down[1]
				for x := 0; x < len(row); x += 4 {
					luma, cb, cr := p.sample(sx, sy)
					c.put(row[x:x+4:x+4], luma, cb, cr)
					sx += across[0]
					sy += across[1]
				}
			}
		}
	})
	return out
}

// convertAYUVRow converts an upright row of V, U, Y, A pixels into R, G, B, A ones of the same
// length.
func convertAYUVRow(row, source []byte, c *coefficients) {
	offset, scale := c.lumaOffset, c.y
	rv, gu, gv, bu := c.rv, c.gu, c.gv, c.bu
	for x := 0; x+3 < len(row) && x+3 < len(source); x += 4 {
		pixel := source[x : x+4 : x+4]
		cr, cb := int32(pixel[0])-128, int32(pixel[1])-128
		luma := (int32(pixel[2])-offset)*scale + 1<<15
		out := row[x : x+4 : x+4]
		out[0] = clamp(luma + cr*rv)
		out[1] = clamp(luma - cb*gu - cr*gv)
		out[2] = clamp(luma + cb*bu)
		out[3] = 255
	}
}

// convertRow converts an upright NV12 row, two pixels for each chroma pair.
func (p *picture) convertRow(row []byte, y int, c *coefficients) {
	lumaRow := p.luma[y*p.stride:]
	chromaRow := p.chroma[(y/2)*p.stride:]
	width := len(row) / 4
	for x := 0; x < width; x += 2 {
		cb, cr := int32(chromaRow[x])-128, int32(chromaRow[x+1])-128
		red, green, blue := cr*c.rv, -cb*c.gu-cr*c.gv, cb*c.bu
		luma := (int32(lumaRow[x])-c.lumaOffset)*c.y + 1<<15
		at := x * 4
		row[at] = clamp(luma + red)
		row[at+1] = clamp(luma + green)
		row[at+2] = clamp(luma + blue)
		row[at+3] = 255
		if x+1 < width {
			luma = (int32(lumaRow[x+1])-c.lumaOffset)*c.y + 1<<15
			row[at+4] = clamp(luma + red)
			row[at+5] = clamp(luma + green)
			row[at+6] = clamp(luma + blue)
			row[at+7] = 255
		}
	}
}

// rgbaPool keeps the RGBA bytes that raster.FromImage copies from.
var rgbaPool sync.Pool

func newRGBA(width, height int) *image.NRGBA {
	size := width * height * 4
	var pix []byte
	if pooled, ok := rgbaPool.Get().(*[]byte); ok && cap(*pooled) >= size {
		pix = (*pooled)[:size]
	} else {
		pix = make([]byte, size)
	}
	return &image.NRGBA{Pix: pix, Stride: width * 4, Rect: image.Rect(0, 0, width, height)}
}

// recycleRGBA returns the bytes of an image made by newRGBA once nothing reads them.
func recycleRGBA(pixels *image.NRGBA) {
	buffer := pixels.Pix[:0]
	rgbaPool.Put(&buffer)
}

// parallel splits count rows into a band for each processor.
func parallel(count int, work func(start, end int)) {
	workers := min(runtime.GOMAXPROCS(0), count)
	if workers <= 1 {
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

// grayOf weighs red 77, green 150 and blue 29 out of 256, as raster's grayscale and
// vImageMatrixMultiply_ARGB8888ToPlanar8 do.
func grayOf(red, green, blue int32) byte {
	return byte((77*red + 150*green + 29*blue) >> 8)
}

// quarterGray shrinks the top-left width by height of the picture to a quarter of its size by
// averaging blocks, grays it and turns it clockwise by quarter turns (SWFrame.m makeQuarter:,
// which scales with VTPixelTransferSession). Averaging Y, Cb and Cr first is averaging the
// colors, as the conversion is linear before it clamps.
func (p *picture) quarterGray(width, height, turns int) *image.Gray {
	width, height = min(width, p.width), min(height, p.height)
	outWidth, outHeight := max(width/4, 1), max(height/4, 1)
	plane := image.NewGray(image.Rect(0, 0, outWidth, outHeight))
	c := coefficientsOf(p.matrix, p.fullRange)
	parallel(outHeight, func(start, end int) {
		sums := make([]int32, outWidth*3)
		for qy := start; qy < end; qy++ {
			clear(sums)
			top, bottom := qy*height/outHeight, (qy+1)*height/outHeight
			for y := top; y < bottom; y++ {
				for qx := range outWidth {
					left, right := qx*width/outWidth, (qx+1)*width/outWidth
					var luma, cb, cr int32
					if p.layout == layoutAYUV {
						row := p.luma[y*p.stride+left*4 : y*p.stride+right*4]
						for x := 0; x+3 < len(row); x += 4 {
							cr += int32(row[x])
							cb += int32(row[x+1])
							luma += int32(row[x+2])
						}
					} else {
						row := p.luma[y*p.stride+left : y*p.stride+right]
						for _, value := range row {
							luma += int32(value)
						}
						chroma := p.chroma[(y/2)*p.stride:]
						for x := left; x < right; x++ {
							pair := x &^ 1
							cb += int32(chroma[pair])
							cr += int32(chroma[pair+1])
						}
					}
					sums[qx*3] += luma
					sums[qx*3+1] += cb
					sums[qx*3+2] += cr
				}
			}
			row := plane.Pix[qy*plane.Stride:]
			for qx := range outWidth {
				count := int32((bottom - top) * ((qx+1)*width/outWidth - qx*width/outWidth))
				if count == 0 {
					continue
				}
				luma, cb, cr := (sums[qx*3]+count/2)/count, (sums[qx*3+1]+count/2)/count-128, (sums[qx*3+2]+count/2)/count-128
				scaled := (luma-c.lumaOffset)*c.y + 1<<15
				row[qx] = grayOf(int32(clamp(scaled+cr*c.rv)), int32(clamp(scaled-cb*c.gu-cr*c.gv)), int32(clamp(scaled+cb*c.bu)))
			}
		}
	})
	return turnedGray(plane, turns)
}

// quarterOfImage shrinks upright pixels to a quarter of their size by averaging blocks, in gray.
func quarterOfImage(source image.Image) *image.Gray {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	outWidth, outHeight := max(width/4, 1), max(height/4, 1)
	plane := image.NewGray(image.Rect(0, 0, outWidth, outHeight))
	pixels, isColor := source.(*image.NRGBA)
	gray, isGray := source.(*image.Gray)
	parallel(outHeight, func(start, end int) {
		for qy := start; qy < end; qy++ {
			top, bottom := qy*height/outHeight, (qy+1)*height/outHeight
			for qx := range outWidth {
				left, right := qx*width/outWidth, (qx+1)*width/outWidth
				var red, green, blue, count int32
				for y := top; y < bottom; y++ {
					for x := left; x < right; x++ {
						switch {
						case isColor:
							at := pixels.PixOffset(bounds.Min.X+x, bounds.Min.Y+y)
							red += int32(pixels.Pix[at])
							green += int32(pixels.Pix[at+1])
							blue += int32(pixels.Pix[at+2])
						case isGray:
							value := int32(gray.Pix[gray.PixOffset(bounds.Min.X+x, bounds.Min.Y+y)])
							red, green, blue = red+value, green+value, blue+value
						default:
							r, g, b, _ := source.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
							red, green, blue = red+int32(r>>8), green+int32(g>>8), blue+int32(b>>8)
						}
						count++
					}
				}
				if count > 0 {
					plane.Pix[qy*plane.Stride+qx] = grayOf((red+count/2)/count, (green+count/2)/count, (blue+count/2)/count)
				}
			}
		}
	})
	return plane
}

// turnedGray turns a gray plane clockwise by quarter turns.
func turnedGray(plane *image.Gray, turns int) *image.Gray {
	turns &= 3
	if turns == 0 {
		return plane
	}
	width, height := plane.Rect.Dx(), plane.Rect.Dy()
	outWidth, outHeight := width, height
	if turns%2 == 1 {
		outWidth, outHeight = height, width
	}
	out := image.NewGray(image.Rect(0, 0, outWidth, outHeight))
	for y := range outHeight {
		for x := range outWidth {
			var sx, sy int
			switch turns {
			case 1:
				sx, sy = y, height-1-x
			case 2:
				sx, sy = width-1-x, height-1-y
			case 3:
				sx, sy = width-1-y, x
			}
			out.Pix[y*out.Stride+x] = plane.Pix[sy*plane.Stride+sx]
		}
	}
	return out
}
