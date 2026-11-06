package device

import (
	"context"
	"fmt"
	"slices"
	"sync"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	"github.com/swipt-automation/swipt/internal/coredevice/hid"
	"github.com/swipt-automation/swipt/internal/rfb"
)

const inputQueueLimit = 4096

type hidItem struct {
	session   *rfb.ServerSession
	owner     context.Context
	event     rfb.ViewerEvent
	write     *rfb.Clipboard
	release   bool
	moves     bool
	unlock    bool
	forget    *conn
	forgotten chan error
	grant     *hidGrant
}

type inputSession struct {
	context context.Context
	cancel  context.CancelFunc
}

type hidQueue struct {
	connection *Connection
	logger     logger.ContextLogger
	done       chan struct{}

	access   sync.Mutex
	queue    []hidItem
	dropping bool
	buttons  map[*rfb.ServerSession]uint8
	sessions map[*rfb.ServerSession]inputSession
	wake     chan struct{}

	conn          *conn
	client        *hid.Client
	generation    uint64
	touch         touchMapper
	keys          *keyboard
	unlockPending bool
	closeErr      error
}

func newHIDQueue(connection *Connection, logger logger.ContextLogger) *hidQueue {
	return &hidQueue{
		connection: connection,
		logger:     logger,
		wake:       make(chan struct{}, 1),
		done:       make(chan struct{}),
		keys:       newKeyboard(),
		buttons:    make(map[*rfb.ServerSession]uint8),
		sessions:   make(map[*rfb.ServerSession]inputSession),
	}
}

func (q *hidQueue) push(item hidItem) {
	q.access.Lock()
	if len(q.queue) >= inputQueueLimit && item.event != nil {
		if !q.dropping {
			q.logger.Warn("the device takes input slower than the viewer sends it; dropping input")
		}
		q.dropping = true
		q.access.Unlock()
		return
	}
	q.dropping = false
	q.queue = append(q.queue, item)
	q.access.Unlock()
	q.signal()
}

func (q *hidQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *hidQueue) post(session *rfb.ServerSession, event rfb.ViewerEvent) {
	q.access.Lock()
	owner, present := q.sessions[session]
	if !present || owner.context.Err() != nil {
		q.access.Unlock()
		return
	}
	pointer, isPointer := event.(rfb.ViewerPointer)
	if !isPointer {
		q.access.Unlock()
		q.push(hidItem{session: session, owner: owner.context, event: event})
		return
	}
	held, known := q.buttons[session]
	q.buttons[session] = pointer.Buttons
	moves := known && held == pointer.Buttons
	if count := len(q.queue); moves && count > 0 {
		last := &q.queue[count-1]
		if last.moves && last.session == session && last.owner == owner.context {
			last.event = pointer
			q.access.Unlock()
			return
		}
	}
	q.access.Unlock()
	q.push(hidItem{session: session, owner: owner.context, event: event, moves: moves})
}

func (q *hidQueue) postClipboard(clipboard *rfb.Clipboard) {
	q.push(hidItem{write: clipboard})
}

func (q *hidQueue) sessionEnded(session *rfb.ServerSession) {
	q.access.Lock()
	if owner, present := q.sessions[session]; present {
		owner.cancel()
		delete(q.sessions, session)
	}
	delete(q.buttons, session)
	q.access.Unlock()
	q.push(hidItem{session: session, release: true})
}

func (q *hidQueue) forgetAndWait(c *conn) error {
	forgotten := make(chan error, 1)
	q.push(hidItem{forget: c, forgotten: forgotten})
	select {
	case err := <-forgotten:
		return err
	case <-q.done:
		return q.closeErr
	}
}

func (q *hidQueue) viewerConnected(session *rfb.ServerSession) {
	q.access.Lock()
	owner, cancel := context.WithCancel(session.Context())
	q.sessions[session] = inputSession{context: owner, cancel: cancel}
	q.access.Unlock()
	if q.connection.settings.Load().AutoUnlockPasscode != "" {
		q.push(hidItem{unlock: true})
	}
}

func (q *hidQueue) sessionResumed(session *rfb.ServerSession) {
	q.access.Lock()
	defer q.access.Unlock()
	if _, present := q.sessions[session]; !present {
		owner, cancel := context.WithCancel(session.Context())
		q.sessions[session] = inputSession{context: owner, cancel: cancel}
	}
}

func (q *hidQueue) videoReady() {
	if q.connection.settings.Load().AutoUnlockPasscode != "" {
		q.push(hidItem{})
	}
}

type hidGrant struct {
	queue *hidQueue
	conn  *conn
	ctx   context.Context
	ready chan struct{}
	done  chan struct{}
	once  sync.Once
}

func (q *hidQueue) acquire(ctx context.Context, c *conn) (*hidGrant, error) {
	grant := &hidGrant{queue: q, conn: c, ctx: ctx, ready: make(chan struct{}), done: make(chan struct{})}
	q.push(hidItem{grant: grant})
	select {
	case <-grant.ready:
		return grant, nil
	case <-ctx.Done():
	}
	select {
	case <-grant.ready:
		grant.release()
	default:
	}
	return nil, E.Cause(ctx.Err(), "wait for the device's input")
}

