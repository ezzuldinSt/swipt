//go:build windows && (amd64 || arm64)

package feed

import (
	_ "embed"
	"image"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	E "github.com/sagernet/sing/common/exceptions"

	"golang.org/x/sys/windows"
)

// shaderSource is compiled once, when the first decoder keeps its frames on the GPU, by the
// D3DCompile of d3dcompiler_47.dll, which ships with Windows.
//
//go:embed gpu.hlsl
var shaderSource []byte

var (
	d3dcompiler    = windows.NewLazySystemDLL("d3dcompiler_47.dll")
	procD3DCompile = d3dcompiler.NewProc("D3DCompile")
)

// Method slots of d3d11.h, d3dcommon.h and mfobjects.h of the Windows SDK 10.0.26100.
const (
	blobBufferPointer = 3
	blobBufferSize    = 4

	deviceCreateBuffer              = 3
	deviceCreateTexture2D           = 5
	deviceCreateShaderResourceView  = 7
	deviceCreateUnorderedAccessView = 8
	deviceCreateComputeShader       = 18
	deviceCheckFormatSupport        = 29
	deviceGetImmediateContext       = 40

	contextMap                          = 14
	contextUnmap                        = 15
	contextDispatch                     = 41
	contextCopySubresourceRegion        = 46
	contextUpdateSubresource            = 48
	contextClearUnorderedAccessViewUint = 51
	contextCSSetShaderResources         = 67
	contextCSSetUnorderedAccessViews    = 68
	contextCSSetShader                  = 69
	contextCSSetConstantBuffers         = 71

	texture2DGetDesc = 10

	dxgiBufferGetResource         = 3
	dxgiBufferGetSubresourceIndex = 4

	multithreadEnter = 3
	multithreadLeave = 4
)

const (
	formatAYUV           = 100 // DXGI_FORMAT_AYUV
	formatR8G8B8A8UInt   = 30
	formatR32Typeless    = 39
	usageDefault         = 0
	usageStaging         = 3
	bindConstantBuffer   = 0x4
	bindShaderResource   = 0x8
	bindUnorderedAccess  = 0x80
	cpuAccessRead        = 0x20000
	miscAllowRawViews    = 0x20
	accessViewBuffer     = 1 // D3D11_UAV_DIMENSION_BUFFER
	shaderViewTexture2D  = 4 // D3D11_SRV_DIMENSION_TEXTURE2D
	accessViewRaw        = 1 // D3D11_BUFFER_UAV_FLAG_RAW
	mapRead              = 1
	supportTexture2D     = 0x20
	supportShaderLoad    = 0x100
	compileOptimization3 = 1 << 15
	// A dispatch has at most this many thread groups along a dimension.
	dispatchLimit = 65535
)

var (
	iidDXGIBuffer = guid(0xe7174cfa, 0x1c9e, 0x48b1, 0x88, 0x66, 0x62, 0x62, 0x26, 0xbf, 0xc2, 0x58)
	iidTexture2D  = guid(0x6f15aaf2, 0xd208, 0x4e89, 0x9a, 0xb4, 0x48, 0x95, 0x35, 0xd3, 0x4f, 0x9c)
)

type textureDesc struct {
	width, height, mipLevels, arraySize, format, sampleCount, sampleQuality, usage, bindFlags, cpuAccess, misc uint32
}

type bufferDesc struct {
	byteWidth, usage, bindFlags, cpuAccess, misc, stride uint32
}

type shaderViewDesc struct {
	format, dimension          uint32
	mostDetailedMip, mipLevels uint32
	_                          [2]uint32
}

type accessViewDesc struct {
	format, dimension   uint32
	first, count, flags uint32
}

type copyBox struct {
	left, top, front, right, bottom, back uint32
}

type mappedResource struct {
	data                 *byte
	rowPitch, depthPitch uint32
}

// callValue invokes a method that answers a value rather than an HRESULT.
func (o *comObject) callValue(method int, args ...uintptr) uintptr {
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, uintptr(unsafe.Pointer(o)))
	all = append(all, args...)
	r, _, _ := syscall.SyscallN(o.vtbl[method], all...)
	return r
}

// The compute shaders of gpu.hlsl.
type shaderKind int

const (
	shaderConvert shaderKind = iota
	shaderGray
	shaderAverages
	shaderCompare
	shaderCount
)

