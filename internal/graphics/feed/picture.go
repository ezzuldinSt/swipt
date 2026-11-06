package feed

import (
	"bytes"
	"image"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/swipt-automation/swipt/internal/graphics/raster"
)

// colorMatrix is the YCbCr to RGB matrix of a stream (MFVideoTransferMatrix, H.273
// MatrixCoefficients).
type colorMatrix int

const (
	matrixBT709 colorMatrix = iota
	matrixBT601
	matrixBT2020
	matrixFCC
	matrixSMPTE240
	// matrixIdentity carries G, B and R in the Y, Cb and Cr planes (GBR).
	matrixIdentity
	matrixYCgCo
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
	// primaries and transfer are the H.273 code points of the stream's colors, 0 when it
	// gives none.
	primaries, transfer uint8
	// time is the sample time the decoder gave the picture, in 100 ns units.
	time int64

	buffer *[]byte
}

// spareBuffers is how many pictures' bytes the pool keeps for the next pictures. The feed holds
// its newest frame, a wait the frame it compares and the one it answers, and the screenshot
// cache the frame before; more spares would only keep memory from the system. A device picture
// is 15 MB (1320x2868 AYUV).
const spareBuffers = 4

// pictureBuffers recycles the bytes of pictures, which come at up to 60 a second. Unlike a
// sync.Pool it keeps them across garbage collections, and at most spareBuffers of them.
var pictureBuffers struct {
	access sync.Mutex
	spare  []*[]byte
}

func takeBuffer(size int) *[]byte {
	pictureBuffers.access.Lock()
	for count := len(pictureBuffers.spare); count > 0; count = len(pictureBuffers.spare) {
		buffer := pictureBuffers.spare[count-1]
		pictureBuffers.spare[count-1] = nil
		pictureBuffers.spare = pictureBuffers.spare[:count-1]
		// A buffer of another size belongs to a stream that is gone.
		if cap(*buffer) >= size && cap(*buffer) <= size+size/4 {
			pictureBuffers.access.Unlock()
			*buffer = (*buffer)[:size]
			return buffer
		}
	}
	pictureBuffers.access.Unlock()
	made := make([]byte, size)
	return &made
}

func giveBuffer(buffer *[]byte) {
	pictureBuffers.access.Lock()
	defer pictureBuffers.access.Unlock()
	if len(pictureBuffers.spare) < spareBuffers {
		pictureBuffers.spare = append(pictureBuffers.spare, buffer)
	}
}

// dropSpareBuffers lets the system have the spare bytes back, as when the stream closes.
func dropSpareBuffers() {
	pictureBuffers.access.Lock()
	defer pictureBuffers.access.Unlock()
	clear(pictureBuffers.spare)
	pictureBuffers.spare = pictureBuffers.spare[:0]
}

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
	buffer := takeBuffer(size)
	p := &picture{layout: layout, width: width, height: height, stride: stride, luma: *buffer, buffer: buffer}
	if layout == layoutNV12 {
		p.luma, p.chroma = (*buffer)[:stride*rows], (*buffer)[stride*rows:]
	}
	return p
}

// recycle returns the bytes to the pool; the picture must not be read afterwards.
func (p *picture) recycle() {
	if p.buffer != nil {
		giveBuffer(p.buffer)
		p.buffer, p.luma, p.chroma = nil, nil, nil
	}
}

// same tells whether two pictures hold the same samples.
func (p *picture) same(other *picture) bool {
	if p.layout != other.layout || p.width != other.width || p.height != other.height || p.stride != other.stride ||
		len(p.luma) != len(other.luma) || len(p.chroma) != len(other.chroma) {
		return false
	}
	if !bytes.Equal(p.chroma, other.chroma) {
		return false
	}
	// Bands compare at once; a change is usually found in the first band that holds it.
	var differ atomic.Bool
	rows := len(p.luma) / max(p.stride, 1)
	parallel(rows, func(start, end int) {
		for y := start; y < end && !differ.Load(); y += 64 {
			stop := min(y+64, end)
			if !bytes.Equal(p.luma[y*p.stride:stop*p.stride], other.luma[y*p.stride:stop*p.stride]) {
				differ.Store(true)
			}
		}
	})
	return !differ.Load() && bytes.Equal(p.luma[rows*p.stride:], other.luma[rows*p.stride:])
}

// coefficients are the matrix in 16.16 fixed point with the range folded in: R = Y·y + Cr·rv,
// G = Y·y − Cb·gu − Cr·gv, B = Y·y + Cb·bu, after removing the luma and chroma offsets. The
// identity and YCgCo matrices do not take that form: general is set and R, G and B are the rows
// of m times Y, Cb and Cr less their offsets.
type coefficients struct {
	lumaOffset     int32
	y              int32
	rv, gu, gv, bu int32

	general bool
	offsets [3]int32
	m       [9]int32
}

