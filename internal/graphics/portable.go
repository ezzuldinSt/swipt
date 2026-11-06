//go:build !darwin || !cgo

package graphics

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"sync"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/automation"
	"github.com/swipt-automation/swipt/internal/graphics/raster"
	"github.com/swipt-automation/swipt/internal/graphics/recognize"
	"github.com/swipt-automation/swipt/internal/jsc"
)

// Without the Objective-C engine, the calls run on internal/graphics/raster
// and internal/graphics/recognize, and the screen is read with screenshots.
type Graphics struct {
	options Options
	access  sync.Mutex
	engines map[uint64]*engine
}

// engine keeps the prepared calls and the results of one JavaScript context
// until it takes them, like SWEngine.m.
type engine struct {
	access   sync.Mutex
	next     int64
	prepared map[int64]*prepared
	objects  map[int64]any
	closed   bool
}

type prepared struct {
	method    string
	arguments json.RawMessage
	objects   []any
}

// The time between screenshots while a script watches the screen.
const watchInterval = 100 * time.Millisecond

var errEnded = &raster.Error{Name: raster.NameAbort, Message: "the JavaScript session ended"}

// bridge turns the Go functions into the native.graphics of SWNative.m: a
// failure the Go side answers becomes the TypeError, RangeError or
// DOMException of its name (SWJavaScriptFail), and a result object becomes a
// handle whose properties and methods are those of SWHandles.m.
const bridge = `(native => {
	const hosts = new WeakMap();
	const raise = ({ name, message }) => {
		if (name === "TypeError" || name === "RangeError") {
			return new globalThis[name](message);
		}
		if (typeof globalThis.DOMException === "function") {
			return new globalThis.DOMException(message, name);
		}
		return Object.assign(new Error(message), { name });
	};
	const check = result => {
		if (result !== null && typeof result === "object" && !hosts.has(result) && result.failure !== undefined) {
			throw raise(result.failure);
		}
		return result;
	};
	const adopt = host => {
		if (host === null || host === undefined) {
			return null;
		}
		const call = (name, ...args) => check(native.op(name, host, ...args));
		let facade;
		switch (call("kind")) {
		case "image": {
			const info = call("info");
			const derive = (name, ...args) => adopt(call(name, ...args));
			facade = {
				width: info.width, height: info.height, scale: info.scale, frame: info.frame,
				time: info.time ?? NaN, timestamp: info.timestamp ?? NaN,
				pixelWidth: info.pixelWidth, pixelHeight: info.pixelHeight,
				crop: (x, y, width, height) => derive("crop", x, y, width, height),
				resize: (width, height) => derive("resize", width, height),
				rotate: degrees => derive("rotate", degrees),
				grayscale: () => derive("grayscale"),
				threshold: level => derive("threshold", level),
				mask: (...range) => derive("mask", ...range),
				invert: () => derive("invert"),
				dilate: radius => derive("dilate", radius),
				erode: radius => derive("erode", radius),
				showBytes: () => call("showBytes"),
			};
			break;
		}
		case "text":
			facade = { boundingBox: (line, candidate, start, end) => call("boundingBox", line, candidate, start, end) };
			break;
		default:
			return null;
		}
		hosts.set(Object.freeze(facade), host);
		return facade;
	};
	return Object.freeze({
		prepare: (method, args, objects) => check(native.prepare(method, args, Array.from(objects, it => hosts.get(it) ?? it))),
		take: id => adopt(check(native.take(id))),
		bytes: id => check(native.bytes(id)),
	});
})`

func New(options Options) *Graphics {
	return &Graphics{options: options, engines: make(map[uint64]*engine)}
}

func (g *Graphics) Install(js *jsc.Context, native jsc.Value, id uint64) {
	e := &engine{prepared: make(map[int64]*prepared), objects: make(map[int64]any)}
	g.access.Lock()
	g.engines[id] = e
	g.access.Unlock()
	factory, err := js.Evaluate(bridge, "graphics.js")
	if err != nil {
		g.options.Logger.Error(E.Cause(err, "install graphics"))
		return
	}
	defer factory.Release()
	functions := js.NewObject()
	defer functions.Release()
	functions.SetFunction("prepare", e.prepare)
	functions.SetFunction("take", func(arguments []jsc.Value) (any, error) {
		return e.take(js, arguments)
	})
	functions.SetFunction("bytes", e.bytes)
	functions.SetFunction("op", func(arguments []jsc.Value) (any, error) {
		return e.op(js, arguments)
	})
	graphics, err := factory.Call(functions)
	if err != nil {
		g.options.Logger.Error(E.Cause(err, "install graphics"))
		return
	}
	native.Set("graphics", graphics)
	graphics.Release()
}