var shaderEntries = [shaderCount]string{"convert", "gray", "averages", "compare"}

var compiled struct {
	once sync.Once
	code [shaderCount][]byte
	err  error
}

// compiledShaders compiles gpu.hlsl's shaders once for the process.
func compiledShaders() ([shaderCount][]byte, error) {
	compiled.once.Do(func() {
		if err := d3dcompiler.Load(); err != nil {
			compiled.err = E.Cause(err, "load the shader compiler")
			return
		}
		for kind, entry := range shaderEntries {
			code, err := compileShader(entry)
			if err != nil {
				compiled.err = err
				return
			}
			compiled.code[kind] = code
		}
	})
	return compiled.code, compiled.err
}

func compileShader(entry string) ([]byte, error) {
	name, _ := windows.BytePtrFromString("gpu.hlsl")
	entryName, _ := windows.BytePtrFromString(entry)
	target, _ := windows.BytePtrFromString("cs_5_0")
	var code, messages *comObject
	hr := procedureCall(procD3DCompile, uintptr(unsafe.Pointer(&shaderSource[0])), uintptr(len(shaderSource)), uintptr(unsafe.Pointer(name)), 0, 0,
		uintptr(unsafe.Pointer(entryName)), uintptr(unsafe.Pointer(target)), compileOptimization3, 0, uintptr(unsafe.Pointer(&code)), uintptr(unsafe.Pointer(&messages)))
	defer messages.release()
	if hr.failed() {
		reason := ""
		if messages != nil {
			reason = ": " + string(blobBytes(messages))
		}
		code.release()
		return nil, E.New("compile the ", entry, " shader: ", hr.String(), reason)
	}
	defer code.release()
	return append([]byte(nil), blobBytes(code)...), nil
}

// blobBytes are the bytes an ID3DBlob holds, valid while it is.
func blobBytes(blob *comObject) []byte {
	pointer := blob.callValue(blobBufferPointer)
	size := blob.callValue(blobBufferSize)
	if pointer == 0 || size == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(*(*unsafe.Pointer)(unsafe.Pointer(&pointer))), size)
}

// parameters are gpu.hlsl's constant buffer, ten int4 vectors.
type parameters [10][4]int32

func (p *parameters) setCoefficients(c *coefficients) {
	general := int32(0)
	if c.general {
		general = 1
	}
	p[4] = [4]int32{c.lumaOffset, c.y, c.rv, c.gu}
	p[5] = [4]int32{c.gv, c.bu, general, 0}
	p[6] = [4]int32{c.offsets[0], c.offsets[1], c.offsets[2], 0}
	p[7] = [4]int32{c.m[0], c.m[1], c.m[2], 0}
	p[8] = [4]int32{c.m[3], c.m[4], c.m[5], 0}
	p[9] = [4]int32{c.m[6], c.m[7], c.m[8], 0}
}

// gpuOutput is a buffer the shaders write and the staging copy the CPU reads it from.
type gpuOutput struct {
	buffer, view, staging *comObject
	size                  int
}

func (o *gpuOutput) release() {
	o.view.release()
	o.buffer.release()
	o.staging.release()
	*o = gpuOutput{}
}

// gpuTexture is a texture of the pool that a frame's picture is copied into.
type gpuTexture struct {
	texture, view *comObject
	width, height int
}

func (t *gpuTexture) release() {
	t.view.release()
	t.texture.release()
}

// residentSpares is how many textures the pool keeps for the next frames, as spareBuffers
// for pictures in Go memory.
const residentSpares = spareBuffers

// gpu keeps the pictures of a decoder on its Direct3D 11 device, in textures of its own pool:
// a decoded picture is copied out of Media Foundation's surface on the GPU, and the shaders of
// gpu.hlsl make only the pixels a call reads, which come back through a staging buffer. The
// decoder and every picture hold it; the last one to let go releases it. Its methods may be
// called from any goroutine: the device's immediate context is protected for many threads, and
// a run holds it for the calls that must not interleave with the decoder's.
type gpu struct {
	device, context, multithread *comObject
	shaders                      [shaderCount]*comObject
	constants                    *comObject
	refs                         atomic.Int32

	access       sync.Mutex
	large, small gpuOutput

	pool   sync.Mutex
	spare  []*gpuTexture
	closed bool
}

