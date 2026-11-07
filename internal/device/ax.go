package device

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	"github.com/swipt-automation/swipt/internal/coredevice/accessibility"
	"github.com/swipt-automation/swipt/internal/coredevice/apps"
)

const (
	stallWindow  = 2 * time.Minute
	stallLimit   = 3
	restartLimit = 10 * time.Second
	silentLimit  = 10 * time.Second
)

type axRun func(ctx context.Context, client *accessibility.RemoteClient) (any, error)

type axJob struct {
	ctx      context.Context
	key      string
	action   bool
	low      bool
	gen      uint64
	run      axRun
	started  bool
	retried  bool
	interest int
	value    any
	err      error
	done     chan struct{}
}

type axWorker struct {
	conn   *conn
	logger logger.ContextLogger
	done   chan struct{}
	wake   chan struct{}

	access  sync.Mutex
	queue   []*axJob
	silence silence

	generation atomic.Uint64
	client     *accessibility.RemoteClient
	daemon     uint64
	blamed     uint64
}

type silence struct {
	pid       uint64
	since     time.Time
	restarted bool
}

func newAXWorker(c *conn) *axWorker {
	return &axWorker{conn: c, logger: c.logger, done: make(chan struct{}), wake: make(chan struct{}, 1)}
}

func (w *axWorker) current() uint64 {
	return w.generation.Load()
}

func (w *axWorker) do(ctx context.Context, action bool, gen uint64, run func(ctx context.Context, client *accessibility.RemoteClient) error) error {
	job := &axJob{ctx: ctx, action: action, gen: gen, interest: 1, done: make(chan struct{}), run: func(ctx context.Context, client *accessibility.RemoteClient) (any, error) {
		return nil, run(ctx, client)
	}}
	_, err := w.await(ctx, w.enqueue(job))
	return err
}

func (w *axWorker) read(ctx context.Context, key string, run axRun) (any, error) {
	return w.queued(ctx, key, false, run)
}

func (w *axWorker) readLow(ctx context.Context, key string, run axRun) (any, error) {
	return w.queued(ctx, key, true, run)
}

func (w *axWorker) queued(ctx context.Context, key string, low bool, run axRun) (any, error) {
	w.access.Lock()
	index := slices.IndexFunc(w.queue, func(job *axJob) bool { return job.key == key && !job.started })
	var job *axJob
	if index >= 0 {
		job = w.queue[index]
		job.interest++
		job.low = job.low && low
		w.access.Unlock()
	} else {
		w.access.Unlock()
		job = w.enqueue(&axJob{ctx: w.conn.ctx, key: key, low: low, interest: 1, run: run, done: make(chan struct{})})
	}
	return w.await(ctx, job)
}

func (w *axWorker) enqueue(job *axJob) *axJob {
	w.access.Lock()
	w.queue = append(w.queue, job)
	w.access.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return job
}

func (w *axWorker) await(ctx context.Context, job *axJob) (any, error) {
	select {
	case <-job.done:
		return job.value, job.err
	case <-ctx.Done():
	}
	w.access.Lock()
	job.interest--
	started := job.started
	if !started && job.interest == 0 {
		w.queue = slices.DeleteFunc(w.queue, func(it *axJob) bool { return it == job })
	}
	w.access.Unlock()
	if started && job.key == "" {
		<-job.done
	}
	return nil, E.Cause(ctx.Err(), "wait for the accessibility service")
}

func (w *axWorker) next() *axJob {
	for {
		w.access.Lock()
		if len(w.queue) > 0 {
			index := max(slices.IndexFunc(w.queue, func(job *axJob) bool { return !job.low }), 0)
			job := w.queue[index]
			w.queue = slices.Delete(w.queue, index, index+1)
			job.started = true
			w.access.Unlock()
			return job
		}
		w.access.Unlock()
		if w.client != nil && w.client.Err() != nil && w.conn.ctx.Err() == nil {
			w.ended()
		}
		select {
		case <-w.conn.ctx.Done():
			return nil
		case <-w.wake:
		}
	}
}

