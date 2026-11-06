//go:build windows && (amd64 || arm64)

package feed

import (
	"errors"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	E "github.com/sagernet/sing/common/exceptions"

	"golang.org/x/sys/windows"
)

var (
	mfplat                        = windows.NewLazySystemDLL("mfplat.dll")
	procMFStartup                 = mfplat.NewProc("MFStartup")
	procMFShutdown                = mfplat.NewProc("MFShutdown")
	procMFTEnumEx                 = mfplat.NewProc("MFTEnumEx")
	procMFCreateMediaType         = mfplat.NewProc("MFCreateMediaType")
	procMFCreateSample            = mfplat.NewProc("MFCreateSample")
	procMFCreateMemoryBuffer      = mfplat.NewProc("MFCreateMemoryBuffer")
	procMFCreateAlignedBuffer     = mfplat.NewProc("MFCreateAlignedMemoryBuffer")
	procMFCreateDXGIDeviceManager = mfplat.NewProc("MFCreateDXGIDeviceManager")

	d3d11                 = windows.NewLazySystemDLL("d3d11.dll")
	procD3D11CreateDevice = d3d11.NewProc("D3D11CreateDevice")
)

func guid(data1 uint32, data2, data3 uint16, data4 ...byte) windows.GUID {
	value := windows.GUID{Data1: data1, Data2: data2, Data3: data3}
	copy(value.Data4[:], data4)
	return value
}

// fourCC names a media subtype by its FOURCC, or its GUID when it has none.
func fourCC(subtype windows.GUID) string {
	if base := mediaSubtype("    "); subtype.Data2 == base.Data2 && subtype.Data3 == base.Data3 && subtype.Data4 == base.Data4 {
		code := subtype.Data1
		return string([]byte{byte(code), byte(code >> 8), byte(code >> 16), byte(code >> 24)})
	}
	return subtype.String()
}

// mediaSubtype is the subtype GUID of a FOURCC (DEFINE_MEDIATYPE_GUID in mfapi.h).
func mediaSubtype(fourcc string) windows.GUID {
	code := uint32(fourcc[0]) | uint32(fourcc[1])<<8 | uint32(fourcc[2])<<16 | uint32(fourcc[3])<<24
	return guid(code, 0x0000, 0x0010, 0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71)
}

