//go:build darwin && cgo

package jsc

/*
#cgo CFLAGS: -fmodules
#cgo LDFLAGS: -framework JavaScriptCore

#include <stdlib.h>
#include "jsc_thread.h"
*/
import "C"

import (
	"context"
	"encoding/json"
	"runtime/cgo"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"github.com/sagernet/sing/common"
)

// JavaScriptCore's collector copies the stack of every thread that ever took
// its lock, from the thread's stack pointer to the origin of the thread's
// stack, into a buffer of the summed size from fastMalloc (WebKit 21bb1f66,
// Source/JavaScriptCore/runtime/JSLock.cpp:144; WebKit 21bb1f66,
// Source/JavaScriptCore/heap/MachineStackMarker.cpp:76, 204). A Go thread runs
// goroutines on stacks in the Go heap, outside its own stack, so a collection
// while a thread that had called in ran Go code took a wrapped size, and
// fastMalloc panicked with pas_panic_on_out_of_memory_error (Mac14,3, macOS
// 27.0.1 26A434).
type Context struct {
	runtime *C.jsc_runtime
}

type Value struct {
	context *Context
	ref     C.JSValueRef
}

func New() *Context {
	c := &Context{runtime: C.jsc_runtime_new()}
	cmd := command(C.JSC_CREATE)
	defer C.free(unsafe.Pointer(cmd))
	c.submit(cmd)
	return c
}

func command(kind C.jsc_kind) *C.jsc_command {
	cmd := (*C.jsc_command)(C.calloc(1, C.size_t(unsafe.Sizeof(C.jsc_command{}))))
	cmd.kind = kind
	return cmd
}

func (c *Context) submit(cmd *C.jsc_command) {
	C.jsc_post(c.runtime, cmd)
	for request := C.jsc_wait(c.runtime, cmd); request != nil; request = C.jsc_wait(c.runtime, cmd) {
		c.serve(request)
	}
	var count C.size_t
	released := C.jsc_take_released(c.runtime, &count)
	for _, handle := range unsafe.Slice(released, int(count)) {
		cgo.Handle(handle).Delete()
	}
	C.free(unsafe.Pointer(released))
}

func (c *Context) serve(request *C.jsc_host_request) {
	function := cgo.Handle(request.handle).Value().(Function)
	arguments := common.Map(unsafe.Slice(request.arguments, int(request.count)), func(ref C.JSValueRef) Value { return Value{c, ref} })
	result, err := function(arguments)
	var data []byte
	if err == nil {
		switch value := result.(type) {
		case nil:
		case Value:
			request.value = value.ref
		case Bytes:
			request.bytes, request.bytes_length = (*C.uint8_t)(C.CBytes(value)), C.size_t(len(value))
		default:
			data, err = json.Marshal(result)
		}
	}
	switch {
	case err != nil:
		request.error = cString(err.Error())
	case data != nil:
		request.json = cString(string(data))
	}
	C.jsc_answer(c.runtime, request)
}

func (c *Context) outcome(cmd *C.jsc_command) (Value, error) {
	if _, err := output(cmd); err != nil {
		return Value{}, err
	}
	return Value{c, cmd.result}, nil
}

func output(cmd *C.jsc_command) (string, error) {
	out := goString(cmd.out)
	if cmd.terminated {
		return "", ErrTerminated
	}
	if cmd.exception {
		return "", &Exception{Message: out}
	}
	return out, nil
}

func (c *Context) TerminateOn(ctx context.Context) (stop func() bool) {
	cmd := command(C.JSC_BEGIN)
	defer C.free(unsafe.Pointer(cmd))
	c.submit(cmd)
	run := cmd.run
	return context.AfterFunc(ctx, func() {
		C.jsc_cancel(c.runtime, run)
	})
}

func (c *Context) SetDescriber(describer Value) {
	cmd := command(C.JSC_SET_DESCRIBER)
	defer C.free(unsafe.Pointer(cmd))
	cmd.value = describer.ref
	c.submit(cmd)
}

func (c *Context) Global() Value {
	cmd := command(C.JSC_GLOBAL)
	defer C.free(unsafe.Pointer(cmd))
	c.submit(cmd)
	return Value{c, cmd.result}
}

func (c *Context) NewObject() Value {
	cmd := command(C.JSC_NEW_OBJECT)
	defer C.free(unsafe.Pointer(cmd))
	c.submit(cmd)
	return Value{c, cmd.result}
}

func (c *Context) NewString(value string) Value {
	cmd := command(C.JSC_NEW_STRING)
	defer C.free(unsafe.Pointer(cmd))
	cmd.text = cString(value)
	c.submit(cmd)
	return Value{c, cmd.result}
}

func (c *Context) Evaluate(source, url string) (Value, error) {
	cmd := command(C.JSC_EVALUATE)
	defer C.free(unsafe.Pointer(cmd))
	cmd.text = cString(source)
	cmd.name = cString(url)
	c.submit(cmd)
	return c.outcome(cmd)
}

func (c *Context) Close() {
	cmd := command(C.JSC_DESTROY)
	defer C.free(unsafe.Pointer(cmd))
	c.submit(cmd)
	C.jsc_runtime_free(c.runtime)
}

func (c *Context) SetUnhandledRejection(function Function) {
	cmd := command(C.JSC_SET_REJECTION)
	defer C.free(unsafe.Pointer(cmd))
	cmd.handle = C.uintptr_t(cgo.NewHandle(function))
	c.submit(cmd)
}

