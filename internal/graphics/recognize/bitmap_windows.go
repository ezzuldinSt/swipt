//go:build windows

package recognize

import (
	"image"
	"image/color"
	"runtime"
	"sync"
	"time"
	"unsafe"

	E "github.com/sagernet/sing/common/exceptions"

	"golang.org/x/image/draw"
)

const bitmapBufferAccessWrite = 2

// bitmapIdle is how long the bitmap of the last reading waits for the next reading of its size.
const bitmapIdle = 5 * time.Second

// bitmaps keeps the SoftwareBitmap of the last reading for the next: a script reads the screen
// again and again at its size, and a bitmap of the device's screen is 17 MB that Windows would
// otherwise commit and fault in for every reading. It goes once no reading took it for
// bitmapIdle.
var bitmaps struct {
	access        sync.Mutex
	spare         *object
	width, height int
	expiry        *time.Timer
}

// takeBitmap answers a premultiplied Bgra8 SoftwareBitmap of width x height, the spare one when
// it has that size. The caller is in the Windows Runtime's multithreaded apartment.
func takeBitmap(width, height int) (*object, error) {
	bitmaps.access.Lock()
	if spare := bitmaps.spare; spare != nil && bitmaps.width == width && bitmaps.height == height {
		bitmaps.spare = nil
		bitmaps.access.Unlock()
		return spare, nil
	}
	bitmaps.access.Unlock()
	factory, err := activate("Windows.Graphics.Imaging.SoftwareBitmap", &iidSoftwareBitmapFactory)
	if err != nil {
		return nil, err
	}
	defer factory.release()
	var bitmap *object
	// ISoftwareBitmapFactory::CreateWithAlpha
	err = factory.call(7, bitmapPixelFormatBgra8, uintptr(width), uintptr(height), bitmapAlphaModePremultiplied, uintptr(unsafe.Pointer(&bitmap)))
	return bitmap, err
}

// giveBitmap keeps a bitmap that a finished reading wrote for the next reading, in place of the
// spare one.
func giveBitmap(bitmap *object, width, height int) {
	bitmaps.access.Lock()
	previous := bitmaps.spare
	bitmaps.spare, bitmaps.width, bitmaps.height = bitmap, width, height
	if bitmaps.expiry == nil {
		bitmaps.expiry = time.AfterFunc(bitmapIdle, dropBitmap)
	} else {
		bitmaps.expiry.Reset(bitmapIdle)
	}
	bitmaps.access.Unlock()
	previous.release()
}

func dropBitmap() {
	bitmaps.access.Lock()
	spare := bitmaps.spare
	bitmaps.spare = nil
	bitmaps.access.Unlock()
	if spare == nil {
		return
	}
	leave, err := enter()
	if err != nil {
		return
	}
	defer leave()
	spare.release()
}

// planeDescription is a Windows.Graphics.Imaging.BitmapPlaneDescription.
type planeDescription struct {
	start, width, height, stride int32
}

// fillBitmap writes a region of pixels, scaled to width x height inside a margin of its border's
// mean colour, into a SoftwareBitmap of premultiplied Bgra8 through its locked buffer.
func fillBitmap(bitmap *object, pixels image.Image, region image.Rectangle, width, height, margin int) error {
	var buffer *object
	// ISoftwareBitmap::LockBuffer
	if err := bitmap.call(15, bitmapBufferAccessWrite, uintptr(unsafe.Pointer(&buffer))); err != nil {
		return err
	}
	defer buffer.release()
	closable, err := buffer.query(&iidClosable)
	if err != nil {
		return err
	}
	// IClosable::Close unlocks the bitmap for the recognizer.
	defer func() {
		closable.call(6)
		closable.release()
	}()
	var plane planeDescription
	// IBitmapBuffer::GetPlaneDescription
	if err := buffer.call(7, 0, uintptr(unsafe.Pointer(&plane))); err != nil {
		return err
	}
	memory, err := buffer.query(&iidMemoryBuffer)
	if err != nil {
		return err
	}
	defer memory.release()
	var reference *object
	// IMemoryBuffer::CreateReference
	if err := memory.call(6, uintptr(unsafe.Pointer(&reference))); err != nil {
		return err
	}
	defer reference.release()
	access, err := reference.query(&iidMemoryByteAccess)
	if err != nil {
		return err
	}
	defer access.release()
	var data *byte
	var capacity uint32
	// IMemoryBufferByteAccess::GetBuffer
	if err := access.call(3, uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&capacity))); err != nil {
		return err
	}
	full := image.Rect(0, 0, width+2*margin, height+2*margin)
	stride, start := int(plane.stride), int(plane.start)
	if data == nil || stride < full.Dx()*4 || start < 0 || int(capacity) < start+stride*(full.Dy()-1)+full.Dx()*4 {
		return E.New("the bitmap's buffer of ", capacity, " bytes does not fit ", full.Dx(), "x", full.Dy(), " pixels")
	}
	canvas := &image.RGBA{Pix: unsafe.Slice(data, capacity)[start:], Stride: stride, Rect: full}
	fillCanvas(canvas, pixels, region, width, height, margin)
	return nil
}

