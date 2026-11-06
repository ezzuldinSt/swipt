package automation

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/jsc"
	"github.com/swipt-automation/swipt/internal/log"

	"github.com/go-sourcemap/sourcemap"
)

//go:embed runtime.js
var runtimeSource string

//go:embed chunks
var chunkSources embed.FS

const (
	extensionHead     = "(function (require, module, exports) {"
	completionBacklog = 64
	streamBacklog     = 1000
	abortGrace        = 5 * time.Second
)

var errEnded = E.New("the execution ended")

type LogEntry struct {
	Time    time.Time
	Level   log.Level
	Message string
}

type CallRequest struct {
	Swiplet string
	Action  string
	Input   json.RawMessage
	Caller  Caller
	Dialog  Dialog
	Log     func(LogEntry)
}

type completion struct {
	id    string
	value any
	err   error
}

type execution struct {
	id       string
	ctx      context.Context
	cancel   context.CancelCauseFunc
	logs     func(LogEntry)
	emit     func(Output)
	finished bool
	failure  string
	output   string
	cleanup  []func() bool
	done     chan struct{}
	err      error
}

type pendingOperation struct {
	cancel context.CancelFunc
	exec   *execution
}

type timer struct {
	id        int
	at        time.Time
	delay     time.Duration
	interval  bool
	nesting   int
	callback  jsc.Value
	arguments []jsc.Value
	exec      *execution
}

func (t *timer) release() {
	t.callback.Release()
	for _, argument := range t.arguments {
		argument.Release()
	}
}

type subscription struct {
	cancel  context.CancelFunc
	exec    *execution
	queue   bool
	access  sync.Mutex
	values  []any
	failure error
}

type hooks struct {
	settle, deliver, end, describe jsc.Value
	load, call, start, abort       jsc.Value
	repl, launch                   jsc.Value
}

type entry struct {
	action string
	input  string
	caller Caller
}

type launch struct {
	exec   *execution
	script Script
}

type engine struct {
	host      *Host
	swiplet   Swiplet
	caches    string
	dialog    Dialog
	logger    log.ContextLogger
	context   *jsc.Context
	snapshots *snapshotStore
	hooks     hooks
	rewrite   jsc.Value
	sources   func() *sourcemap.Consumer
	ctx       context.Context
	cancel    context.CancelCauseFunc
	graphics  uint64

	completions chan completion
	wake        chan struct{}
	launches    chan launch
	stops       chan *execution
	served      chan struct{}

	main          *execution
	current       *execution
	executions    map[string]*execution
	launched      int
	ready         []completion
	timers        []*timer
	sequence      int
	nesting       int
	ops           map[string]*pendingOperation
	results       map[string]completion
	subscriptions map[string]*subscription

	access           sync.Mutex
	blobs            map[string][]byte
	blobSequence     int
	resources        map[string]*resource
	resourceSequence int
	interrupts       map[string]func(error)
	jars             map[string]*cookieStore
}

func (h *Host) Call(ctx context.Context, request CallRequest) (json.RawMessage, error) {
	switch request.Caller {
	case CallerApp, CallerShortcuts, CallerAPI:
	default:
		return nil, E.Cause1(ErrInvalid, E.New("unknown caller ", strconv.Quote(string(request.Caller))))
	}
	input := request.Input
	if len(input) == 0 {
		input = json.RawMessage("null")
	} else if !json.Valid(input) {
		return nil, E.Cause1(ErrInvalid, E.New("the input is not JSON"))
	}
	r, err := h.runner(request.Swiplet)
	if err != nil {
		return nil, err
	}
	ctx, swiplet, exit, err := r.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer exit()
	index := slices.IndexFunc(swiplet.Manifest.Actions, func(action ActionInfo) bool { return action.Name == request.Action })
	if index < 0 {
		return nil, E.Cause1(ErrNotFound, E.New("the swiplet ", strconv.Quote(swiplet.ID), " has no action ", strconv.Quote(request.Action)))
	}
	if request.Caller == CallerShortcuts && !slices.Contains(swiplet.Manifest.Actions[index].Expose, CallerShortcuts) {
		return nil, E.Cause1(ErrNotAllowed, E.New("the action ", strconv.Quote(request.Action), " is not exposed to shortcuts"))
	}
	h.timeline.record(TimelineEvent{Time: time.Now().UnixMilli(), Type: "action", Swiplet: swiplet.ID, Action: request.Action, Caller: string(request.Caller)})
	started := time.Now()
	output, err := h.execute(ctx, swiplet, request.Dialog, request.Log, entry{action: request.Action, input: string(input), caller: request.Caller}, nil)
	if err != nil && ctx.Err() == nil {
		level := log.LevelError
		if h.deviceLost(err, started) {
			level = log.LevelInfo
		}
		h.Log(swiplet.ID, level, "the action "+strconv.Quote(request.Action)+" failed: "+failureOf(err).Describe())
	}
	return output, err
}