func (w *axWorker) run() {
	defer close(w.done)
	if !w.conn.connection.options.DeferAccessibilityRestart && !w.conn.connection.axClean() {
		if err := w.restart("its state from before is not known to be clean"); err != nil {
			w.logger.Debug(E.Cause(err, "restart axauditd"))
		}
	}
	for {
		job := w.next()
		if job == nil {
			break
		}
		if job.ctx.Err() == nil {
			job.value, job.err = w.execute(job)
		} else {
			job.err = job.ctx.Err()
		}
		close(job.done)
	}
	w.access.Lock()
	queued := w.queue
	w.queue = nil
	w.access.Unlock()
	for _, job := range queued {
		job.err = E.New("the device is not connected")
		close(job.done)
	}
	w.closeClient()
}

func (w *axWorker) execute(job *axJob) (any, error) {
	if job.action {
		defer w.conn.tree.act()()
	}
	if job.gen != 0 && job.gen != w.current() {
		return nil, E.Extend(ErrStalled, "it was restarted since these elements were read, and they are gone; read the screen again")
	}
	client, err := w.open(job.ctx)
	if err != nil {
		return nil, err
	}
	value, err := job.run(job.ctx, client)
	silent, isSilent := errors.AsType[*accessibility.SilentFrontmostError](err)
	switch {
	case err == nil:
		w.blamed = 0
	case errors.Is(err, accessibility.ErrStalled):
		return nil, w.stalled(job, err)
	case client.Err() != nil && w.conn.ctx.Err() == nil:
		w.ended()
		// The device reset this connection, with axauditd still running, right
		// after the application in front was relaunched (iPhone18,2, iOS 27.3
		// 24C5037i).
		if !job.action && !job.retried {
			job.retried = true
			return w.execute(job)
		}
	case isSilent:
		w.silent(silent.PID)
	}
	return value, err
}

// After the device's accessibility had failed, each application launched
// later answered nothing, for more than ten minutes and across connections,
// while those launched before answered; restarting axauditd ended it
// (iPhone18,2, iOS 27.2 24B115).
func (w *axWorker) silent(pid uint64) {
	w.access.Lock()
	if w.silence.pid != pid {
		w.silence = silence{pid: pid, since: time.Now()}
	}
	due := !w.silence.restarted && time.Since(w.silence.since) >= silentLimit
	if due {
		w.silence.restarted = true
	}
	w.access.Unlock()
	if !due {
		return
	}
	w.logger.Warn("the application in front, process ", pid, ", has answered nothing for ", silentLimit)
	w.dropClient()
	if err := w.restart("the application in front does not answer it"); err != nil {
		w.logger.Debug(E.Cause(err, "restart axauditd"))
	}
}

func (w *axWorker) answered(pid uint64) {
	w.access.Lock()
	if w.silence.pid == pid {
		w.silence = silence{}
	}
	w.access.Unlock()
}

func (w *axWorker) open(ctx context.Context) (*accessibility.RemoteClient, error) {
	if w.client != nil {
		if w.client.Err() == nil {
			return w.client, nil
		}
		w.ended()
	}
	s := w.conn.connection
	if err := s.stallsAllowed(w.conn.front.lastPID()); err != nil {
		return nil, err
	}
	if !s.axClean() {
		if err := w.restart("its state from before is not known to be clean"); err != nil {
			w.logger.Debug(E.Cause(err, "restart axauditd"))
		}
	}
	client, err := accessibility.DialRemote(w.conn.ctx, w.conn.owner, w.conn.device, w.conn.pushed)
	if err != nil {
		if errors.Is(err, accessibility.ErrStalled) {
			return nil, w.stalled(&axJob{ctx: ctx}, err)
		}
		return nil, err
	}
	w.client = client
	listed, cancel := context.WithTimeout(w.conn.ctx, restartLimit)
	w.daemon, err = w.daemonPID(listed)
	cancel()
	if err != nil {
		w.logger.Debug(E.Cause(err, "find axauditd"))
	}
	go w.watch(client)
	return client, nil
}

// A remoteAXService connection that ended while its axauditd kept running
// left that daemon generating the frontmost application's tree at each change
// of the screen, for no client, until the daemon was killed; this was so after
// Swipt was killed on the device and after swipt-server was killed on a Mac
// (iPhone18,2, iOS 27.3 24C5037i).
func (w *axWorker) ended() {
	served := w.daemon
	w.logger.Debug("the connection to the device's accessibility service ended: ", w.client.Err())
	w.dropClient()
	ctx, cancel := context.WithTimeout(w.conn.ctx, restartLimit)
	defer cancel()
	pid, err := w.daemonPID(ctx)
	if err != nil {
		w.logger.Debug(E.Cause(err, "find axauditd"))
		return
	}
	if served != 0 && pid != served {
		pid = 0
	}
	if err = w.kill(ctx, pid, "its connection ended while it kept running"); err != nil {
		w.logger.Debug(E.Cause(err, "restart axauditd"))
	}
}

