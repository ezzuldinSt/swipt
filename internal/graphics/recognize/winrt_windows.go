//go:build windows

package recognize

import (
	"context"
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	E "github.com/sagernet/sing/common/exceptions"

	"golang.org/x/sys/windows"
)

var (
	combase                       = windows.NewLazySystemDLL("combase.dll")
	procRoInitialize              = combase.NewProc("RoInitialize")
	procRoUninitialize            = combase.NewProc("RoUninitialize")
	procRoGetActivationFactory    = combase.NewProc("RoGetActivationFactory")
	procWindowsCreateString       = combase.NewProc("WindowsCreateString")
	procWindowsDeleteString       = combase.NewProc("WindowsDeleteString")
	procWindowsGetStringRawBuffer = combase.NewProc("WindowsGetStringRawBuffer")
)

// Interface IDs from the Windows SDK headers named beside each.
var (
	iidAsyncInfo             = windows.GUID{Data1: 0x00000036, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xc0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}} // asyncinfo.h
	iidLanguageFactory       = windows.GUID{Data1: 0x9b0252ac, Data2: 0x0c27, Data3: 0x44f8, Data4: [8]byte{0xb7, 0x92, 0x97, 0x93, 0xfb, 0x66, 0xc6, 0x3e}} // windows.globalization.h
	iidOcrEngineStatics      = windows.GUID{Data1: 0x5bffa85a, Data2: 0x3384, Data3: 0x3540, Data4: [8]byte{0x99, 0x40, 0x69, 0x91, 0x20, 0xd4, 0x28, 0xa8}} // windows.media.ocr.h
	iidClosable              = windows.GUID{Data1: 0x30d5a829, Data2: 0x7fa4, Data3: 0x4026, Data4: [8]byte{0x83, 0xbb, 0xd7, 0x5b, 0xae, 0x4e, 0xa9, 0x9e}} // windows.foundation.h
	iidMemoryBuffer          = windows.GUID{Data1: 0xfbc4dd2a, Data2: 0x245b, Data3: 0x11e4, Data4: [8]byte{0xaf, 0x98, 0x68, 0x94, 0x23, 0x26, 0x0c, 0xf8}} // windows.foundation.h
	iidMemoryByteAccess      = windows.GUID{Data1: 0x5b0d3235, Data2: 0x4dba, Data3: 0x4d44, Data4: [8]byte{0x86, 0x5e, 0x8f, 0x1d, 0x0e, 0x4f, 0xd0, 0x4d}} // MemoryBuffer.h
	iidSoftwareBitmapFactory = windows.GUID{Data1: 0xc99feb69, Data2: 0x2d62, Data3: 0x4d47, Data4: [8]byte{0xa6, 0xb3, 0x4f, 0xdb, 0x6a, 0x07, 0xfd, 0xf8}} // windows.graphics.imaging.h
)

const (
	roInitMultithreaded = 1
	sFalse              = 1
	rpcEChangedMode     = 0x80010106

	asyncCompleted = 1
	asyncCanceled  = 2
	asyncError     = 3
)

type hresult uint32

func (h hresult) Error() string {
	return fmt.Sprintf("%s (0x%08X)", windows.Errno(h).Error(), uint32(h))
}

func failed(r uintptr) error {
	if int32(r) < 0 {
		return hresult(r)
	}
	return nil
}

// enter joins the calling thread to the multithreaded apartment for a call into Windows Runtime;
// leave uninitializes only if this join counted.
func enter() (leave func(), err error) {
	runtime.LockOSThread()
	r, _, _ := procRoInitialize.Call(roInitMultithreaded)
	switch {
	case r == 0 || r == sFalse:
		return func() {
			procRoUninitialize.Call()
			runtime.UnlockOSThread()
		}, nil
	case uint32(r) == rpcEChangedMode:
		// The thread's owner made it a single-threaded apartment; Windows Runtime works there too.
		return runtime.UnlockOSThread, nil
	default:
		runtime.UnlockOSThread()
		return nil, E.Cause(hresult(r), "graphics: initialize Windows Runtime")
	}
}

// object is a COM interface pointer: its first word points at the interface's method table, where
// IUnknown takes slots 0 to 2 and IInspectable 3 to 5.
type object struct {
	methods *[16]uintptr
}

//go:uintptrescapes
func (o *object) call(method int, args ...uintptr) error {
	r, _, _ := syscall.SyscallN(o.methods[method], append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)...)
	return failed(r)
}

func (o *object) release() {
	if o != nil {
		syscall.SyscallN(o.methods[2], uintptr(unsafe.Pointer(o)))
	}
}

func (o *object) query(iid *windows.GUID) (*object, error) {
	var out *object
	return out, o.call(0, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out)))
}

// text reads the HSTRING a getter in slot method returns.
func (o *object) text(method int) (string, error) {
	var value hstring
	if err := o.call(method, uintptr(unsafe.Pointer(&value))); err != nil {
		return "", err
	}
	defer value.delete()
	return value.String(), nil
}

// size and at read an IVectorView<T> (windows.foundation.collections.h): get_Size is slot 7, GetAt 6.
func (o *object) size() (uint32, error) {
	var size uint32
	return size, o.call(7, uintptr(unsafe.Pointer(&size)))
}

func (o *object) at(index uint32) (*object, error) {
	var item *object
	return item, o.call(6, uintptr(index), uintptr(unsafe.Pointer(&item)))
}

func activate(class string, iid *windows.GUID) (*object, error) {
	name, err := newHString(class)
	if err != nil {
		return nil, err
	}
	defer name.delete()
	var factory *object
	r, _, _ := procRoGetActivationFactory.Call(uintptr(name), uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&factory)))
	if err := failed(r); err != nil {
		return nil, E.Cause(err, "activate ", class)
	}
	return factory, nil
}

type hstring uintptr

func newHString(value string) (hstring, error) {
	units := utf16.Encode([]rune(value))
	var first *uint16
	if len(units) > 0 {
		first = &units[0]
	}
	var out hstring
	r, _, _ := procWindowsCreateString.Call(uintptr(unsafe.Pointer(first)), uintptr(len(units)), uintptr(unsafe.Pointer(&out)))
	return out, failed(r)
}

func (h hstring) String() string {
	var length uint32
	address, _, _ := procWindowsGetStringRawBuffer.Call(uintptr(h), uintptr(unsafe.Pointer(&length)))
	if address == 0 || length == 0 {
		return ""
	}
	// Reading the address through its variable keeps vet's unsafeptr check off a uintptr the call returned.
	return string(utf16.Decode(unsafe.Slice(*(**uint16)(unsafe.Pointer(&address)), length)))
}

func (h hstring) delete() {
	procWindowsDeleteString.Call(uintptr(h))
}

// wait polls an IAsyncOperation's IAsyncInfo (asyncinfo.h: get_Status 7, get_ErrorCode 8, Cancel 9)
// until it settles; polling spares implementing the parameterized completion delegate in Go.
func wait(ctx context.Context, operation *object) error {
	info, err := operation.query(&iidAsyncInfo)
	if err != nil {
		return err
	}
	defer info.release()
	delay := time.Millisecond
	for {
		var status int32
		if err := info.call(7, uintptr(unsafe.Pointer(&status))); err != nil {
			return err
		}
		switch status {
		case asyncCompleted:
			return nil
		case asyncCanceled:
			return E.New("the operation was canceled")
		case asyncError:
			var code uint32
			if err := info.call(8, uintptr(unsafe.Pointer(&code))); err != nil {
				return err
			}
			return hresult(code)
		}
		select {
		case <-ctx.Done():
			info.call(9)
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, 16*time.Millisecond)
	}
}
