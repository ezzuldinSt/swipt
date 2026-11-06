package automation

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	"github.com/swipt-automation/swipt/internal/jsc"
	"github.com/swipt-automation/swipt/internal/log"
)

//go:embed rewrite.js
var rewriteSource string

var (
	errReset     = E.New("a reset of the JavaScript state interrupted the call")
	errSession   = E.New("the JavaScript session ended")
	errUnsettled = E.New("the script awaits a promise that nothing can settle")
)

var (
	exceptionName  = regexp.MustCompile(`^((?:[^:\n]*: )*)([A-Z][A-Za-z]*Error): `)
	exceptionFrame = regexp.MustCompile(`^ {4}at (?:(.*) \((.+):(\d+):(\d+)\)|(.+):(\d+):(\d+))$`)
)

type TimeLimitError struct {
	Limit time.Duration
}

func (e *TimeLimitError) Error() string {
	return "the script ran past its time limit of " + e.Limit.String()
}

func (e *TimeLimitError) Is(target error) bool {
	return target == context.DeadlineExceeded
}

type OutputKind int

const (
	OutputConsole OutputKind = iota
	OutputValue
	OutputImage
)

type Script struct {
	Code    string
	Args    []string
	Program bool
}

type Output struct {
	Kind  OutputKind
	Level log.Level
	Text  string
	JSON  json.RawMessage
	Image []byte
}

func ScriptFailure(err error) (*Failure, bool) {
	if failure, isFailure := errors.AsType[*Failure](err); isFailure {
		return failure, true
	}
	if _, isException := errors.AsType[*jsc.Exception](err); isException {
		text, trace, _ := strings.Cut(Message(err), "\n    at ")
		failure := &Failure{Name: "Error", Message: text}
		if match := exceptionName.FindStringSubmatch(text); match != nil {
			failure.Name, failure.Message = match[2], match[1]+text[len(match[0]):]
		}
		if trace != "" {
			for line := range strings.SplitSeq("    at "+trace, "\n") {
				match := exceptionFrame.FindStringSubmatch(line)
				if match == nil {
					continue
				}
				frame := Frame{Function: match[1], File: match[2] + match[5]}
				frame.Line, _ = strconv.Atoi(match[3] + match[6])
				frame.Column, _ = strconv.Atoi(match[4] + match[7])
				failure.Stack = append(failure.Stack, frame)
			}
		}
		return failure, true
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return &Failure{Name: "TimeoutError", Message: Message(err)}, true
	case E.IsMulti(err, errReset, errSession):
		return &Failure{Name: "AbortError", Message: Message(err)}, true
	case errors.Is(err, errUnsettled):
		return &Failure{Name: "Error", Message: Message(err)}, true
	}
	return nil, false
}

func failureOf(err error) *Failure {
	if failure, isFailure := ScriptFailure(err); isFailure {
		return failure
	}
	return &Failure{Name: "Error", Message: Message(err)}
}

func describeError(err error) string {
	if failure, isFailure := ScriptFailure(err); isFailure {
		return failure.Summary()
	}
	return Message(err)
}

type REPL struct {
	host   *Host
	id     string
	dialog Dialog

	access sync.Mutex
	engine *engine
	closed bool
}

const (
	replPrefix    = "@repl-"
	REPLDirectory = "@repl"
)

var replSequence atomic.Uint64

func (e *engine) dataOwner() string {
	if strings.HasPrefix(e.swiplet.ID, replPrefix) {
		return REPLDirectory
	}
	return e.swiplet.ID
}

func (h *Host) NewREPL(dialog Dialog) *REPL {
	return &REPL{host: h, id: replPrefix + strconv.FormatUint(replSequence.Add(1), 10), dialog: dialog}
}

func (r *REPL) session() (*engine, error) {
	r.access.Lock()
	defer r.access.Unlock()
	if r.closed {
		return nil, errSession
	}
	if r.engine == nil {
		e, err := r.host.newREPLEngine(r.id, r.dialog)
		if err != nil {
			return nil, E.Cause(err, "start JavaScript")
		}
		r.engine = e
		go e.serve()
	}
	return r.engine, nil
}