// GUIDs from mfapi.h, mftransform.h, mfobjects.h and d3d10.h of the Windows SDK 10.0.26100.
var (
	categoryVideoDecoder = guid(0xd6c02d4b, 0x6833, 0x45b4, 0x97, 0x1a, 0x05, 0xa4, 0xb0, 0x4b, 0xab, 0x91)
	mediaTypeVideo       = guid(0x73646976, 0x0000, 0x0010, 0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71)
	subtypeHEVC          = mediaSubtype("HEVC")
	subtypeNV12          = mediaSubtype("NV12")
	subtypeAYUV          = mediaSubtype("AYUV")

	attributeMajorType       = guid(0x48eba18e, 0xf8c9, 0x4687, 0xbf, 0x11, 0x0a, 0x74, 0xc9, 0xf9, 0x6a, 0x8f)
	attributeSubtype         = guid(0xf7e34c9a, 0x42e8, 0x4714, 0xb7, 0x4b, 0xcb, 0x29, 0xd7, 0x2c, 0x35, 0xe5)
	attributeFrameSize       = guid(0x1652c33d, 0xd6b2, 0x4012, 0xb8, 0x34, 0x72, 0x03, 0x08, 0x49, 0xa3, 0x7d)
	attributeInterlaceMode   = guid(0xe2724bb8, 0xe676, 0x4806, 0xb4, 0xb2, 0xa8, 0xd6, 0xef, 0xb4, 0x4c, 0xcd)
	attributeYUVMatrix       = guid(0x3e23d450, 0x2c75, 0x4d25, 0xa0, 0x0e, 0xb9, 0x16, 0x70, 0xd1, 0x23, 0x27)
	attributeNominalRange    = guid(0xc21b8ee5, 0xb956, 0x4071, 0x8d, 0xaf, 0x32, 0x5e, 0xdf, 0x5c, 0xab, 0x11)
	attributeDisplayAperture = guid(0xd7388766, 0x18fe, 0x48c6, 0xa1, 0x77, 0xee, 0x89, 0x48, 0x67, 0xc8, 0xc4)
	attributeDefaultStride   = guid(0x644b4e48, 0x1e02, 0x4516, 0xb0, 0xeb, 0xc0, 0x1c, 0xa9, 0xd4, 0x9a, 0xc6)
	attributeLowLatency      = guid(0x9c27891a, 0xed7a, 0x40e1, 0x88, 0xe8, 0xb2, 0x27, 0x27, 0xa0, 0x24, 0xee)
	attributeCleanPoint      = guid(0x9cdf01d8, 0xa0f0, 0x43ba, 0xb0, 0x77, 0xea, 0xa0, 0x6c, 0xbd, 0x72, 0x8a)
	attributeD3D11Aware      = guid(0x206b4fc8, 0xfcf9, 0x4c51, 0xaf, 0xe3, 0x97, 0x64, 0x36, 0x9e, 0x33, 0xa0)
	attributeAsync           = guid(0xf81a699a, 0x649a, 0x497d, 0x8c, 0x73, 0x29, 0xf8, 0xfe, 0xd6, 0xad, 0x7a)
	attributeFriendlyName    = guid(0x314ffbae, 0x5b41, 0x4c95, 0x9c, 0x19, 0x4e, 0x7d, 0x58, 0x6f, 0xac, 0xe3)

	iidTransform   = guid(0xbf94c121, 0x5b05, 0x4e6f, 0x80, 0x00, 0xba, 0x59, 0x89, 0x61, 0x41, 0x4d)
	iid2DBuffer    = guid(0x7DC9D5F9, 0x9ED9, 0x44ec, 0x9B, 0xBF, 0x06, 0x00, 0xBB, 0x58, 0x9F, 0xBB)
	iid2DBuffer2   = guid(0x33ae5ea6, 0x4316, 0x436f, 0x8d, 0xdd, 0xd7, 0x3d, 0x22, 0xf8, 0x29, 0xec)
	iidMultithread = guid(0x9B7E4E00, 0x342C, 0x4106, 0xA1, 0x9F, 0x4F, 0x27, 0x04, 0xF6, 0x89, 0xF0)
)

const (
	mfVersion   = 0x0002<<16 | 0x0070 // MF_SDK_VERSION << 16 | MF_API_VERSION
	mfStartLite = 0x1                 // MFSTARTUP_NOSOCKET

	enumSync          = 0x01
	enumAsync         = 0x02
	enumHardware      = 0x04
	enumLocal         = 0x10
	enumSortAndFilter = 0x40

	messageFlush            = 0x0
	messageSetD3DManager    = 0x2
	messageBeginStreaming   = 0x10000000
	messageEndStreaming     = 0x10000001
	messageEndOfStream      = 0x10000002
	messageStartOfStream    = 0x10000003
	outputProvidesSamples   = 0x100
	outputCanProvideSamples = 0x200
	outputFormatChange      = 0x100
	interlaceProgressive    = 2
	lockRead                = 0x1
	nominalRangeFull        = 1
	matrixValueBT601        = 2
	matrixValueBT2020Ten    = 4
	matrixValueBT2020Twelve = 5

	hrInvalidMediaType hresult = 0xC00D36B4
	hrNotAccepting     hresult = 0xC00D36B5
	hrNoMoreTypes      hresult = 0xC00D36B9
	hrTypeNotSet       hresult = 0xC00D6D60
	hrStreamChange     hresult = 0xC00D6D61
	hrNeedMoreInput    hresult = 0xC00D6D72
	hrChangedMode      hresult = 0x80010106 // RPC_E_CHANGED_MODE
)

// Method slots (see comObject).
const (
	attributesGetUINT32          = 7
	attributesGetUINT64          = 8
	attributesGetGUID            = 10
	attributesGetAllocatedString = 13
	attributesGetBlob            = 15
	attributesSetUINT32          = 21
	attributesSetUINT64          = 22
	attributesSetGUID            = 24

	activateActivateObject = 33
	activateShutdownObject = 34

	sampleGetSampleTime    = 35
	sampleSetSampleTime    = 36
	sampleGetBufferByIndex = 40
	sampleAddBuffer        = 42

	bufferLock             = 3
	bufferUnlock           = 4
	bufferSetCurrentLength = 6

	buffer2DLock2D     = 3
	buffer2DUnlock2D   = 4
	buffer2DLock2DSize = 10

	transformGetOutputStreamInfo    = 7
	transformGetAttributes          = 8
	transformGetOutputAvailableType = 14
	transformSetInputType           = 15
	transformSetOutputType          = 16
	transformGetOutputCurrentType   = 18
	transformProcessMessage         = 23
	transformProcessInput           = 24
	transformProcessOutput          = 25

	managerResetDevice      = 7
	multithreadSetProtected = 5
)

