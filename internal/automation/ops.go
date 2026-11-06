package automation

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/coredevice/accessibility"
	"github.com/swipt-automation/swipt/internal/device"
)

type operation struct {
	engine   *engine
	exec     *execution
	name     string
	args     []json.RawMessage
	recorded []json.RawMessage
	device   *device.Phone
}

func (o *operation) record(aim device.Aim, arguments ...json.RawMessage) {
	element, _ := json.Marshal(aim.String())
	o.recorded = append([]json.RawMessage{element}, arguments...)
}

type operationFunc func(ctx context.Context, call *operation) (any, error)

var operations map[string]operationFunc

func init() {
	operations = map[string]operationFunc{
		"device.info":  deviceInfo,
		"device.state": deviceState,
		"device.unlock": screenAction(func(ctx context.Context, phone *device.Phone, _ *operation) error {
			return phone.Unlock(ctx)
		}),
		"device.installedApplications": deviceApps,
		"device.applicationIcon":       deviceAppIcon,
		"device.launchApplication":     screenAction(deviceLaunch),
		"device.pasteboard":            deviceClipboard,
		"device.setPasteboard":         deviceSetClipboard,
		"device.setSimulatedLocation":  deviceSetLocation,
		"input.tap":                    screenAction(deviceTap),
		"input.swipe":                  screenAction(deviceGesture),
		"input.drag":                   screenAction(deviceGesture),
		"input.pinch":                  screenAction(devicePinch),
		"input.touch":                  screenAction(deviceTouch),
		"input.pressKey":               screenAction(devicePressKey),
		"input.button":                 screenAction(deviceButton),
		"accessibility.snapshot":       uiSnapshot,
		"accessibility.find":           uiFind,
		"accessibility.changed":        uiChanged,
		"accessibility.setText":        screenAction(uiSetText),
		"accessibility.perform":        screenAction(uiPerform),
		"accessibility.tap":            screenAction(uiTap),
		"fs.stat":                      fsStat,
		"fs.list":                      fsList,
		"fs.read":                      fsRead,
		"fs.write":                     fsWrite,
		"fs.mkdir":                     fsMkdir,
		"fs.remove":                    fsRemove,
		"fs.copy":                      fsTransfer(false),
		"fs.move":                      fsTransfer(true),
		"dialog":                       presentDialog,
		"fs.writer":                    fsWriter,
		"stream.read":                  streamRead,
		"stream.write":                 streamWrite,
		"stream.close":                 streamClose,
		"stream.pipe":                  streamPipe,
		"graphics":                     graphicsRun,
		"crypto.encrypt":               cryptoEncrypt,
		"crypto.decrypt":               cryptoDecrypt,
		"crypto.sign":                  cryptoSign,
		"crypto.verify":                cryptoVerify,
		"crypto.derive":                cryptoDerive,
		"crypto.generate":              cryptoGenerate,
		"crypto.import":                cryptoImport,
		"crypto.export":                cryptoExport,
		"sqlite":                       sqliteOperation,
	}
}

func (o *operation) decode(required int, values ...any) error {
	args := o.args
	if len(args) > len(values) {
		return E.Cause1(ErrInvalid, E.New("takes ", len(values), " arguments, not ", len(args)))
	}
	for index, value := range values {
		if index >= len(args) || string(args[index]) == "null" {
			if index < required {
				if numeric(reflect.TypeOf(value)) {
					return E.Cause1(ErrInvalid, E.New("argument ", index+1, " is missing or not a finite number"))
				}
				return E.Cause1(ErrInvalid, E.New("argument ", index+1, " is missing"))
			}
			continue
		}
		if err := json.Unmarshal(args[index], value); err != nil {
			return E.Cause1(ErrInvalid, E.Cause(err, "argument ", index+1))
		}
	}
	return nil
}

func (o *operation) phone() (*device.Phone, error) {
	phone := o.engine.host.options.Device
	if phone == nil {
		return nil, E.Cause1(device.ErrUnavailable, E.New("this server has no device"))
	}
	o.device = phone
	return phone, nil
}

