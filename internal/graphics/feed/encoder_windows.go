//go:build windows && (amd64 || arm64)

package feed

import (
	"image"
	"math"
	"os"
	"time"
	"unsafe"

	E "github.com/sagernet/sing/common/exceptions"

	"golang.org/x/sys/windows"
)

var (
	mfreadwrite                   = windows.NewLazySystemDLL("mfreadwrite.dll")
	procMFCreateSinkWriterFromURL = mfreadwrite.NewProc("MFCreateSinkWriterFromURL")
	procMFCreateAttributes        = mfplat.NewProc("MFCreateAttributes")
)

// GUIDs from mfapi.h, mfreadwrite.h and mftransform.h of the Windows SDK 10.0.26100.
var (
	attributeHardwareTransforms = guid(0xa634a91c, 0x822b, 0x41b9, 0xa4, 0x94, 0x4d, 0xe4, 0x64, 0x36, 0x12, 0xb0)
	attributeContainerType      = guid(0x150ff23f, 0x4abc, 0x478b, 0xac, 0x4f, 0xe1, 0x91, 0x6f, 0xba, 0x1c, 0xca)
	containerMPEG4              = guid(0xdc6cd05d, 0xb9d0, 0x40ef, 0xbd, 0x35, 0xfa, 0x62, 0x2c, 0x1a, 0xb2, 0x8a)
	attributeAverageBitrate     = guid(0x20332624, 0xfb0d, 0x4d9e, 0xbd, 0x0d, 0xcb, 0xf6, 0x78, 0x6c, 0x10, 0x2e)
	attributeFrameRate          = guid(0xc459a2e8, 0x3d2c, 0x4e44, 0xb1, 0x32, 0xfe, 0xe5, 0x15, 0x6c, 0x7b, 0xb0)
	attributePixelAspectRatio   = guid(0xc6376a1e, 0x8d0a, 0x4027, 0xbe, 0x45, 0x6d, 0x9a, 0x0a, 0xd3, 0x9b, 0xb6)
	attributeProfile            = guid(0xad76a80b, 0x2d5c, 0x4e0b, 0xb3, 0x75, 0x64, 0xe5, 0x20, 0x13, 0x70, 0x36)
	attributePrimaries          = guid(0xdbfbe4d7, 0x0740, 0x4ee0, 0x81, 0x92, 0x85, 0x0a, 0xb0, 0xe2, 0x19, 0x35)
	attributeTransferFunction   = guid(0x5fb0fce9, 0xbe5c, 0x4935, 0xa8, 0x11, 0xec, 0x83, 0x8f, 0x8e, 0xed, 0x93)
)

const (
	sinkWriterAddStream         = 3
	sinkWriterSetInputMediaType = 4
	sinkWriterBeginWriting      = 5
	sinkWriterWriteSample       = 6
	sinkWriterFinalize          = 11

	sampleSetSampleDuration = 38

	profileH264High   = 100 // eAVEncH264VProfile_High
	profileHEVCMain   = 1   // eAVEncH265VProfile_Main_420_8
	matrixBT709Value  = 1   // MFVideoTransferMatrix_BT709
	primariesBT709    = 2   // MFVideoPrimaries_BT709
	transferBT709     = 5   // MFVideoTransFunc_709
	nominalRangeVideo = 2   // MFNominalRange_16_235
)

// Encoder writes an MPEG-4 movie with Media Foundation's Sink Writer, which picks a hardware
// encoder where there is one (SWVideoExport.m writes with AVAssetWriter).
type Encoder struct {
	worker *worker
	writer *comObject
	stream uint32
	hevc   bool
	width  int
	height int
	frame  []byte
}

// NewEncoder starts a movie at options.Path, replacing any file there.
func NewEncoder(options EncoderOptions) (*Encoder, error) {
	if options.Width <= 0 || options.Height <= 0 || options.Width%2 != 0 || options.Height%2 != 0 {
		return nil, E.New("graphics: a movie of ", options.Width, "x", options.Height, " pixels")
	}
	e := &Encoder{width: options.Width, height: options.Height, frame: make([]byte, options.Width*options.Height*3/2)}
	var stopMF func()
	w, err := startWorker(func() (func(), error) {
		stop, err := startMediaFoundation()
		if err != nil {
			return nil, err
		}
		if err = mfreadwrite.Load(); err != nil {
			stop()
			return nil, E.Cause(err, "load the Media Foundation sink writer")
		}
		stopMF = stop
		return func() {
			e.writer.release()
			e.writer = nil
			stopMF()
		}, nil
	})
	if err != nil {
		return nil, err
	}
	e.worker = w
	var failures []error
	attempts := []struct {
		hevc, hardware bool
	}{{options.HEVC, true}, {options.HEVC, false}}
	if options.HEVC {
		attempts = append(attempts, struct{ hevc, hardware bool }{false, true}, struct{ hevc, hardware bool }{false, false})
	}
	for _, attempt := range attempts {
		w.do(func() {
			err = e.open(options, attempt.hevc, attempt.hardware)
		})
		if err == nil {
			e.hevc = attempt.hevc
			return e, nil
		}
		failures = append(failures, err)
	}
	w.stop()
	os.Remove(options.Path)
	return nil, E.Cause(E.Errors(failures...), "graphics: create the video encoder")
}

