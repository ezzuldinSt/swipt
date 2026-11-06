package device

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/coredevice/accessibility"
	"github.com/swipt-automation/swipt/internal/coredevice/apps"
	"github.com/swipt-automation/swipt/internal/coredevice/instruments"
)

// Enabling the notifications sent the state of every app within 40 ms. They
// report a process as Running when it starts or resumes and again when it is
// suspended, and the frontmost application turned to a cold-launched app 70 to
// 550 ms after its process was reported (iPhone18,2, iOS 27.2 24B115).
const (
	appStatesBacklog = 250 * time.Millisecond
	recentFront      = 250 * time.Millisecond
	frontBurst       = 1200 * time.Millisecond
	frontProbe       = 3 * time.Second
)

// While waiting for an app to come to the front or leave it, the process
// state is read every processPoll on the kept instruments connection, which
// answers in a round trip while the accessibility service may take seconds
// during the transition. Tracking, once started, lasts as long as the
// connection, and a lost app state connection is made again.
const (
	processPoll           = 40 * time.Millisecond
	processProbeLimit     = 2 * time.Second
	appStatesRetryFirst   = time.Second
	appStatesRetryLongest = 30 * time.Second
)

// AXP notification 3, which the translator forwards at once for a change of
// first responder or native focus (24B5110a AccessibilityPlatformTranslation
// __axEventHandler 0x253fd8ba0, -[AXPRemoteCacheManager
// handleNotification:data:associatedObject:] 0x253fe623c); launching or
// switching to an app sometimes sent it (iPhone18,2, iOS 27.2 24B115).
const notificationFocusChanged = 3

type ForegroundApp struct {
	BundleID string
	PID      uint64
	Launched bool
	At       time.Time
}

type frontApplication struct {
	pid      uint64
	bundleID string
	at       time.Time
	via      string
}

func (a frontApplication) app() bool {
	return a.pid != 0 && a.bundleID != springboardBundleID
}

type foreground struct {
	conn *conn

	access    sync.Mutex
	current   frontApplication
	arrived   time.Time
	frontmost uint64
	tracked   bool
	pushing   bool
	actedAt   time.Time
	listeners map[int]func(ForegroundApp)
	next      int
	stop      context.CancelFunc
	stopped   chan struct{}
	until     time.Time
	pending   uint64
	kick      chan struct{}
	bundles   map[string]string
	paths     map[uint64]string
	probes    map[uint64]time.Time

	infoAccess sync.Mutex
	info       *instruments.DeviceInfo

	processes func(ctx context.Context, pids []uint64) ([]uint64, error)
	tracker   func(ctx context.Context, stopped chan struct{})
}

func newForeground(c *conn) *foreground {
	f := &foreground{
		conn: c, listeners: make(map[int]func(ForegroundApp)), kick: make(chan struct{}, 1), paths: make(map[uint64]string),
		probes: make(map[uint64]time.Time),
	}
	f.processes, f.tracker = f.foregroundProcesses, f.track
	return f
}

// get answers the app in front, reading it again when what is known of it was
// seen before the last action, which may have changed it.
func (f *foreground) get(ctx context.Context) (frontApplication, error) {
	f.access.Lock()
	current, tracked, acted := f.current, f.tracked, f.actedAt
	f.access.Unlock()
	if !current.at.Before(acted) && (tracked && current.bundleID != "" || time.Since(current.at) < recentFront) {
		return current, nil
	}
	return f.read(ctx)
}

// known answers the pid of the app in front when get would answer it without
// reading.
func (f *foreground) known() (uint64, bool) {
	f.access.Lock()
	defer f.access.Unlock()
	current := f.current
	if current.pid != 0 && !current.at.Before(f.actedAt) && (f.tracked && current.bundleID != "" || time.Since(current.at) < recentFront) {
		return current.pid, true
	}
	return 0, false
}