type registerTypeInfo struct {
	major, subtype windows.GUID
}

type outputStreamInfo struct {
	flags, size, alignment uint32
}

type outputDataBuffer struct {
	streamID uint32
	sample   *comObject
	status   uint32
	events   *comObject
}

type videoArea struct {
	offsetXFraction uint16
	offsetX         int16
	offsetYFraction uint16
	offsetY         int16
	width, height   int32
}

// startMediaFoundation joins the thread to the multithreaded apartment and starts Media
// Foundation; stop undoes both. The caller keeps its goroutine on the thread in between.
func startMediaFoundation() (stop func(), err error) {
	if err = mfplat.Load(); err != nil {
		return nil, E.Cause(err, "load Media Foundation")
	}
	uninitialize := true
	if err = windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); err != nil {
		var errno windows.Errno
		switch {
		case errors.As(err, &errno) && errno == 1: // S_FALSE: already in the apartment
		case errors.As(err, &errno) && hresult(errno) == hrChangedMode:
			uninitialize = false
		default:
			return nil, E.Cause(err, "initialize COM")
		}
	}
	if err = check("MFStartup", procedureCall(procMFStartup, mfVersion, mfStartLite)); err != nil {
		if uninitialize {
			windows.CoUninitialize()
		}
		return nil, err
	}
	return func() {
		procedureCall(procMFShutdown)
		if uninitialize {
			windows.CoUninitialize()
		}
	}, nil
}

// decoderCandidate is an HEVC decoder that MFTEnumEx listed.
type decoderCandidate struct {
	activate *comObject
	name     string
	hardware bool
}

// enumerateDecoders lists the HEVC decoders, hardware ones first in the order Media
// Foundation ranks them, then the synchronous software ones.
func enumerateDecoders() ([]decoderCandidate, error) {
	input := registerTypeInfo{major: mediaTypeVideo, subtype: subtypeHEVC}
	var candidates []decoderCandidate
	var failure error
	for _, pass := range []struct {
		flags    uint32
		hardware bool
	}{
		{flags: enumHardware | enumSortAndFilter, hardware: true},
		{flags: enumSync | enumLocal | enumSortAndFilter},
	} {
		var activates **comObject
		var count uint32
		args := guidArguments(&categoryVideoDecoder)
		args = append(args, uintptr(pass.flags), uintptr(unsafe.Pointer(&input)), 0,
			uintptr(unsafe.Pointer(&activates)), uintptr(unsafe.Pointer(&count)))
		if err := check("MFTEnumEx", procedureCall(procMFTEnumEx, args...)); err != nil {
			failure = err
			continue
		}
		if activates == nil {
			continue
		}
		for _, activate := range unsafe.Slice(activates, count) {
			candidates = append(candidates, decoderCandidate{activate: activate, name: friendlyName(activate), hardware: pass.hardware})
		}
		windows.CoTaskMemFree(unsafe.Pointer(activates))
	}
	if len(candidates) == 0 && failure != nil {
		return nil, failure
	}
	return candidates, nil
}

func friendlyName(attributes *comObject) string {
	var text *uint16
	var length uint32
	if attributes.call(attributesGetAllocatedString, uintptr(unsafe.Pointer(&attributeFriendlyName)), uintptr(unsafe.Pointer(&text)), uintptr(unsafe.Pointer(&length))).failed() {
		return "unnamed decoder"
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(text))
	return utf16Text(text)
}

func uint32Attribute(attributes *comObject, key *windows.GUID) (uint32, bool) {
	var value uint32
	if attributes.call(attributesGetUINT32, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&value))).failed() {
		return 0, false
	}
	return value, true
}

func uint64Attribute(attributes *comObject, key *windows.GUID) (uint64, bool) {
	var value uint64
	if attributes.call(attributesGetUINT64, uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(&value))).failed() {
		return 0, false
	}
	return value, true
}