func (o *operation) dropped(ctx context.Context, err error, since time.Time) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	if _, scripted := errors.AsType[*Failure](err); scripted {
		return false
	}
	return errors.Is(err, context.Canceled) || o.device != nil && o.device.Interrupted(since)
}

func (o *operation) droppedError(err error, doing string) error {
	if o.device != nil {
		if cause := o.device.DropCause(); cause != nil {
			err = cause
		}
	}
	if errors.Is(err, context.Canceled) {
		return E.Extend(device.ErrDisconnected, "the connection dropped ", doing)
	}
	return E.Extend(device.ErrDisconnected, "the connection dropped ", doing, ": ", err)
}

func (o *operation) screen(ctx context.Context) (*device.Phone, error) {
	phone, err := o.phone()
	if err != nil {
		return nil, err
	}
	if err = o.engine.host.lease.wait(ctx); err != nil {
		return nil, err
	}
	return phone, nil
}

func screenAction(act func(ctx context.Context, phone *device.Phone, call *operation) error) operationFunc {
	return func(ctx context.Context, call *operation) (any, error) {
		phone, err := call.screen(ctx)
		if err != nil {
			return nil, err
		}
		return nil, act(ctx, phone, call)
	}
}

var terseErrors = []error{ErrInvalid, ErrNotFound, ErrNotAllowed, device.ErrNotFound, device.ErrTimeout}

func Message(err error) string {
	message := err.Error()
	for _, sentinel := range terseErrors {
		if !errors.Is(err, sentinel) {
			continue
		}
		prefix := sentinel.Error() + ": "
		for strings.HasPrefix(message, prefix) {
			message = message[len(prefix):]
		}
		message = strings.ReplaceAll(message, ": "+prefix, ": ")
	}
	return message
}

func FailureOf(err error) string {
	type failure struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	}
	out := failure{Name: "Error", Message: Message(err)}
	scripted, isScripted := errors.AsType[*Failure](err)
	switch {
	case isScripted && scripted.Name != "":
		out.Name = scripted.Name
	case E.IsMulti(err, device.ErrDisconnected, device.ErrUnavailable, device.ErrLocked, device.ErrStalled, accessibility.ErrStalled):
		out.Name = "UnavailableError"
	case E.IsMulti(err, context.DeadlineExceeded, device.ErrTimeout):
		out.Name = "TimeoutError"
	case errors.Is(err, ErrDismissed), E.IsCanceled(err):
		out.Name = "AbortError"
	case errors.Is(err, ErrInvalid):
		out.Name = "TypeError"
	case E.IsMulti(err, ErrNotFound, device.ErrNotFound):
		out.Name = "NotFoundError"
	case errors.Is(err, ErrNotAllowed):
		out.Name = "NotAllowedError"
	}
	data, _ := json.Marshal(out)
	return string(data)
}