func (f *foreground) read(ctx context.Context) (frontApplication, error) {
	value, err := f.conn.ax.readLow(ctx, "frontmost", func(ctx context.Context, client *accessibility.RemoteClient) (any, error) {
		seen := time.Now()
		batch := &accessibility.Batch{}
		front := batch.Frontmost()
		if err := client.Run(ctx, batch); err != nil {
			return nil, err
		}
		if err := front.Err("read the frontmost application"); err != nil {
			return nil, err
		}
		bundleID := f.bundleOf(front.Value.Ref.PID)
		if bundleID == "" {
			read := &accessibility.Batch{}
			bundle := read.BundleID(front.Value)
			if err := client.Run(ctx, read); err != nil {
				return nil, err
			}
			if err := bundle.Err("read the frontmost application's bundle ID"); err != nil {
				return nil, err
			}
			bundleID = bundle.Value
		}
		return frontApplication{pid: front.Value.Ref.PID, bundleID: bundleID, at: seen, via: "frontmost"}, nil
	})
	if err != nil {
		return frontApplication{}, err
	}
	front := value.(frontApplication)
	f.conn.connection.trace(front.at, "frontmost", front.bundleID+" pid "+strconv.FormatUint(front.pid, 10), time.Since(front.at))
	f.access.Lock()
	f.frontmost = front.pid
	f.access.Unlock()
	if front.bundleID == springboardBundleID {
		front = f.beneath(ctx, front)
	} else if f.contradicted(ctx, front) {
		return f.current, nil
	}
	return f.saw(front), nil
}

// The frontmost application and the tree named the previous app for up to
// 1.8 s after the device ran a launched one in front, and the device kept
// running the previous app in front for at least 700 ms of that (iPhone18,2,
// iOS 27.2 24B119).
//
// SpringBoard reported in front once the app before it left is contradicted
// by the same lag: an app named then must also be running in front.
func (f *foreground) contradicted(ctx context.Context, front frontApplication) bool {
	f.access.Lock()
	current := f.current
	f.access.Unlock()
	if !front.app() || front.pid == current.pid {
		return false
	}
	pids := []uint64{front.pid, current.pid}
	switch {
	case current.via == viaLeft:
		pids = pids[:1]
	case !current.app():
		return false
	}
	shown, err := f.processes(ctx, pids)
	if err != nil {
		f.conn.connection.trace(front.at, "stale.unknown", front.bundleID+" pid "+strconv.FormatUint(front.pid, 10)+": "+err.Error(), time.Since(front.at))
		return false
	}
	reason := ""
	switch {
	case current.app() && slices.Contains(shown, current.pid):
		reason = current.bundleID + " is still in front"
	case !slices.Contains(shown, front.pid):
		reason = "it is not in front"
	default:
		return false
	}
	f.conn.connection.trace(front.at, "stale", front.bundleID+" pid "+strconv.FormatUint(front.pid, 10)+" via "+front.via+": "+reason, time.Since(front.at))
	return true
}

// The frontmost application is SpringBoard on the home screen and also while
// one of its alerts, such as the paste prompt, covers an application
// (iPhone18,2, iOS 27.2 24B115).
func (f *foreground) beneath(ctx context.Context, springboard frontApplication) frontApplication {
	f.access.Lock()
	current := f.current
	var pids []uint64
	if current.app() {
		pids = append(pids, current.pid)
	}
	for pid, until := range f.probes {
		switch {
		case until.Before(springboard.at):
			delete(f.probes, pid)
		case pid != current.pid && pid != springboard.pid:
			pids = append(pids, pid)
		}
	}
	f.access.Unlock()
	if len(pids) == 0 {
		return springboard
	}
	shown, err := f.processes(ctx, pids)
	if err != nil {
		if ctx.Err() == nil {
			f.conn.logger.Debug(E.Cause(err, "read the foreground state of processes"))
		}
		return springboard
	}
	f.conn.connection.trace(springboard.at, "beneath", "probed "+fmt.Sprint(pids)+", foreground "+fmt.Sprint(shown), time.Since(springboard.at))
	switch {
	case len(shown) == 0:
		return springboard
	case shown[0] == current.pid:
		return frontApplication{pid: current.pid, bundleID: current.bundleID, at: springboard.at, via: "beneath"}
	}
	if bundleID := f.bundleOf(shown[0]); bundleID != "" {
		return frontApplication{pid: shown[0], bundleID: bundleID, at: springboard.at, via: "beneath"}
	}
	return springboard
}

