package feed

import (
	"bytes"
	"context"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	"github.com/swipt-automation/swipt/internal/device"
	"github.com/swipt-automation/swipt/internal/graphics/raster"
	"github.com/swipt-automation/swipt/internal/log"
)

var (
	// How long Latest waits for the first frame (SWScreenFirstFrameLimit).
	firstFrameLimit = 5 * time.Second
	// The stream stays open this long after the last lease goes, so calls in a row share it
	// (SWScreenFeed.m SWScreenLinger).
	lingerTime = time.Second
)

const (
	// How long opening the stream may take (graphics/screen.go screenOpenLimit).
	openLimit = 20 * time.Second
	// The batches that may wait for the decoder; past that a batch is dropped, which the
	// depacketizer sees as lost packets, so the decoder waits for a keyframe and asks for one.
	batchBacklog = 256
)

// UnavailableError reports that this PC or platform cannot decode the screen stream; the screen
// calls then fall back to screenshots.
type UnavailableError struct {
	Reason error
}

func (e *UnavailableError) Error() string {
	return "graphics: the live screen feed is not available: " + e.Reason.Error()
}

func (e *UnavailableError) Unwrap() error {
	return e.Reason
}

// Name is the DOMException name the JavaScript sees (raster.UnsupportedError).
func (e *UnavailableError) Name() string {
	return raster.NameNotSupported
}

// Available tells whether the screen stream can be decoded here, without opening it.
func Available() error {
	return available()
}

// Geometry is the screen as the device describes it when the stream opens: its size in pixels
// and its pixels a point (SWScreenGeometry).
type Geometry struct {
	Width  int
	Height int
	Scale  float64
}

// Stats counts what the feed has done since it was made.
type Stats struct {
	Decoder          string
	Frames           int64
	Converted        int64
	DroppedBatches   int64
	DamagedUnits     int64
	KeyFrameRequests int64
}

// screen is the device's stream, a *device.ScreenFeed.
type screen interface {
	Info() device.ScreenInfo
	RequestKeyFrame()
	Close()
}

type opener func(ctx context.Context, deliver func(batch []byte), end func(err error)) (screen, error)

// Feed keeps the newest frame of the device's screen while anyone holds a lease (SWScreenFeed.m).
// The first lease opens the stream; it closes a second after the last one is released.
// Decoding runs on its own goroutine for every frame, while the RGBA pixels are made only for
// the frames that a lease answers.
type Feed struct {
	open     opener
	check    func() error
	newCodec codecFactory
	options  codecOptions
	logger   log.ContextLogger

	access    sync.Mutex
	changed   chan struct{}
	users     int
	opening   bool
	lingering *time.Timer
	screen    screen
	session   *session
	geometry  Geometry
	latest    *frame
	sequence  int64
	failure   error

	frames, converted, dropped, damaged, requests atomic.Int64
	decoderName                                   atomic.Pointer[string]
	// observeDecoded, when set before the first Acquire, sees the arrival of every decoded frame.
	observeDecoded func(arrival float64)
}

// New makes a feed of the connection's screen.
func New(connection *device.Connection, logger log.ContextLogger) *Feed {
	f := newFeed(func(ctx context.Context, deliver func([]byte), end func(error)) (screen, error) {
		opened, err := connection.OpenScreen(ctx, deliver, end)
		if err != nil {
			return nil, err
		}
		return opened, nil
	}, newCodec, logger)
	f.check = available
	return f
}

func newFeed(open opener, factory codecFactory, contextLogger log.ContextLogger) *Feed {
	if contextLogger == nil {
		contextLogger = logger.NOP()
	}
	return &Feed{open: open, newCodec: factory, logger: contextLogger, changed: make(chan struct{})}
}

// broadcastLocked wakes everyone waiting for a frame or for the stream to open.
func (f *Feed) broadcastLocked() {
	close(f.changed)
	f.changed = make(chan struct{})
}

// Stats answers the feed's counters.
func (f *Feed) Stats() Stats {
	stats := Stats{
		Frames: f.frames.Load(), Converted: f.converted.Load(), DroppedBatches: f.dropped.Load(),
		DamagedUnits: f.damaged.Load(), KeyFrameRequests: f.requests.Load(),
	}
	if name := f.decoderName.Load(); name != nil {
		stats.Decoder = *name
	}
	return stats
}