func (v Value) Get(name string) (Value, error) {
	cmd := command(C.JSC_GET)
	defer C.free(unsafe.Pointer(cmd))
	cmd.object = v.ref
	cmd.name = cString(name)
	v.context.submit(cmd)
	return v.context.outcome(cmd)
}

func (v Value) Set(name string, value Value) {
	cmd := command(C.JSC_SET)
	defer C.free(unsafe.Pointer(cmd))
	cmd.object, cmd.value = v.ref, value.ref
	cmd.name = cString(name)
	v.context.submit(cmd)
}

func (v Value) SetFunction(name string, function Function) {
	cmd := command(C.JSC_SET_FUNCTION)
	defer C.free(unsafe.Pointer(cmd))
	cmd.object = v.ref
	cmd.name = cString(name)
	cmd.handle = C.uintptr_t(cgo.NewHandle(function))
	v.context.submit(cmd)
}

//go:linkname nanotime runtime.nanotime
func nanotime() int64

func (v Value) SetClock(name string) time.Time {
	cmd := command(C.JSC_SET_CLOCK)
	defer C.free(unsafe.Pointer(cmd))
	cmd.object = v.ref
	cmd.name = cString(name)
	cmd.native_argument = C.uint64_t(nanotime())
	origin := time.Now()
	v.context.submit(cmd)
	return origin
}

func (v Value) Describe() string {
	cmd := command(C.JSC_DESCRIBE)
	defer C.free(unsafe.Pointer(cmd))
	cmd.value = v.ref
	v.context.submit(cmd)
	return goString(cmd.out)
}

func (v Value) Call(arguments ...Value) (Value, error) {
	cmd := command(C.JSC_CALL)
	defer C.free(unsafe.Pointer(cmd))
	cmd.object = v.ref
	if len(arguments) > 0 {
		refs := (*C.JSValueRef)(C.malloc(C.size_t(len(arguments)) * C.size_t(unsafe.Sizeof(C.JSValueRef(nil)))))
		defer C.free(unsafe.Pointer(refs))
		slots := unsafe.Slice(refs, len(arguments))
		for index, argument := range arguments {
			slots[index] = argument.ref
		}
		cmd.arguments, cmd.count = refs, C.size_t(len(arguments))
	}
	v.context.submit(cmd)
	return v.context.outcome(cmd)
}

func (v Value) ToString() (string, error) {
	cmd := command(C.JSC_TO_STRING)
	defer C.free(unsafe.Pointer(cmd))
	cmd.value = v.ref
	v.context.submit(cmd)
	return output(cmd)
}

func (v Value) IsFunction() bool {
	cmd := command(C.JSC_IS_FUNCTION)
	defer C.free(unsafe.Pointer(cmd))
	cmd.value = v.ref
	v.context.submit(cmd)
	return bool(cmd.flag)
}

func (v Value) Number() float64 {
	cmd := command(C.JSC_NUMBER)
	defer C.free(unsafe.Pointer(cmd))
	cmd.value = v.ref
	v.context.submit(cmd)
	return float64(cmd.number)
}

func (v Value) Bytes() ([]byte, bool) {
	cmd := command(C.JSC_BYTES)
	defer C.free(unsafe.Pointer(cmd))
	cmd.value = v.ref
	v.context.submit(cmd)
	if !cmd.flag {
		return nil, false
	}
	defer C.free(unsafe.Pointer(cmd.data))
	return C.GoBytes(unsafe.Pointer(cmd.data), C.int(cmd.data_length)), true
}

func (v Value) Protect() {
	cmd := command(C.JSC_PROTECT)
	defer C.free(unsafe.Pointer(cmd))
	cmd.value = v.ref
	v.context.submit(cmd)
}

func (v Value) Release() {
	cmd := command(C.JSC_UNPROTECT)
	defer C.free(unsafe.Pointer(cmd))
	cmd.value = v.ref
	v.context.submit(cmd)
}

func (c *Context) RunNative(function uintptr, kind int32, value Value, argument uint64) {
	cmd := command(C.JSC_NATIVE)
	defer C.free(unsafe.Pointer(cmd))
	cmd.native, cmd.native_command, cmd.value, cmd.native_argument = C.uintptr_t(function), C.int32_t(kind), value.ref, C.uint64_t(argument)
	c.submit(cmd)
}

// JavaScriptCore keeps an all-ASCII string in 8 bits, and makes an empty
// string of invalid UTF-8 (WebKit 21bb1f66,
// Source/JavaScriptCore/API/JSStringRef.cpp:44; WebKit 21bb1f66,
// Source/WTF/wtf/text/StringImpl.cpp:267).
func cString(value string) C.jsc_text {
	if utf8.ValidString(value) && !strings.ContainsRune(value, 0) {
		return C.jsc_text{data: unsafe.Pointer(C.CString(value)), length: C.size_t(len(value))}
	}
	units := unsafe.Slice((*uint16)(C.malloc(C.size_t(len(value))*C.size_t(unsafe.Sizeof(C.JSChar(0))))), len(value))[:0]
	for _, r := range value {
		units = utf16.AppendRune(units, r)
	}
	return C.jsc_text{data: unsafe.Pointer(unsafe.SliceData(units)), length: C.size_t(len(units)), wide: true}
}

func goString(text C.jsc_text) string {
	defer C.free(text.data)
	if !text.wide {
		return string(unsafe.Slice((*byte)(text.data), int(text.length)))
	}
	return string(utf16.Decode(unsafe.Slice((*uint16)(text.data), int(text.length))))
}
