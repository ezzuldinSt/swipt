package automation

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/device"
	"github.com/swipt-automation/swipt/internal/log"
)

type ServiceStatus string

const (
	ServiceStarting ServiceStatus = "starting"
	ServiceRunning  ServiceStatus = "running"
	ServiceStopped  ServiceStatus = "stopped"
	ServiceFailed   ServiceStatus = "failed"
)

type ServiceState struct {
	Enabled bool
	Status  ServiceStatus
	Error   string
	Failure *Failure
	Since   time.Time
}

const (
	settleLimit    = 15 * time.Second
	stableService  = time.Minute
	firstRestart   = 5 * time.Second
	longestRestart = 5 * time.Minute
	keptLogs       = 1024
)

type runner struct {
	host       *Host
	id         string
	logger     log.ContextLogger
	ctx        context.Context
	cancel     context.CancelCauseFunc
	executions sync.WaitGroup
	retry      chan struct{}

	access      sync.Mutex
	state       ServiceState
	changed     chan struct{}
	stop        context.CancelFunc
	done        chan struct{}
	logWatchers map[*logWatcher]struct{}
}

type serviceWatcher struct {
	access  sync.Mutex
	pending map[string]ServiceState
	signal  chan struct{}
}

type logWatcher struct {
	access  sync.Mutex
	pending []LogEntry
	signal  chan struct{}
}

func (r *runner) enter(ctx context.Context) (context.Context, Swiplet, func(), error) {
	h := r.host
	swiplet, err := h.Swiplet(r.id)
	if err != nil {
		return nil, Swiplet{}, nil, err
	}
	h.access.Lock()
	current := h.runners[r.id] == r
	if current {
		r.executions.Add(1)
	}
	h.access.Unlock()
	if !current {
		return nil, Swiplet{}, nil, E.Cause1(ErrNotFound, E.New("there is no swiplet ", strconv.Quote(r.id)))
	}
	ctx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(r.ctx, func() { cancel(context.Cause(r.ctx)) })
	return ctx, swiplet, func() {
		stop()
		cancel(nil)
		r.executions.Done()
	}, nil
}

func (r *runner) end() {
	r.access.Lock()
	r.stopLocked()
	done := r.done
	r.access.Unlock()
	if done != nil {
		<-done
	}
	r.executions.Wait()
}

func (r *runner) startLocked() {
	if r.stop != nil {
		if r.state.Status == ServiceFailed {
			select {
			case r.retry <- struct{}{}:
			default:
			}
		}
		return
	}
	ctx, cancel := context.WithCancel(r.ctx)
	previous, done := r.done, make(chan struct{})
	r.stop, r.done = cancel, done
	go r.supervise(ctx, cancel, previous, done)
}

func (r *runner) stopLocked() {
	if r.stop != nil {
		r.stop()
		r.stop = nil
	}
}

func (r *runner) supervise(ctx context.Context, cancel context.CancelFunc, previous, done chan struct{}) {
	defer close(done)
	defer cancel()
	if previous != nil {
		select {
		case <-previous:
		case <-ctx.Done():
		}
	}
	failures := 0
	for ctx.Err() == nil {
		select {
		case <-r.retry:
		default:
		}
		r.set(ServiceStarting, nil)
		started := time.Now()
		err := r.runService(ctx)
		if ctx.Err() != nil {
			break
		}
		if err == nil {
			r.access.Lock()
			if r.done == done {
				r.stop = nil
			}
			r.setLocked(ServiceStopped, nil)
			r.access.Unlock()
			return
		}
		if r.host.deviceLost(err, started) {
			r.logger.Info("the service waits for the device: ", describeError(err))
			r.set(ServiceStarting, err)
			err = r.host.options.Device.WaitConnected(ctx)
			if err == nil {
				continue
			}
			if ctx.Err() != nil {
				break
			}
		}
		r.logger.Error("the service stopped: ", failureOf(err).Describe())
		r.set(ServiceFailed, err)
		if time.Since(started) >= stableService {
			failures = 0
		}
		wait := time.NewTimer(min(firstRestart<<min(failures, 16), longestRestart))
		failures++
		select {
		case <-wait.C:
		case <-r.retry:
			failures = 0
		case <-ctx.Done():
		}
		wait.Stop()
	}
	r.set(ServiceStopped, nil)
}

func (r *runner) runService(ctx context.Context) error {
	ctx, swiplet, exit, err := r.enter(ctx)
	if err != nil {
		return err
	}
	defer exit()
	if !swiplet.Manifest.Service {
		return E.Cause1(ErrNotFound, E.New("the swiplet ", strconv.Quote(r.id), " has no service"))
	}
	_, err = r.host.execute(ctx, swiplet, r.host.options.Dialog, r.log, entry{}, func() { r.set(ServiceRunning, nil) })
	return err
}

func (r *runner) set(status ServiceStatus, err error) {
	r.access.Lock()
	defer r.access.Unlock()
	r.setLocked(status, err)
}

func (r *runner) setLocked(status ServiceStatus, err error) {
	r.state.Status, r.state.Error, r.state.Failure, r.state.Since = status, "", nil, time.Now()
	if err != nil {
		r.state.Error = describeError(err)
		r.state.Failure = failureOf(err)
	}
	r.publishLocked()
}

func (r *runner) publishLocked() {
	r.host.publish(r.id, r.state)
	close(r.changed)
	r.changed = make(chan struct{})
}

func (s ServiceState) settled(enabled bool, requested time.Time) bool {
	switch s.Status {
	case ServiceRunning:
		return enabled
	case ServiceStopped:
		return !enabled || s.Since.After(requested)
	case ServiceFailed:
		return enabled && s.Since.After(requested)
	}
	return false
}