var (
	availability     error
	availabilityOnce sync.Once
)

// available tells whether this PC has an HEVC decoder for Media Foundation, such as the one
// of Microsoft's HEVC Video Extension; it looks once.
func available() error {
	availabilityOnce.Do(func() {
		done := make(chan error, 1)
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			stop, err := startMediaFoundation()
			if err != nil {
				done <- err
				return
			}
			defer stop()
			candidates, err := enumerateDecoders()
			for _, candidate := range candidates {
				candidate.activate.release()
			}
			if err == nil && len(candidates) == 0 {
				err = E.New("Media Foundation lists no HEVC decoder; install Microsoft's HEVC Video Extensions")
			}
			done <- err
		}()
		if err := <-done; err != nil {
			availability = &UnavailableError{Reason: err}
		}
	})
	return availability
}

// mfCodec is a Media Foundation HEVC decoder transform that answers NV12 or AYUV pictures. All its
// calls must come from the goroutine that made it, locked to its thread.
type mfCodec struct {
	stop      func()
	activate  *comObject
	transform *comObject
	device    *comObject
	manager   *comObject
	name      string
	gpu       bool
	width     int
	height    int

	layout          pixelLayout
	providesSamples bool
	output          *comObject
	frameWidth      int
	frameHeight     int
	stride          int
	visibleWidth    int
	visibleHeight   int
	fullRange       bool
	matrix          colorMatrix
}

// newCodec makes the first decoder that accepts the stream's frame size, trying the hardware
// ones first. Unless software is set, a decoder that can use Direct3D 11 decodes on the GPU.
func newCodec(sets *parameterSets, options codecOptions) (codec, error) {
	stop, err := startMediaFoundation()
	if err != nil {
		return nil, err
	}
	candidates, err := enumerateDecoders()
	if err != nil {
		stop()
		return nil, err
	}
	if len(candidates) == 0 {
		stop()
		return nil, E.New("Media Foundation lists no HEVC decoder; install Microsoft's HEVC Video Extensions")
	}
	var chosen *mfCodec
	var failures []error
	for _, candidate := range candidates {
		if chosen != nil || (candidate.hardware && options.software) {
			candidate.activate.release()
			continue
		}
		c := &mfCodec{activate: candidate.activate, name: candidate.name, width: sets.width, height: sets.height}
		if err := c.open(options); err != nil {
			failures = append(failures, E.Cause(err, candidate.name))
			c.release()
			continue
		}
		chosen = c
	}
	if chosen == nil {
		stop()
		return nil, E.Cause(E.Errors(failures...), "no HEVC decoder accepted the stream")
	}
	chosen.stop = stop
	return chosen, nil
}

func (c *mfCodec) Name() string {
	if c.gpu {
		return c.name + " (Direct3D 11)"
	}
	return c.name + " (software)"
}

func (c *mfCodec) Accepts(sets *parameterSets) bool {
	return sets.width == c.width && sets.height == c.height
}

func (c *mfCodec) open(options codecOptions) error {
	if err := check("activate the decoder", c.activate.call(activateActivateObject, uintptr(unsafe.Pointer(&iidTransform)), uintptr(unsafe.Pointer(&c.transform)))); err != nil {
		return err
	}
	var attributes *comObject
	if !c.transform.call(transformGetAttributes, uintptr(unsafe.Pointer(&attributes))).failed() && attributes != nil {
		defer attributes.release()
		if async, _ := uint32Attribute(attributes, &attributeAsync); async != 0 {
			// The event model of asynchronous transforms is not implemented; Media Foundation's
			// software decoder uses the same GPU through Direct3D 11.
			return E.New("asynchronous transforms are not supported")
		}
		attributes.call(attributesSetUINT32, uintptr(unsafe.Pointer(&attributeLowLatency)), 1)
		if aware, _ := uint32Attribute(attributes, &attributeD3D11Aware); aware != 0 && !options.software {
			c.gpu = c.useDirect3D() == nil
		}
	}
	if err := c.setInputType(); err != nil {
		return err
	}
	if err := c.negotiateOutput(); err != nil {
		return err
	}
	if err := check("begin streaming", c.transform.call(transformProcessMessage, messageBeginStreaming, 0)); err != nil {
		return err
	}
	return check("start the stream", c.transform.call(transformProcessMessage, messageStartOfStream, 0))
}