func (h *Host) execute(ctx context.Context, swiplet Swiplet, dialog Dialog, logs func(LogEntry), entry entry, running func()) (json.RawMessage, error) {
	source, err := os.ReadFile(filepath.Join(swiplet.Directory, SourceFolder, ExtensionFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, E.Cause1(ErrNotFound, E.New("the swiplet ", strconv.Quote(swiplet.ID), " has no extension"))
	}
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Join(swiplet.Directory, DataFolder), 0o755); err != nil {
		return nil, err
	}
	e, err := h.newEngine(swiplet, dialog, h.logFactory.NewLogger("swiplet/"+swiplet.ID), logs)
	if err != nil {
		return nil, E.Cause(err, "start JavaScript")
	}
	defer e.close()
	defer context.AfterFunc(ctx, func() {
		time.AfterFunc(abortGrace, func() { e.cancel(context.Cause(ctx)) })
	})()
	err = e.enter(e.main, func() error {
		if err := e.load(string(source)); err != nil {
			return E.Cause(err, "load ", ExtensionFile)
		}
		if entry.action == "" {
			return e.callHook(e.hooks.start)
		}
		return e.callHook(e.hooks.call, entry.action, entry.input, string(entry.caller))
	})
	if e.ctx.Err() != nil {
		return nil, context.Cause(e.ctx)
	}
	if err != nil {
		return nil, err
	}
	if running != nil {
		running()
	}
	return e.run(ctx)
}

func (e *engine) load(source string) error {
	factory, err := e.context.Evaluate(extensionHead+source+"\n})", ExtensionFile)
	if err != nil {
		return err
	}
	defer factory.Release()
	out, err := e.hooks.load.Call(factory)
	if err != nil {
		return err
	}
	out.Release()
	return nil
}