// newGPU sets up the shaders on the decoder's device, which must load AYUV textures in them.
func newGPU(device *comObject) (*gpu, error) {
	code, err := compiledShaders()
	if err != nil {
		return nil, err
	}
	var support uint32
	if err = check("check the AYUV format", device.call(deviceCheckFormatSupport, formatAYUV, uintptr(unsafe.Pointer(&support)))); err != nil {
		return nil, err
	}
	if support&supportTexture2D == 0 || support&supportShaderLoad == 0 {
		return nil, E.New("the GPU does not load AYUV textures in shaders")
	}
	g := &gpu{device: device}
	device.call(methodAddRef)
	g.refs.Store(1)
	device.call(deviceGetImmediateContext, uintptr(unsafe.Pointer(&g.context)))
	if g.context == nil {
		g.free()
		return nil, E.New("the Direct3D device has no immediate context")
	}
	multithread, hr := device.query(&iidMultithread)
	if err = check("read the device's thread protection", hr); err != nil {
		g.free()
		return nil, err
	}
	g.multithread = multithread
	for kind := range shaderCount {
		if err = check("create the "+shaderEntries[kind]+" shader", device.call(deviceCreateComputeShader, uintptr(unsafe.Pointer(&code[kind][0])), uintptr(len(code[kind])), 0,
			uintptr(unsafe.Pointer(&g.shaders[kind])))); err != nil {
			g.free()
			return nil, err
		}
	}
	desc := bufferDesc{byteWidth: uint32(unsafe.Sizeof(parameters{})), usage: usageDefault, bindFlags: bindConstantBuffer}
	if err = check("create the shaders' constants", device.call(deviceCreateBuffer, uintptr(unsafe.Pointer(&desc)), 0, uintptr(unsafe.Pointer(&g.constants)))); err != nil {
		g.free()
		return nil, err
	}
	return g, nil
}

func (g *gpu) free() {
	g.pool.Lock()
	for _, t := range g.spare {
		t.release()
	}
	g.spare = nil
	g.pool.Unlock()
	g.large.release()
	g.small.release()
	g.constants.release()
	for _, shader := range g.shaders {
		shader.release()
	}
	g.multithread.release()
	g.context.release()
	g.device.release()
}

func (g *gpu) unref() {
	if g.refs.Add(-1) == 0 {
		g.free()
	}
}

// close lets the decoder's hold go: the spare textures go now, and the ones pictures hold as
// they are released.
func (g *gpu) close() {
	g.pool.Lock()
	g.closed = true
	spare := g.spare
	g.spare = nil
	g.pool.Unlock()
	for _, t := range spare {
		t.release()
	}
	g.unref()
}

func (g *gpu) take(width, height int) (*gpuTexture, error) {
	g.pool.Lock()
	for count := len(g.spare); count > 0; count = len(g.spare) {
		t := g.spare[count-1]
		g.spare[count-1] = nil
		g.spare = g.spare[:count-1]
		if t.width == width && t.height == height {
			g.pool.Unlock()
			return t, nil
		}
		t.release()
	}
	g.pool.Unlock()
	t := &gpuTexture{width: width, height: height}
	desc := textureDesc{width: uint32(width), height: uint32(height), mipLevels: 1, arraySize: 1, format: formatAYUV, sampleCount: 1, usage: usageDefault, bindFlags: bindShaderResource}
	if err := check("create a frame texture", g.device.call(deviceCreateTexture2D, uintptr(unsafe.Pointer(&desc)), 0, uintptr(unsafe.Pointer(&t.texture)))); err != nil {
		return nil, err
	}
	view := shaderViewDesc{format: formatR8G8B8A8UInt, dimension: shaderViewTexture2D, mipLevels: 1}
	if err := check("view a frame texture", g.device.call(deviceCreateShaderResourceView, uintptr(unsafe.Pointer(t.texture)), uintptr(unsafe.Pointer(&view)), uintptr(unsafe.Pointer(&t.view)))); err != nil {
		t.release()
		return nil, err
	}
	return t, nil
}

func (g *gpu) give(t *gpuTexture) {
	g.pool.Lock()
	if !g.closed && len(g.spare) < residentSpares {
		g.spare = append(g.spare, t)
		t = nil
	}
	g.pool.Unlock()
	if t != nil {
		t.release()
	}
}

