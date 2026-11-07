//go:build windows && (amd64 || arm64)

package feed

import (
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// comObject is a COM interface pointer: its first word points at the method table. Method
// numbers below are the slots of the C vtables in the Windows SDK headers (mfobjects.h,
// mftransform.h, d3d10.h).
type comObject struct {
	vtbl *[1 << 10]uintptr
}

const (
	methodQueryInterface = 0
	methodAddRef         = 1
	methodRelease        = 2
)

// call invokes a method and answers its HRESULT. Pointers converted to uintptr in its arguments
// escape to the heap, so a stack that grows in the call does not move what they point at.
//
//go:uintptrescapes
func (o *comObject) call(method int, args ...uintptr) hresult {
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, uintptr(unsafe.Pointer(o)))
	all = append(all, args...)
	r, _, _ := syscall.SyscallN(o.vtbl[method], all...)
	return hresult(uint32(r))
}

func (o *comObject) release() {
	if o != nil {
		o.call(methodRelease)
	}
}

func (o *comObject) query(iid *windows.GUID) (*comObject, hresult) {
	var out *comObject
	hr := o.call(methodQueryInterface, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out)))
	return out, hr
}

// hresult is a COM status; the negative ones are failures.
type hresult uint32

func (h hresult) failed() bool {
	return int32(h) < 0
}

var hresultNames = map[hresult]string{
	0x80004001:         "E_NOTIMPL",
	0x80004002:         "E_NOINTERFACE",
	0x80004003:         "E_POINTER",
	0x80004005:         "E_FAIL",
	0x8007000E:         "E_OUTOFMEMORY",
	0x80070057:         "E_INVALIDARG",
	0x887A0005:         "DXGI_ERROR_DEVICE_REMOVED",
	hrInvalidMediaType: "MF_E_INVALIDMEDIATYPE",
	hrNotAccepting:     "MF_E_NOTACCEPTING",
	hrNoMoreTypes:      "MF_E_NO_MORE_TYPES",
	hrTypeNotSet:       "MF_E_TRANSFORM_TYPE_NOT_SET",
	hrStreamChange:     "MF_E_TRANSFORM_STREAM_CHANGE",
	hrNeedMoreInput:    "MF_E_TRANSFORM_NEED_MORE_INPUT",
	0xC00D36B2:         "MF_E_INVALIDREQUEST",
	0xC00D36BB:         "MF_E_SHUTDOWN",
	0xC00D36B3:         "MF_E_INVALIDSTREAMNUMBER",
	0xC00D6D77:         "MF_E_TRANSFORM_ASYNC_LOCKED",
	0xC00D3E85:         "MF_E_UNSUPPORTED_D3D_TYPE",
	0xC00D6D74:         "MF_E_TRANSFORM_CONFLICTS_WITH_OTHER_CURRENTLY_ENABLED_FEATURES",
	0xC00D36B1:         "MF_E_UNEXPECTED",
	0xC00D36E6:         "MF_E_NOT_FOUND",
	0xC00D36C4:         "MF_E_UNSUPPORTED_BYTESTREAM_TYPE",
	0xC00D5212:         "MF_E_TOPO_CODEC_NOT_FOUND",
	0xC00D36BC:         "MF_E_NO_SAMPLE_TIMESTAMP",
}

func (h hresult) String() string {
	text := fmt.Sprintf("HRESULT %#08x", uint32(h))
	if name, ok := hresultNames[h]; ok {
		return text + " (" + name + ")"
	}
	if message := strings.TrimSpace(syscall.Errno(h).Error()); message != "" && !strings.HasPrefix(message, "winapi error") {
		return text + " (" + message + ")"
	}
	return text
}

// mfError is a failed Media Foundation or COM call.
type mfError struct {
	operation string
	status    hresult
}

func (e *mfError) Error() string {
	return e.operation + ": " + e.status.String()
}

func check(operation string, hr hresult) error {
	if hr.failed() {
		return &mfError{operation: operation, status: hr}
	}
	return nil
}

// guidArguments passes a GUID by value: x64 passes a 16-byte structure by reference, ARM64 in
// two registers.
func guidArguments(guid *windows.GUID) []uintptr {
	if runtime.GOARCH == "amd64" {
		return []uintptr{uintptr(unsafe.Pointer(guid))}
	}
	words := (*[2]uint64)(unsafe.Pointer(guid))
	return []uintptr{uintptr(words[0]), uintptr(words[1])}
}

// procedureCall calls an exported function that answers an HRESULT; a missing one answers
// HRESULT_FROM_WIN32(ERROR_PROC_NOT_FOUND). Pointers in its arguments escape as in call.
//
//go:uintptrescapes
func procedureCall(procedure *windows.LazyProc, args ...uintptr) hresult {
	if procedure.Find() != nil {
		return 0x8007007F
	}
	r, _, _ := syscall.SyscallN(procedure.Addr(), args...)
	return hresult(uint32(r))
}

func utf16Text(pointer *uint16) string {
	if pointer == nil {
		return ""
	}
	return windows.UTF16PtrToString(pointer)
}