// useDirect3D hands the decoder a Direct3D 11 device on the default adapter, so it decodes
// with DXVA and answers its pictures in GPU memory.
func (c *mfCodec) useDirect3D() error {
	if err := d3d11.Load(); err != nil {
		return err
	}
	levels := []uint32{0xb100, 0xb000} // D3D_FEATURE_LEVEL_11_1, 11_0
	var context *comObject
	var level uint32
	const hardwareDriver, videoSupport, sdkVersion = 1, 0x800, 7
	if err := check("D3D11CreateDevice", procedureCall(procD3D11CreateDevice, 0, hardwareDriver, 0, videoSupport,
		uintptr(unsafe.Pointer(&levels[0])), uintptr(len(levels)), sdkVersion,
		uintptr(unsafe.Pointer(&c.device)), uintptr(unsafe.Pointer(&level)), uintptr(unsafe.Pointer(&context)))); err != nil {
		return err
	}
	context.release()
	if multithread, hr := c.device.query(&iidMultithread); !hr.failed() {
		multithread.call(multithreadSetProtected, 1)
		multithread.release()
	}
	var token uint32
	if err := check("MFCreateDXGIDeviceManager", procedureCall(procMFCreateDXGIDeviceManager, uintptr(unsafe.Pointer(&token)), uintptr(unsafe.Pointer(&c.manager)))); err != nil {
		return err
	}
	if err := check("reset the DXGI device manager", c.manager.call(managerResetDevice, uintptr(unsafe.Pointer(c.device)), uintptr(token))); err != nil {
		return err
	}
	if err := check("give the decoder the Direct3D device", c.transform.call(transformProcessMessage, messageSetD3DManager, uintptr(unsafe.Pointer(c.manager)))); err != nil {
		c.transform.call(transformProcessMessage, messageSetD3DManager, 0)
		return err
	}
	return nil
}

func (c *mfCodec) setInputType() error {
	var mediaType *comObject
	if err := check("MFCreateMediaType", procedureCall(procMFCreateMediaType, uintptr(unsafe.Pointer(&mediaType)))); err != nil {
		return err
	}
	defer mediaType.release()
	mediaType.call(attributesSetGUID, uintptr(unsafe.Pointer(&attributeMajorType)), uintptr(unsafe.Pointer(&mediaTypeVideo)))
	mediaType.call(attributesSetGUID, uintptr(unsafe.Pointer(&attributeSubtype)), uintptr(unsafe.Pointer(&subtypeHEVC)))
	mediaType.call(attributesSetUINT64, uintptr(unsafe.Pointer(&attributeFrameSize)), uintptr(uint64(c.width)<<32|uint64(c.height)))
	mediaType.call(attributesSetUINT32, uintptr(unsafe.Pointer(&attributeInterlaceMode)), interlaceProgressive)
	return check("set the decoder's input type", c.transform.call(transformSetInputType, 0, uintptr(unsafe.Pointer(mediaType)), 0))
}

// negotiateOutput picks NV12 among the output types, or AYUV for a 4:4:4 stream such as the
// device's remote desktop one, and reads its geometry and colors; the
// decoder asks for it again with MF_E_TRANSFORM_STREAM_CHANGE.
func (c *mfCodec) negotiateOutput() error {
	var offered []string
	var chosen, fallback *comObject
	for index := 0; chosen == nil; index++ {
		var mediaType *comObject
		hr := c.transform.call(transformGetOutputAvailableType, 0, uintptr(index), uintptr(unsafe.Pointer(&mediaType)))
		if hr == hrNoMoreTypes {
			break
		}
		if err := check("list the decoder's output types", hr); err != nil {
			fallback.release()
			return err
		}
		var subtype windows.GUID
		mediaType.call(attributesGetGUID, uintptr(unsafe.Pointer(&attributeSubtype)), uintptr(unsafe.Pointer(&subtype)))
		switch {
		case subtype == subtypeNV12:
			chosen = mediaType
			c.layout = layoutNV12
		case subtype == subtypeAYUV && fallback == nil:
			fallback = mediaType
		default:
			offered = append(offered, fourCC(subtype))
			mediaType.release()
		}
	}
	if chosen == nil && fallback != nil {
		chosen, fallback = fallback, nil
		c.layout = layoutAYUV
	}
	fallback.release()
	if chosen == nil {
		return E.New("the decoder offers neither NV12 nor AYUV, only ", strings.Join(offered, ", "))
	}
	hr := c.transform.call(transformSetOutputType, 0, uintptr(unsafe.Pointer(chosen)), 0)
	chosen.release()
	if err := check("set the decoder's output type", hr); err != nil {
		return err
	}
	return c.readOutput()
}

