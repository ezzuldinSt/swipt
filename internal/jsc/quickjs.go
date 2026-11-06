//go:build !darwin || !cgo

package jsc

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
	"unsafe"

	E "github.com/sagernet/sing/common/exceptions"

	"modernc.org/libc"
	lib "modernc.org/libquickjs"
)

// Without JavaScriptCore the scripts run on QuickJS, translated to Go by
// modernc.org/libquickjs, behind the interface of jsc.go: one goroutine owns
// the engine, a host function runs on the goroutine whose call into the
// engine reached it, and calls made from a host function are served while the
// engine waits for its answer.
type Context struct {
	commands  chan *command
	cancelled atomic.Uint64
	external  atomic.Int64
	collect   atomic.Bool

	// The rest belongs to the engine's goroutine.
	tls          *libc.TLS
	runtime      uintptr
	context      uintptr
	scratch      uintptr
	running      []*command
	protected    map[uintptr]protection
	describer    lib.TJSValue
	uint8Array   lib.TJSValue
	isUint8Array lib.TJSValue
	errorClass   lib.TJSValue
	rejection    Function
	rejected     []rejection
	run          uint64
	terminated   bool
	quit         bool
}

type Value struct {
	context *Context
	ref     lib.TJSValue
}

type command struct {
	run      func()
	requests chan *request
	done     chan struct{}
}

type request struct {
	function  Function
	arguments []Value
	result    any
	err       error
	done      chan struct{}
}

type protection struct {
	value lib.TJSValue
	count int
}

type rejection struct {
	promise lib.TJSValue
	reason  lib.TJSValue
}

type host struct {
	context  *Context
	function Function
	clock    bool
	origin   int64
	payload  any
	finalize func()
}

const (
	// The external memory that makes the outermost call collect when it returns.
	externalCollection = 256 << 20

	valueSize    = unsafe.Sizeof(lib.TJSValue{})
	scratchWords = 4
	jsonFile     = "<json>"
)

var (
	undefined = lib.TJSValue{Ftag: lib.EJS_TAG_UNDEFINED}

	hostAccess  sync.Mutex
	hosts       = map[uintptr]*host{}
	lastHost    uintptr
	runtimes    sync.Map
	classes     sync.Once
	hostClass   lib.TJSClassID
	objectClass lib.TJSClassID
)

// The engine's prototypes and constructors, taken before any script can
// replace them. A Uint8Array is told by the [[TypedArrayName]] that the
// %TypedArray%.prototype[@@toStringTag] getter reads (ECMA-262 23.2.3.38),
// which subclasses keep, like JSValueGetTypedArrayType.
const intrinsics = `(() => {
	const tag = Object.getOwnPropertyDescriptor(Object.getPrototypeOf(Uint8Array.prototype), Symbol.toStringTag).get;
	return [Uint8Array, value => tag.call(value) === "Uint8Array", Error, Function.prototype, Object.prototype];
})()`

func New() *Context {
	c := &Context{commands: make(chan *command), protected: make(map[uintptr]protection)}
	go c.loop()
	c.submit(c.create)
	return c
}

func (c *Context) loop() {
	for !c.quit {
		c.execute(<-c.commands)
	}
}

func (c *Context) execute(cmd *command) {
	c.running = append(c.running, cmd)
	cmd.run()
	c.running = c.running[:len(c.running)-1]
	close(cmd.done)
}

func (c *Context) submit(run func()) {
	cmd := &command{run: run, requests: make(chan *request), done: make(chan struct{})}
	c.commands <- cmd
	for {
		select {
		case r := <-cmd.requests:
			r.answer()
		case <-cmd.done:
			return
		}
	}
}

func (r *request) answer() {
	answered := false
	defer func() {
		if !answered {
			r.err = E.New("jsc: the host function panicked")
			close(r.done)
		}
	}()
	r.result, r.err = r.function(r.arguments)
	answered = true
	close(r.done)
}