// Acquire takes a lease on the feed, opening the stream when it is the first (SWScreenFeed.m
// acquire:). Release the lease when done.
func (f *Feed) Acquire(ctx context.Context) (*Lease, error) {
	if f.check != nil {
		if err := f.check(); err != nil {
			return nil, err
		}
	}
	f.access.Lock()
	f.users++
	if f.lingering != nil {
		f.lingering.Stop()
		f.lingering = nil
	}
	for f.opening {
		changed := f.changed
		f.access.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			f.access.Lock()
			f.releaseLocked()
			f.access.Unlock()
			return nil, ctx.Err()
		}
		f.access.Lock()
	}
	if f.screen != nil {
		geometry := f.geometry
		f.access.Unlock()
		return &Lease{feed: f, geometry: geometry}, nil
	}
	f.opening = true
	opening := newSession(f)
	f.access.Unlock()
	openContext, cancel := context.WithTimeout(ctx, openLimit)
	opened, err := f.open(openContext, opening.deliver, opening.end)
	cancel()
	f.access.Lock()
	defer f.access.Unlock()
	f.opening = false
	f.broadcastLocked()
	if err == nil && opening.endedWith != nil {
		opened.Close()
		err = opening.endedWith
	}
	if err != nil {
		close(opening.done)
		f.users--
		return nil, err
	}
	info := opened.Info()
	f.geometry = Geometry{Width: info.Width, Height: info.Height, Scale: max(info.Scale, 1)}
	f.screen = opened
	f.session = opening
	f.failure = nil
	f.replaceLatestLocked(nil)
	opening.start(opened)
	return &Lease{feed: f, geometry: f.geometry}, nil
}

// releaseLocked gives a lease back; the last one closes the stream after the linger time
// unless another lease comes first (SWScreenFeed.m releaseFeed).
func (f *Feed) releaseLocked() {
	f.users--
	if f.users > 0 {
		return
	}
	f.lingering = time.AfterFunc(lingerTime, f.closeIfIdle)
}

func (f *Feed) closeIfIdle() {
	f.access.Lock()
	if f.users != 0 || f.screen == nil {
		f.access.Unlock()
		return
	}
	closing, stopping := f.detachLocked()
	f.access.Unlock()
	closing.Close()
	stopping.stop()
}

// detachLocked forgets the stream and its decoding; the caller closes and stops them.
func (f *Feed) detachLocked() (screen, *session) {
	closing, stopping := f.screen, f.session
	f.screen, f.session = nil, nil
	f.replaceLatestLocked(nil)
	f.broadcastLocked()
	return closing, stopping
}

// Close stops the stream now, whatever leases are out; they then answer that it stopped.
func (f *Feed) Close() {
	f.access.Lock()
	if f.lingering != nil {
		f.lingering.Stop()
		f.lingering = nil
	}
	if f.screen == nil {
		f.access.Unlock()
		return
	}
	closing, stopping := f.detachLocked()
	f.access.Unlock()
	closing.Close()
	stopping.stop()
}

// RequestKeyFrame asks the device for a keyframe, as the recording does when it starts.
func (f *Feed) RequestKeyFrame() {
	f.access.Lock()
	opened := f.screen
	f.access.Unlock()
	if opened != nil {
		f.requests.Add(1)
		opened.RequestKeyFrame()
	}
}

// ended is the end of the stream that the device reports (SWScreenFeed.m onEnd:).
func (f *Feed) ended(s *session, err error) {
	if err == nil {
		err = E.New("the screen stream ended")
	}
	f.access.Lock()
	if f.session != s {
		if f.opening && s.endedWith == nil {
			s.endedWith = err
		}
		f.access.Unlock()
		return
	}
	f.failure = err
	f.screen, f.session = nil, nil
	f.replaceLatestLocked(nil)
	f.broadcastLocked()
	f.access.Unlock()
	s.stop()
}

// decoded makes a picture the newest frame (SWScreenFeed.m decoded:unit:).
func (f *Feed) decoded(s *session, picture *picture, unit *AccessUnit) {
	f.access.Lock()
	if f.session != s || f.screen == nil {
		f.access.Unlock()
		picture.recycle()
		return
	}
	made := &frame{picture: picture, time: unit.Arrival, turns: unit.Turns(), geometry: f.geometry, feed: f}
	made.refs.Store(1)
	f.replaceLatestLocked(made)
	f.sequence++
	f.broadcastLocked()
	f.access.Unlock()
	f.frames.Add(1)
	if f.observeDecoded != nil {
		f.observeDecoded(unit.Arrival)
	}
}

