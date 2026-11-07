package device

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	"github.com/swipt-automation/swipt/internal/coredevice"
	"github.com/swipt-automation/swipt/internal/coredevice/display"
	"github.com/swipt-automation/swipt/internal/coredevice/imagemounter"
	"github.com/swipt-automation/swipt/internal/rfb"
)

// The Developer Disk Image registers this RemoteService for /System/Developer/usr/libexec/dtremotedisplayd
// (DDI 27A9266e, Library/LaunchDaemons/com.apple.coredevice.dtremotedisplayd.plist).
const displayService = "com.apple.coredevice.displayservice"

var (
	errRerouted       = E.New("a new connection to the device would take a different route; reconnecting")
	errRetryRequested = E.New("a new connection to the device was requested")
)

var retryDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second}

const (
	stableConnection    = 30 * time.Second
	defaultIdleTimeout  = 30 * time.Second
	displayPollInterval = 5 * time.Second
	routeCheckInterval  = 5 * time.Second
	failureRetry        = 10 * time.Second
	connectPatience     = 10 * time.Second
)

type State int

const (
	StateStopped State = iota
	StateConnecting
	StatePreparing
	StateRunning
	StateFailed
)

func (s State) String() string {
	switch s {
	case StateStopped:
		return "stopped"
	case StateConnecting:
		return "connecting"
	case StatePreparing:
		return "preparing"
	case StateRunning:
		return "running"
	case StateFailed:
		return "failed"
	}
	return "State(" + strconv.Itoa(int(s)) + ")"
}

type Observer interface {
	StateChanged(state State, err error)
	DeviceConnected(info coredevice.DeviceInfo)
	ViewerConnected(session *rfb.ServerSession)
	ViewerDisconnected(session *rfb.ServerSession, err error)
	StreamingChanged(active bool)
}

type Options struct {
	Name     string
	Connect  func(ctx context.Context) (*coredevice.Device, error)
	Logger   logger.ContextLogger
	Observer Observer

	// BLSBacklightState: 0 off, 1 inactiveOn, 2 activeOn, 3 dimmed (BacklightServices
	// 24A446, NSStringFromBLSBacklightState 0x22582161c, table 0x268a47640); -1 unavailable.
	BacklightState func() int32

	DiskImageDirectory string
	IdleTimeout        time.Duration

	// Moved, when set, reports whether a new connection would reach the device
	// another way than the connected one; that one is then replaced. It is
	// asked every routeCheckInterval.
	Moved func(ctx context.Context, device *coredevice.Device) bool

	// ScreenTiles is how many horizontal bands the screen feed's stream asks the device to
	// encode each frame in, each its own RTP stream (display.StreamOptions.TilesPerFrame); 0
	// takes ScreenTilesVariable, and one band is the default.
	ScreenTiles int
}

// ScreenTilesVariable, set to 2 or 4, makes the screen feed ask for that many bands a frame.
const ScreenTilesVariable = "SWIPT_SCREEN_TILES"

type Settings struct {
	AutoUnlockPasscode     string
	AutoUnlockPasscodeType PasscodeType
}

type Connection struct {
	options  Options
	name     atomic.Pointer[string]
	settings atomic.Pointer[Settings]
	logger   logger.ContextLogger
	notifier *notifier
	streams  *streamManager
	feeds    *screenFeeds
	input    *hidQueue
	clip     *clipboardBridge
	phone    *Phone

	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	retry       chan struct{}
	demand      chan struct{}
	displayWake chan struct{}

	access         sync.Mutex
	started        bool
	closed         bool
	state          State
	stateErr       error
	closeErr       error
	leases         int
	idle           *time.Timer
	endSession     context.CancelFunc
	failure        error
	failedAt       time.Time
	unavailable    error
	attemptErr     error
	failedAttempts uint64
	attached       *conn
	detachedAt     time.Time
	detachCause    error
	attemptCancel  context.CancelCauseFunc
	deviceWake     chan struct{}
	screen         rfb.ServerScreen
	display        displayGeometry
	videoProblem   error
	rfb            *rfb.Server
	daemonClean    bool
	stalls         []time.Time

	frontends sync.Mutex
	listening bool
	retired   bool
	sharing   *frontend

	relisted bool
	info     coredevice.DeviceInfo
}