// serve runs function on the goroutine that submitted the innermost running
// command, which waits in submit, and runs the commands that function submits
// in turn until it answers.
func (c *Context) serve(function Function, arguments []Value) (any, error) {
	if len(c.running) == 0 {
		return function(arguments)
	}
	r := &request{function: function, arguments: arguments, done: make(chan struct{})}
	c.running[len(c.running)-1].requests <- r
	for {
		select {
		case <-r.done:
			return r.result, r.err
		case cmd := <-c.commands:
			c.execute(cmd)
		}
	}
}

func (c *Context) create() {
	c.tls = libc.NewTLS()
	classes.Do(func() {
		lib.XJS_NewClassID(c.tls, uintptr(unsafe.Pointer(&hostClass)))
		lib.XJS_NewClassID(c.tls, uintptr(unsafe.Pointer(&objectClass)))
	})
	c.runtime = lib.XJS_NewRuntime(c.tls)
	// The stack limit of the Go port counts libc.TLS stack slots, about one a
	// call frame (modernc.org/libquickjs v0.14.0, libquickjs.go:64).
	lib.XJS_SetMaxStackSize(c.tls, c.runtime, lib.Tsize_t(lib.MaxStackSlots))
	c.newClass(hostClass, "Function", fp(callHost), fp(finalizeHost))
	c.newClass(objectClass, "Object", 0, fp(finalizeObject))
	c.context = lib.XJS_NewContext(c.tls, c.runtime)
	runtimes.Store(c.runtime, c)
	lib.XJS_SetInterruptHandler(c.tls, c.runtime, fp(interrupt), 0)
	lib.XJS_SetHostPromiseRejectionTracker(c.tls, c.runtime, fp(trackRejection), 0)
	c.scratch = libc.Xcalloc(c.tls, scratchWords, 8)
	list := c.evaluate(intrinsics, "intrinsics.js")
	item := func(index uint32) lib.TJSValue {
		return lib.XJS_GetPropertyUint32(c.tls, c.context, list, index)
	}
	c.uint8Array, c.isUint8Array, c.errorClass = item(0), item(1), item(2)
	lib.XJS_SetClassProto(c.tls, c.context, hostClass, item(3))
	lib.XJS_SetClassProto(c.tls, c.context, objectClass, item(4))
	c.free(list)
}

func (c *Context) newClass(id lib.TJSClassID, name string, call, finalizer uintptr) {
	definition := libc.Xcalloc(c.tls, 1, libc.Tsize_t(unsafe.Sizeof(lib.TJSClassDef{})))
	className := c.cString(name)
	*(*lib.TJSClassDef)(unsafe.Pointer(definition)) = lib.TJSClassDef{Fclass_name: className, Ffinalizer: finalizer, Fcall: call}
	lib.XJS_NewClass(c.tls, c.runtime, id, definition)
	libc.Xfree(c.tls, className)
	libc.Xfree(c.tls, definition)
}

func (c *Context) Close() {
	c.submit(func() {
		for _, held := range c.protected {
			for range held.count {
				c.free(held.value)
			}
		}
		clear(c.protected)
		for _, it := range c.rejected {
			c.free(it.promise)
			c.free(it.reason)
		}
		c.rejected = nil
		for _, value := range []lib.TJSValue{c.describer, c.uint8Array, c.isUint8Array, c.errorClass} {
			c.free(value)
		}
		lib.XJS_FreeContext(c.tls, c.context)
		lib.XJS_FreeRuntime(c.tls, c.runtime)
		runtimes.Delete(c.runtime)
		libc.Xfree(c.tls, c.scratch)
		c.tls.Close()
		c.quit = true
	})
}

func (c *Context) TerminateOn(ctx context.Context) (stop func() bool) {
	var run uint64
	c.submit(func() {
		c.run++
		run = c.run
		c.terminated = false
	})
	return context.AfterFunc(ctx, func() {
		c.cancelled.Store(run)
	})
}

func (c *Context) SetDescriber(describer Value) {
	c.submit(func() {
		c.free(c.describer)
		c.describer = c.dup(describer.ref)
	})
}

func (c *Context) SetUnhandledRejection(function Function) {
	c.submit(func() {
		c.rejection = function
	})
}