func (f *Feed) replaceLatestLocked(next *frame) {
	if f.latest != nil {
		f.latest.release()
	}
	f.latest = next
}

// frameAfter answers the newest frame once its sequence passes previous, nil at until, or the
// failure that ended the stream (SWScreenFeed.m frameAfter:until:token:sequence:error:).
func (f *Feed) frameAfter(ctx context.Context, previous int64, until time.Time) (*frame, int64, error) {
	f.access.Lock()
	for {
		switch {
		case f.failure != nil:
			err := f.failure
			f.access.Unlock()
			return nil, 0, err
		case f.screen == nil && !f.opening:
			f.access.Unlock()
			return nil, 0, &raster.Error{Name: raster.NameUnavailable, Message: "graphics: the screen stream stopped"}
		case f.latest != nil && f.sequence > previous:
			latest, sequence := f.latest, f.sequence
			latest.refs.Add(1)
			f.access.Unlock()
			return latest, sequence, nil
		case ctx.Err() != nil:
			f.access.Unlock()
			return nil, 0, &raster.Error{Name: raster.NameAbort, Message: "the operation was aborted"}
		}
		wait := time.Until(until)
		if wait <= 0 {
			f.access.Unlock()
			return nil, 0, nil
		}
		changed := f.changed
		f.access.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-changed:
		case <-ctx.Done():
		case <-timer.C:
		}
		timer.Stop()
		f.access.Lock()
	}
}

// Lease is a hold on the feed; it delivers the frames of screen.get, screen.waitForChange,
// screen.waitForStable and screen.find as a raster.Feed.
type Lease struct {
	feed     *Feed
	geometry Geometry
	access   sync.Mutex
	sequence int64
	released bool
}

var _ raster.Feed = (*Lease)(nil)

// Geometry is the screen as it was when the stream opened.
func (l *Lease) Geometry() Geometry {
	return l.geometry
}

// RequestKeyFrame asks the device for a keyframe.
func (l *Lease) RequestKeyFrame() {
	l.feed.RequestKeyFrame()
}

// Release gives the lease back; it may be called more than once.
func (l *Lease) Release() {
	l.access.Lock()
	released := l.released
	l.released = true
	l.access.Unlock()
	if released {
		return
	}
	l.feed.access.Lock()
	l.feed.releaseLocked()
	l.feed.access.Unlock()
}

func (l *Lease) check() error {
	l.access.Lock()
	defer l.access.Unlock()
	if l.released {
		return &raster.Error{Name: raster.NameState, Message: "graphics: the screen lease was released"}
	}
	return nil
}

func (l *Lease) answer(ctx context.Context, answered *frame, sequence int64) (*raster.Image, error) {
	defer answered.release()
	l.access.Lock()
	l.sequence = max(l.sequence, sequence)
	l.access.Unlock()
	return answered.image(ctx)
}

// Latest answers the newest frame, waiting up to 5 seconds for the first one
// (SWScreenFeed.m SWScreenFirstFrame).
func (l *Lease) Latest(ctx context.Context) (*raster.Image, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	f := l.feed
	f.access.Lock()
	if latest := f.latest; latest != nil {
		sequence := f.sequence
		latest.refs.Add(1)
		f.access.Unlock()
		return l.answer(ctx, latest, sequence)
	}
	f.access.Unlock()
	next, sequence, err := f.frameAfter(ctx, 0, time.Now().Add(firstFrameLimit))
	if err != nil {
		return nil, err
	}
	if next == nil {
		return nil, &raster.Error{Name: raster.NameUnavailable, Message: "graphics: the screen sent no frame within " + strconv.Itoa(int(firstFrameLimit/time.Second)) + " s; it may be off"}
	}
	return l.answer(ctx, next, sequence)
}