func (c *mfCodec) readOutput() error {
	var mediaType *comObject
	if err := check("read the decoder's output type", c.transform.call(transformGetOutputCurrentType, 0, uintptr(unsafe.Pointer(&mediaType)))); err != nil {
		return err
	}
	defer mediaType.release()
	size, ok := uint64Attribute(mediaType, &attributeFrameSize)
	if !ok || size>>32 == 0 || uint32(size) == 0 {
		return E.New("the decoder's output type has no frame size")
	}
	c.frameWidth, c.frameHeight = int(size>>32), int(uint32(size))
	c.stride = 0
	if stride, ok := uint32Attribute(mediaType, &attributeDefaultStride); ok && int32(stride) > 0 {
		c.stride = int(int32(stride))
	}
	c.visibleWidth, c.visibleHeight = min(c.width, c.frameWidth), min(c.height, c.frameHeight)
	var area videoArea
	var length uint32
	if !mediaType.call(attributesGetBlob, uintptr(unsafe.Pointer(&attributeDisplayAperture)), uintptr(unsafe.Pointer(&area)), unsafe.Sizeof(area), uintptr(unsafe.Pointer(&length))).failed() &&
		length == uint32(unsafe.Sizeof(area)) && area.offsetX == 0 && area.offsetY == 0 && area.width > 0 && area.height > 0 {
		c.visibleWidth, c.visibleHeight = min(int(area.width), c.frameWidth), min(int(area.height), c.frameHeight)
	}
	nominal, _ := uint32Attribute(mediaType, &attributeNominalRange)
	c.fullRange = nominal == nominalRangeFull
	c.matrix = matrixBT709
	switch value, _ := uint32Attribute(mediaType, &attributeYUVMatrix); value {
	case matrixValueBT601:
		c.matrix = matrixBT601
	case matrixValueBT2020Ten, matrixValueBT2020Twelve:
		c.matrix = matrixBT2020
	}
	var info outputStreamInfo
	if err := check("read the decoder's output stream", c.transform.call(transformGetOutputStreamInfo, 0, uintptr(unsafe.Pointer(&info)))); err != nil {
		return err
	}
	c.output.release()
	c.output = nil
	c.providesSamples = info.flags&(outputProvidesSamples|outputCanProvideSamples) != 0
	if c.providesSamples {
		return nil
	}
	return c.makeOutputSample(info)
}

func (c *mfCodec) makeOutputSample(info outputStreamInfo) error {
	size := max(info.size, uint32(c.frameWidth*c.frameHeight*3/2))
	if c.layout == layoutAYUV {
		size = max(info.size, uint32(c.frameWidth*c.frameHeight*4))
	}
	var buffer *comObject
	var hr hresult
	if info.alignment > 1 {
		hr = procedureCall(procMFCreateAlignedBuffer, uintptr(size), uintptr(info.alignment-1), uintptr(unsafe.Pointer(&buffer)))
	} else {
		hr = procedureCall(procMFCreateMemoryBuffer, uintptr(size), uintptr(unsafe.Pointer(&buffer)))
	}
	if err := check("allocate the output buffer", hr); err != nil {
		return err
	}
	defer buffer.release()
	if err := check("MFCreateSample", procedureCall(procMFCreateSample, uintptr(unsafe.Pointer(&c.output)))); err != nil {
		return err
	}
	return check("add the output buffer", c.output.call(sampleAddBuffer, uintptr(unsafe.Pointer(buffer))))
}