func (h *Host) newEngine(swiplet Swiplet, dialog Dialog, logger log.ContextLogger, logs func(LogEntry)) (_ *engine, err error) {
	e := &engine{
		host: h, swiplet: swiplet, dialog: dialog, logger: logger,
		context: jsc.New(), snapshots: newSnapshotStore(), completions: make(chan completion, completionBacklog), wake: make(chan struct{}, 1),
		executions: make(map[string]*execution), ops: make(map[string]*pendingOperation), results: make(map[string]completion),
		subscriptions: make(map[string]*subscription), blobs: make(map[string][]byte), resources: make(map[string]*resource),
		interrupts: make(map[string]func(error)),
		sources: sync.OnceValue(func() *sourcemap.Consumer {
			data, err := os.ReadFile(filepath.Join(swiplet.Directory, SourceFolder, ExtensionMap))
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			var consumer *sourcemap.Consumer
			if err == nil {
				consumer, err = sourcemap.Parse("", data)
			}
			if err != nil {
				logger.Warn("read the source map: ", err)
			}
			return consumer
		}),
	}
	e.caches = h.cacheDirectory(e.dataOwner())
	e.ctx, e.cancel = context.WithCancelCause(context.Background())
	e.main = &execution{ctx: e.ctx, logs: logs}
	e.current = e.main
	defer func() {
		if err != nil {
			e.cancel(err)
			e.uninstallGraphics()
			e.context.Close()
		}
	}()
	evaluated, err := e.context.Evaluate(runtimeSource, "runtime.js")
	if err != nil {
		return nil, err
	}
	defer evaluated.Release()
	install, err := evaluated.Get("install")
	if err != nil {
		return nil, err
	}
	defer install.Release()
	native := e.context.NewObject()
	defer native.Release()
	origin := native.SetClock("now")
	properties := map[string]string{
		"swiplet": swiplet.ID, "dataDirectory": filepath.Join(swiplet.Directory, DataFolder), "languages": strings.Join(h.options.Languages, "\n"),
		"assetsDirectory": filepath.Join(swiplet.Directory, AssetsFolder), "cacheDirectory": e.caches, "extensionHead": extensionHead,
		"timeOrigin": strconv.FormatFloat(float64(origin.UnixMicro())/1000, 'f', -1, 64),
	}
	for name, text := range properties {
		value := e.context.NewString(text)
		native.Set(name, value)
		value.Release()
	}
	for name, function := range e.natives() {
		native.SetFunction(name, function)
	}
	e.installGraphics(native)
	installed, err := install.Call(native)
	if err != nil {
		return nil, err
	}
	defer installed.Release()
	for name, slot := range map[string]*jsc.Value{
		"settle": &e.hooks.settle, "deliver": &e.hooks.deliver, "end": &e.hooks.end, "describe": &e.hooks.describe,
		"load": &e.hooks.load, "call": &e.hooks.call, "start": &e.hooks.start, "abort": &e.hooks.abort,
		"repl": &e.hooks.repl, "launch": &e.hooks.launch,
	} {
		if *slot, err = installed.Get(name); err != nil {
			return nil, err
		}
	}
	e.context.SetDescriber(e.hooks.describe)
	e.context.SetUnhandledRejection(func(arguments []jsc.Value) (any, error) {
		e.log(log.LevelError, "a promise was rejected with nothing to handle it: "+arguments[1].Describe())
		return nil, nil
	})
	return e, nil
}

func (e *engine) run(ctx context.Context) (json.RawMessage, error) {
	abort := ctx.Done()
	for !e.main.finished {
		aborted, err := e.step(abort)
		if aborted {
			abort = nil
			err = e.enter(e.main, func() error { return e.callHook(e.hooks.abort) })
		}
		if e.ctx.Err() != nil {
			return nil, context.Cause(e.ctx)
		}
		if err != nil {
			e.log(log.LevelError, err.Error())
		}
	}
	if e.main.failure != "" {
		return nil, failureError(e.main.failure)
	}
	return json.RawMessage(e.main.output), nil
}

func (e *engine) enter(exec *execution, run func() error) error {
	e.current = exec
	defer e.context.TerminateOn(exec.ctx)()
	return run()
}

func (e *engine) close() {
	e.cancel(errEnded)
	e.dropWork()
	e.closeResources()
	e.host.CloseDatabases(e, "")
	e.uninstallGraphics()
	e.context.Close()
}

func (e *engine) dropWork() {
	for _, it := range e.timers {
		it.release()
	}
	e.timers = nil
	for _, it := range e.subscriptions {
		it.cancel()
	}
	clear(e.subscriptions)
	for _, op := range e.ops {
		op.cancel()
	}
	clear(e.ops)
	e.ready = nil
	clear(e.results)
}