func (f *foreground) foregroundProcesses(ctx context.Context, pids []uint64) ([]uint64, error) {
	f.infoAccess.Lock()
	defer f.infoAccess.Unlock()
	if f.info != nil && f.info.Err() != nil {
		f.info.Close()
		f.info = nil
	}
	if f.info == nil {
		info, err := instruments.DialDeviceInfo(ctx, f.conn.ctx, f.conn.device)
		if err != nil {
			return nil, err
		}
		f.info = info
	}
	var shown []uint64
	for _, pid := range pids {
		running, err := f.info.Foreground(ctx, pid)
		if err != nil {
			f.info.Close()
			f.info = nil
			return nil, err
		}
		if running {
			shown = append(shown, pid)
		}
	}
	return shown, nil
}

func (f *foreground) closeInfo() {
	f.infoAccess.Lock()
	defer f.infoAccess.Unlock()
	if f.info != nil {
		f.info.Close()
		f.info = nil
	}
}

// probeFront reports an app as soon as the device runs it in front, as the
// frontmost application stayed on the previous app for up to 1.8 s while a
// launched one already drew its first screens, reading it waited on the
// launching app, and the accessibility service stopped answering for 3 s while
// a resumed app came to the front (iPhone18,2, iOS 27.2 24B119).
func (f *foreground) probeFront(ctx context.Context, pid uint64, bundleID string) {
	if bundleID == "" || bundleID == springboardBundleID {
		return
	}
	deadline := time.Now().Add(frontProbe)
	for time.Now().Before(deadline) {
		f.access.Lock()
		current := f.current.pid
		f.access.Unlock()
		if current == pid {
			return
		}
		asked := time.Now()
		shown, err := f.processInFront(ctx, pid)
		if err != nil {
			if ctx.Err() == nil {
				f.conn.logger.Debug(E.Cause(err, "read the foreground state of process ", pid))
			}
			return
		}
		if shown {
			f.saw(frontApplication{pid: pid, bundleID: bundleID, at: asked, via: "instruments"})
			return
		}
		if sleep(ctx, processPoll-time.Since(asked)) != nil {
			return
		}
	}
}

func (f *foreground) observed(front frontApplication) {
	f.access.Lock()
	moved := front.pid != f.frontmost
	f.frontmost = front.pid
	held := front.bundleID == springboardBundleID && f.current.app()
	f.access.Unlock()
	switch {
	case !held:
		if !f.contradicted(f.conn.ctx, front) {
			f.saw(front)
		}
	case moved:
		f.trigger(0)
	}
}

func (f *foreground) bundleOf(pid uint64) string {
	f.access.Lock()
	defer f.access.Unlock()
	if f.bundles == nil {
		return ""
	}
	return f.bundles[f.paths[pid]]
}

func (f *foreground) saw(front frontApplication) frontApplication {
	f.access.Lock()
	defer f.access.Unlock()
	if !front.at.After(f.current.at) {
		return f.current
	}
	pid := front.pid
	changed := pid != f.current.pid || front.bundleID != f.current.bundleID
	f.current = front
	if path := f.paths[pid]; path != "" && f.bundles != nil {
		f.bundles[path] = front.bundleID
	}
	if !changed {
		return front
	}
	f.arrived = front.at
	f.conn.connection.trace(front.at, "foreground", front.bundleID+" pid "+strconv.FormatUint(pid, 10)+" via "+front.via, time.Since(front.at))
	app := ForegroundApp{BundleID: front.bundleID, PID: pid, At: front.at}
	f.conn.events.publish(Event{Kind: EventForeground, PID: app.PID, BundleID: app.BundleID})
	for _, listener := range f.listeners {
		listener(app)
	}
	return front
}

func (f *foreground) watch(listener func(ForegroundApp)) (cancel func()) {
	f.access.Lock()
	f.next++
	id := f.next
	f.listeners[id] = listener
	if f.stop == nil {
		ctx, stop := context.WithCancel(f.conn.ctx)
		f.stop, f.stopped = stop, make(chan struct{})
		go f.tracker(ctx, f.stopped)
	}
	f.access.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.access.Lock()
			delete(f.listeners, id)
			f.access.Unlock()
		})
	}
}

func (f *foreground) trigger(window time.Duration) {
	f.access.Lock()
	if until := time.Now().Add(window); until.After(f.until) {
		f.until = until
	}
	f.access.Unlock()
	select {
	case f.kick <- struct{}{}:
	default:
	}
}

// acted notes an action, after which the app in front is read again before it
// is answered, and read at once for those watching it.
func (f *foreground) acted() {
	f.access.Lock()
	f.actedAt = time.Now()
	watched := len(f.listeners) > 0
	f.access.Unlock()
	if watched {
		f.trigger(frontBurst)
	}
}