// HEVC tells whether the movie is HEVC rather than H.264.
func (e *Encoder) HEVC() bool {
	return e.hevc
}

func setColors(mediaType *comObject) {
	mediaType.call(attributesSetUINT32, uintptr(unsafe.Pointer(&attributeYUVMatrix)), matrixBT709Value)
	mediaType.call(attributesSetUINT32, uintptr(unsafe.Pointer(&attributePrimaries)), primariesBT709)
	mediaType.call(attributesSetUINT32, uintptr(unsafe.Pointer(&attributeTransferFunction)), transferBT709)
	mediaType.call(attributesSetUINT32, uintptr(unsafe.Pointer(&attributeNominalRange)), nominalRangeVideo)
}

func (e *Encoder) videoType(options EncoderOptions, subtype windows.GUID) (*comObject, error) {
	var mediaType *comObject
	if err := check("MFCreateMediaType", procedureCall(procMFCreateMediaType, uintptr(unsafe.Pointer(&mediaType)))); err != nil {
		return nil, err
	}
	numerator, denominator := options.frameRate()
	mediaType.call(attributesSetGUID, uintptr(unsafe.Pointer(&attributeMajorType)), uintptr(unsafe.Pointer(&mediaTypeVideo)))
	mediaType.call(attributesSetGUID, uintptr(unsafe.Pointer(&attributeSubtype)), uintptr(unsafe.Pointer(&subtype)))
	mediaType.call(attributesSetUINT64, uintptr(unsafe.Pointer(&attributeFrameSize)), uintptr(uint64(e.width)<<32|uint64(e.height)))
	mediaType.call(attributesSetUINT64, uintptr(unsafe.Pointer(&attributeFrameRate)), uintptr(uint64(numerator)<<32|uint64(denominator)))
	mediaType.call(attributesSetUINT64, uintptr(unsafe.Pointer(&attributePixelAspectRatio)), uintptr(uint64(1)<<32|1))
	mediaType.call(attributesSetUINT32, uintptr(unsafe.Pointer(&attributeInterlaceMode)), interlaceProgressive)
	setColors(mediaType)
	return mediaType, nil
}

func (e *Encoder) open(options EncoderOptions, hevc, hardware bool) error {
	e.writer.release()
	e.writer = nil
	os.Remove(options.Path)
	var attributes *comObject
	if err := check("MFCreateAttributes", procedureCall(procMFCreateAttributes, uintptr(unsafe.Pointer(&attributes)), 4)); err != nil {
		return err
	}
	defer attributes.release()
	if hardware {
		attributes.call(attributesSetUINT32, uintptr(unsafe.Pointer(&attributeHardwareTransforms)), 1)
	}
	attributes.call(attributesSetGUID, uintptr(unsafe.Pointer(&attributeContainerType)), uintptr(unsafe.Pointer(&containerMPEG4)))
	path, err := windows.UTF16PtrFromString(options.Path)
	if err != nil {
		return err
	}
	if err = check("MFCreateSinkWriterFromURL", procedureCall(procMFCreateSinkWriterFromURL, uintptr(unsafe.Pointer(path)), 0,
		uintptr(unsafe.Pointer(attributes)), uintptr(unsafe.Pointer(&e.writer)))); err != nil {
		return err
	}
	subtype, profile, kind := subtypeH264, uint32(profileH264High), "H.264"
	if hevc {
		subtype, profile, kind = subtypeHEVC, profileHEVCMain, "HEVC"
	}
	output, err := e.videoType(options, subtype)
	if err != nil {
		return err
	}
	defer output.release()
	output.call(attributesSetUINT32, uintptr(unsafe.Pointer(&attributeAverageBitrate)), uintptr(options.bitrate(hevc)))
	output.call(attributesSetUINT32, uintptr(unsafe.Pointer(&attributeProfile)), uintptr(profile))
	if err = check("add the "+kind+" stream", e.writer.call(sinkWriterAddStream, uintptr(unsafe.Pointer(output)), uintptr(unsafe.Pointer(&e.stream)))); err != nil {
		return err
	}
	input, err := e.videoType(options, subtypeNV12)
	if err != nil {
		return err
	}
	defer input.release()
	input.call(attributesSetUINT32, uintptr(unsafe.Pointer(&attributeDefaultStride)), uintptr(e.width))
	if err = check("set the "+kind+" encoder's input", e.writer.call(sinkWriterSetInputMediaType, uintptr(e.stream), uintptr(unsafe.Pointer(input)), 0)); err != nil {
		return err
	}
	return check("begin writing", e.writer.call(sinkWriterBeginWriting))
}