func (g *hidGrant) release() {
	g.once.Do(func() { close(g.done) })
}

func (g *hidGrant) software() *hid.Client {
	c := g.conn
	if c.software == nil {
		c.software = hid.New(c.owner, c.device, g.queue.logger, hid.SoftwareKeyboard)
	}
	return c.software
}

func (g *hidGrant) closeSoftware() error {
	c := g.conn
	if c.software == nil {
		return nil
	}
	err := c.software.Close()
	c.software = nil
	return err
}

func (g *hidGrant) send(send func(input *hid.Client) error) error {
	err := send(g.software())
	if err != nil {
		return E.Append(err, g.closeSoftware(), func(err error) error { return E.Cause(err, "close the device input") })
	}
	return nil
}

func (g *hidGrant) hardware() *hid.Client {
	q := g.queue
	if q.client != nil && q.conn != g.conn {
		q.closeClient()
	}
	if q.client == nil {
		q.client = hid.New(g.conn.owner, g.conn.device, q.logger, hid.HardwareKeyboard)
		q.conn = g.conn
		_, _, q.generation = q.connection.streams.inputState()
	}
	return q.client
}

func (g *hidGrant) settleHardware(err error) {
	q := g.queue
	if q.client == nil {
		return
	}
	if err != nil || len(q.connection.sessions()) == 0 {
		q.closeClient()
		return
	}
	if releaseErr := q.client.ReleaseAll(g.conn.ctx); releaseErr != nil {
		q.logger.Warn(E.Cause(releaseErr, "release input"))
		q.closeClient()
		return
	}
	q.resetState()
}

func (q *hidQueue) run(ctx context.Context) {
	defer close(q.done)
	defer q.closeClient()
	for {
		item, ok := q.next(ctx)
		if !ok {
			return
		}
		q.handle(ctx, item)
	}
}

func (q *hidQueue) next(ctx context.Context) (hidItem, bool) {
	for {
		q.access.Lock()
		index := 0
		if q.touch.busy() {
			index = slices.IndexFunc(q.queue, func(item hidItem) bool { return item.grant == nil })
		}
		if index >= 0 && index < len(q.queue) {
			item := q.queue[index]
			q.queue = slices.Delete(q.queue, index, index+1)
			q.access.Unlock()
			return item, true
		}
		q.access.Unlock()
		select {
		case <-ctx.Done():
			return hidItem{}, false
		case <-q.wake:
		}
	}
}

func (q *hidQueue) closeClient() {
	var err error
	if q.client != nil {
		err = q.client.Close()
	}
	q.client = nil
	q.conn = nil
	q.resetState()
	q.keys.capsLock = false
	if err != nil {
		q.logger.Warn(E.Cause(err, "close device input"))
		q.closeErr = E.Append(q.closeErr, err, func(err error) error {
			return E.Cause(err, "close device input")
		})
	}
}

func (q *hidQueue) resetState() {
	q.touch.reset()
	q.keys.reset()
}

func (q *hidQueue) handle(ctx context.Context, item hidItem) {
	if item.grant != nil {
		grant := item.grant
		if grant.ctx.Err() != nil {
			return
		}
		close(grant.ready)
		select {
		case <-grant.done:
		case <-ctx.Done():
		}
		return
	}
	if item.forgotten != nil {
		if q.conn == item.forget {
			q.closeClient()
		}
		if software := item.forget.software; software != nil {
			q.closeErr = E.Append(q.closeErr, software.Close(), func(err error) error { return E.Cause(err, "close the device input") })
			item.forget.software = nil
		}
		item.forgotten <- q.closeErr
		q.closeErr = nil
		return
	}
	c := q.connection.currentConn()
	if item.write != nil {
		if c != nil {
			q.writeClipboard(c, item.write)
		}
		return
	}
	if item.unlock {
		q.unlockPending = true
	}
	if q.client != nil && (c == nil || c != q.conn) {
		q.closeClient()
	}
	if c == nil {
		return
	}
	switch {
	case item.release:
		if q.client != nil {
			c.tree.changed()
			if len(q.connection.sessions()) == 0 {
				q.closeClient()
				return
			}
			if err := q.client.ReleaseAll(c.ctx); err != nil {
				q.logger.Warn(E.Cause(err, "release input"))
				q.closeClient()
			} else {
				q.resetState()
			}
		}
		return
	case item.event == nil && !q.unlockPending:
		return
	}
	running, _, generation := q.connection.streams.inputState()
	if !running || c.ctx.Err() != nil {
		if q.touch.busy() && q.client != nil {
			if err := q.client.ReleaseAll(c.ctx); err != nil {
				q.closeClient()
			} else {
				q.resetState()
			}
		}
		return
	}
	if q.client == nil {
		q.client = hid.New(c.owner, c.device, q.logger, hid.HardwareKeyboard)
		q.conn = c
		q.generation = generation
	}
	if q.unlockPending && q.checkUnlock(c) {
		return
	}
	if item.event == nil || item.owner.Err() != nil {
		return
	}
	eventContext, cancel := c.operation(item.owner)
	defer cancel()
	if generation != q.generation {
		q.generation = generation
		if err := q.client.ReleaseAll(eventContext); err != nil {
			q.logger.Warn(E.Cause(err, "release input after the video restarted"))
			q.closeClient()
			return
		}
		q.resetState()
	}
	running, wait, currentGeneration := q.connection.streams.inputState()
	if !running || currentGeneration != generation {
		return
	}
	if sleep(eventContext, wait) != nil {
		return
	}
	running, _, currentGeneration = q.connection.streams.inputState()
	if !running || currentGeneration != generation || eventContext.Err() != nil {
		return
	}
	q.touch.input = q.client
	q.touch.geometry = q.connection.geometry()
	if q.touch.geometry.Width < 2 || q.touch.geometry.Height < 2 {
		return
	}
	acted := c.tree.act()
	defer acted()
	if err := q.dispatch(eventContext, item); err != nil {
		q.logger.Debug(E.Cause(err, "send input ", fmt.Sprint(item.event)))
		q.closeClient()
	}
	if !q.touch.busy() {
		c.events.publish(Event{Kind: EventInput})
	}
}