func (c *mfCodec) inputSample(data []byte, time int64, keyframe bool) (*comObject, error) {
	var buffer *comObject
	if err := check("allocate the input buffer", procedureCall(procMFCreateMemoryBuffer, uintptr(len(data)), uintptr(unsafe.Pointer(&buffer)))); err != nil {
		return nil, err
	}
	defer buffer.release()
	var start *byte
	if err := check("lock the input buffer", buffer.call(bufferLock, uintptr(unsafe.Pointer(&start)), 0, 0)); err != nil {
		return nil, err
	}
	copy(unsafe.Slice(start, len(data)), data)
	buffer.call(bufferUnlock)
	buffer.call(bufferSetCurrentLength, uintptr(len(data)))
	var sample *comObject
	if err := check("MFCreateSample", procedureCall(procMFCreateSample, uintptr(unsafe.Pointer(&sample)))); err != nil {
		return nil, err
	}
	sample.call(sampleAddBuffer, uintptr(unsafe.Pointer(buffer)))
	sample.call(sampleSetSampleTime, uintptr(time))
	if keyframe {
		sample.call(attributesSetUINT32, uintptr(unsafe.Pointer(&attributeCleanPoint)), 1)
	}
	return sample, nil
}

// Decode feeds an access unit and collects every picture the decoder has ready. A decoder that
// is full answers MF_E_NOTACCEPTING until its output is drained.
func (c *mfCodec) Decode(data []byte, time int64, keyframe bool) ([]*picture, error) {
	sample, err := c.inputSample(data, time, keyframe)
	if err != nil {
		return nil, err
	}
	defer sample.release()
	var pictures []*picture
	for attempt := 0; ; attempt++ {
		hr := c.transform.call(transformProcessInput, 0, uintptr(unsafe.Pointer(sample)), 0)
		if hr == hrNotAccepting && attempt < 4 {
			pictures, err = c.drain(pictures)
			if err != nil {
				return pictures, err
			}
			continue
		}
		if err := check("ProcessInput", hr); err != nil {
			return pictures, err
		}
		break
	}
	return c.drain(pictures)
}

func (c *mfCodec) drain(pictures []*picture) ([]*picture, error) {
	changes := 0
	for {
		buffer := outputDataBuffer{sample: c.output}
		var status uint32
		hr := c.transform.call(transformProcessOutput, 0, 1, uintptr(unsafe.Pointer(&buffer)), uintptr(unsafe.Pointer(&status)))
		buffer.events.release()
		owned := c.providesSamples && buffer.sample != nil
		switch {
		case hr == hrNeedMoreInput:
			return pictures, nil
		case hr == hrStreamChange:
			if owned {
				buffer.sample.release()
			}
			changes++
			if changes > 4 {
				return pictures, E.New("the decoder keeps changing its output type")
			}
			if err := c.negotiateOutput(); err != nil {
				return pictures, err
			}
			continue
		case hr.failed():
			if owned {
				buffer.sample.release()
			}
			return pictures, check("ProcessOutput", hr)
		}
		if buffer.sample != nil {
			picture, err := c.copyPicture(buffer.sample)
			if owned {
				buffer.sample.release()
			}
			if err != nil {
				return pictures, err
			}
			pictures = append(pictures, picture)
		}
		if buffer.status&outputFormatChange != 0 {
			if err := c.readOutput(); err != nil {
				return pictures, err
			}
		}
	}
}