func New(options Options) *Connection {
	if options.Logger == nil {
		options.Logger = logger.NOP()
	}
	if options.IdleTimeout == 0 {
		options.IdleTimeout = defaultIdleTimeout
	}
	if options.ScreenTiles == 0 {
		options.ScreenTiles, _ = strconv.Atoi(os.Getenv(ScreenTilesVariable))
	}
	s := &Connection{
		options:     options,
		logger:      options.Logger,
		done:        make(chan struct{}),
		retry:       make(chan struct{}, 1),
		demand:      make(chan struct{}, 1),
		displayWake: make(chan struct{}, 1),
		deviceWake:  make(chan struct{}),
	}
	s.name.Store(&options.Name)
	s.settings.Store(&Settings{})
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.notifier = newNotifier()
	s.streams = newStreamManager(s.logger, s.streamingChanged)
	s.feeds = newScreenFeeds(s)
	s.input = newHIDQueue(s, s.logger)
	s.clip = newClipboardBridge(s, s.logger)
	s.phone = &Phone{connection: s, logger: s.logger}
	return s
}

func (s *Connection) Start() error {
	s.access.Lock()
	if s.closed {
		s.access.Unlock()
		return E.New("device: closed")
	}
	if s.started {
		s.access.Unlock()
		return E.New("device: already started")
	}
	s.started = true
	s.access.Unlock()
	go s.run()
	return s.listen()
}

func (s *Connection) UpdateSettings(settings Settings) error {
	err := validateAutoUnlock(settings)
	if err != nil {
		return err
	}
	s.settings.Store(&settings)
	return nil
}

func (s *Connection) SetName(name string) {
	s.name.Store(&name)
}

func (s *Connection) hold() (release func()) {
	s.access.Lock()
	s.leases++
	if s.failure != nil && time.Since(s.failedAt) >= failureRetry {
		s.failure = nil
	}
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
	if s.attached == nil && s.attemptErr != nil {
		select {
		case s.retry <- struct{}{}:
		default:
		}
	}
	s.access.Unlock()
	s.wakeLoop()
	var once sync.Once
	return func() { once.Do(s.release) }
}

func (s *Connection) release() {
	s.access.Lock()
	defer s.access.Unlock()
	s.leases--
	if s.leases > 0 || s.closed {
		return
	}
	var timer *time.Timer
	timer = time.AfterFunc(s.options.IdleTimeout, func() {
		s.access.Lock()
		if s.idle != timer {
			s.access.Unlock()
			return
		}
		s.idle = nil
		end := s.endSession
		s.access.Unlock()
		if end != nil {
			s.logger.Debug("nothing uses the device connection; disconnecting")
			end()
		}
	})
	s.idle = timer
}

func (s *Connection) acquire(ctx context.Context) (*conn, func(), error) {
	release := s.hold()
	c, err := s.waitConn(ctx, false)
	if err != nil {
		release()
		return nil, nil, err
	}
	return c, release, nil
}

func (s *Connection) waitConn(ctx context.Context, persistent bool) (*conn, error) {
	s.access.Lock()
	attempts := s.failedAttempts
	s.access.Unlock()
	patience := time.NewTimer(connectPatience)
	defer patience.Stop()
	patient := true
	for {
		var err error
		s.access.Lock()
		c, wake := s.attached, s.deviceWake
		if c != nil && (c.ctx.Err() != nil || c.device.Closed()) {
			c = nil
		}
		switch {
		case c != nil:
		case s.closed:
			err = E.Extend(ErrDisconnected, "the device connection is closing")
		case s.unavailable != nil:
			err = E.Cause1(ErrUnavailable, s.unavailable)
		case s.failure != nil:
			err = E.Cause1(ErrUnavailable, s.failure)
		case !patient && s.attemptErr != nil && s.failedAttempts != attempts:
			err = E.Cause1(ErrUnavailable, s.attemptErr)
		}
		s.access.Unlock()
		if c != nil || err != nil {
			return c, err
		}
		select {
		case <-wake:
		case <-patience.C:
			patient = persistent
		case <-ctx.Done():
			s.access.Lock()
			attempt := s.attemptErr
			s.access.Unlock()
			if attempt != nil {
				return nil, E.Cause(ctx.Err(), "wait for the device, which did not connect: ", attempt)
			}
			return nil, E.Cause(ctx.Err(), "wait for the device")
		case <-s.ctx.Done():
			return nil, E.Extend(ErrDisconnected, "the device connection is closing")
		}
	}
}