// copyIn copies the top-left width by height of a subresource of a decoder's AYUV texture into
// a texture of the pool; the copy runs on the GPU in order with the decoder's work, so the
// decoder may reuse its surface at once.
func (g *gpu) copyIn(source *comObject, index uint32, width, height int) (*gpuSurface, error) {
	t, err := g.take(width, height)
	if err != nil {
		return nil, err
	}
	region := copyBox{right: uint32(width), bottom: uint32(height), back: 1}
	g.context.call(contextCopySubresourceRegion, uintptr(unsafe.Pointer(t.texture)), 0, 0, 0, 0, uintptr(unsafe.Pointer(source)), uintptr(index), uintptr(unsafe.Pointer(&region)))
	g.refs.Add(1)
	return &gpuSurface{gpu: g, texture: t}, nil
}

func (o *gpuOutput) reserve(device *comObject, size int) error {
	if o.size >= size {
		return nil
	}
	o.release()
	size = (size + 0xffff) &^ 0xffff
	desc := bufferDesc{byteWidth: uint32(size), usage: usageDefault, bindFlags: bindUnorderedAccess, misc: miscAllowRawViews}
	if err := check("create the shaders' output", device.call(deviceCreateBuffer, uintptr(unsafe.Pointer(&desc)), 0, uintptr(unsafe.Pointer(&o.buffer)))); err != nil {
		return err
	}
	view := accessViewDesc{format: formatR32Typeless, dimension: accessViewBuffer, count: uint32(size / 4), flags: accessViewRaw}
	if err := check("view the shaders' output", device.call(deviceCreateUnorderedAccessView, uintptr(unsafe.Pointer(o.buffer)), uintptr(unsafe.Pointer(&view)), uintptr(unsafe.Pointer(&o.view)))); err != nil {
		o.release()
		return err
	}
	desc = bufferDesc{byteWidth: uint32(size), usage: usageStaging, cpuAccess: cpuAccessRead}
	if err := check("create the readback buffer", device.call(deviceCreateBuffer, uintptr(unsafe.Pointer(&desc)), 0, uintptr(unsafe.Pointer(&o.staging)))); err != nil {
		o.release()
		return err
	}
	o.size = size
	return nil
}

// run dispatches a shader over textures with the parameters and reads size bytes of what it
// wrote; read gets them while they are mapped. A small run clears its output first.
func (g *gpu) run(kind shaderKind, textures []*gpuTexture, p *parameters, groupsX, groupsY int, size int, read func(data []byte)) error {
	if size <= 0 || groupsX <= 0 || groupsY <= 0 {
		return nil
	}
	if groupsX > dispatchLimit || groupsY > dispatchLimit {
		return E.New("the region is too large for the GPU")
	}
	g.access.Lock()
	defer g.access.Unlock()
	output := &g.large
	if kind == shaderCompare {
		output = &g.small
	}
	if err := output.reserve(g.device, size); err != nil {
		return err
	}
	var views [2]*comObject
	for index, t := range textures {
		views[index] = t.view
	}
	var none [2]*comObject
	c := g.context
	// The decoder's thread uses the same context; the critical section that protects it
	// belongs to a thread, so the goroutine stays on one while it holds it.
	runtime.LockOSThread()
	g.multithread.call(multithreadEnter)
	c.call(contextUpdateSubresource, uintptr(unsafe.Pointer(g.constants)), 0, 0, uintptr(unsafe.Pointer(p)), 0, 0)
	if kind == shaderCompare {
		var zeros [4]uint32
		c.call(contextClearUnorderedAccessViewUint, uintptr(unsafe.Pointer(output.view)), uintptr(unsafe.Pointer(&zeros)))
	}
	c.call(contextCSSetShader, uintptr(unsafe.Pointer(g.shaders[kind])), 0, 0)
	c.call(contextCSSetShaderResources, 0, uintptr(len(textures)), uintptr(unsafe.Pointer(&views[0])))
	c.call(contextCSSetUnorderedAccessViews, 0, 1, uintptr(unsafe.Pointer(&output.view)), 0)
	c.call(contextCSSetConstantBuffers, 0, 1, uintptr(unsafe.Pointer(&g.constants)))
	c.call(contextDispatch, uintptr(groupsX), uintptr(groupsY), 1)
	c.call(contextCSSetShaderResources, 0, uintptr(len(textures)), uintptr(unsafe.Pointer(&none[0])))
	c.call(contextCSSetUnorderedAccessViews, 0, 1, uintptr(unsafe.Pointer(&none[0])), 0)
	region := copyBox{right: uint32(size), bottom: 1, back: 1}
	c.call(contextCopySubresourceRegion, uintptr(unsafe.Pointer(output.staging)), 0, 0, 0, 0, uintptr(unsafe.Pointer(output.buffer)), 0, uintptr(unsafe.Pointer(&region)))
	g.multithread.call(multithreadLeave)
	runtime.UnlockOSThread()
	var mapped mappedResource
	if err := check("read back the GPU's output", c.call(contextMap, uintptr(unsafe.Pointer(output.staging)), 0, mapRead, 0, uintptr(unsafe.Pointer(&mapped)))); err != nil {
		return err
	}
	read(unsafe.Slice(mapped.data, size))
	c.call(contextUnmap, uintptr(unsafe.Pointer(output.staging)), 0)
	return nil
}