func (c *Context) Global() Value {
	var global lib.TJSValue
	c.submit(func() {
		// The global object lives as long as the context, so it is handed out
		// unprotected, like JSContextGetGlobalObject.
		global = lib.XJS_GetGlobalObject(c.tls, c.context)
		c.free(global)
	})
	return Value{c, global}
}

func (c *Context) NewObject() Value {
	var object lib.TJSValue
	c.submit(func() {
		object = lib.XJS_NewObjectProto(c.tls, c.context, lib.TJSValue{Ftag: lib.EJS_TAG_NULL})
		c.hold(object)
	})
	return Value{c, object}
}

func (c *Context) NewString(value string) Value {
	var text lib.TJSValue
	c.submit(func() {
		text = c.newString(value)
		c.hold(text)
	})
	return Value{c, text}
}

// NewHostObject returns an object that carries payload for HostObject, and
// calls finalize once the engine collects it.
func (c *Context) NewHostObject(payload any, finalize func()) Value {
	var object lib.TJSValue
	c.submit(func() {
		object = c.newHost(objectClass, &host{payload: payload, finalize: finalize})
		c.hold(object)
	})
	return Value{c, object}
}

// ReportExternalCost tells the engine about memory that its objects keep alive outside it, like
// JSReportExtraMemoryCost: QuickJS counts only its own allocations, so a cycle that holds a large
// host object would otherwise wait for its next collection long after the memory ran out.
func (c *Context) ReportExternalCost(bytes int64) {
	if bytes > 0 && c.external.Add(bytes) >= externalCollection {
		c.external.Store(0)
		c.collect.Store(true)
	}
}

func (c *Context) Evaluate(source, url string) (Value, error) {
	var out Value
	var err error
	c.submit(func() {
		if c.terminated {
			err = ErrTerminated
			return
		}
		out, err = c.finish(c.evaluate(source, url))
	})
	return out, err
}

func (c *Context) evaluate(source, url string) lib.TJSValue {
	input := c.cString(source)
	name := c.cString(url)
	value := lib.XJS_Eval(c.tls, c.context, input, lib.Tsize_t(len(source)), name, lib.MJS_EVAL_TYPE_GLOBAL)
	libc.Xfree(c.tls, input)
	libc.Xfree(c.tls, name)
	return value
}

func (v Value) Get(name string) (Value, error) {
	c := v.context
	var out Value
	var err error
	c.submit(func() {
		if c.terminated {
			err = ErrTerminated
			return
		}
		key := c.cString(name)
		value := lib.XJS_GetPropertyStr(c.tls, c.context, v.ref, key)
		libc.Xfree(c.tls, key)
		out, err = c.finish(value)
	})
	return out, err
}

func (v Value) Set(name string, value Value) {
	v.context.submit(func() {
		v.context.define(v.ref, name, v.context.dup(value.ref))
	})
}

func (v Value) SetFunction(name string, function Function) {
	c := v.context
	c.submit(func() {
		c.define(v.ref, name, c.newHost(hostClass, &host{function: function}))
	})
}

//go:linkname nanotime runtime.nanotime
func nanotime() int64

func (v Value) SetClock(name string) time.Time {
	c := v.context
	start := nanotime()
	origin := time.Now()
	c.submit(func() {
		c.define(v.ref, name, c.newHost(hostClass, &host{clock: true, origin: start}))
	})
	return origin
}

func (v Value) Describe() string {
	var text string
	v.context.submit(func() {
		text = v.context.describe(v.ref)
	})
	return text
}

func (v Value) Call(arguments ...Value) (Value, error) {
	c := v.context
	var out Value
	var err error
	c.submit(func() {
		if c.terminated {
			err = ErrTerminated
			return
		}
		refs := make([]lib.TJSValue, len(arguments))
		for index, argument := range arguments {
			refs[index] = argument.value()
		}
		argv := c.values(refs)
		value := lib.XJS_Call(c.tls, c.context, v.ref, undefined, int32(len(refs)), argv)
		libc.Xfree(c.tls, argv)
		out, err = c.finish(value)
	})
	return out, err
}