func (s *Connection) SetUnavailable(err error) {
	s.access.Lock()
	s.unavailable = err
	if err != nil {
		s.wakeWaitersLocked()
	}
	s.access.Unlock()
	s.wakeLoop()
}

func (s *Connection) Retry() {
	s.access.Lock()
	cancel := s.attemptCancel
	s.failure = nil
	s.attemptErr = nil
	select {
	case s.retry <- struct{}{}:
	default:
	}
	s.access.Unlock()
	s.wakeLoop()
	if cancel != nil {
		cancel(errRetryRequested)
	}
}

func (s *Connection) wakeLoop() {
	select {
	case s.demand <- struct{}{}:
	default:
	}
}

func (s *Connection) wakeWaitersLocked() {
	close(s.deviceWake)
	s.deviceWake = make(chan struct{})
}

func (s *Connection) Close() error {
	s.access.Lock()
	if s.closed {
		s.access.Unlock()
		<-s.done
		return s.closeErr
	}
	s.closed = true
	started := s.started
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
	s.access.Unlock()
	s.cancel()
	s.closeFrontends()
	if started {
		<-s.done
	} else {
		close(s.done)
	}
	s.stopped(s.closeErr)
	return s.closeErr
}

func (s *Connection) stopped(err error) {
	s.access.Lock()
	changed := s.state != StateStopped || !errors.Is(s.stateErr, err) || !errors.Is(err, s.stateErr)
	s.state, s.stateErr = StateStopped, err
	s.access.Unlock()
	var last func()
	if changed {
		if err != nil {
			s.logger.Info("state: ", StateStopped, ": ", err)
		} else {
			s.logger.Debug("state: ", StateStopped)
		}
		last = func() { s.options.Observer.StateChanged(StateStopped, err) }
	}
	s.notifier.close(last)
}

func (s *Connection) setState(state State, err error) {
	s.access.Lock()
	if s.state == state && errors.Is(err, s.stateErr) && errors.Is(s.stateErr, err) {
		s.access.Unlock()
		return
	}
	s.state, s.stateErr = state, err
	s.access.Unlock()
	if err != nil {
		s.logger.Info("state: ", state, ", ", ProblemOf(err), ": ", err)
	} else {
		s.logger.Debug("state: ", state)
	}
	s.notify(func(observer Observer) { observer.StateChanged(state, err) })
}

func (s *Connection) State() (State, error) {
	s.access.Lock()
	defer s.access.Unlock()
	return s.state, s.stateErr
}

func (s *Connection) Name() string {
	return *s.name.Load()
}

func (s *Connection) DeviceInfo() coredevice.DeviceInfo {
	s.access.Lock()
	defer s.access.Unlock()
	return s.info
}

func (s *Connection) notify(call func(Observer)) {
	s.notifier.post(func() { call(s.options.Observer) })
}

func (s *Connection) streamingChanged(active bool) {
	s.logger.Debug("device video running: ", active)
	s.notify(func(observer Observer) { observer.StreamingChanged(active) })
}