func (r *REPL) Execute(ctx context.Context, script Script, timeout time.Duration, emit func(Output)) error {
	e, err := r.session()
	if err != nil {
		return err
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, timeout, &TimeLimitError{Limit: timeout})
		defer cancel()
	}
	var access sync.Mutex
	open := true
	output := func(out Output) {
		access.Lock()
		defer access.Unlock()
		if open {
			emit(out)
		}
	}
	defer func() {
		access.Lock()
		open = false
		access.Unlock()
	}()
	exec := &execution{emit: output, logs: func(entry LogEntry) {
		output(Output{Kind: OutputConsole, Level: entry.Level, Text: entry.Message})
	}, done: make(chan struct{})}
	exec.ctx, exec.cancel = context.WithCancelCause(ctx)
	exec.cleanup = append(exec.cleanup, context.AfterFunc(e.ctx, func() { exec.cancel(context.Cause(e.ctx)) }))
	select {
	case e.launches <- launch{exec: exec, script: script}:
	case <-exec.ctx.Done():
		for _, stop := range exec.cleanup {
			stop()
		}
		return stopped(exec.ctx)
	}
	select {
	case <-exec.done:
		return exec.err
	case <-exec.ctx.Done():
		select {
		case <-exec.done:
			return exec.err
		default:
			return stopped(exec.ctx)
		}
	}
}

func (r *REPL) Reset(ctx context.Context) error {
	r.access.Lock()
	if r.closed {
		r.access.Unlock()
		return errSession
	}
	e := r.engine
	r.engine = nil
	r.access.Unlock()
	return r.discard(ctx, e, errReset)
}

func (r *REPL) Close() {
	r.access.Lock()
	r.closed = true
	e := r.engine
	r.engine = nil
	r.access.Unlock()
	_ = r.discard(context.Background(), e, errSession)
}