// copyPicture copies the visible part of an output sample's planes into Go memory. A
// Direct3D surface is read back by Media Foundation when it is locked.
func (c *mfCodec) copyPicture(sample *comObject) (*picture, error) {
	var time int64
	sample.call(sampleGetSampleTime, uintptr(unsafe.Pointer(&time)))
	var buffer *comObject
	if err := check("read the output buffer", sample.call(sampleGetBufferByIndex, 0, uintptr(unsafe.Pointer(&buffer)))); err != nil {
		return nil, err
	}
	defer buffer.release()
	var scanline *byte
	var pitch int32
	length := 0
	var unlock func()
	if buffer2, hr := buffer.query(&iid2DBuffer2); !hr.failed() {
		defer buffer2.release()
		var start *byte
		var size uint32
		if err := check("lock the output buffer", buffer2.call(buffer2DLock2DSize, lockRead, uintptr(unsafe.Pointer(&scanline)), uintptr(unsafe.Pointer(&pitch)),
			uintptr(unsafe.Pointer(&start)), uintptr(unsafe.Pointer(&size)))); err != nil {
			return nil, err
		}
		unlock = func() { buffer2.call(buffer2DUnlock2D) }
		length = int(size) - int(uintptr(unsafe.Pointer(scanline))-uintptr(unsafe.Pointer(start)))
	} else if buffer2D, hr := buffer.query(&iid2DBuffer); !hr.failed() {
		defer buffer2D.release()
		if err := check("lock the output buffer", buffer2D.call(buffer2DLock2D, uintptr(unsafe.Pointer(&scanline)), uintptr(unsafe.Pointer(&pitch)))); err != nil {
			return nil, err
		}
		unlock = func() { buffer2D.call(buffer2DUnlock2D) }
	} else {
		var size uint32
		if err := check("lock the output buffer", buffer.call(bufferLock, uintptr(unsafe.Pointer(&scanline)), 0, uintptr(unsafe.Pointer(&size)))); err != nil {
			return nil, err
		}
		unlock = func() { buffer.call(bufferUnlock) }
		length = int(size)
		// A plain buffer holds the frame's rows back to back. HEVCVideoExtension gave an AYUV
		// MF_MT_DEFAULT_STRIDE of 1328 for a 1328-pixel row of 5312 bytes, so the length decides.
		if c.layout == layoutAYUV {
			pitch = int32(length / c.frameHeight)
		} else {
			pitch = int32(length * 2 / (3 * c.frameHeight))
		}
		if c.stride > 0 && c.layout == layoutNV12 && c.stride*c.frameHeight*3/2 <= length {
			pitch = int32(c.stride)
		}
	}
	defer unlock()
	rowBytes := c.visibleWidth
	if c.layout == layoutAYUV {
		rowBytes *= 4
	}
	if pitch <= 0 || int(pitch) < rowBytes {
		return nil, E.New("the decoder's output pitch ", pitch, " does not fit a width of ", c.visibleWidth)
	}
	if c.layout == layoutAYUV {
		if length > 0 && int(pitch)*c.visibleHeight > length {
			return nil, E.New("the decoder's output buffer of ", length, " bytes is too small")
		}
		out := newPicture(layoutAYUV, c.visibleWidth, c.visibleHeight, rowBytes, c.visibleHeight)
		out.fullRange, out.matrix, out.time = c.fullRange, c.matrix, time
		source := unsafe.Slice(scanline, int(pitch)*c.visibleHeight)
		parallel(c.visibleHeight, func(start, end int) {
			for y := start; y < end; y++ {
				copy(out.luma[y*rowBytes:(y+1)*rowBytes], source[y*int(pitch):])
			}
		})
		return out, nil
	}
	// The chroma plane follows the luma rows of the surface, which may be taller than the frame
	// size; the buffer's length tells how many there are.
	rows := c.frameHeight
	if length > 0 {
		if counted := length * 2 / (3 * int(pitch)); counted >= c.frameHeight {
			rows = counted
		}
	}
	if length > 0 && int(pitch)*(rows+(c.visibleHeight+1)/2) > length {
		return nil, E.New("the decoder's output buffer of ", length, " bytes is too small")
	}
	width := (c.visibleWidth + 1) &^ 1
	out := newPicture(layoutNV12, c.visibleWidth, c.visibleHeight, width, c.visibleHeight)
	out.fullRange, out.matrix, out.time = c.fullRange, c.matrix, time
	source := unsafe.Slice(scanline, int(pitch)*(rows+(c.visibleHeight+1)/2))
	for y := range c.visibleHeight {
		copy(out.luma[y*width:(y+1)*width], source[y*int(pitch):])
	}
	chroma := source[int(pitch)*rows:]
	for y := range (c.visibleHeight + 1) / 2 {
		copy(out.chroma[y*width:(y+1)*width], chroma[y*int(pitch):])
	}
	return out, nil
}

// Close flushes the decoder and releases every COM object it holds, then stops Media
// Foundation for this thread.
func (c *mfCodec) Close() {
	if c.transform != nil {
		c.transform.call(transformProcessMessage, messageFlush, 0)
		c.transform.call(transformProcessMessage, messageEndOfStream, 0)
		c.transform.call(transformProcessMessage, messageEndStreaming, 0)
	}
	c.release()
	if c.stop != nil {
		c.stop()
		c.stop = nil
	}
}

func (c *mfCodec) release() {
	c.output.release()
	c.output = nil
	c.transform.release()
	c.transform = nil
	if c.activate != nil {
		c.activate.call(activateShutdownObject)
		c.activate.release()
		c.activate = nil
	}
	c.manager.release()
	c.manager = nil
	c.device.release()
	c.device = nil
}