// gpuSurface is a picture kept in a texture of a gpu's pool.
type gpuSurface struct {
	gpu      *gpu
	texture  *gpuTexture
	released atomic.Bool
}

func (s *gpuSurface) bytes() int {
	return s.texture.width * s.texture.height * 4
}

func (s *gpuSurface) release() {
	if s.released.CompareAndSwap(false, true) {
		s.gpu.give(s.texture)
		s.gpu.unref()
	}
}

func (s *gpuSurface) convert(out []byte, rect image.Rectangle, width, height, turns int, c *coefficients) error {
	if rect.Empty() {
		return nil
	}
	origin, across, down := turning(width, height, turns)
	var p parameters
	p[0] = [4]int32{int32(origin[0]), int32(origin[1]), int32(across[0]), int32(across[1])}
	p[1] = [4]int32{int32(down[0]), int32(down[1]), 0, 0}
	p[2] = [4]int32{int32(rect.Min.X), int32(rect.Min.Y), int32(rect.Dx()), int32(rect.Dy())}
	p.setCoefficients(c)
	size := rect.Dx() * rect.Dy() * 4
	return s.gpu.run(shaderConvert, []*gpuTexture{s.texture}, &p, (rect.Dx()+15)/16, (rect.Dy()+15)/16, size, func(data []byte) {
		rowBytes := rect.Dx() * 4
		parallel(rect.Dy(), func(start, end int) {
			copy(out[start*rowBytes:end*rowBytes], data[start*rowBytes:end*rowBytes])
		})
	})
}

func (s *gpuSurface) gray(out []byte, rect image.Rectangle, outWidth, outHeight int, c *coefficients, colors bool) error {
	if outWidth <= 0 || outHeight <= 0 {
		return nil
	}
	var p parameters
	p[2] = [4]int32{int32(rect.Min.X), int32(rect.Min.Y), int32(rect.Dx()), int32(rect.Dy())}
	p[3] = [4]int32{int32(outWidth), int32(outHeight), 0, 0}
	p.setCoefficients(c)
	total := outWidth * outHeight
	if colors {
		return s.gpu.run(shaderAverages, []*gpuTexture{s.texture}, &p, (outWidth+15)/16, (outHeight+15)/16, total*4, func(data []byte) {
			copy(out, data[:total*4])
		})
	}
	groups := ((total+3)/4 + 63) / 64
	groupsX := min(groups, dispatchLimit)
	groupsY := (groups + groupsX - 1) / groupsX
	p[1][2] = int32(groupsX * 64)
	return s.gpu.run(shaderGray, []*gpuTexture{s.texture}, &p, groupsX, groupsY, (total+3)&^3, func(data []byte) {
		copy(out, data[:total])
	})
}

func (s *gpuSurface) differs(other surface, width, height, bandRows int) (uint32, error) {
	o, ok := other.(*gpuSurface)
	if !ok || o.gpu != s.gpu {
		return 0, errOtherSurface
	}
	if width <= 0 || height <= 0 {
		return 0, nil
	}
	var p parameters
	p[2] = [4]int32{0, 0, int32(width), int32(height)}
	p[3] = [4]int32{int32(max(bandRows, 1)), 0, 0, 0}
	var mask uint32
	err := s.gpu.run(shaderCompare, []*gpuTexture{s.texture, o.texture}, &p, (width+15)/16, (height+15)/16, 4, func(data []byte) {
		mask = uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16 | uint32(data[3])<<24
	})
	return mask, err
}

var errOtherSurface = E.New("graphics: the frames are on different GPUs")