// Next answers a frame newer than the last one this lease answered, or nil when none comes by
// until.
func (l *Lease) Next(ctx context.Context, until time.Time) (*raster.Image, error) {
	if err := l.check(); err != nil {
		return nil, err
	}
	l.access.Lock()
	previous := l.sequence
	l.access.Unlock()
	next, sequence, err := l.feed.frameAfter(ctx, previous, until)
	if err != nil || next == nil {
		return nil, err
	}
	return l.answer(ctx, next, sequence)
}

// frame is a decoded picture with the arrival of its first packet in milliseconds since the
// epoch, the quarter turns that make it upright and the screen it belongs to (SWFrame.m). Its
// RGBA image is made once, when a lease first answers it; the picture's bytes go back to the
// pool when no one holds the frame or the image is made.
type frame struct {
	feed     *Feed
	time     float64
	turns    int
	geometry Geometry
	refs     atomic.Int32

	access  sync.Mutex
	picture *picture
	made    *raster.Image
}

func (fr *frame) release() {
	if fr.refs.Add(-1) == 0 {
		fr.access.Lock()
		if fr.picture != nil {
			fr.picture.recycle()
			fr.picture = nil
		}
		fr.access.Unlock()
	}
}

// image converts the frame to RGBA in points of the screen, cropped to the screen's size and
// turned upright, with the arrival as its timestamp (SWImageNode.m nodeWithFrame:).
func (fr *frame) image(ctx context.Context) (*raster.Image, error) {
	fr.access.Lock()
	defer fr.access.Unlock()
	if fr.made != nil {
		return fr.made, nil
	}
	if ctx.Err() != nil {
		return nil, &raster.Error{Name: raster.NameAbort, Message: "the operation was aborted"}
	}
	if fr.picture == nil {
		return nil, &raster.Error{Name: raster.NameUnavailable, Message: "graphics: the frame is gone"}
	}
	width, height := fr.picture.width, fr.picture.height
	if fr.geometry.Width > 0 {
		width = min(width, fr.geometry.Width)
	}
	if fr.geometry.Height > 0 {
		height = min(height, fr.geometry.Height)
	}
	pixels := fr.picture.rgba(width, height, fr.turns)
	fr.made = raster.FromImage(pixels, fr.geometry.Scale, fr.time)
	recycleRGBA(pixels)
	fr.picture.recycle()
	fr.picture = nil
	fr.feed.converted.Add(1)
	return fr.made, nil
}

// session is one opening of the stream: its batches, and the goroutine that depacketizes and
// decodes them (SWScreenFeed.m's serial queue).
type session struct {
	feed      *Feed
	batches   chan []byte
	done      chan struct{}
	stopOnce  sync.Once
	finished  chan struct{}
	endedWith error
}

func newSession(f *Feed) *session {
	return &session{feed: f, batches: make(chan []byte, batchBacklog), done: make(chan struct{}), finished: make(chan struct{})}
}

// deliver takes a batch from the device's reading goroutine; it never blocks it.
func (s *session) deliver(batch []byte) {
	select {
	case <-s.done:
		return
	default:
	}
	select {
	case s.batches <- bytes.Clone(batch):
	default:
		s.feed.dropped.Add(1)
	}
}

func (s *session) end(err error) {
	s.feed.ended(s, err)
}

func (s *session) start(opened screen) {
	go s.run(opened)
}

// stop ends the decoding and waits for the decoder to be released.
func (s *session) stop() {
	s.stopOnce.Do(func() {
		close(s.done)
	})
	<-s.finished
}

func (s *session) run(opened screen) {
	defer close(s.finished)
	// Media Foundation's objects live on this thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	f := s.feed
	decoder := newFrameDecoder(f.newCodec, f.options, f.logger, func(picture *picture, unit *AccessUnit) {
		f.decoded(s, picture, unit)
	})
	defer decoder.close()
	var depacketizer Depacketizer
	var units []*AccessUnit
	var named codec
	for {
		select {
		case <-s.done:
			return
		case batch := <-s.batches:
			units = depacketizer.PushBatch(batch, units[:0])
			for _, unit := range units {
				if unit.Damaged {
					f.damaged.Add(1)
				}
				if decoder.decode(unit) {
					f.requests.Add(1)
					opened.RequestKeyFrame()
				}
				if decoder.codec != nil && decoder.codec != named {
					named = decoder.codec
					name := named.Name()
					f.decoderName.Store(&name)
				}
			}
			clear(units)
		}
	}
}