// settle ends the reads an action or a wait asked for, once its outcome is
// known.
func (f *foreground) settle() {
	f.access.Lock()
	f.until = time.Now()
	f.access.Unlock()
}

func (f *foreground) pushed(push accessibility.Push) {
	switch {
	case push.Frontmost != nil:
		pid := push.Frontmost.Ref.PID
		f.access.Lock()
		changed := f.stop != nil && pid != f.current.pid && pid != f.frontmost
		f.frontmost = pid
		if changed {
			f.pending = pid
		}
		f.access.Unlock()
		if changed {
			f.trigger(0)
		}
	case push.Notification == notificationFocusChanged:
		f.trigger(frontBurst)
	}
}

func (f *foreground) track(ctx context.Context, stopped chan struct{}) {
	defer close(stopped)
	if err := f.loadBundles(ctx); err != nil && ctx.Err() == nil {
		f.conn.logger.Debug(E.Cause(err, "list apps"))
	}
	var states *instruments.AppStates
	var pushes <-chan instruments.AppState
	var pushesDone <-chan struct{}
	var redial <-chan time.Time
	var backlog time.Time
	retry := appStatesRetryFirst
	dial := func() {
		var err error
		states, err = instruments.DialAppStates(ctx, ctx, f.conn.device)
		if err != nil {
			states = nil
			if ctx.Err() == nil {
				f.conn.logger.Debug(E.Cause(err, "watch app states"))
				redial = time.After(retry)
				retry = min(retry*2, appStatesRetryLongest)
			}
			return
		}
		pushes, pushesDone, redial = states.States(), states.Done(), nil
		backlog = time.Now().Add(appStatesBacklog)
		f.access.Lock()
		f.pushing = true
		f.access.Unlock()
	}
	dial()
	defer func() {
		if states != nil {
			states.Close()
		}
	}()
	confirm := time.NewTimer(time.Hour)
	confirm.Stop()
	defer confirm.Stop()
	confirmed := make(chan struct{}, 1)
	confirming := true
	go func() {
		f.confirm(ctx, 0)
		confirmed <- struct{}{}
	}()
	var delay time.Duration
	for {
		select {
		case <-confirmed:
			confirming = false
			f.access.Lock()
			pending, until := f.pending, f.until
			f.access.Unlock()
			if pending != 0 || time.Now().Before(until) {
				confirm.Reset(min(max(delay*2, pollFirst), pollLongest))
			}
			continue
		case state := <-pushes:
			relevant, launched := f.record(state)
			if state.At.Before(backlog) {
				continue
			}
			f.conn.events.publish(Event{Kind: EventAppState, PID: state.PID, Name: state.State})
			f.conn.connection.trace(state.At, "appState", "pid "+strconv.FormatUint(state.PID, 10)+" "+state.State+" "+f.bundleOf(state.PID), time.Since(state.At))
			if launched {
				f.launched(state)
			}
			if relevant {
				if state.State == "Running" {
					go f.probeFront(ctx, state.PID, f.bundleOf(state.PID))
				}
				f.trigger(frontBurst)
			}
			continue
		case <-pushesDone:
			f.conn.logger.Debug("watch app states: ", states.Err())
			states.Close()
			states, pushes, pushesDone = nil, nil, nil
			f.access.Lock()
			f.pushing, f.tracked = false, false
			f.access.Unlock()
			retry = appStatesRetryFirst
			redial = time.After(retry)
			continue
		case <-redial:
			if dial(); states != nil {
				f.trigger(0)
			}
			continue
		case <-f.kick:
			delay = 0
		case <-confirm.C:
		case <-ctx.Done():
			return
		}
		if confirming {
			continue
		}
		f.access.Lock()
		pending, until := f.pending, f.until
		f.pending = 0
		f.access.Unlock()
		confirming = true
		go func() {
			f.confirm(ctx, pending)
			confirmed <- struct{}{}
		}()
		if left := time.Until(until); left > 0 {
			delay = min(max(delay*2, pollFirst), pollLongest)
			confirm.Reset(min(delay, left))
		}
	}
}