// fillCanvas draws a region of pixels, scaled to width x height, inside a margin of canvas that it
// fills with the mean colour of the region's border, all in premultiplied BGRA.
func fillCanvas(canvas *image.RGBA, pixels image.Image, region image.Rectangle, width, height, margin int) {
	inside := image.Rect(margin, margin, margin+width, margin+height)
	source := region.Add(pixels.Bounds().Min)
	if source.Dx() != width || source.Dy() != height {
		draw.BiLinear.Scale(canvas, inside, pixels, source, draw.Src, nil)
		swapRedBlue(canvas, inside)
	} else if !copyBGRA(canvas, inside, pixels, source.Min) {
		draw.Draw(canvas, inside, pixels, source.Min, draw.Src)
		swapRedBlue(canvas, inside)
	}
	var sum [4]int
	border := 0
	for y := inside.Min.Y; y < inside.Max.Y; y++ {
		step := 1
		if y != inside.Min.Y && y != inside.Max.Y-1 {
			step = max(width-1, 1)
		}
		for x := inside.Min.X; x < inside.Max.X; x += step {
			for channel, value := range canvas.Pix[canvas.PixOffset(x, y):][:4] {
				sum[channel] += int(value)
			}
			border++
		}
	}
	background := image.NewUniform(color.RGBA{uint8(sum[0] / border), uint8(sum[1] / border), uint8(sum[2] / border), uint8(sum[3] / border)})
	for _, strip := range []image.Rectangle{
		image.Rect(0, 0, canvas.Rect.Dx(), margin),
		image.Rect(0, inside.Max.Y, canvas.Rect.Dx(), canvas.Rect.Dy()),
		image.Rect(0, margin, margin, inside.Max.Y),
		image.Rect(inside.Max.X, margin, canvas.Rect.Dx(), inside.Max.Y),
	} {
		draw.Draw(canvas, strip, background, image.Point{}, draw.Src)
	}
}

// copyBGRA copies the pixels at from of the frames' NRGBA and gray images into rect of canvas
// as premultiplied BGRA, in bands at once; it answers false for other images.
func copyBGRA(canvas *image.RGBA, rect image.Rectangle, pixels image.Image, from image.Point) bool {
	switch source := pixels.(type) {
	case *image.NRGBA:
		bands(rect.Dy(), func(top, bottom int) {
			for y := top; y < bottom; y++ {
				in := source.Pix[source.PixOffset(from.X, from.Y+y):][:rect.Dx()*4]
				out := canvas.Pix[canvas.PixOffset(rect.Min.X, rect.Min.Y+y):][:rect.Dx()*4]
				for x := 0; x < len(in); x += 4 {
					r, g, b, a := in[x], in[x+1], in[x+2], in[x+3]
					if a != 0xff {
						// As image/draw premultiplies an NRGBA source.
						alpha := uint32(a) * 0x101
						r = uint8(uint32(r) * alpha / 0xff >> 8)
						g = uint8(uint32(g) * alpha / 0xff >> 8)
						b = uint8(uint32(b) * alpha / 0xff >> 8)
					}
					out[x], out[x+1], out[x+2], out[x+3] = b, g, r, a
				}
			}
		})
	case *image.RGBA:
		bands(rect.Dy(), func(top, bottom int) {
			for y := top; y < bottom; y++ {
				in := source.Pix[source.PixOffset(from.X, from.Y+y):][:rect.Dx()*4]
				out := canvas.Pix[canvas.PixOffset(rect.Min.X, rect.Min.Y+y):][:rect.Dx()*4]
				for x := 0; x < len(in); x += 4 {
					out[x], out[x+1], out[x+2], out[x+3] = in[x+2], in[x+1], in[x], in[x+3]
				}
			}
		})
	case *image.Gray:
		bands(rect.Dy(), func(top, bottom int) {
			for y := top; y < bottom; y++ {
				in := source.Pix[source.PixOffset(from.X, from.Y+y):][:rect.Dx()]
				out := canvas.Pix[canvas.PixOffset(rect.Min.X, rect.Min.Y+y):][:rect.Dx()*4]
				for x, value := range in {
					out[x*4], out[x*4+1], out[x*4+2], out[x*4+3] = value, value, value, 0xff
				}
			}
		})
	default:
		return false
	}
	return true
}

// swapRedBlue turns rect of canvas from RGBA into BGRA.
func swapRedBlue(canvas *image.RGBA, rect image.Rectangle) {
	bands(rect.Dy(), func(top, bottom int) {
		for y := top; y < bottom; y++ {
			row := canvas.Pix[canvas.PixOffset(rect.Min.X, rect.Min.Y+y):][:rect.Dx()*4]
			for x := 0; x < len(row); x += 4 {
				row[x], row[x+2] = row[x+2], row[x]
			}
		}
	})
}

// bands runs work over the rows [0, rows) in a band for each processor.
func bands(rows int, work func(top, bottom int)) {
	workers := min(runtime.GOMAXPROCS(0), max(rows/64, 1))
	band := (rows + workers - 1) / workers
	var group sync.WaitGroup
	for top := 0; top < rows; top += band {
		bottom := min(top+band, rows)
		group.Go(func() {
			work(top, bottom)
		})
	}
	group.Wait()
}