func (w *axWorker) dropClient() {
	if w.client == nil {
		return
	}
	if err := w.client.Close(); err != nil {
		w.logger.Debug(E.Cause(err, "close remote AX"))
	}
	w.conn.connection.setAXClean(false)
	w.client = nil
}

func (w *axWorker) closeClient() {
	if w.client == nil {
		return
	}
	err := w.client.Close()
	if err != nil {
		w.logger.Debug(E.Cause(err, "close remote AX"))
	}
	w.conn.connection.setAXClean(err == nil)
	w.client = nil
}

// axauditd reads the frontmost application's tree for every remoteAXService
// client on its main queue, which also serves requests and accepts
// connections, waiting up to 10000 s on an application that does not answer;
// a new connection only adds another such reader, while killing the daemon
// ended the wait, and dropped every object ID it had handed out
// (iPhone18,2, iOS 27.2 24B115).
func (w *axWorker) stalled(job *axJob, stall error) error {
	var named uint64
	reason := stall.Error()
	if stalled, isNamed := errors.AsType[*accessibility.StallError](stall); isNamed {
		named, reason = stalled.PID, stalled.Reason
	}
	pid, subject, basis, shown := w.shown(named)
	if shown {
		w.blamed = pid
		detail := subject + " has not answered the accessibility service yet: " + reason + "; " + basis + ", so the service is left running"
		w.logger.Warn(detail)
		return E.Extend(ErrNotAnswering, detail)
	}
	w.blamed = 0
	waited := "it waited on " + subject + "; " + basis
	w.logger.Warn("the device's accessibility service stopped answering: ", reason, "; ", waited, ", so it is restarted")
	if w.client != nil {
		_ = w.client.CloseCause(stall)
		w.client = nil
	}
	w.conn.connection.recordStall(w.conn.front.lastPID())
	w.conn.connection.setAXClean(false)
	performed := ""
	if job.action {
		performed = "; the action may have been performed"
	}
	if err := w.restart("it stopped answering"); err != nil {
		return E.Extend(ErrStalled, reason, "; ", waited, performed, "; restarting it failed: ", err)
	}
	return E.Extend(ErrStalled, reason, "; ", waited, performed, "; it was restarted, and the elements read before are gone; read the screen again")
}

func (w *axWorker) shown(named uint64) (pid uint64, subject, basis string, shown bool) {
	front := w.conn.front
	front.access.Lock()
	current := front.current
	front.access.Unlock()
	ctx, cancel := context.WithTimeout(w.conn.ctx, restartLimit)
	defer cancel()
	executables := make(map[uint64]string)
	processes, err := apps.Processes(ctx, w.conn.device)
	if err != nil {
		w.logger.Debug(E.Cause(err, "list the processes to name a stalled one"))
	}
	for _, process := range processes {
		executables[process.PID] = process.Executable
	}
	describe := func(pid uint64) string {
		text := "process " + strconv.FormatUint(pid, 10)
		if executable := executables[pid]; executable != "" {
			text += " " + executable
		}
		bundleID := front.bundleOf(pid)
		if bundleID == "" && pid == current.pid {
			bundleID = current.bundleID
		}
		if bundleID != "" {
			return bundleID + " (" + text + ")"
		}
		return text
	}
	pid = named
	via := ""
	switch {
	case named != 0:
	case w.blamed != 0:
		pid, via = w.blamed, "the request names no process, and "+describe(w.blamed)+" was in front when the service first stopped answering; "
	case current.pid != 0:
		pid, via = current.pid, "the request names no process, so it waits on the application in front; "
	default:
		return 0, "the application in front (not known yet)", "the request names no process, so it waits on the application in front, which is on the screen", true
	}
	subject = describe(pid)
	switch {
	case pid == current.pid:
		return pid, subject, via + "it is the application in front", true
	case pid == w.conn.springboard.Load(), strings.HasSuffix(executables[pid], "/SpringBoard.app/SpringBoard"):
		return pid, subject, via + "it is SpringBoard, which is on the screen", true
	}
	running, err := front.foregroundProcesses(ctx, []uint64{pid})
	switch {
	case err != nil:
		return pid, subject, via + "its foreground state could not be read: " + err.Error(), true
	case len(running) > 0:
		return pid, subject, via + "the device runs it in the foreground", true
	}
	return pid, subject, via + "the device does not run it in the foreground", false
}