func (g *Graphics) Uninstall(_ *jsc.Context, id uint64) {
	g.access.Lock()
	e := g.engines[id]
	delete(g.engines, id)
	g.access.Unlock()
	if e != nil {
		e.close()
	}
}

func (g *Graphics) engine(id uint64) *engine {
	g.access.Lock()
	defer g.access.Unlock()
	return g.engines[id]
}

func (g *Graphics) Run(id uint64, callID int64, call *automation.GraphicsCall) (json.RawMessage, error) {
	e := g.engine(id)
	var it *prepared
	if e != nil {
		it = e.takePrepared(callID)
	}
	if it == nil {
		return nil, failureOf(errEnded)
	}
	ctx := call.Context()
	type outcome struct {
		value any
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		value, err := g.execute(ctx, e, it, call)
		done <- outcome{value, err}
	}()
	var result outcome
	select {
	case result = <-done:
	case <-ctx.Done():
		return nil, failureOf(&raster.Error{Name: raster.NameAbort, Message: "the operation was aborted"})
	}
	if result.err != nil {
		return nil, failureOf(result.err)
	}
	data, err := json.Marshal(result.value)
	if err != nil {
		return nil, failureOf(E.New("graphics: the result does not pass through JSON"))
	}
	return data, nil
}

func (g *Graphics) execute(ctx context.Context, e *engine, it *prepared, call *automation.GraphicsCall) (any, error) {
	env := environment{engine: e, call: call}
	switch it.method {
	case "screen.get":
		image, err := env.screen(ctx)
		if err != nil {
			return nil, err
		}
		return raster.ImageResult{Image: e.keep(image)}, nil
	case "text":
		image, options, err := textCall(it)
		if err != nil {
			return nil, err
		}
		source, err := recognizable(ctx, image)
		if err != nil {
			return nil, err
		}
		text, err := recognize.ReadText(ctx, source, options)
		if err != nil {
			return nil, err
		}
		return map[string]any{"lines": text.Lines, "text": e.keep(text)}, nil
	case "barcodes":
		image, isImage := firstImage(it)
		if !isImage {
			return nil, &raster.Error{Name: raster.NameType, Message: "graphics: argument 1 of barcodes is not an Image"}
		}
		source, err := recognizable(ctx, image)
		if err != nil {
			return nil, err
		}
		return recognize.ReadBarcodes(ctx, source)
	}
	return raster.Run(ctx, it.method, it.arguments, it.objects, env)
}

func (g *Graphics) Watch(id uint64, callID int64, call *automation.GraphicsCall, publish func(any)) error {
	e := g.engine(id)
	var it *prepared
	if e != nil {
		it = e.takePrepared(callID)
	}
	if it == nil {
		return failureOf(errEnded)
	}
	if it.method != "screen.watch" {
		return failureOf(&raster.Error{Name: raster.NameType, Message: "graphics: there is no stream " + it.method})
	}
	ctx := call.Context()
	env := environment{engine: e, call: call}
	var previous int64
	defer func() {
		if previous != 0 {
			e.drop(previous)
		}
	}()
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	for {
		image, err := env.screen(ctx)
		if err != nil {
			return failureOf(err)
		}
		identifier := e.keep(image)
		if previous != 0 {
			e.drop(previous)
		}
		previous = identifier
		publish(json.RawMessage(strconv.FormatInt(identifier, 10)))
		select {
		case <-ctx.Done():
			return failureOf(&raster.Error{Name: raster.NameAbort, Message: "the operation was aborted"})
		case <-ticker.C:
		}
	}
}

func (e *engine) prepare(arguments []jsc.Value) (any, error) {
	if len(arguments) != 3 {
		return failure(raster.NameType, "graphics: prepare takes 3 arguments"), nil
	}
	method, err := arguments[0].ToString()
	if err != nil {
		return nil, err
	}
	data, err := arguments[1].ToString()
	if err != nil {
		return nil, err
	}
	length, err := arguments[2].Get("length")
	if err != nil {
		return nil, err
	}
	count := int(length.Number())
	length.Release()
	objects := make([]any, 0, count)
	for index := range count {
		item, err := arguments[2].Get(strconv.Itoa(index))
		if err != nil {
			return nil, err
		}
		if bytes, isBytes := item.Bytes(); isBytes {
			objects = append(objects, bytes)
		} else if payload, isHost := item.HostObject(); isHost {
			objects = append(objects, payload)
		} else {
			item.Release()
			return failure(raster.NameType, "graphics: argument ", index+1, " of ", method, " is not an Image, a Recording, a Video or a Uint8Array"), nil
		}
		item.Release()
	}
	e.access.Lock()
	defer e.access.Unlock()
	e.next++
	e.prepared[e.next] = &prepared{method: method, arguments: json.RawMessage(data), objects: objects}
	return e.next, nil
}