func (v Value) ToString() (string, error) {
	c := v.context
	var text string
	var err error
	c.submit(func() {
		if c.terminated {
			err = ErrTerminated
			return
		}
		var converted bool
		text, converted = c.goString(v.ref)
		var exception lib.TJSValue
		if !converted {
			exception = lib.XJS_GetException(c.tls, c.context)
		}
		c.checkpoint()
		switch {
		case c.terminated:
			text, err = "", ErrTerminated
		case !converted:
			err = &Exception{Message: c.describe(exception)}
		}
		c.free(exception)
	})
	return text, err
}

func (v Value) IsFunction() bool {
	var function bool
	v.context.submit(func() {
		function = lib.XJS_IsFunction(v.context.tls, v.context.context, v.ref) != 0
	})
	return function
}

func (v Value) Number() float64 {
	c := v.context
	number := math.NaN()
	c.submit(func() {
		if lib.XJS_ToFloat64(c.tls, c.context, c.scratch, v.ref) < 0 {
			c.free(lib.XJS_GetException(c.tls, c.context))
			return
		}
		number = *(*float64)(unsafe.Pointer(c.scratch))
	})
	return number
}

func (v Value) Bytes() ([]byte, bool) {
	c := v.context
	var data []byte
	var loaded bool
	c.submit(func() {
		argv := c.values([]lib.TJSValue{v.ref})
		test := lib.XJS_Call(c.tls, c.context, c.isUint8Array, undefined, 1, argv)
		libc.Xfree(c.tls, argv)
		if c.failed(test) {
			return
		}
		isBytes := lib.XJS_ToBool(c.tls, c.context, test) == 1
		c.free(test)
		if !isBytes {
			return
		}
		offset, length := c.scratch, c.scratch+8
		buffer := lib.XJS_GetTypedArrayBuffer(c.tls, c.context, v.ref, offset, length, c.scratch+16)
		if c.failed(buffer) {
			return
		}
		defer c.free(buffer)
		start, size := *(*lib.Tsize_t)(unsafe.Pointer(offset)), *(*lib.Tsize_t)(unsafe.Pointer(length))
		if size == 0 {
			data, loaded = []byte{}, true
			return
		}
		pointer := lib.XJS_GetArrayBuffer(c.tls, c.context, c.scratch+24, buffer)
		if pointer == 0 {
			c.free(lib.XJS_GetException(c.tls, c.context))
			return
		}
		data = append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(pointer+uintptr(start))), size)...)
		loaded = true
	})
	return data, loaded
}

// HostObject returns the payload of an object from NewHostObject.
func (v Value) HostObject() (any, bool) {
	var payload any
	var found bool
	v.context.submit(func() {
		if v.ref.Ftag != lib.EJS_TAG_OBJECT {
			return
		}
		if it := lookup(lib.XJS_GetOpaque(v.context.tls, v.ref, objectClass)); it != nil {
			payload, found = it.payload, true
		}
	})
	return payload, found
}

func (v Value) Protect() {
	v.context.submit(func() {
		v.context.hold(v.context.dup(v.ref))
	})
}

func (v Value) Release() {
	v.context.submit(func() {
		v.context.unhold(v.ref)
	})
}

func (v Value) value() lib.TJSValue {
	if v.context == nil {
		return undefined
	}
	return v.ref
}

// finish answers a call into the engine like jsc_finish: a termination wins
// over the outcome, and an exception is described.
func (c *Context) finish(value lib.TJSValue) (Value, error) {
	var exception lib.TJSValue
	failed := value.Ftag == lib.EJS_TAG_EXCEPTION
	if failed {
		exception = lib.XJS_GetException(c.tls, c.context)
	}
	c.checkpoint()
	switch {
	case c.terminated:
		c.free(exception)
		c.free(value)
		return Value{}, ErrTerminated
	case failed:
		message := c.describe(exception)
		c.free(exception)
		return Value{}, &Exception{Message: message}
	}
	c.hold(value)
	return Value{c, value}, nil
}