func (w *axWorker) restart(reason string) error {
	ctx, cancel := context.WithTimeout(w.conn.ctx, restartLimit)
	defer cancel()
	pid, err := w.daemonPID(ctx)
	if err != nil {
		return err
	}
	return w.kill(ctx, pid, reason)
}

func (w *axWorker) kill(ctx context.Context, pid uint64, reason string) error {
	if pid != 0 {
		w.logger.Debug("restarting axauditd, as ", reason)
		if err := apps.Kill(ctx, w.conn.device, pid); err != nil {
			return err
		}
	}
	w.generation.Add(1)
	w.conn.nodes.clear()
	w.conn.connection.setAXClean(true)
	return nil
}

func (w *axWorker) daemonPID(ctx context.Context) (uint64, error) {
	processes, err := apps.Processes(ctx, w.conn.device)
	if err != nil {
		return 0, err
	}
	return common.Find(processes, func(it apps.Process) bool {
		return strings.HasSuffix(it.Executable, "/AccessibilityAudit.framework/Support/axauditd")
	}).PID, nil
}

func (w *axWorker) watch(client *accessibility.RemoteClient) {
	for {
		select {
		case <-client.Done():
			select {
			case w.wake <- struct{}{}:
			default:
			}
			return
		case <-client.ScreenViewingNotifications():
		}
		go func() {
			var count int
			err := w.do(w.conn.ctx, true, 0, func(ctx context.Context, client *accessibility.RemoteClient) (err error) {
				count, err = client.DismissScreenViewingNotifications(ctx)
				return
			})
			if count != 0 {
				w.logger.Debug("cleared ", count, " screen-viewing notifications")
			}
			if err != nil && w.conn.ctx.Err() == nil {
				w.logger.Warn(E.Cause(err, "clear screen-viewing notifications"))
			}
		}()
	}
}

func (s *Connection) axClean() bool {
	s.access.Lock()
	defer s.access.Unlock()
	return s.daemonClean
}

func (s *Connection) setAXClean(clean bool) {
	s.access.Lock()
	s.daemonClean = clean
	s.access.Unlock()
}

// stall is when the accessibility service stopped answering, and the process
// then in front, or 0 when none was known.
type stall struct {
	at  time.Time
	pid uint64
}

// An application that does not answer axauditd stalls it each time it is in
// front, while others go on answering (X, iPhone18,2, iOS 27.0.1 24A446): so
// the stalls count against the application in front when they happened, and
// one that keeps stalling it does not shut out the rest.
func (s *Connection) recordStall(pid uint64) {
	s.access.Lock()
	defer s.access.Unlock()
	s.stalls = append(common.Filter(s.stalls, func(it stall) bool { return time.Since(it.at) < stallWindow }), stall{at: time.Now(), pid: pid})
}

// stalledLately tells whether the process stalled axauditd within
// stallWindow.
func (s *Connection) stalledLately(pid uint64) bool {
	s.access.Lock()
	defer s.access.Unlock()
	return slices.ContainsFunc(s.stalls, func(it stall) bool { return it.pid == pid && time.Since(it.at) < stallWindow })
}

func (s *Connection) stallsAllowed(pid uint64) error {
	s.access.Lock()
	defer s.access.Unlock()
	s.stalls = common.Filter(s.stalls, func(it stall) bool { return time.Since(it.at) < stallWindow })
	counted := common.Filter(s.stalls, func(it stall) bool { return pid == 0 || it.pid == pid })
	if len(counted) < stallLimit {
		return nil
	}
	return E.Cause1(ErrUnavailable, E.New("the device's accessibility service stopped answering ", len(counted), " times in ", stallWindow,
		" with the same application in front, and was restarted each time; it is tried again ",
		time.Until(counted[0].at.Add(stallWindow)).Round(time.Second), " from now, or at once with another application in front"))
}