// Write adds a frame shown from at for duration; its pixels are cropped or padded with black
// to the movie's size.
func (e *Encoder) Write(pixels *image.NRGBA, at, duration time.Duration) error {
	frame := pixels
	if bounds := pixels.Bounds(); bounds.Dx() != e.width || bounds.Dy() != e.height {
		frame = image.NewNRGBA(image.Rect(0, 0, e.width, e.height))
		for y := range min(bounds.Dy(), e.height) {
			copy(frame.Pix[y*frame.Stride:y*frame.Stride+min(bounds.Dx(), e.width)*4], pixels.Pix[pixels.PixOffset(bounds.Min.X, bounds.Min.Y+y):])
		}
		for index := 3; index < len(frame.Pix); index += 4 {
			frame.Pix[index] = 255
		}
	}
	toNV12(e.frame, frame, e.width, e.height)
	var err error
	if !e.worker.do(func() {
		err = e.writeSample(at, duration)
	}) {
		return errClosed
	}
	return err
}

func (e *Encoder) writeSample(at, duration time.Duration) error {
	var buffer *comObject
	if err := check("allocate a frame", procedureCall(procMFCreateMemoryBuffer, uintptr(len(e.frame)), uintptr(unsafe.Pointer(&buffer)))); err != nil {
		return err
	}
	defer buffer.release()
	var start *byte
	if err := check("lock a frame", buffer.call(bufferLock, uintptr(unsafe.Pointer(&start)), 0, 0)); err != nil {
		return err
	}
	copy(unsafe.Slice(start, len(e.frame)), e.frame)
	buffer.call(bufferUnlock)
	buffer.call(bufferSetCurrentLength, uintptr(len(e.frame)))
	var sample *comObject
	if err := check("MFCreateSample", procedureCall(procMFCreateSample, uintptr(unsafe.Pointer(&sample)))); err != nil {
		return err
	}
	defer sample.release()
	sample.call(sampleAddBuffer, uintptr(unsafe.Pointer(buffer)))
	sample.call(sampleSetSampleTime, uintptr(int64(at/100)))
	sample.call(sampleSetSampleDuration, uintptr(int64(max(duration, time.Millisecond)/100)))
	return check("write a frame", e.writer.call(sinkWriterWriteSample, uintptr(e.stream), uintptr(unsafe.Pointer(sample))))
}

// Finish writes the end of the movie and releases the encoder.
func (e *Encoder) Finish() error {
	var err error
	if !e.worker.do(func() {
		err = check("finish the video", e.writer.call(sinkWriterFinalize))
	}) {
		err = errClosed
	}
	e.worker.stop()
	return err
}

// Close releases the encoder without finishing the movie.
func (e *Encoder) Close() {
	e.worker.stop()
}

// bitrate is a quarter of a bit a pixel at the frame rate for H.264, which keeps the text of a
// screen sharp, and half of that for HEVC; AVFoundation picks its own.
func (o EncoderOptions) bitrate(hevc bool) uint32 {
	bits := float64(o.Width) * float64(o.Height) * max(o.FrameRate, 1) / 4
	if hevc {
		bits /= 2
	}
	return uint32(min(max(bits, 500_000), 100_000_000))
}

// frameRate is the frame rate as a ratio.
func (o EncoderOptions) frameRate() (uint32, uint32) {
	rate := max(o.FrameRate, 0.001)
	if rate == math.Trunc(rate) {
		return uint32(rate), 1
	}
	return uint32(math.Round(rate * 1000)), 1000
}

// toNV12 converts opaque RGBA to NV12 in BT.709 video range: a luma plane, then a plane of
// interleaved Cb and Cr from each 2x2 block. The width and height are even.
func toNV12(out []byte, pixels *image.NRGBA, width, height int) {
	chroma := out[width*height:]
	bounds := pixels.Bounds()
	parallel(height/2, func(start, end int) {
		for pair := start; pair < end; pair++ {
			for half := range 2 {
				y := pair*2 + half
				row := pixels.Pix[pixels.PixOffset(bounds.Min.X, bounds.Min.Y+y):]
				luma := out[y*width : (y+1)*width]
				for x := range width {
					p := row[x*4 : x*4+3 : x*4+3]
					luma[x] = byte(16 + (47*int32(p[0])+157*int32(p[1])+16*int32(p[2])+128)>>8)
				}
			}
			top := pixels.Pix[pixels.PixOffset(bounds.Min.X, bounds.Min.Y+pair*2):]
			bottom := pixels.Pix[pixels.PixOffset(bounds.Min.X, bounds.Min.Y+pair*2+1):]
			target := chroma[pair*width : (pair+1)*width]
			for x := 0; x+1 < width; x += 2 {
				var red, green, blue int32
				for _, p := range [][]byte{top[x*4:], top[x*4+4:], bottom[x*4:], bottom[x*4+4:]} {
					red += int32(p[0])
					green += int32(p[1])
					blue += int32(p[2])
				}
				red, green, blue = (red+2)/4, (green+2)/4, (blue+2)/4
				target[x] = clampChroma(128 + (-26*red-86*green+112*blue+128)>>8)
				target[x+1] = clampChroma(128 + (112*red-102*green-10*blue+128)>>8)
			}
		}
	})
}

func clampChroma(value int32) byte {
	return byte(min(max(value, 16), 240))
}