func (e *engine) take(js *jsc.Context, arguments []jsc.Value) (any, error) {
	if len(arguments) != 1 {
		return nil, nil
	}
	object := e.object(int64(arguments[0].Number()))
	switch object.(type) {
	case *raster.Image, *recognize.Text:
		return js.NewHostObject(object, nil), nil
	}
	return nil, nil
}

func (e *engine) bytes(arguments []jsc.Value) (any, error) {
	if len(arguments) != 1 {
		return nil, nil
	}
	if data, isBytes := e.object(int64(arguments[0].Number())).([]byte); isBytes {
		return jsc.Bytes(data), nil
	}
	return nil, nil
}

// op runs a synchronous method of a handle (SWHandles.m).
func (e *engine) op(js *jsc.Context, arguments []jsc.Value) (any, error) {
	if len(arguments) < 2 {
		return failure(raster.NameType, "graphics: a handle method takes a name and a handle"), nil
	}
	name, err := arguments[0].ToString()
	if err != nil {
		return nil, err
	}
	payload, _ := arguments[1].HostObject()
	numbers := make([]float64, len(arguments)-2)
	for index, argument := range arguments[2:] {
		numbers[index] = argument.Number()
	}
	integer := func(index int) int {
		if index >= len(numbers) || math.IsNaN(numbers[index]) {
			return 0
		}
		return int(numbers[index])
	}
	number := func(index int) float64 {
		if index >= len(numbers) {
			return math.NaN()
		}
		return numbers[index]
	}
	handle := func(image *raster.Image) any {
		return js.NewHostObject(image, nil)
	}
	switch it := payload.(type) {
	case *raster.Image:
		switch name {
		case "kind":
			return "image", nil
		case "info":
			return map[string]any{
				"width": rounded(it.Width()), "height": rounded(it.Height()), "scale": rounded(it.Scale()),
				"frame": it.Frame(), "time": finite(it.Time()), "timestamp": finite(it.Timestamp()),
				"pixelWidth": it.PixelWidth(), "pixelHeight": it.PixelHeight(),
			}, nil
		case "crop":
			cropped, err := it.Crop(raster.Rect{number(0), number(1), number(2), number(3)})
			if err != nil {
				return failureObject(err), nil
			}
			if cropped == nil {
				return nil, nil
			}
			return handle(cropped), nil
		case "resize":
			return handle(it.Resize(integer(0), integer(1))), nil
		case "rotate":
			return handle(it.Rotate(number(0))), nil
		case "grayscale":
			return handle(it.Grayscale()), nil
		case "threshold":
			return handle(it.Threshold(integer(0))), nil
		case "mask":
			return handle(it.Mask([3]int{integer(0), integer(1), integer(2)}, [3]int{integer(3), integer(4), integer(5)})), nil
		case "invert":
			return handle(it.Invert()), nil
		case "dilate":
			return handle(it.Dilate(integer(0))), nil
		case "erode":
			return handle(it.Erode(integer(0))), nil
		case "showBytes":
			data, err := it.ShowBytes(context.Background())
			if err != nil {
				return failureObject(err), nil
			}
			return jsc.Bytes(data), nil
		}
	case *recognize.Text:
		switch name {
		case "kind":
			return "text", nil
		case "boundingBox":
			box := it.BoundingBox(integer(0), integer(1), integer(2), integer(3))
			if box == nil {
				return nil, nil
			}
			return box, nil
		}
	default:
		if name == "kind" {
			return "", nil
		}
	}
	return failure(raster.NameType, "graphics: there is no handle method ", name), nil
}

func (e *engine) takePrepared(id int64) *prepared {
	e.access.Lock()
	defer e.access.Unlock()
	it := e.prepared[id]
	delete(e.prepared, id)
	return it
}

func (e *engine) keep(object any) int64 {
	e.access.Lock()
	defer e.access.Unlock()
	e.next++
	if !e.closed {
		e.objects[e.next] = object
	}
	return e.next
}

func (e *engine) object(id int64) any {
	e.access.Lock()
	defer e.access.Unlock()
	object := e.objects[id]
	delete(e.objects, id)
	return object
}

func (e *engine) drop(id int64) {
	e.object(id)
}

func (e *engine) close() {
	e.access.Lock()
	defer e.access.Unlock()
	e.closed = true
	clear(e.prepared)
	clear(e.objects)
}