// checkpoint runs the jobs the outermost call queued, as JavaScriptCore runs
// its microtasks when that call returns, then reports the rejections nothing
// handled.
func (c *Context) checkpoint() {
	if len(c.running) != 1 {
		return
	}
	defer func() {
		if c.collect.Swap(false) {
			lib.XJS_RunGC(c.tls, c.runtime)
		}
	}()
	for !c.terminated {
		switch ran := lib.XJS_ExecutePendingJob(c.tls, c.runtime, c.scratch); {
		case ran < 0:
			job := *(*uintptr)(unsafe.Pointer(c.scratch))
			c.free(lib.XJS_GetException(c.tls, job))
		case ran == 0:
			if len(c.rejected) == 0 {
				return
			}
			c.report()
		}
	}
}

func (c *Context) report() {
	rejected := c.rejected
	c.rejected = nil
	for _, it := range rejected {
		if c.rejection != nil && !c.terminated {
			_, _ = c.serve(c.rejection, []Value{{c, it.promise}, {c, it.reason}})
		}
		c.free(it.promise)
		c.free(it.reason)
	}
}

func (c *Context) describe(value lib.TJSValue) string {
	described, owned := value, false
	if c.describer.Ftag == lib.EJS_TAG_OBJECT {
		argv := c.values([]lib.TJSValue{value})
		result := lib.XJS_Call(c.tls, c.context, c.describer, undefined, 1, argv)
		libc.Xfree(c.tls, argv)
		if !c.failed(result) {
			described, owned = result, true
		}
	}
	text, converted := c.goString(described)
	if !converted {
		c.free(lib.XJS_GetException(c.tls, c.context))
	}
	if owned {
		c.free(described)
	}
	return text
}

func (c *Context) define(object lib.TJSValue, name string, value lib.TJSValue) {
	key := c.cString(name)
	// Read-only and not deletable, like kJSPropertyAttributeReadOnly |
	// kJSPropertyAttributeDontDelete; a failure is dropped like JSObjectSetProperty
	// drops it without an exception argument.
	if lib.XJS_DefinePropertyValueStr(c.tls, c.context, object, key, value, lib.MJS_PROP_ENUMERABLE) < 0 {
		c.free(lib.XJS_GetException(c.tls, c.context))
	}
	libc.Xfree(c.tls, key)
}

func (c *Context) newHost(class lib.TJSClassID, it *host) lib.TJSValue {
	it.context = c
	object := lib.XJS_NewObjectClass(c.tls, c.context, int32(class))
	if object.Ftag == lib.EJS_TAG_EXCEPTION {
		return object
	}
	hostAccess.Lock()
	lastHost++
	id := lastHost
	hosts[id] = it
	hostAccess.Unlock()
	lib.XJS_SetOpaque(c.tls, object, id)
	return object
}

func lookup(id uintptr) *host {
	hostAccess.Lock()
	defer hostAccess.Unlock()
	return hosts[id]
}

func release(id uintptr) *host {
	hostAccess.Lock()
	defer hostAccess.Unlock()
	it := hosts[id]
	delete(hosts, id)
	return it
}

// answer turns what a host function returned into its result, like
// jsc_call_host.
func (c *Context) answer(result any, err error) lib.TJSValue {
	if err == nil {
		switch value := result.(type) {
		case nil:
			return undefined
		case Value:
			if value.context == nil {
				return undefined
			}
			return c.transfer(value.ref)
		case Bytes:
			return c.newBytes(value)
		default:
			var data []byte
			if data, err = json.Marshal(result); err == nil {
				return c.parseJSON(data)
			}
		}
	}
	message := c.newString(err.Error())
	argv := c.values([]lib.TJSValue{message})
	exception := lib.XJS_CallConstructor(c.tls, c.context, c.errorClass, 1, argv)
	libc.Xfree(c.tls, argv)
	c.free(message)
	if exception.Ftag == lib.EJS_TAG_EXCEPTION {
		return exception
	}
	return lib.XJS_Throw(c.tls, c.context, exception)
}