type Frame struct {
	Function string `json:"function"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
}

type Failure struct {
	Name    string  `json:"name"`
	Message string  `json:"message"`
	Stack   []Frame `json:"stack"`
}

func (f *Failure) Error() string {
	return f.Message
}

func (f *Failure) Summary() string {
	text := f.Name + ": " + f.Message
	if len(f.Stack) > 0 {
		frame := f.Stack[0]
		text += " (" + frame.File + ":" + strconv.Itoa(frame.Line) + ":" + strconv.Itoa(frame.Column) + ")"
	}
	return text
}

func (f *Failure) Describe() string {
	var text strings.Builder
	text.WriteString(f.Name + ": " + f.Message)
	for _, frame := range f.Stack {
		position := frame.File + ":" + strconv.Itoa(frame.Line) + ":" + strconv.Itoa(frame.Column)
		if frame.Function != "" {
			position = frame.Function + " (" + position + ")"
		}
		text.WriteString("\n    at " + position)
	}
	return text.String()
}

func (f *Failure) Is(target error) bool {
	switch f.Name {
	case "AbortError":
		return target == context.Canceled
	case "TypeError":
		return target == ErrInvalid
	case "NotFoundError":
		return target == ErrNotFound
	case "NotAllowedError":
		return target == ErrNotAllowed
	case "UnavailableError":
		return target == device.ErrUnavailable
	}
	return false
}

func failureError(text string) error {
	failure := &Failure{}
	if json.Unmarshal([]byte(text), failure) != nil {
		return &Failure{Name: "Error", Message: text}
	}
	return failure
}

type point device.Point

func (p *point) UnmarshalJSON(data []byte) error {
	values, ok := numbers(data, 2)
	if !ok {
		return E.New("expected a point [x, y] of finite numbers")
	}
	*p = point{values[0], values[1]}
	return nil
}

func numbers(data []byte, count int) ([]float64, bool) {
	var values []*float64
	if json.Unmarshal(data, &values) != nil || len(values) != count || slices.Contains(values, nil) {
		return nil, false
	}
	return common.Map(values, func(value *float64) float64 { return *value }), true
}

type strict struct {
	value any
}

func (o *strict) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return E.New("expected an options object")
	}
	structType := reflect.TypeOf(o.value).Elem()
	names := make([]string, structType.NumField())
	for index := range names {
		names[index], _, _ = strings.Cut(structType.Field(index).Tag.Get("json"), ",")
	}
	for name, value := range fields {
		index := slices.Index(names, name)
		if index < 0 {
			return E.New("unknown option ", strconv.Quote(name), "; the options are ", strings.Join(names, ", "))
		}
		if string(value) == "null" {
			return E.New("option ", name, " is null")
		}
	}
	return json.Unmarshal(data, o.value)
}

type touchOptions struct {
	Count *int `json:"count"`
}

func (o touchOptions) values() (int, error) {
	if o.Count != nil && *o.Count < 1 {
		return 0, E.Cause1(ErrInvalid, E.New("count must be at least 1"))
	}
	return common.PtrValueOrDefault(o.Count), nil
}

func numeric(value reflect.Type) bool {
	for value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	return value.Kind() >= reflect.Int && value.Kind() <= reflect.Float64
}

func duration(name string, value *float64) (time.Duration, error) {
	if value == nil {
		return 0, nil
	}
	if *value <= 0 {
		return 0, E.Cause1(ErrInvalid, E.New(name, " must be positive"))
	}
	return milliseconds(*value), nil
}

func milliseconds(value float64) time.Duration {
	return time.Duration(value * float64(time.Millisecond))
}

func deviceInfo(ctx context.Context, call *operation) (any, error) {
	if err := call.decode(0); err != nil {
		return nil, err
	}
	phone, err := call.phone()
	if err != nil {
		return nil, err
	}
	info, err := phone.Info(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"name": info.Name, "model": info.Model, "os": info.OSVersion, "build": info.Build, "language": info.Language,
		"screen": map[string]any{
			"width": info.Screen.Width, "height": info.Screen.Height,
			"scale": info.Screen.Scale, "orientation": info.Screen.Orientation,
		},
	}, nil
}

func readState(ctx context.Context, phone *device.Phone, name string) (any, error) {
	switch name {
	case "foreground":
		return phone.Frontmost(ctx)
	case "locked":
		return phone.Locked(ctx)
	case "display":
		return phone.Display(ctx)
	}
	return nil, E.Cause1(ErrInvalid, E.New("unknown state ", strconv.Quote(name)))
}

func deviceState(ctx context.Context, call *operation) (any, error) {
	var name string
	if err := call.decode(1, &name); err != nil {
		return nil, err
	}
	phone, err := call.phone()
	if err != nil {
		return nil, err
	}
	if name == "simulatedLocation" {
		location, _ := call.engine.host.currentLocation()
		return location, nil
	}
	return readState(ctx, phone, name)
}

func deviceTap(ctx context.Context, phone *device.Phone, call *operation) error {
	var at point
	var options touchOptions
	if err := call.decode(1, &at, &strict{&options}); err != nil {
		return err
	}
	count, err := options.values()
	if err != nil {
		return err
	}
	return phone.TapPoint(ctx, device.Point(at), count)
}

func deviceGesture(ctx context.Context, phone *device.Phone, call *operation) error {
	var from, to point
	var options struct {
		DurationMs *float64 `json:"durationMs"`
		PressMs    *float64 `json:"pressMs"`
	}
	if err := call.decode(2, &from, &to, &strict{&options}); err != nil {
		return err
	}
	move, err := duration("durationMs", options.DurationMs)
	if err != nil {
		return err
	}
	var fingers [][]device.TouchSample
	if call.name == "input.swipe" {
		if options.PressMs != nil {
			return E.Cause1(ErrInvalid, E.New("pressMs applies to drag"))
		}
		fingers, err = device.SwipePath(device.Point(from), device.Point(to), move)
	} else {
		if options.PressMs != nil && *options.PressMs < 0 {
			return E.Cause1(ErrInvalid, E.New("pressMs must not be negative"))
		}
		fingers, err = device.DragPath(device.Point(from), device.Point(to), move, milliseconds(common.PtrValueOrDefault(options.PressMs)))
	}
	if err != nil {
		return E.Cause1(ErrInvalid, err)
	}
	return phone.Touch(ctx, fingers)
}

func devicePinch(ctx context.Context, phone *device.Phone, call *operation) error {
	var center point
	var scale float64
	var options struct {
		DurationMs *float64 `json:"durationMs"`
	}
	if err := call.decode(2, &center, &scale, &strict{&options}); err != nil {
		return err
	}
	pinch, err := duration("durationMs", options.DurationMs)
	if err != nil {
		return err
	}
	return phone.Pinch(ctx, device.Point(center), scale, pinch)
}

func deviceTouch(ctx context.Context, phone *device.Phone, call *operation) error {
	var fingers [][][3]*float64
	if err := call.decode(1, &fingers); err != nil {
		return err
	}
	samples := make([][]device.TouchSample, len(fingers))
	for index, finger := range fingers {
		for _, sample := range finger {
			if slices.Contains(sample[:], nil) {
				return E.Cause1(ErrInvalid, E.New("touch samples need finite numbers [x, y, ms]"))
			}
			samples[index] = append(samples[index], device.TouchSample{X: *sample[0], Y: *sample[1], T: milliseconds(*sample[2])})
		}
	}
	return phone.Touch(ctx, samples)
}

func devicePressKey(ctx context.Context, phone *device.Phone, call *operation) error {
	var combo string
	if err := call.decode(1, &combo); err != nil {
		return err
	}
	return phone.Key(ctx, combo)
}

func deviceButton(ctx context.Context, phone *device.Phone, call *operation) error {
	var name string
	if err := call.decode(1, &name); err != nil {
		return err
	}
	return phone.Button(ctx, name)
}

func deviceLaunch(ctx context.Context, phone *device.Phone, call *operation) error {
	var bundleID string
	var options struct {
		URL     string `json:"url"`
		Restart bool   `json:"restart"`
	}
	if err := call.decode(1, &bundleID, &strict{&options}); err != nil {
		return err
	}
	return phone.Launch(ctx, bundleID, options.URL, options.Restart, true)
}

func deviceClipboard(ctx context.Context, call *operation) (any, error) {
	if err := call.decode(0); err != nil {
		return nil, err
	}
	phone, err := call.phone()
	if err != nil {
		return nil, err
	}
	text, found, err := phone.Clipboard(ctx)
	if err != nil || !found {
		return nil, err
	}
	return text, nil
}

func deviceSetClipboard(ctx context.Context, call *operation) (any, error) {
	if len(call.args) == 0 {
		return nil, E.Cause1(ErrInvalid, E.New("argument 1 is missing"))
	}
	var text *string
	if err := call.decode(0, &text); err != nil {
		return nil, err
	}
	phone, err := call.phone()
	if err != nil {
		return nil, err
	}
	return nil, phone.SetClipboard(ctx, text)
}

func deviceSetLocation(ctx context.Context, call *operation) (any, error) {
	if len(call.args) == 0 {
		return nil, E.Cause1(ErrInvalid, E.New("argument 1 is missing"))
	}
	var location *struct {
		Latitude  *float64 `json:"latitude"`
		Longitude *float64 `json:"longitude"`
	}
	if err := call.decode(0, &location); err != nil {
		return nil, err
	}
	phone, err := call.phone()
	if err != nil {
		return nil, err
	}
	if location == nil {
		return nil, call.engine.host.clearLocation(ctx, phone)
	}
	if location.Latitude == nil || *location.Latitude < -90 || *location.Latitude > 90 || location.Longitude == nil || *location.Longitude < -180 || *location.Longitude > 180 {
		return nil, E.Cause1(ErrInvalid, E.New("the latitude must be within ±90 and the longitude within ±180 degrees"))
	}
	return nil, call.engine.host.setLocation(ctx, phone, call.engine.swiplet.ID, *location.Latitude, *location.Longitude)
}

type snapshotOptions struct {
	Scope string `json:"scope"`
	Full  bool   `json:"full"`
}

func uiSnapshot(ctx context.Context, call *operation) (any, error) {
	var options snapshotOptions
	if err := call.decode(0, &strict{&options}); err != nil {
		return nil, err
	}
	if options.Scope != "" && options.Scope != "frontApp" && options.Scope != "screen" {
		return nil, E.Cause1(ErrInvalid, E.New("scope must be frontApp or screen"))
	}
	phone, err := call.screen(ctx)
	if err != nil {
		return nil, err
	}
	view, err := phone.Snapshot(ctx, device.SnapshotOptions{Front: options.Scope != "screen", Raw: options.Full})
	if err != nil {
		return nil, err
	}
	return call.engine.snapshots.keep(view), nil
}

func uiFind(ctx context.Context, call *operation) (any, error) {
	var encoded []json.RawMessage
	if err := call.decode(1, &encoded); err != nil {
		return nil, err
	}
	selectors := make([]*device.Selector, 0, len(encoded))
	for _, data := range encoded {
		selector, err := device.ParseSelector(data)
		if err != nil {
			return nil, err
		}
		selectors = append(selectors, selector)
	}
	phone, err := call.screen(ctx)
	if err != nil {
		return nil, err
	}
	view, seen, err := phone.Find(ctx, selectors)
	if err != nil {
		return nil, err
	}
	var found any
	if view != nil {
		found = call.engine.snapshots.keep(view)
	}
	return map[string]any{"snapshot": found, "seen": seen}, nil
}

func uiChanged(ctx context.Context, call *operation) (any, error) {
	var seen uint64
	var timeoutMs *float64
	if err := call.decode(1, &seen, &timeoutMs); err != nil {
		return nil, err
	}
	timeout := time.Duration(-1)
	if timeoutMs != nil {
		if *timeoutMs < 0 {
			return nil, E.Cause1(ErrInvalid, E.New("timeoutMs must not be negative"))
		}
		timeout = milliseconds(*timeoutMs)
	}
	phone, err := call.screen(ctx)
	if err != nil {
		return nil, err
	}
	return nil, phone.Changed(ctx, seen, timeout)
}

func uiSetText(ctx context.Context, phone *device.Phone, call *operation) error {
	var ref, text string
	var timeoutMs *float64
	if err := call.decode(2, &ref, &text, &timeoutMs); err != nil {
		return err
	}
	timeout, err := elementTimeout(timeoutMs)
	if err != nil {
		return err
	}
	aim, err := phone.SetText(ctx, ref, text, timeout)
	if aim.Element == "" || aim.Secure {
		call.record(aim, call.args[2:]...)
	} else {
		call.record(aim, call.args[1:]...)
	}
	return err
}

func uiPerform(ctx context.Context, phone *device.Phone, call *operation) error {
	var ref, name string
	if err := call.decode(2, &ref, &name); err != nil {
		return err
	}
	aim, err := phone.Perform(ctx, ref, name)
	call.record(aim, call.args[1:]...)
	return err
}

func uiTap(ctx context.Context, phone *device.Phone, call *operation) error {
	var ref string
	var timeoutMs *float64
	if err := call.decode(1, &ref, &timeoutMs); err != nil {
		return err
	}
	timeout, err := elementTimeout(timeoutMs)
	if err != nil {
		return err
	}
	aim, err := phone.Tap(ctx, ref, timeout)
	call.record(aim, call.args[1:]...)
	return err
}

// A missing limit waits forever.
func elementTimeout(timeoutMs *float64) (time.Duration, error) {
	if timeoutMs == nil {
		return -1, nil
	}
	if *timeoutMs < 0 {
		return 0, E.Cause1(ErrInvalid, E.New("timeoutMs must not be negative"))
	}
	return milliseconds(*timeoutMs), nil
}
