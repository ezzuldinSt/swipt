package device

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// A tree read of the device's accessibility is served again, to a snapshot or
// find that would read it the same way, while it is provably current: no
// action began or ended since the read began, the device reported no event
// (an accessibility notification, a tree push, a change of the app in front,
// input), axauditd was not restarted, the read saw no UIView animation running,
// and it began less than treeLife ago. treeLife bounds what no event reports,
// such as content arriving silently. On a still home screen axauditd pushed
// nothing for 20 s, and a second find of SpringBoard was answered in 1 ms
// instead of 335 ms, a second snapshot in 1 ms instead of 0.9 s (iPhone18,2,
// iOS 27.0.1 24A446 over Wi-Fi).
//
// After an action, once the device has been quiet for treeQuiet, the tree
// read last is read again in the background, so that the next snapshot or
// find finds it ready; this goes on for treeSettle after the action, as long
// as that tree was read within treeInterest.
//
// An application answers a tree read on its main thread, so reading again
// after every action weighs on one that answers slowly: X answered in several
// seconds or not at all, and axauditd stalled once every few actions
// (iPhone18,2, iOS 27.0.1 24A446). The background read leaves an application
// alone for treeSlowLife after one of its reads took treeSlow or more, or it
// stalled axauditd or left it waiting; snapshots and finds still read it.
const (
	treeLife     = 3 * time.Second
	treeQuiet    = 250 * time.Millisecond
	treeSettle   = 3 * time.Second
	treeInterest = 30 * time.Second
	treeWanted   = 1
	treeSlow     = 1500 * time.Millisecond
	treeSlowLife = 2 * time.Minute
)

// treeMark is what must not change for a tree to stay current.
type treeMark struct {
	screen, epoch, generation uint64
}

type treeLoad func(ctx context.Context, background bool) (*PhoneModel, error)

type treeEntry struct {
	model *PhoneModel
	mark  treeMark
	began time.Time
}

type treeWant struct {
	key  string
	load treeLoad
	used time.Time
}

type treeFlight struct {
	mark treeMark
	done chan struct{}
}

type treeCache struct {
	conn *conn
	// epoch counts the starts and ends of actions, and acting the actions
	// under way.
	epoch  atomic.Uint64
	acting atomic.Int32
	acted  chan struct{}

	access   sync.Mutex
	entries  map[string]treeEntry
	wants    []treeWant
	flights  map[string]*treeFlight
	settleBy time.Time
	// pids is the process each tree was read from last, and slow when each
	// slow process is left alone until.
	pids      map[string]uint64
	slow      map[uint64]time.Time
	slowAfter time.Duration
}

func newTreeCache(c *conn) *treeCache {
	return &treeCache{
		conn: c, acted: make(chan struct{}, 1), entries: make(map[string]treeEntry), flights: make(map[string]*treeFlight),
		pids: make(map[string]uint64), slow: make(map[uint64]time.Time), slowAfter: treeSlow,
	}
}

// noted records which process a tree came from, and marks it slow when its
// read took long, stalled axauditd, or went unanswered.
func (t *treeCache) noted(key string, model *PhoneModel, took time.Duration, err error) {
	pid := t.conn.front.lastPID()
	if model != nil && model.Root != nil && model.Root.AX.Ref.PID != 0 {
		pid = model.Root.AX.Ref.PID
	}
	if pid == 0 {
		return
	}
	t.access.Lock()
	defer t.access.Unlock()
	t.pids[key] = pid
	if took >= t.slowAfter || errors.Is(err, ErrStalled) || errors.Is(err, ErrNotAnswering) {
		t.slow[pid] = time.Now().Add(treeSlowLife)
	}
}

// slowFor tells whether the tree for key comes from a process the background
// read leaves alone, and which.
func (t *treeCache) slowFor(key string) (uint64, bool) {
	t.access.Lock()
	pid := t.pids[key]
	until, slow := t.slow[pid]
	t.access.Unlock()
	if pid == 0 {
		return 0, false
	}
	if slow && time.Now().Before(until) {
		return pid, true
	}
	return pid, t.conn.connection != nil && t.conn.connection.stalledLately(pid)
}

func (t *treeCache) mark() treeMark {
	return treeMark{screen: t.conn.events.screen.Load(), epoch: t.epoch.Load(), generation: t.conn.ax.current()}
}

// act marks an action from now until the returned function runs: no tree is
// served meanwhile, and none read before its end is served afterwards.
func (t *treeCache) act() func() {
	t.acting.Add(1)
	t.epoch.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			t.epoch.Add(1)
			t.acting.Add(-1)
			t.access.Lock()
			t.settleBy = time.Now().Add(treeSettle)
			t.access.Unlock()
			select {
			case t.acted <- struct{}{}:
			default:
			}
		})
	}
}