func (c *Context) newBytes(data []byte) lib.TJSValue {
	buffer := libc.Xmalloc(c.tls, libc.Tsize_t(max(len(data), 1)))
	copy(unsafe.Slice((*byte)(unsafe.Pointer(buffer)), len(data)), data)
	arrayBuffer := lib.XJS_NewArrayBufferCopy(c.tls, c.context, buffer, lib.Tsize_t(len(data)))
	libc.Xfree(c.tls, buffer)
	if arrayBuffer.Ftag == lib.EJS_TAG_EXCEPTION {
		return arrayBuffer
	}
	argv := c.values([]lib.TJSValue{arrayBuffer})
	array := lib.XJS_CallConstructor(c.tls, c.context, c.uint8Array, 1, argv)
	libc.Xfree(c.tls, argv)
	c.free(arrayBuffer)
	return array
}

func (c *Context) parseJSON(data []byte) lib.TJSValue {
	buffer := c.cString(string(data))
	name := c.cString(jsonFile)
	value := lib.XJS_ParseJSON(c.tls, c.context, buffer, lib.Tsize_t(len(data)), name)
	libc.Xfree(c.tls, buffer)
	libc.Xfree(c.tls, name)
	return value
}

// hold records a reference Go owns, which Release gives back and Close frees.
func (c *Context) hold(value lib.TJSValue) {
	if !counted(value) {
		return
	}
	pointer := pointerOf(value)
	held := c.protected[pointer]
	held.value = value
	held.count++
	c.protected[pointer] = held
}

// unhold gives back a reference Go owns; like JSValueUnprotect, it does
// nothing to a value that is not protected.
func (c *Context) unhold(value lib.TJSValue) bool {
	if !counted(value) {
		return false
	}
	pointer := pointerOf(value)
	held, found := c.protected[pointer]
	if !found {
		return false
	}
	if held.count--; held.count == 0 {
		delete(c.protected, pointer)
	} else {
		c.protected[pointer] = held
	}
	c.free(value)
	return true
}

// transfer hands a value to the engine as a host function's result, which
// takes one protection like jsc_call_host.
func (c *Context) transfer(value lib.TJSValue) lib.TJSValue {
	if !counted(value) {
		return value
	}
	pointer := pointerOf(value)
	held, found := c.protected[pointer]
	if !found {
		return c.dup(value)
	}
	if held.count--; held.count == 0 {
		delete(c.protected, pointer)
	} else {
		c.protected[pointer] = held
	}
	return value
}

func (c *Context) dup(value lib.TJSValue) lib.TJSValue {
	if !counted(value) {
		return value
	}
	return lib.XDupValue(c.tls, c.context, value)
}

func (c *Context) free(value lib.TJSValue) {
	if counted(value) {
		lib.XFreeValue(c.tls, c.context, value)
	}
}

// failed drops the exception of a failed call.
func (c *Context) failed(value lib.TJSValue) bool {
	if value.Ftag != lib.EJS_TAG_EXCEPTION {
		return false
	}
	c.free(lib.XJS_GetException(c.tls, c.context))
	return true
}

func counted(value lib.TJSValue) bool {
	return value.Ftag < 0
}

func pointerOf(value lib.TJSValue) uintptr {
	return *(*uintptr)(unsafe.Pointer(&value.Fu))
}

func (c *Context) values(refs []lib.TJSValue) uintptr {
	argv := libc.Xmalloc(c.tls, libc.Tsize_t(max(len(refs), 1))*libc.Tsize_t(valueSize))
	copy(unsafe.Slice((*lib.TJSValue)(unsafe.Pointer(argv)), len(refs)), refs)
	return argv
}

// newString makes a string of the runes of value, which turns each byte of
// invalid UTF-8 into U+FFFD like cString in jsc.go.
func (c *Context) newString(value string) lib.TJSValue {
	if !utf8.ValidString(value) {
		valid := make([]rune, 0, len(value))
		for _, r := range value {
			valid = append(valid, r)
		}
		value = string(valid)
	}
	buffer := c.cString(value)
	text := lib.XJS_NewStringLen(c.tls, c.context, buffer, lib.Tsize_t(len(value)))
	libc.Xfree(c.tls, buffer)
	return text
}

func (c *Context) cString(value string) uintptr {
	buffer := libc.Xmalloc(c.tls, libc.Tsize_t(len(value)+1))
	target := unsafe.Slice((*byte)(unsafe.Pointer(buffer)), len(value)+1)
	copy(target, value)
	target[len(value)] = 0
	return buffer
}