func (s *Connection) run() {
	defer close(s.done)
	inputContext, stopInput := context.WithCancel(context.Background())
	go s.input.run(inputContext)
	defer func() {
		stopInput()
		<-s.input.done
	}()
	for {
		var session context.Context
		var end context.CancelFunc
		s.access.Lock()
		if !s.closed && s.leases > 0 && s.unavailable == nil && s.failure == nil {
			session, end = context.WithCancel(s.ctx)
			s.endSession = end
		}
		s.access.Unlock()
		if session == nil {
			select {
			case <-s.ctx.Done():
				return
			case <-s.demand:
			}
			continue
		}
		s.serveSession(session)
		s.access.Lock()
		s.endSession = nil
		s.access.Unlock()
		end()
		if s.ctx.Err() != nil {
			return
		}
	}
}

func (s *Connection) serveSession(session context.Context) {
	failures := 0
	var lastErr error
	for session.Err() == nil {
		select {
		case <-s.retry:
		default:
		}
		s.setState(StateConnecting, lastErr)
		var connected time.Time
		err := func() error {
			ctx, cancel := context.WithCancelCause(session)
			defer cancel(nil)
			s.access.Lock()
			s.attemptCancel = cancel
			s.access.Unlock()
			device, err := s.options.Connect(ctx)
			s.access.Lock()
			s.attemptCancel = nil
			s.access.Unlock()
			if err != nil {
				if ctx.Err() != nil {
					return context.Cause(ctx)
				}
				return err
			}
			if err := session.Err(); err != nil {
				_ = device.Close()
				return err
			}
			connected = time.Now()
			return s.serveDevice(session, device)
		}()
		if session.Err() != nil {
			break
		}
		switch {
		case E.IsMulti(err, errRemounted, errRerouted, errRetryRequested):
			lastErr = nil
			failures = 0
			continue
		case stopping(err):
			s.fail(err)
			return
		}
		if connected.IsZero() {
			s.logger.Debug("connect to the device: ", err)
		} else {
			s.logger.Info("device connection ended: ", err)
		}
		lastErr = err
		s.access.Lock()
		s.attemptErr = err
		s.failedAttempts++
		s.wakeWaitersLocked()
		s.access.Unlock()
		if !connected.IsZero() && time.Since(connected) >= stableConnection {
			failures = 0
		}
		if !s.wait(session, retryDelays[min(failures, len(retryDelays)-1)]) {
			break
		}
		failures++
	}
	if s.ctx.Err() == nil {
		s.setState(StateStopped, nil)
	}
}

func stopping(err error) bool {
	switch ProblemOf(err) {
	case ProblemPairingRejected, ProblemDeveloperModeOff, ProblemNoDiskImage, ProblemDiskImage,
		ProblemListen, ProblemUnsupported, ProblemLoopbackAddress:
		return true
	}
	return false
}

func (s *Connection) fail(err error) {
	s.logger.Error("device connection stopped, ", ProblemOf(err), ": ", err)
	s.access.Lock()
	s.failure = err
	s.failedAt = time.Now()
	s.wakeWaitersLocked()
	s.access.Unlock()
	s.setState(StateFailed, err)
}

func (s *Connection) wait(session context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-session.Done():
		return false
	case <-s.retry:
		return true
	case <-timer.C:
		return true
	}
}