func (f *foreground) confirm(ctx context.Context, pushed uint64) {
	if pushed != 0 {
		if bundleID := f.bundleOf(pushed); bundleID != "" && bundleID != springboardBundleID {
			front := frontApplication{pid: pushed, bundleID: bundleID, at: time.Now(), via: "push"}
			if !f.contradicted(ctx, front) {
				f.saw(front)
				f.markTracked()
				return
			}
		}
	}
	if _, err := f.read(ctx); err != nil {
		if ctx.Err() == nil {
			f.conn.logger.Debug(E.Cause(err, "read the frontmost application"))
		}
		return
	}
	f.markTracked()
}

// markTracked trusts the app in front from then on, as the app state pushes
// keep it current.
func (f *foreground) markTracked() {
	f.access.Lock()
	f.tracked = f.stop != nil && f.pushing
	f.access.Unlock()
}

func (f *foreground) loadBundles(ctx context.Context) error {
	f.access.Lock()
	loaded := f.bundles != nil
	f.access.Unlock()
	if loaded {
		return nil
	}
	installed, err := apps.List(ctx, f.conn.device)
	if err != nil {
		return err
	}
	bundles := make(map[string]string)
	for _, app := range installed {
		if app.Path != "" {
			bundles[app.Path] = app.BundleID
		}
	}
	f.access.Lock()
	f.bundles = bundles
	f.access.Unlock()
	return nil
}

func (f *foreground) record(state instruments.AppState) (relevant, launched bool) {
	f.access.Lock()
	defer f.access.Unlock()
	_, known := f.paths[state.PID]
	switch {
	case state.State == "Terminated":
		delete(f.paths, state.PID)
		delete(f.probes, state.PID)
	case state.Path != "":
		f.paths[state.PID] = state.Path
	}
	if state.PID == f.current.pid {
		return true, false
	}
	if state.State != "Running" || f.bundles[state.Path] == "" {
		return false, false
	}
	f.probes[state.PID] = time.Now().Add(frontBurst)
	return true, !known
}

func (f *foreground) launched(state instruments.AppState) {
	f.access.Lock()
	defer f.access.Unlock()
	app := ForegroundApp{BundleID: f.bundles[state.Path], PID: state.PID, Launched: true, At: time.Now()}
	f.conn.connection.trace(app.At, "launch", app.BundleID+" pid "+strconv.FormatUint(app.PID, 10), 0)
	for _, listener := range f.listeners {
		listener(app)
	}
}

// waitFor waits until the app with bundleID is in front, seen after the last
// action, or the deadline passes, and answers the app in front. The tracking
// keeps reading the frontmost application meanwhile; when the app's process is
// known, or the wait is for SpringBoard and an app was in front, its process
// state is followed as well.
func (f *foreground) waitFor(ctx context.Context, bundleID string, pid uint64, deadline time.Time) (string, error) {
	changed := make(chan struct{}, 1)
	cancel := f.watch(func(ForegroundApp) {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	defer cancel()
	waiting, stop := context.WithDeadline(ctx, deadline)
	defer stop()
	f.access.Lock()
	from, acted := f.current, f.actedAt
	f.access.Unlock()
	f.trigger(time.Until(deadline))
	started := time.Now()
	switch {
	case pid != 0:
		f.conn.connection.trace(started, "wait.arrival", bundleID+" pid "+strconv.FormatUint(pid, 10), 0)
		go f.followArrival(waiting, pid, bundleID)
	case bundleID == springboardBundleID && from.app():
		f.conn.connection.trace(started, "wait.leave", from.bundleID+" pid "+strconv.FormatUint(from.pid, 10), 0)
		go f.followLeave(waiting, from)
	}
	// Seeing the same app again tells no listener, so the wait also looks
	// every processPoll.
	look := time.NewTicker(processPoll)
	defer look.Stop()
	for {
		f.access.Lock()
		current := f.current
		f.access.Unlock()
		if current.bundleID == bundleID && !current.at.Before(acted) {
			f.settle()
			f.conn.connection.trace(started, "wait.done", bundleID+" via "+current.via, time.Since(started))
			return bundleID, nil
		}
		select {
		case <-changed:
		case <-look.C:
		case <-waiting.Done():
			return current.bundleID, ctx.Err()
		}
	}
}

// SpringBoard came to the front as the app before it left, which its process
// state told. The followers report what they find as seen when the answer
// comes, so that the accessibility service, which names the previous app for a
// while after a switch, does not undo it with a read started earlier.
const viaLeft = "left"

// followArrival reads whether a launched process runs in front until it does.
func (f *foreground) followArrival(ctx context.Context, pid uint64, bundleID string) {
	for {
		asked := time.Now()
		shown, err := f.processInFront(ctx, pid)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				f.conn.logger.Debug(E.Cause(err, "read the foreground state of process ", pid))
			}
			return
		case shown:
			f.saw(frontApplication{pid: pid, bundleID: bundleID, at: time.Now(), via: "instruments"})
			return
		}
		if sleep(ctx, processPoll-time.Since(asked)) != nil {
			return
		}
	}
}