func (e *engine) natives() map[string]jsc.Function {
	return map[string]jsc.Function{
		"start":                  e.startOperation,
		"cancel":                 e.cancelOperation,
		"failure":                e.takeFailure,
		"value":                  e.value,
		"subscribe":              e.subscribe,
		"unsubscribe":            e.unsubscribe,
		"print":                  e.print,
		"put":                    e.put,
		"take":                   e.take,
		"random":                 e.random,
		"digest":                 e.digest,
		"finish":                 e.finish,
		"show":                   e.show,
		"match":                  e.match,
		"preferredLocalizations": e.preferredLocalizations,
		"locate":                 e.locate,
		"sqlite":                 e.sqlite,
		"release":                e.release,
		"compression":            e.compression,
		"openFile":               e.openFile,
		"encodeText":             e.encodeText,
		"decodeText":             e.decodeText,
		"chunk":                  e.chunk,
		"setTimeout":             e.schedule(false),
		"setInterval":            e.schedule(true),
		"clearTimer":             e.clear,
	}
}

func (e *engine) chunk(arguments []jsc.Value) (any, error) {
	values, err := texts(arguments, 1)
	if err != nil {
		return nil, err
	}
	name := values[0] + ".js"
	source, err := chunkSources.ReadFile(path.Join("chunks", name))
	if err != nil {
		return nil, err
	}
	return e.context.Evaluate(string(source), name)
}

func (e *engine) sqlite(arguments []jsc.Value) (any, error) {
	values, err := texts(arguments, 1)
	if err != nil {
		return nil, err
	}
	return e.host.SQLite(e, e.dataOwner(), []byte(values[0]))
}

func texts(arguments []jsc.Value, count int) ([]string, error) {
	if len(arguments) < count {
		return nil, E.New("expected ", count, " arguments")
	}
	values := make([]string, count)
	for index := range values {
		value, err := arguments[index].ToString()
		if err != nil {
			return nil, err
		}
		values[index] = value
	}
	return values, nil
}

func (e *engine) startOperation(arguments []jsc.Value) (any, error) {
	values, err := texts(arguments, 3)
	if err != nil {
		return nil, err
	}
	id, name, encoded := values[0], values[1], values[2]
	run, found := operations[name]
	if !found {
		e.ready = append(e.ready, completion{id: id, err: E.New("unknown operation ", name)})
		e.ops[id] = &pendingOperation{cancel: func() {}, exec: e.current}
		return nil, nil
	}
	var args []json.RawMessage
	if err = json.Unmarshal([]byte(encoded), &args); err != nil {
		return nil, E.Cause(err, "decode the arguments of ", name)
	}
	ctx, cancel := context.WithCancel(e.current.ctx)
	e.ops[id] = &pendingOperation{cancel: cancel, exec: e.current}
	call := &operation{engine: e, exec: e.current, name: name, args: args}
	go func() {
		started := time.Now()
		value, err := run(ctx, call)
		attempted := started
		if call.dropped(ctx, err, attempted) && slices.Contains(readOperations, name) {
			attempted = time.Now()
			value, err = run(ctx, call)
		}
		if call.dropped(ctx, err, attempted) {
			err = call.droppedError(err, "during "+name)
		}
		if slices.Contains(timelineOperations, name) && e.host.timeline.active() {
			arguments := args
			if strings.HasPrefix(name, "accessibility.") {
				arguments = call.recorded
			}
			event := TimelineEvent{Time: started.UnixMilli(), Type: "operation", Name: name, Arguments: arguments, DurationMs: float64(time.Since(started).Microseconds()) / 1000}
			if err != nil {
				event.Error = Message(err)
			}
			e.host.timeline.record(event)
		}
		select {
		case e.completions <- completion{id: id, value: value, err: err}:
		case <-e.ctx.Done():
		}
	}()
	return nil, nil
}

func (e *engine) cancelOperation(arguments []jsc.Value) (any, error) {
	values, err := texts(arguments, 1)
	if err != nil {
		return nil, err
	}
	if op, found := e.ops[values[0]]; found {
		op.cancel()
	}
	return nil, nil
}

func (e *engine) locate(arguments []jsc.Value) (any, error) {
	if len(arguments) < 2 {
		return nil, E.New("expected the line and the column")
	}
	sources := e.sources()
	if sources == nil {
		return nil, nil
	}
	file, _, line, column, found := sources.Source(int(arguments[0].Number()), int(arguments[1].Number())-1)
	if !found || file == "" {
		return nil, nil
	}
	return struct {
		File   string `json:"file"`
		Line   int    `json:"line"`
		Column int    `json:"column"`
	}{file, line, column + 1}, nil
}