// goString reads a string as UTF-8. QuickJS writes a lone surrogate as its
// three-byte encoding, which becomes U+FFFD, as utf16.Decode makes it of the
// UTF-16 that jsc_copy_out returns for such a string.
func (c *Context) goString(value lib.TJSValue) (string, bool) {
	pointer := lib.XJS_ToCStringLen2(c.tls, c.context, c.scratch, value, 0)
	if pointer == 0 {
		return "", false
	}
	length := *(*lib.Tsize_t)(unsafe.Pointer(c.scratch))
	text := wellFormed(unsafe.Slice((*byte)(unsafe.Pointer(pointer)), length))
	lib.XJS_FreeCString(c.tls, c.context, pointer)
	return text, true
}

func wellFormed(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	out := make([]byte, 0, len(data))
	for index := 0; index < len(data); {
		if index+2 < len(data) && data[index] == 0xED && data[index+1]&0xE0 == 0xA0 && data[index+2]&0xC0 == 0x80 {
			out = utf8.AppendRune(out, utf8.RuneError)
			index += 3
			continue
		}
		r, size := utf8.DecodeRune(data[index:])
		out = utf8.AppendRune(out, r)
		index += size
	}
	return string(out)
}

func fp(function any) uintptr {
	type iface [2]uintptr
	return (*iface)(unsafe.Pointer(&function))[1]
}

func contextOf(runtime uintptr) *Context {
	if loaded, found := runtimes.Load(runtime); found {
		return loaded.(*Context)
	}
	return nil
}

// callHost is the JSClassCall of host functions.
func callHost(tls *libc.TLS, ctx uintptr, function, this lib.TJSValue, argc int32, argv uintptr, flags int32) lib.TJSValue {
	it := lookup(lib.XJS_GetOpaque(tls, function, hostClass))
	if it == nil {
		return undefined
	}
	c := it.context
	if it.clock {
		var number lib.TJSValue
		*(*float64)(unsafe.Pointer(&number.Fu)) = float64(nanotime()-it.origin) / 1e6
		number.Ftag = lib.EJS_TAG_FLOAT64
		return number
	}
	arguments := make([]Value, argc)
	for index, ref := range unsafe.Slice((*lib.TJSValue)(unsafe.Pointer(argv)), argc) {
		arguments[index] = Value{c, ref}
	}
	return c.answer(c.serve(it.function, arguments))
}

func finalizeHost(tls *libc.TLS, runtime uintptr, value lib.TJSValue) {
	release(lib.XJS_GetOpaque(tls, value, hostClass))
}

func finalizeObject(tls *libc.TLS, runtime uintptr, value lib.TJSValue) {
	if it := release(lib.XJS_GetOpaque(tls, value, objectClass)); it != nil && it.finalize != nil {
		it.finalize()
	}
}

// interrupt is polled while scripts run; it stops the run that TerminateOn
// began once its context is done.
func interrupt(tls *libc.TLS, runtime, opaque uintptr) int32 {
	c := contextOf(runtime)
	if c == nil {
		return 0
	}
	if cancelled := c.cancelled.Load(); cancelled != 0 && cancelled == c.run {
		c.terminated = true
		return 1
	}
	return 0
}

// trackRejection keeps the rejected promises without a handler until the
// checkpoint, as JavaScriptCore reports them once its microtasks ran out.
func trackRejection(tls *libc.TLS, ctx uintptr, promise, reason lib.TJSValue, handled int32, opaque uintptr) {
	c := contextOf(lib.XJS_GetRuntime(tls, ctx))
	if c == nil {
		return
	}
	if handled == 0 {
		c.rejected = append(c.rejected, rejection{promise: c.dup(promise), reason: c.dup(reason)})
		return
	}
	for index, it := range c.rejected {
		if pointerOf(it.promise) == pointerOf(promise) {
			c.free(it.promise)
			c.free(it.reason)
			c.rejected = append(c.rejected[:index], c.rejected[index+1:]...)
			return
		}
	}
}