// environment gives raster.Run what SWGoBridge.m gives the Objective-C calls.
type environment struct {
	engine *engine
	call   *automation.GraphicsCall
}

func (v environment) Keep(object any) int64 {
	return v.engine.keep(object)
}

func (v environment) Authorize(path string, write bool) error {
	return v.call.Authorize(path, write)
}

func (v environment) Screenshot(context.Context) ([]byte, float64, error) {
	return v.call.Screenshot()
}

func (v environment) Tracing() bool {
	return v.call.Tracing()
}

func (v environment) Trace(started time.Time, name, detail string, duration time.Duration) {
	v.call.Trace(started, name, detail, duration)
}

// RecognizeText reads the text targets of screen.find.
func (v environment) RecognizeText(ctx context.Context, frame *raster.Image, arguments json.RawMessage) (raster.Text, error) {
	var options recognize.TextOptions
	if err := json.Unmarshal(arguments, &options); err != nil {
		return nil, E.Cause(err, "graphics: decode the text options")
	}
	source, err := recognizable(ctx, frame)
	if err != nil {
		return nil, err
	}
	text, err := recognize.ReadText(ctx, source, options)
	if err != nil {
		return nil, err
	}
	return recognized{text}, nil
}

type recognized struct {
	text *recognize.Text
}

func (r recognized) Lines() []raster.TextLine {
	lines := make([]raster.TextLine, len(r.text.Lines))
	for index, line := range r.text.Lines {
		lines[index] = raster.TextLine{Text: line.Text, Confidence: line.Confidence, Frame: raster.Rect(line.Frame)}
	}
	return lines
}

func (r recognized) BoundingBox(line, candidate, start, end int) (raster.Rect, bool, error) {
	box := r.text.BoundingBox(line, candidate, start, end)
	if box == nil {
		return raster.Rect{}, false, nil
	}
	return raster.Rect(box.Frame), true, nil
}

// screen reads the screen as an image stamped with the time it arrived.
func (v environment) screen(ctx context.Context) (*raster.Image, error) {
	data, scale, err := v.call.Screenshot()
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return raster.Decode(data, scale, float64(time.Now().UnixMicro())/1000)
}

func firstImage(it *prepared) (*raster.Image, bool) {
	if len(it.objects) == 0 {
		return nil, false
	}
	image, isImage := it.objects[0].(*raster.Image)
	return image, isImage
}

func textCall(it *prepared) (*raster.Image, recognize.TextOptions, error) {
	var options recognize.TextOptions
	image, isImage := firstImage(it)
	if !isImage {
		return nil, options, &raster.Error{Name: raster.NameType, Message: "graphics: argument 1 of text is not an Image"}
	}
	if len(it.arguments) > 0 {
		if err := json.Unmarshal(it.arguments, &options); err != nil {
			return nil, options, &raster.Error{Name: raster.NameType, Message: "graphics: decode the arguments: " + err.Error()}
		}
	}
	return image, options, nil
}

func recognizable(ctx context.Context, image *raster.Image) (recognize.Image, error) {
	pixels, err := image.Pixels(ctx)
	if err != nil {
		return recognize.Image{}, err
	}
	return recognize.Image{Pixels: pixels, Scale: image.Scale(), Frame: image.Frame()}, nil
}

func failure(name string, message ...any) map[string]any {
	return map[string]any{"failure": &raster.Error{Name: name, Message: E.New(message...).Error()}}
}

func failureObject(err error) map[string]any {
	return map[string]any{"failure": named(err)}
}

// named gives an error the name and message the JavaScript sees.
func named(err error) *raster.Error {
	var rasterError *raster.Error
	var recognizeError *recognize.Error
	var unsupported *raster.UnsupportedError
	switch {
	case errors.As(err, &rasterError):
		return rasterError
	case errors.As(err, &recognizeError):
		return &raster.Error{Name: recognizeError.Name, Message: recognizeError.Message}
	case errors.As(err, &unsupported):
		return &raster.Error{Name: unsupported.Name(), Message: unsupported.Error()}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return &raster.Error{Name: raster.NameAbort, Message: "the operation was aborted"}
	}
	if failure, isFailure := errors.AsType[*automation.Failure](err); isFailure {
		return &raster.Error{Name: failure.Name, Message: failure.Message}
	}
	return &raster.Error{Name: raster.NameError, Message: err.Error()}
}

func failureOf(err error) error {
	it := named(err)
	return &automation.Failure{Name: it.Name, Message: it.Message}
}

// rounded is SWRounded: two decimals.
func rounded(value float64) float64 {
	return math.Round(value*100) / 100
}

// finite is a number JSON can carry: the bridge reads null as NaN.
func finite(value float64) any {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	return value
}