func (e *engine) takeFailure(arguments []jsc.Value) (any, error) {
	values, err := texts(arguments, 1)
	if err != nil {
		return nil, err
	}
	result := e.results[values[0]]
	if result.err == nil {
		return "", nil
	}
	delete(e.results, values[0])
	return FailureOf(result.err), nil
}

func (e *engine) value(arguments []jsc.Value) (any, error) {
	values, err := texts(arguments, 1)
	if err != nil {
		return nil, err
	}
	result := e.results[values[0]]
	delete(e.results, values[0])
	if result.value == nil {
		return json.RawMessage("null"), nil
	}
	return result.value, nil
}

func (e *engine) settle(result completion) error {
	op, live := e.ops[result.id]
	if !live {
		return nil
	}
	delete(e.ops, result.id)
	op.cancel()
	e.results[result.id] = result
	defer delete(e.results, result.id)
	return e.enter(op.exec, func() error { return e.callHook(e.hooks.settle, result.id) })
}

func (e *engine) subscribe(arguments []jsc.Value) (any, error) {
	values, err := texts(arguments, 3)
	if err != nil {
		return nil, err
	}
	id, kind, encoded := values[0], values[1], values[2]
	var args []json.RawMessage
	if err = json.Unmarshal([]byte(encoded), &args); err != nil {
		return nil, E.Cause(err, "decode the arguments of ", kind)
	}
	ctx, cancel := context.WithCancel(e.current.ctx)
	it := &subscription{cancel: cancel, exec: e.current, queue: kind == "device.applicationEvents" || kind == "sqlite.changes"}
	e.subscriptions[id] = it
	publish := func(value any, failure error) {
		it.access.Lock()
		switch {
		case failure != nil:
			it.failure = failure
		case it.queue && len(it.values) >= streamBacklog:
			it.failure = E.New("the stream ended with ", streamBacklog, " events unread")
		case it.queue:
			it.values = append(it.values, value)
		default:
			it.values = []any{value}
		}
		it.access.Unlock()
		select {
		case e.wake <- struct{}{}:
		default:
		}
	}
	if err = e.watch(ctx, kind, args, publish); err != nil {
		cancel()
		delete(e.subscriptions, id)
		return nil, err
	}
	return nil, nil
}

func (e *engine) unsubscribe(arguments []jsc.Value) (any, error) {
	values, err := texts(arguments, 1)
	if err != nil {
		return nil, err
	}
	if it, found := e.subscriptions[values[0]]; found {
		it.cancel()
		delete(e.subscriptions, values[0])
	}
	return nil, nil
}