func (r *REPL) discard(ctx context.Context, e *engine, cause error) error {
	if e != nil {
		e.cancel(cause)
		select {
		case <-e.served:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func stopped(ctx context.Context) error {
	cause := context.Cause(ctx)
	if errors.Is(cause, context.Canceled) {
		return E.New("the call was cancelled")
	}
	return cause
}

func (h *Host) newREPLEngine(id string, dialog Dialog) (*engine, error) {
	directory := filepath.Join(h.options.Directory, REPLDirectory)
	if err := os.MkdirAll(filepath.Join(directory, DataFolder), 0o755); err != nil {
		return nil, err
	}
	e, err := h.newEngine(Swiplet{ID: id, Directory: directory}, dialog, logger.NOP(), nil)
	if err != nil {
		return nil, err
	}
	e.launches, e.stops, e.served = make(chan launch), make(chan *execution), make(chan struct{})
	evaluated, err := e.context.Evaluate(rewriteSource+"\n;__swiptRewrite", "rewrite.js")
	if err == nil {
		e.rewrite, err = evaluated.Get("rewrite")
		evaluated.Release()
	}
	if err == nil {
		err = e.callHook(e.hooks.repl)
	}
	if err != nil {
		e.close()
		return nil, err
	}
	stop := context.AfterFunc(h.ctx, func() { e.cancel(context.Cause(h.ctx)) })
	e.main.cleanup = append(e.main.cleanup, stop)
	return e, nil
}

func (e *engine) serve() {
	defer close(e.served)
	for e.ctx.Err() == nil {
		_, err := e.step(nil)
		if e.ctx.Err() != nil {
			break
		}
		if _, thrown := errors.AsType[*jsc.Exception](err); thrown {
			e.log(log.LevelError, "uncaught in a timer: "+err.Error())
		} else if err != nil && !errors.Is(err, jsc.ErrTerminated) {
			e.log(log.LevelError, err.Error())
		}
		e.collect()
	}
	for _, exec := range e.executions {
		e.end(exec, context.Cause(e.ctx))
	}
	for _, stop := range e.main.cleanup {
		stop()
	}
	e.close()
}

func (e *engine) start(request launch) {
	exec := request.exec
	e.launched++
	exec.id = strconv.Itoa(e.launched)
	e.executions[exec.id] = exec
	exec.cleanup = append(exec.cleanup, context.AfterFunc(exec.ctx, func() {
		select {
		case e.stops <- exec:
		case <-e.ctx.Done():
		}
	}))
	if err := e.enter(exec, func() error { return e.evaluate(exec.id, request.script) }); err != nil {
		e.end(exec, err)
	}
}

func (e *engine) evaluate(id string, script Script) error {
	input := e.context.NewString(script.Code)
	rewritten, err := e.rewrite.Call(input)
	input.Release()
	if err != nil {
		return err
	}
	defer rewritten.Release()
	source, err := rewritten.Get("text")
	if err != nil {
		return err
	}
	text, err := source.ToString()
	source.Release()
	if err != nil {
		return err
	}
	columns, err := rewritten.Get("columns")
	if err != nil {
		return err
	}
	defer columns.Release()
	function, err := e.context.Evaluate(text, "input-"+id+".js")
	if err != nil {
		return err
	}
	defer function.Release()
	name := e.context.NewString(id)
	defer name.Release()
	encoded, _ := json.Marshal(struct {
		Args    []string `json:"args,omitempty"`
		Program bool     `json:"program"`
	}{script.Args, script.Program})
	request := e.context.NewString(string(encoded))
	defer request.Release()
	result, err := e.hooks.launch.Call(function, columns, name, request)
	if err != nil {
		return err
	}
	result.Release()
	return nil
}

func (e *engine) collect() {
	for _, exec := range e.executions {
		if !exec.finished {
			continue
		}
		if exec.failure != "" {
			e.end(exec, failureError(exec.failure))
			continue
		}
		var value struct {
			Text  string          `json:"text"`
			JSON  json.RawMessage `json:"json"`
			Image string          `json:"image"`
		}
		if exec.output != "" && json.Unmarshal([]byte(exec.output), &value) == nil {
			if value.Image != "" {
				if data, err := e.takeBlob(value.Image); err == nil {
					exec.emit(Output{Kind: OutputImage, Image: data})
				}
			} else {
				exec.emit(Output{Kind: OutputValue, Text: value.Text, JSON: value.JSON})
			}
		}
		e.end(exec, nil)
	}
	if len(e.executions) > 0 && len(e.ops) == 0 && len(e.timers) == 0 && len(e.subscriptions) == 0 && len(e.ready) == 0 {
		for _, exec := range e.executions {
			e.end(exec, errUnsettled)
		}
	}
}

func (e *engine) end(exec *execution, err error) {
	if e.executions[exec.id] != exec {
		return
	}
	delete(e.executions, exec.id)
	for _, stop := range exec.cleanup {
		stop()
	}
	var operations, timers, subscriptions int
	for id, op := range e.ops {
		if op.exec == exec {
			op.cancel()
			delete(e.ops, id)
			operations++
		}
	}
	kept := e.timers[:0]
	for _, it := range e.timers {
		if it.exec == exec {
			it.release()
			timers++
		} else {
			kept = append(kept, it)
		}
	}
	clear(e.timers[len(kept):])
	e.timers = kept
	for id, it := range e.subscriptions {
		if it.exec == exec {
			it.cancel()
			delete(e.subscriptions, id)
			subscriptions++
		}
	}
	if err == nil {
		var pending []string
		for _, it := range []struct {
			count int
			noun  string
		}{{operations, "operation"}, {timers, "timer"}, {subscriptions, "subscription"}} {
			switch it.count {
			case 0:
			case 1:
				pending = append(pending, "1 "+it.noun+" was")
			default:
				pending = append(pending, strconv.Itoa(it.count)+" "+it.noun+"s were")
			}
		}
		if len(pending) > 0 {
			exec.logs(LogEntry{Time: time.Now(), Level: log.LevelWarn, Message: strings.Join(pending, " and ") + " pending when the script finished, cancelled"})
		}
	}
	exec.err = err
	close(exec.done)
	exec.cancel(errEnded)
}