func (s *Connection) serveDevice(session context.Context, device *coredevice.Device) error {
	defer device.Close()
	owner, cancelOwner := context.WithCancelCause(context.Background())
	defer cancelOwner(context.Canceled)
	go func() {
		select {
		case <-device.Done():
			cancelOwner(device.Err())
		case <-owner.Done():
		}
	}()
	info := device.Info()
	s.logger.Info("device connected: ", info.UDID, ", ", info.ProductType, ", ", info.OSVersion, " (", info.BuildVersion, ")")
	s.access.Lock()
	s.info = info
	s.access.Unlock()
	s.notify(func(observer Observer) { observer.DeviceConnected(info) })
	s.setModel(info.ProductType)
	if major, ok := majorVersion(info.OSVersion); ok && major < 27 {
		return newError(ProblemUnsupported, "iOS ", info.OSVersion, " cannot share its screen; it needs iOS 27 or later")
	}
	s.setState(StatePreparing, nil)
	err := s.prepareDiskImage(session, device, imagemounter.New(device, s.logger))
	if err != nil {
		return lostOr(device, err)
	}
	if !device.HasService(displayService) {
		return newError(ProblemUnsupported, "the device does not offer ", displayService, " although its disk image is mounted")
	}
	displayClient := display.New(owner, device, s.logger)
	state, err := displayClient.Displays(session)
	if err != nil {
		return lostOr(device, E.Cause(err, "read the device's displays"))
	}
	primary, found := primaryDisplay(state)
	if !found {
		return lostOr(device, E.New("the device lists no usable screen among its ", len(state), " displays"))
	}
	s.updateDisplay(primary)
	c := s.attach(session, device, displayClient, owner)
	s.setRunning()
	rerouted := s.watchRoute(session, device)
	if err := s.detach(c); err != nil {
		s.access.Lock()
		closed := s.closed
		if closed {
			s.closeErr = err
		}
		s.access.Unlock()
		if !closed && session.Err() != nil {
			s.logger.Warn(E.Cause(err, "disconnect the device"))
		}
		return err
	}
	if err := session.Err(); err != nil {
		return err
	}
	if rerouted != nil {
		return rerouted
	}
	return &Error{Problem: ProblemNoTunnel, Err: E.Cause(device.Err(), "device connection lost")}
}

func (s *Connection) watchRoute(session context.Context, device *coredevice.Device) error {
	local, remote := device.TransportAddr()
	var tick <-chan time.Time
	if local.IsValid() && remote.IsValid() || s.options.Moved != nil {
		ticker := time.NewTicker(routeCheckInterval)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		select {
		case <-device.Done():
			return nil
		case <-session.Done():
			return nil
		case <-tick:
		}
		if s.options.Moved != nil && s.options.Moved(session, device) {
			return errRerouted
		}
		if !local.IsValid() || !remote.IsValid() {
			continue
		}
		conn, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(remote))
		if err != nil {
			continue
		}
		current := M.AddrPortFromNet(conn.LocalAddr()).Addr().Unmap()
		_ = conn.Close()
		if current != local.Addr().Unmap() {
			s.logger.Debug("device route changed from ", local.Addr(), " to ", current)
			return errRerouted
		}
	}
}

func lostOr(device *coredevice.Device, err error) error {
	select {
	case <-device.Done():
		return &Error{Problem: ProblemNoTunnel, Err: E.Cause(device.Err(), "device connection lost")}
	default:
		return err
	}
}

func (s *Connection) setRunning() {
	s.access.Lock()
	problem := s.videoProblem
	s.access.Unlock()
	s.setState(StateRunning, problem)
}

func (s *Connection) setVideoProblem(err error) {
	s.access.Lock()
	changed := (err == nil) != (s.videoProblem == nil)
	s.videoProblem = err
	running := s.state == StateRunning
	s.access.Unlock()
	if running && (changed || err != nil) {
		s.setState(StateRunning, err)
	}
}

func (s *Connection) attach(session context.Context, device *coredevice.Device, displayClient *display.Client, owner context.Context) *conn {
	c := newConn(s, session, device, displayClient, owner)
	s.access.Lock()
	s.attached = c
	s.attemptErr = nil
	s.wakeWaitersLocked()
	s.access.Unlock()
	go s.pollDisplay(c)
	s.clip.update()
	s.phone.attached(c)
	return c
}

func (s *Connection) detach(c *conn) error {
	s.access.Lock()
	s.attached = nil
	s.detachedAt = time.Now()
	s.detachCause = c.device.Err()
	s.wakeWaitersLocked()
	s.access.Unlock()
	c.cancel()
	s.clip.update()
	inputErr := s.input.forgetAndWait(c)
	streamErr := s.streams.stopDevice(c.device)
	return E.Errors(inputErr, streamErr, c.close())
}

func (s *Connection) currentConn() *conn {
	s.access.Lock()
	defer s.access.Unlock()
	if s.attached != nil && s.attached.ctx.Err() == nil {
		return s.attached
	}
	return nil
}