func (e *engine) flush() error {
	for _, id := range slices.Sorted(maps.Keys(e.subscriptions)) {
		it, live := e.subscriptions[id]
		if !live {
			continue
		}
		it.access.Lock()
		values, failure := it.values, it.failure
		it.values, it.failure = nil, nil
		it.access.Unlock()
		if len(values) == 0 && failure == nil {
			continue
		}
		err := e.enter(it.exec, func() error {
			for _, value := range values {
				data, err := json.Marshal(value)
				if err != nil {
					failure = err
					break
				}
				if err = e.callHook(e.hooks.deliver, id, string(data)); err != nil {
					return err
				}
			}
			if failure == nil {
				return nil
			}
			it.cancel()
			delete(e.subscriptions, id)
			return e.callHook(e.hooks.end, id, FailureOf(failure))
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *engine) callHook(hook jsc.Value, arguments ...string) error {
	values := make([]jsc.Value, len(arguments))
	for index, text := range arguments {
		values[index] = e.context.NewString(text)
	}
	defer func() {
		for _, value := range values {
			value.Release()
		}
	}()
	out, err := hook.Call(values...)
	if err != nil {
		return err
	}
	out.Release()
	return nil
}

func (e *engine) print(arguments []jsc.Value) (any, error) {
	values, err := texts(arguments, 2)
	if err != nil {
		return nil, err
	}
	level := log.LevelInfo
	switch values[0] {
	case "error":
		level = log.LevelError
	case "warn":
		level = log.LevelWarn
	case "debug":
		level = log.LevelDebug
	}
	e.log(level, values[1])
	return nil, nil
}

func (e *engine) log(level log.Level, message string) {
	e.logTo(e.current, level, message)
}

// logTo logs for an execution; the goroutines of its operations log with it too.
func (e *engine) logTo(exec *execution, level log.Level, message string) {
	name := writeLog(e.logger, level, message)
	if logs := exec.logs; logs != nil {
		logs(LogEntry{Time: time.Now(), Level: level, Message: message})
	}
	if !strings.HasPrefix(e.swiplet.ID, replPrefix) {
		e.host.timeline.record(TimelineEvent{Time: time.Now().UnixMilli(), Type: "log", Swiplet: e.swiplet.ID, Level: name, Message: message})
	}
}

func (h *Host) Log(swiplet string, level log.Level, message string) {
	if swiplet == "" {
		writeLog(h.logger, level, message)
		return
	}
	name := writeLog(h.logFactory.NewLogger("swiplet/"+swiplet), level, message)
	h.timeline.record(TimelineEvent{Time: time.Now().UnixMilli(), Type: "log", Swiplet: swiplet, Level: name, Message: message})
}

func writeLog(logger log.ContextLogger, level log.Level, message string) string {
	switch level {
	case log.LevelError:
		logger.Error(message)
		return "error"
	case log.LevelWarn:
		logger.Warn(message)
		return "warn"
	case log.LevelDebug:
		logger.Debug(message)
		return "debug"
	default:
		logger.Info(message)
		return "info"
	}
}

func (e *engine) finish(arguments []jsc.Value) (any, error) {
	values, err := texts(arguments, 2)
	if err != nil {
		return nil, err
	}
	exec := e.main
	if len(arguments) > 2 {
		id, err := arguments[2].ToString()
		if err != nil {
			return nil, err
		}
		if exec = e.executions[id]; exec == nil {
			return nil, nil
		}
	}
	exec.finished, exec.failure, exec.output = true, values[0], values[1]
	return nil, nil
}

func (e *engine) putBlob(data []byte) string {
	e.access.Lock()
	defer e.access.Unlock()
	e.blobSequence++
	key := "blob:" + strconv.Itoa(e.blobSequence)
	e.blobs[key] = data
	return key
}

func (e *engine) takeBlob(key string) ([]byte, error) {
	e.access.Lock()
	defer e.access.Unlock()
	data, found := e.blobs[key]
	if !found {
		return nil, E.New("the data ", strconv.Quote(key), " was already taken")
	}
	delete(e.blobs, key)
	return data, nil
}

func (e *engine) put(arguments []jsc.Value) (any, error) {
	if len(arguments) == 0 {
		return nil, E.New("expected the data")
	}
	data, isBytes := arguments[0].Bytes()
	if !isBytes {
		return nil, E.New("the data is not a Uint8Array")
	}
	return e.putBlob(data), nil
}

func (e *engine) take(arguments []jsc.Value) (any, error) {
	values, err := texts(arguments, 1)
	if err != nil {
		return nil, err
	}
	data, err := e.takeBlob(values[0])
	if err != nil {
		return nil, err
	}
	return jsc.Bytes(data), nil
}

func (e *engine) random(arguments []jsc.Value) (any, error) {
	if len(arguments) == 0 {
		return nil, E.New("expected the length")
	}
	data := make([]byte, int(arguments[0].Number()))
	_, err := rand.Read(data)
	return jsc.Bytes(data), err
}

func (e *engine) digest(arguments []jsc.Value) (any, error) {
	if len(arguments) < 2 {
		return nil, E.New("expected the algorithm and the data")
	}
	algorithm, err := arguments[0].ToString()
	if err != nil {
		return nil, err
	}
	data, isBytes := arguments[1].Bytes()
	if !isBytes {
		return nil, E.New("the data is not a Uint8Array")
	}
	sum, err := digestOf(algorithm, data)
	if err != nil {
		return nil, err
	}
	return jsc.Bytes(sum), nil
}

func digestOf(algorithm string, data []byte) ([]byte, error) {
	switch algorithm {
	case "SHA-1":
		sum := sha1.Sum(data)
		return sum[:], nil
	case "SHA-256":
		sum := sha256.Sum256(data)
		return sum[:], nil
	case "SHA-384":
		sum := sha512.Sum384(data)
		return sum[:], nil
	case "SHA-512":
		sum := sha512.Sum512(data)
		return sum[:], nil
	}
	return nil, E.New("unknown digest ", algorithm)
}

func (e *engine) schedule(interval bool) jsc.Function {
	return func(arguments []jsc.Value) (any, error) {
		if len(arguments) == 0 || !arguments[0].IsFunction() {
			return nil, E.New("the callback is not a function")
		}
		// HTML's timer initialization steps: no clamp, except 4 ms once timers nest more than 5 deep
		var delay time.Duration
		if len(arguments) > 1 {
			if ms := arguments[1].Number(); ms > 0 && ms <= math.MaxInt32 {
				delay = time.Duration(ms * float64(time.Millisecond))
			}
		}
		nesting := e.nesting + 1
		if nesting > 5 {
			delay = max(delay, 4*time.Millisecond)
		}
		rest := arguments[min(len(arguments), 2):]
		arguments[0].Protect()
		for _, argument := range rest {
			argument.Protect()
		}
		e.sequence++
		e.timers = append(e.timers, &timer{id: e.sequence, at: time.Now().Add(delay), delay: delay, interval: interval, nesting: nesting, callback: arguments[0], arguments: rest, exec: e.current})
		return e.sequence, nil
	}
}

func (e *engine) clear(arguments []jsc.Value) (any, error) {
	if len(arguments) == 0 {
		return nil, nil
	}
	id := arguments[0].Number()
	index := slices.IndexFunc(e.timers, func(it *timer) bool { return float64(it.id) == id })
	if index >= 0 {
		e.timers[index].release()
		e.timers = slices.Delete(e.timers, index, index+1)
	}
	return nil, nil
}

func (e *engine) fire(next *timer) error {
	nesting := next.nesting
	if next.interval {
		if next.nesting++; next.nesting > 5 {
			next.delay = max(next.delay, 4*time.Millisecond)
		}
		next.at = time.Now().Add(next.delay)
	} else {
		e.timers = slices.DeleteFunc(e.timers, func(it *timer) bool { return it == next })
		defer next.release()
	}
	e.nesting = nesting
	defer func() { e.nesting = 0 }()
	return e.enter(next.exec, func() error {
		result, err := next.callback.Call(next.arguments...)
		if err != nil {
			return err
		}
		result.Release()
		return nil
	})
}

func (e *engine) step(abort <-chan struct{}) (aborted bool, err error) {
	if len(e.ready) > 0 {
		result := e.ready[0]
		e.ready = e.ready[1:]
		return false, e.settle(result)
	}
	var wake <-chan time.Time
	var next *timer
	if len(e.timers) > 0 {
		next = common.MinBy(e.timers, func(it *timer) int64 { return it.at.UnixNano() })
		wait := time.NewTimer(time.Until(next.at))
		defer wait.Stop()
		wake = wait.C
	}
	select {
	case <-wake:
		return false, e.fire(next)
	case result := <-e.completions:
		return false, e.settle(result)
	case <-e.wake:
		return false, e.flush()
	case request := <-e.launches:
		e.start(request)
		return false, nil
	case exec := <-e.stops:
		e.end(exec, stopped(exec.ctx))
		return false, nil
	case <-abort:
		return true, nil
	case <-e.ctx.Done():
		return false, context.Cause(e.ctx)
	}
}