func fixed(value float64) int32 {
	if value < 0 {
		return -int32(-value*65536 + 0.5)
	}
	return int32(value*65536 + 0.5)
}

func coefficientsOf(matrix colorMatrix, fullRange bool) coefficients {
	var kr, kb float64
	switch matrix {
	case matrixBT601:
		kr, kb = 0.299, 0.114
	case matrixBT2020:
		kr, kb = 0.2627, 0.0593
	case matrixFCC:
		kr, kb = 0.30, 0.11
	case matrixSMPTE240:
		kr, kb = 0.212, 0.087
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
	switch matrix {
	case matrixIdentity:
		// Every plane is quantized like luma (H.273 8.3).
		s := fixed(lumaScale)
		return coefficients{general: true, offsets: [3]int32{offset, offset, offset}, m: [9]int32{0, 0, s, s, 0, 0, 0, s, 0}}
	case matrixYCgCo:
		// R = Y − Cg + Co, G = Y + Cg, B = Y − Cg − Co, Cg and Co in the Cb and Cr planes.
		y, c := fixed(lumaScale), fixed(chromaScale)
		return coefficients{general: true, offsets: [3]int32{offset, 128, 128}, m: [9]int32{y, -c, c, y, c, 0, y, -c, -c}}
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

// rgb converts Y, Cb and Cr samples, or averages of them, by either form.
func (c *coefficients) rgb(luma, cb, cr int32) (red, green, blue byte) {
	if c.general {
		luma, cb, cr = luma-c.offsets[0], cb-c.offsets[1], cr-c.offsets[2]
		m := &c.m
		return clamp(luma*m[0] + cb*m[1] + cr*m[2] + 1<<15), clamp(luma*m[3] + cb*m[4] + cr*m[5] + 1<<15), clamp(luma*m[6] + cb*m[7] + cr*m[8] + 1<<15)
	}
	scaled := (luma-c.lumaOffset)*c.y + 1<<15
	cb -= 128
	cr -= 128
	return clamp(scaled + cr*c.rv), clamp(scaled - cb*c.gu - cr*c.gv), clamp(scaled + cb*c.bu)
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
	pixel[0], pixel[1], pixel[2] = c.rgb(luma, cb, cr)
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

// rgba converts the top-left width by height of the picture to opaque sRGB turned clockwise by
// quarter turns, as SWFrame.m crops the pixel buffer and rotates the raster. The rows are
// shared among the processors.
func (p *picture) rgba(width, height, turns int) *image.NRGBA {
	width, height = min(width, p.width), min(height, p.height)
	outWidth, outHeight := p.uprightSize(width, height, turns)
	out := newRGBA(outWidth, outHeight)
	p.rgbaInto(out.Pix, image.Rect(0, 0, outWidth, outHeight), width, height, turns)
	return out
}

// uprightSize is the size of the top-left width by height of the picture turned upright.
func (p *picture) uprightSize(width, height, turns int) (int, int) {
	width, height = min(width, p.width), min(height, p.height)
	if turns&1 == 1 {
		return height, width
	}
	return width, height
}

// rgbaInto converts rect of the upright output of the top-left width by height of the picture,
// turned clockwise by quarter turns, into out, rect.Dx()·4 bytes a row, in sRGB.
func (p *picture) rgbaInto(out []byte, rect image.Rectangle, width, height, turns int) {
	p.convertInto(out, rect, min(width, p.width), min(height, p.height), turns)
	if conversion := p.conversionToSRGB(); conversion != nil {
		conversion.ConvertNRGBA(&image.NRGBA{Pix: out, Stride: rect.Dx() * 4, Rect: image.Rect(0, 0, rect.Dx(), rect.Dy())})
	}
}

// turning maps the output of a quarter turn back to the picture: the source pixel of (x, y) in
// the upright output of the top-left width by height is origin + x·across + y·down
// (raster.remapped).
func turning(width, height, turns int) (origin, across, down [2]int) {
	switch turns & 3 {
	case 1:
		return [2]int{0, height - 1}, [2]int{0, -1}, [2]int{1, 0}
	case 2:
		return [2]int{width - 1, height - 1}, [2]int{-1, 0}, [2]int{0, -1}
	case 3:
		return [2]int{width - 1, 0}, [2]int{0, 1}, [2]int{-1, 0}
	}
	return [2]int{0, 0}, [2]int{1, 0}, [2]int{0, 1}
}

// convertInto converts rect of the upright output of the top-left width by height of the
// picture into out, without changing its colors' primaries.
func (p *picture) convertInto(out []byte, rect image.Rectangle, width, height, turns int) {
	turns &= 3
	c := coefficientsOf(p.matrix, p.fullRange)
	origin, across, down := turning(width, height, turns)
	rowBytes := rect.Dx() * 4
	parallel(rect.Dy(), func(start, end int) {
		for y := start; y < end; y++ {
			row := out[y*rowBytes : (y+1)*rowBytes]
			sourceY := rect.Min.Y + y
			switch {
			case turns == 0 && p.layout == layoutAYUV && !c.general:
				at := sourceY*p.stride + rect.Min.X*4
				convertAYUVRow(row, p.luma[at:at+rowBytes], &c)
			case turns == 0 && rect.Min.X == 0 && !c.general:
				p.convertRow(row, sourceY, &c)
			default:
				sx := origin[0] + rect.Min.X*across[0] + sourceY*down[0]
				sy := origin[1] + rect.Min.X*across[1] + sourceY*down[1]
				for x := 0; x < len(row); x += 4 {
					luma, cb, cr := p.sample(sx, sy)
					c.put(row[x:x+4:x+4], luma, cb, cr)
					sx += across[0]
					sy += across[1]
				}
			}
		}
	})
}

// grayInto shrinks rect of the upright output of the top-left width by height of the picture to
// outWidth by outHeight by averaging blocks, in sRGB gray, into out (raster.Source's Gray). An
// upright picture is averaged in Y, Cb and Cr, which is averaging its colors as the conversion is
// linear before it clamps; a turned one is converted first.
func (p *picture) grayInto(out []byte, rect image.Rectangle, width, height, turns, outWidth, outHeight int) {
	if turns&3 != 0 {
		pixels := make([]byte, rect.Dx()*rect.Dy()*4)
		p.rgbaInto(pixels, rect, width, height, turns)
		raster.AveragedGray(&image.NRGBA{Pix: pixels, Stride: rect.Dx() * 4, Rect: image.Rect(0, 0, rect.Dx(), rect.Dy())}, outWidth, outHeight, out)
		return
	}
	c := coefficientsOf(p.matrix, p.fullRange)
	conversion := p.conversionToSRGB()
	rectWidth, rectHeight := rect.Dx(), rect.Dy()
	parallel(outHeight, func(start, end int) {
		sums := make([]int32, outWidth*3)
		colors := make([]byte, outWidth*4)
		for qy := start; qy < end; qy++ {
			clear(sums)
			top, bottom := rect.Min.Y+qy*rectHeight/outHeight, rect.Min.Y+(qy+1)*rectHeight/outHeight
			for y := top; y < bottom; y++ {
				for qx := range outWidth {
					left, right := rect.Min.X+qx*rectWidth/outWidth, rect.Min.X+(qx+1)*rectWidth/outWidth
					var luma, cb, cr int32
					if p.layout == layoutAYUV {
						row := p.luma[y*p.stride+left*4 : y*p.stride+right*4]
						for x := 0; x+3 < len(row); x += 4 {
							cr += int32(row[x])
							cb += int32(row[x+1])
							luma += int32(row[x+2])
						}
					} else {
						for _, value := range p.luma[y*p.stride+left : y*p.stride+right] {
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
			for qx := range outWidth {
				count := int32((bottom - top) * ((qx+1)*rectWidth/outWidth - qx*rectWidth/outWidth))
				pixel := colors[qx*4 : qx*4+4 : qx*4+4]
				if count == 0 {
					pixel[0], pixel[1], pixel[2], pixel[3] = 0, 0, 0, 255
					continue
				}
				pixel[0], pixel[1], pixel[2] = c.rgb((sums[qx*3]+count/2)/count, (sums[qx*3+1]+count/2)/count, (sums[qx*3+2]+count/2)/count)
				pixel[3] = 255
			}
			if conversion != nil {
				conversion.ConvertNRGBA(&image.NRGBA{Pix: colors, Stride: outWidth * 4, Rect: image.Rect(0, 0, outWidth, 1)})
			}
			row := out[qy*outWidth : (qy+1)*outWidth]
			for qx := range outWidth {
				row[qx] = grayOf(int32(colors[qx*4]), int32(colors[qx*4+1]), int32(colors[qx*4+2]))
			}
		}
	})
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
				red, green, blue := c.rgb((sums[qx*3]+count/2)/count, (sums[qx*3+1]+count/2)/count, (sums[qx*3+2]+count/2)/count)
				row[qx] = grayOf(int32(red), int32(green), int32(blue))
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