func (r *runner) log(entry LogEntry) {
	r.access.Lock()
	defer r.access.Unlock()
	for watcher := range r.logWatchers {
		watcher.access.Lock()
		watcher.pending = append(watcher.pending, entry)
		if len(watcher.pending) > keptLogs {
			watcher.pending = slices.Delete(watcher.pending, 0, len(watcher.pending)-keptLogs)
		}
		watcher.access.Unlock()
		select {
		case watcher.signal <- struct{}{}:
		default:
		}
	}
}

func (h *Host) deviceLost(err error, since time.Time) bool {
	phone := h.options.Device
	return phone != nil && errors.Is(err, device.ErrUnavailable) && phone.Disconnected(since)
}

func (h *Host) publish(id string, state ServiceState) {
	h.watchAccess.Lock()
	defer h.watchAccess.Unlock()
	for watcher := range h.watchers {
		watcher.put(id, state)
	}
}

func (w *serviceWatcher) put(id string, state ServiceState) {
	w.access.Lock()
	w.pending[id] = state
	w.access.Unlock()
	select {
	case w.signal <- struct{}{}:
	default:
	}
}

func (h *Host) serviceRunner(id string) (*runner, Swiplet, error) {
	swiplet, err := h.Swiplet(id)
	if err != nil {
		return nil, Swiplet{}, err
	}
	if !swiplet.Manifest.Service {
		return nil, Swiplet{}, E.Cause1(ErrNotFound, E.New("the swiplet ", strconv.Quote(id), " has no service"))
	}
	r, err := h.runner(id)
	return r, swiplet, err
}

func (h *Host) SetServiceEnabled(id string, enabled bool) error {
	h.installAccess.Lock()
	defer h.installAccess.Unlock()
	r, swiplet, err := h.serviceRunner(id)
	if err != nil {
		return err
	}
	marker := filepath.Join(swiplet.Directory, serviceFile)
	if enabled {
		err = os.WriteFile(marker, nil, 0o644)
	} else if err = os.Remove(marker); errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return err
	}
	r.access.Lock()
	defer r.access.Unlock()
	if r.state.Enabled != enabled {
		r.state.Enabled = enabled
		r.publishLocked()
	}
	if enabled {
		r.startLocked()
	} else {
		r.stopLocked()
	}
	return nil
}

func (h *Host) AwaitService(ctx context.Context, id string, enabled bool, requested time.Time) (ServiceState, error) {
	r, _, err := h.serviceRunner(id)
	if err != nil {
		return ServiceState{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, settleLimit)
	defer cancel()
	for {
		r.access.Lock()
		state, changed := r.state, r.changed
		r.access.Unlock()
		if state.settled(enabled, requested) {
			return state, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return state, nil
		case <-r.ctx.Done():
			return state, nil
		}
	}
}

func (h *Host) Service(id string) (ServiceState, error) {
	r, _, err := h.serviceRunner(id)
	if err != nil {
		return ServiceState{}, err
	}
	r.access.Lock()
	defer r.access.Unlock()
	return r.state, nil
}

func (h *Host) RetryServices() {
	h.access.Lock()
	runners := slices.Collect(maps.Values(h.runners))
	h.access.Unlock()
	for _, r := range runners {
		r.access.Lock()
		if r.stop != nil && r.state.Status == ServiceFailed {
			select {
			case r.retry <- struct{}{}:
			default:
			}
		}
		r.access.Unlock()
	}
}

func (h *Host) WatchServices(ctx context.Context, observe func(id string, state ServiceState)) error {
	watcher := &serviceWatcher{pending: make(map[string]ServiceState), signal: make(chan struct{}, 1)}
	h.watchAccess.Lock()
	h.watchers[watcher] = struct{}{}
	h.watchAccess.Unlock()
	defer func() {
		h.watchAccess.Lock()
		delete(h.watchers, watcher)
		h.watchAccess.Unlock()
	}()
	swiplets, err := h.Swiplets()
	if err != nil {
		return err
	}
	for _, swiplet := range swiplets {
		if !swiplet.Manifest.Service {
			continue
		}
		if r, err := h.runner(swiplet.ID); err == nil {
			r.access.Lock()
			watcher.put(swiplet.ID, r.state)
			r.access.Unlock()
		}
	}
	for {
		select {
		case <-watcher.signal:
		case <-ctx.Done():
			return ctx.Err()
		case <-h.ctx.Done():
			return context.Cause(h.ctx)
		}
		watcher.access.Lock()
		pending := watcher.pending
		watcher.pending = make(map[string]ServiceState)
		watcher.access.Unlock()
		for _, id := range slices.Sorted(maps.Keys(pending)) {
			observe(id, pending[id])
		}
	}
}

func (h *Host) WatchServiceLogs(ctx context.Context, id string, observe func(LogEntry)) error {
	r, _, err := h.serviceRunner(id)
	if err != nil {
		return err
	}
	watcher := &logWatcher{signal: make(chan struct{}, 1)}
	r.access.Lock()
	r.logWatchers[watcher] = struct{}{}
	r.access.Unlock()
	defer func() {
		r.access.Lock()
		delete(r.logWatchers, watcher)
		r.access.Unlock()
	}()
	for {
		select {
		case <-watcher.signal:
		case <-ctx.Done():
			return ctx.Err()
		case <-r.ctx.Done():
			return context.Cause(r.ctx)
		}
		watcher.access.Lock()
		pending := watcher.pending
		watcher.pending = nil
		watcher.access.Unlock()
		for _, entry := range pending {
			observe(entry)
		}
	}
}