// changed drops every tree kept, for a change no event reports.
func (t *treeCache) changed() {
	t.epoch.Add(1)
}

func (t *treeCache) current(key string, mark treeMark) *PhoneModel {
	if t.acting.Load() != 0 {
		return nil
	}
	t.access.Lock()
	defer t.access.Unlock()
	entry, found := t.entries[key]
	if !found || entry.mark != mark || time.Since(entry.began) >= treeLife || entry.model.generation != mark.generation {
		return nil
	}
	return entry.model
}

// store keeps a tree read from mark on, unless it was read while UIView
// animations ran, which may leave its frames between two places.
func (t *treeCache) store(key string, model *PhoneModel, mark treeMark, began time.Time) {
	if model == nil || model.animating {
		return
	}
	t.access.Lock()
	defer t.access.Unlock()
	if previous, found := t.entries[key]; found && previous.began.After(began) {
		return
	}
	t.entries[key] = treeEntry{model: model, mark: mark, began: began}
}

func (t *treeCache) want(key string, load treeLoad) {
	t.access.Lock()
	defer t.access.Unlock()
	t.wants = slices.DeleteFunc(t.wants, func(want treeWant) bool { return want.key == key })
	t.wants = append(t.wants, treeWant{key: key, load: load, used: time.Now()})
	if len(t.wants) > treeWanted {
		t.wants = slices.Delete(t.wants, 0, len(t.wants)-treeWanted)
	}
}

// read answers the tree load reads for key: the kept one while it is current,
// the one a read under way since nothing changed brings, or a new read, which
// is kept.
func (t *treeCache) read(ctx context.Context, key string, load treeLoad) (*PhoneModel, bool, error) {
	if t.acting.Load() == 0 {
		t.want(key, load)
	}
	mark := t.mark()
	if model := t.current(key, mark); model != nil {
		return model, true, nil
	}
	t.access.Lock()
	flight := t.flights[key]
	t.access.Unlock()
	if flight != nil && flight.mark == mark {
		select {
		case <-flight.done:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
		if model := t.current(key, t.mark()); model != nil {
			return model, true, nil
		}
	}
	model, err := t.load(ctx, key, load, false)
	return model, false, err
}

func (t *treeCache) load(ctx context.Context, key string, load treeLoad, background bool) (*PhoneModel, error) {
	mark := t.mark()
	began := time.Now()
	flight := &treeFlight{mark: mark, done: make(chan struct{})}
	t.access.Lock()
	t.flights[key] = flight
	t.access.Unlock()
	defer func() {
		t.access.Lock()
		if t.flights[key] == flight {
			delete(t.flights, key)
		}
		t.access.Unlock()
		close(flight.done)
	}()
	model, err := load(ctx, background)
	t.noted(key, model, time.Since(began), err)
	if err == nil && t.acting.Load() == 0 {
		t.store(key, model, mark, began)
	}
	return model, err
}

// stale answers the trees wanted lately that are not current and not being
// read.
func (t *treeCache) stale(mark treeMark) []treeWant {
	t.access.Lock()
	wants := slices.Clone(t.wants)
	t.access.Unlock()
	var out []treeWant
	for _, want := range slices.Backward(wants) {
		t.access.Lock()
		_, flying := t.flights[want.key]
		t.access.Unlock()
		if !flying && time.Since(want.used) < treeInterest && t.current(want.key, mark) == nil {
			out = append(out, want)
		}
	}
	return out
}

func (t *treeCache) settling() bool {
	t.access.Lock()
	defer t.access.Unlock()
	return time.Now().Before(t.settleBy)
}

// run reads the trees wanted lately again after an action, once the device
// has been quiet for treeQuiet, until they are current or treeSettle passed.
func (t *treeCache) run() {
	ctx := t.conn.ctx
	changed, stop := t.conn.events.watch(true)
	defer stop()
	quiet := time.NewTimer(time.Hour)
	quiet.Stop()
	defer quiet.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.acted:
			quiet.Reset(treeQuiet)
			continue
		case <-changed:
			if t.settling() {
				quiet.Reset(treeQuiet)
			}
			continue
		case <-quiet.C:
		}
		if t.acting.Load() != 0 || !t.settling() {
			continue
		}
		for _, want := range t.stale(t.mark()) {
			if t.acting.Load() != 0 || ctx.Err() != nil {
				break
			}
			if pid, slow := t.slowFor(want.key); slow {
				record(time.Now(), "tree.background", "skipped: process "+strconv.FormatUint(pid, 10)+" answers slowly", 0)
				continue
			}
			// A failed read in the background waits for the next action.
			if _, err := t.load(ctx, want.key, want.load, true); err != nil {
				t.access.Lock()
				t.settleBy = time.Time{}
				t.access.Unlock()
				break
			}
		}
	}
}