// followLeave reads whether an app is still running in front until it is not,
// as it is on going to the home screen, and then reports SpringBoard in front.
// It gives up when the app was not running in front to begin with.
func (f *foreground) followLeave(ctx context.Context, from frontApplication) {
	seen := false
	for {
		asked := time.Now()
		shown, err := f.processInFront(ctx, from.pid)
		answered := time.Now()
		switch {
		case err != nil:
			if ctx.Err() == nil {
				f.conn.logger.Debug(E.Cause(err, "read the foreground state of process ", from.pid))
			}
			return
		case shown:
			seen = true
		case !seen:
			return
		default:
			springboard, err := f.conn.springboardPID(ctx)
			if err != nil {
				return
			}
			f.saw(frontApplication{pid: springboard, bundleID: springboardBundleID, at: answered, via: viaLeft})
			return
		}
		if sleep(ctx, processPoll-time.Since(asked)) != nil {
			return
		}
	}
}

// processInFront reads whether a process runs in front on the kept
// instruments connection. The read is bound to the connection rather than to
// ctx, as canceling it would end that connection.
func (f *foreground) processInFront(ctx context.Context, pid uint64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	probe, cancel := context.WithTimeout(f.conn.ctx, processProbeLimit)
	defer cancel()
	shown, err := f.processes(probe, []uint64{pid})
	if err == nil {
		err = ctx.Err()
	}
	return len(shown) > 0, err
}

func (f *foreground) close() {
	f.access.Lock()
	stop, stopped := f.stop, f.stopped
	f.stop = nil
	f.access.Unlock()
	if stop != nil {
		stop()
		<-stopped
	}
	f.closeInfo()
}

type AppEvent struct {
	Kind     string
	BundleID string
	PID      uint64
	Time     time.Time
}

func (p *Phone) WatchApps(ctx context.Context, emit func(AppEvent)) error {
	var last ForegroundApp
	for {
		c, operation, done, err := p.begin(ctx)
		if err != nil {
			return err
		}
		var access sync.Mutex
		var queued []ForegroundApp
		changed := make(chan struct{}, 1)
		cancel := c.front.watch(func(app ForegroundApp) {
			access.Lock()
			queued = append(queued, app)
			access.Unlock()
			select {
			case changed <- struct{}{}:
			default:
			}
		})
		if last.PID == 0 {
			if front, err := c.front.get(operation); err == nil {
				access.Lock()
				if len(queued) == 0 {
					last = ForegroundApp{BundleID: front.bundleID, PID: front.pid}
				}
				access.Unlock()
			}
		}
		for operation.Err() == nil {
			select {
			case <-changed:
			case <-operation.Done():
				continue
			}
			access.Lock()
			arrived := queued
			queued = nil
			access.Unlock()
			for _, app := range arrived {
				if app.Launched {
					emit(AppEvent{Kind: "launch", BundleID: app.BundleID, PID: app.PID, Time: app.At})
					continue
				}
				if app.PID == last.PID {
					continue
				}
				if last.PID != 0 {
					emit(AppEvent{Kind: "background", BundleID: last.BundleID, PID: last.PID, Time: app.At})
				}
				emit(AppEvent{Kind: "foreground", BundleID: app.BundleID, PID: app.PID, Time: app.At})
				last = app
			}
		}
		cancel()
		done()
		if err = ctx.Err(); err != nil {
			return err
		}
	}
}

// arrival is when the application came to the front, as far as the device
// told, or zero when another one is in front.
func (f *foreground) arrival(pid uint64) time.Time {
	f.access.Lock()
	defer f.access.Unlock()
	if f.current.pid != pid {
		return time.Time{}
	}
	return f.arrived
}