func (q *hidQueue) dispatch(ctx context.Context, item hidItem) error {
	switch event := item.event.(type) {
	case rfb.ViewerPointer:
		return q.touch.pointer(ctx, event)
	case rfb.ViewerKey:
		return q.keys.handle(ctx, q.client, event)
	case rfb.ViewerScroll:
		if event.Momentum != 0 {
			return nil
		}
		if event.Continuous && event.Phase != rfb.PhaseNone {
			return q.touch.continuousScroll(ctx, event)
		}
		deltaX, deltaY := q.mergeWheel(ctx, item.session, event)
		return q.touch.wheel(ctx, event.X, event.Y, deltaX, deltaY)
	case rfb.ViewerGesture:
		if event.Kind == rfb.GestureKindScroll {
			return nil
		}
		return q.touch.gesture(ctx, event)
	case rfb.ViewerSystemCommand:
		return q.touch.systemCommand(ctx, event.Command)
	}
	return nil
}

func isWheel(event rfb.ViewerScroll) bool {
	return event.Momentum == 0 && (!event.Continuous || event.Phase == rfb.PhaseNone)
}

func wheelDelta(event rfb.ViewerScroll) (float64, float64) {
	deltaX, deltaY := event.DeltaX, event.DeltaY
	if deltaX == 0 && deltaY == 0 {
		deltaX, deltaY = float64(event.LinesX*10), float64(event.LinesY*10)
	}
	return deltaX, deltaY
}

func (q *hidQueue) mergeWheel(ctx context.Context, session *rfb.ServerSession, first rfb.ViewerScroll) (float64, float64) {
	deltaX, deltaY := wheelDelta(first)
	if sleep(ctx, wheelMerge) != nil {
		return deltaX, deltaY
	}
	q.access.Lock()
	defer q.access.Unlock()
	for len(q.queue) > 0 {
		next := q.queue[0]
		scroll, isScroll := next.event.(rfb.ViewerScroll)
		if !isScroll || next.session != session || !isWheel(scroll) {
			break
		}
		x, y := wheelDelta(scroll)
		deltaX += x
		deltaY += y
		q.queue[0] = hidItem{}
		q.queue = q.queue[1:]
	}
	return deltaX, deltaY
}

func (s *Connection) AdjustBrightness(ctx context.Context, steps int) error {
	button := hid.ButtonBrightnessUp
	if steps < 0 {
		button, steps = hid.ButtonBrightnessDown, -steps
	}
	return s.pressButton(ctx, button, steps)
}

func (s *Connection) LockScreen(ctx context.Context) error {
	button := hid.ButtonLock
	button.Hold = 0
	return s.pressButton(ctx, button, 1)
}

// Home wakes an already-locked display without toggling an ongoing video wake
// back off, as a second power-button event would.
func (s *Connection) WakeScreen(ctx context.Context) error {
	button := hid.ButtonHome
	// Back-to-back down/up woke ActiveOn from AOD on iPhone18,2 / 24B5110a.
	button.Hold = 0
	return s.pressButton(ctx, button, 1)
}

// dtuhidd drops the events it has not dispatched yet once their connection
// ends, however it ends (observed on iPhone18,2, iOS 27.2 24B5110a), so the
// buttons go through the client that stays for the viewers.
func (s *Connection) pressButton(ctx context.Context, button hid.Button, count int) error {
	c, release, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	defer c.tree.act()()
	ctx, cancel := c.operation(ctx)
	defer cancel()
	grant, err := s.input.acquire(ctx, c)
	if err != nil {
		return err
	}
	defer grant.release()
	input := grant.hardware()
	for range count {
		if err = input.PressButton(ctx, button); err != nil {
			s.input.closeClient()
			return err
		}
	}
	c.events.publish(Event{Kind: EventInput})
	return nil
}