func (s *Connection) pollDisplay(c *conn) {
	timer := time.NewTimer(displayPollInterval)
	defer timer.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-s.displayWake:
		case <-timer.C:
			if !s.streams.videoRunning() {
				timer.Reset(displayPollInterval)
				continue
			}
		}
		state, err := c.display.Displays(c.ctx)
		if err != nil {
			if c.ctx.Err() == nil {
				s.logger.Debug(E.Cause(err, "read the device's displays"))
			}
		} else if primary, found := primaryDisplay(state); found {
			s.updateDisplay(primary)
		}
		timer.Reset(displayPollInterval)
	}
}

func (s *Connection) checkDisplay() {
	select {
	case s.displayWake <- struct{}{}:
	default:
	}
}

func (s *Connection) updateDisplay(info display.DisplayInfo) {
	s.access.Lock()
	previous := s.display
	s.display = geometryOf(info)
	changed := previous.Width != info.Width || previous.Height != info.Height
	if changed {
		// In mode 7 the device produced 2420×1668 visible video in a 2432×1680 coded frame;
		// mode 5 exposed the full 2432×1680 frame (observed on iPad17,1, iPadOS 27.2 24B5099f).
		screen := rfb.ServerScreen{Width: info.Width, Height: info.Height, Scale: float64(max(info.PointScale, 1)), Model: s.screen.Model}
		changed = screen != s.screen
		s.screen = screen
	}
	s.access.Unlock()
	if previous.Orientation != info.CurrentOrientation && previous.Orientation != "" {
		s.logger.Debug("screen orientation: ", info.CurrentOrientation, ", ", info.Width, "x", info.Height)
	}
	if changed {
		s.logger.Debug("screen size changed: ", info.Width, "x", info.Height)
		s.screenChanged()
	}
}

func (s *Connection) setModel(model string) {
	s.access.Lock()
	changed := s.screen.Model != model
	s.screen.Model = model
	s.access.Unlock()
	if changed {
		s.screenChanged()
	}
}

func (s *Connection) videoSize(width, height int) {
	if width <= 0 || height <= 0 {
		return
	}
	s.access.Lock()
	changed := s.screen.Width != width || s.screen.Height != height
	s.screen.Width, s.screen.Height = width, height
	s.access.Unlock()
	if changed {
		s.logger.Debug("the video's size differs from the screen's: ", width, "x", height)
		go s.screenChanged()
	}
}

func (s *Connection) geometry() screenGeometry {
	s.access.Lock()
	defer s.access.Unlock()
	return screenGeometry{
		Width:   s.screen.Width,
		Height:  s.screen.Height,
		Scale:   s.screen.Scale,
		Density: s.screen.Density(),
		Turns:   touchTurns(s.display, s.screen.Width, s.screen.Height),
	}
}

func majorVersion(version string) (int, bool) {
	major, _, _ := strings.Cut(version, ".")
	value, err := strconv.Atoi(major)
	return value, err == nil
}

type notifier struct {
	access sync.Mutex
	queue  []func()
	wake   chan struct{}
	closed bool
}

func newNotifier() *notifier {
	n := &notifier{wake: make(chan struct{}, 1)}
	go n.run()
	return n
}

func (n *notifier) post(call func()) {
	n.access.Lock()
	if n.closed {
		n.access.Unlock()
		return
	}
	n.queue = append(n.queue, call)
	n.access.Unlock()
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

func (n *notifier) run() {
	for {
		n.access.Lock()
		queue := n.queue
		n.queue = nil
		closed := n.closed
		n.access.Unlock()
		for _, call := range queue {
			call()
		}
		if closed && len(queue) == 0 {
			return
		}
		if len(queue) == 0 {
			<-n.wake
		}
	}
}

func (n *notifier) close(last func()) {
	n.access.Lock()
	if last != nil {
		n.queue = append(n.queue, last)
	}
	n.closed = true
	n.access.Unlock()
	select {
	case n.wake <- struct{}{}:
	default:
	}
}
