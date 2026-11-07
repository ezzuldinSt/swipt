package feed

import (
	"bytes"
	"context"
	"image"
	"runtime"
	"runtime/debug"
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

// streamLinger is how long the stream stays open after the last lease goes, so the calls of a
// script, and the scripts an agent runs one after another, share it: opening it again takes
// a second or more to the first frame. SWScreenFeed.m's SWScreenLinger is a second.
const streamLinger = 30 * time.Second

var (
	// How long Latest waits for the first frame (SWScreenFirstFrameLimit).
	firstFrameLimit = 5 * time.Second
	lingerTime      = streamLinger
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
	Decoder string
	// DecoderThread tells how the decoding thread was raised on Windows: "MMCSS Playback",
	// "highest" or "normal".
	DecoderThread string
	// Color tells how the newest stream's pictures are converted: where the range came from,
	// and the H.273 code points of the stream's VUI.
	Color            string
	Frames           int64
	Converted        int64
	DroppedBatches   int64
	DamagedUnits     int64
	KeyFrameRequests int64
	// Decoding is the time from a unit's first packet reaching the feed to its picture coming
	// out of the decoder; Converting is the RGBA conversion of a frame a lease answers, and
	// Answering the time from the first packet to that frame's RGBA.
	Decoding, Converting, Answering Timing
	// Units and UnitBytes count the access units and their bytes; CodedUnits,
	// SkippedUnits and UnreadUnits count them by UnitInfo.Skip, and RepeatedUnits the ones
	// that repeat their picture; LastUnit is the newest.
	Units, UnitBytes                      int64
	CodedUnits, SkippedUnits, UnreadUnits int64
	RepeatedUnits                         int64
	LastUnit                              UnitInfo
}

// Screen is an open stream of the device's screen, a *device.ScreenFeed.
type Screen interface {
	Info() device.ScreenInfo
	RequestKeyFrame()
	Close()
}

type screen = Screen

// Opener opens the screen's stream: deliver takes its batches of records (device.appendFeedRecord)
// and end its failure, as device.Connection.OpenScreen does.
type Opener func(ctx context.Context, deliver func(batch []byte), end func(err error)) (Screen, error)

type opener = Opener

// Feed keeps the newest frame of the device's screen while anyone holds a lease (SWScreenFeed.m).
// The first lease opens the stream; it closes streamLinger after the last one is released.
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
	decoderName, colorName, threadPriority        atomic.Pointer[string]
	prewarming                                    atomic.Bool
	clock                                         feedClock
	decoding, converting, answering               timing
	// The running unit counts, which the decoding goroutine adds to, and the counts at the
	// last decoded frame, under access.
	units, unitBytes, changedAt, movedAt atomic.Int64
	coded, skipped, unread, repeated     atomic.Int64
	lastUnit                             atomic.Pointer[UnitInfo]
	framedTotals                         unitTotals
	// observeDecoded, when set before the first Acquire, sees the arrival of every decoded
	// frame and when it was decoded, in milliseconds since the epoch.
	observeDecoded func(arrival, decoded float64)

	nextListener   int
	unitListeners  map[int]func(*AccessUnit)
	frameListeners map[int]func(*Frame)
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

// NewWithOpener makes a feed of the stream that open opens, such as a stored one in a test;
// it decodes with Media Foundation like New.
func NewWithOpener(open Opener, logger log.ContextLogger) *Feed {
	f := newFeed(open, newCodec, logger)
	f.check = available
	return f
}

func newFeed(open opener, factory codecFactory, contextLogger log.ContextLogger) *Feed {
	if contextLogger == nil {
		contextLogger = logger.NOP()
	}
	return &Feed{
		open: open, newCodec: factory, logger: contextLogger, changed: make(chan struct{}), clock: preciseClock,
		unitListeners: make(map[int]func(*AccessUnit)), frameListeners: make(map[int]func(*Frame)),
	}
}

// AddUnitListener calls listener with every access unit of the stream, damaged ones too, in
// order on the decoding goroutine before the unit is decoded, while anyone holds a lease
// (SWScreenFeed.m addUnitListener:). The unit's NAL units stay valid after the call.
func (f *Feed) AddUnitListener(listener func(*AccessUnit)) int {
	f.access.Lock()
	defer f.access.Unlock()
	f.nextListener++
	f.unitListeners[f.nextListener] = listener
	return f.nextListener
}

// AddFrameListener calls listener with every decoded frame on the decoding goroutine
// (SWScreenFeed.m addFrameListener:); the listener releases the frame when done with it, at
// once or later.
func (f *Feed) AddFrameListener(listener func(*Frame)) int {
	f.access.Lock()
	defer f.access.Unlock()
	f.nextListener++
	f.frameListeners[f.nextListener] = listener
	return f.nextListener
}

// RemoveListener stops a unit or frame listener; a call already under way finishes.
func (f *Feed) RemoveListener(identifier int) {
	f.access.Lock()
	defer f.access.Unlock()
	delete(f.unitListeners, identifier)
	delete(f.frameListeners, identifier)
}

func (f *Feed) unitListenersLocked() []func(*AccessUnit) {
	if len(f.unitListeners) == 0 {
		return nil
	}
	listeners := make([]func(*AccessUnit), 0, len(f.unitListeners))
	for _, listener := range f.unitListeners {
		listeners = append(listeners, listener)
	}
	return listeners
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
	if name := f.colorName.Load(); name != nil {
		stats.Color = *name
	}
	if name := f.threadPriority.Load(); name != nil {
		stats.DecoderThread = *name
	}
	stats.Decoding, stats.Converting, stats.Answering = f.decoding.read(), f.converting.read(), f.answering.read()
	stats.Units, stats.UnitBytes = f.units.Load(), f.unitBytes.Load()
	stats.CodedUnits, stats.SkippedUnits, stats.UnreadUnits = f.coded.Load(), f.skipped.Load(), f.unread.Load()
	stats.RepeatedUnits = f.repeated.Load()
	if last := f.lastUnit.Load(); last != nil {
		stats.LastUnit = *last
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

// Prewarm opens the stream in the background when it is closed, so the next lease finds it
// open or opening, and starts its linger again when it is open; it does not wait. The server
// calls it as a script starts.
func (f *Feed) Prewarm() {
	if !f.prewarming.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer f.prewarming.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), openLimit)
		defer cancel()
		lease, err := f.Acquire(ctx)
		if err != nil {
			f.logger.Debug(E.Cause(err, "open the screen stream ahead of a script"))
			return
		}
		lease.Release()
	}()
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
	// A stream goes through some 30 MB of frames a second, which the Go runtime would keep
	// from the system long after the stream stopped.
	dropSpareBuffers()
	go debug.FreeOSMemory()
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
	// A stream goes through some 30 MB of frames a second, which the Go runtime would keep
	// from the system long after the stream stopped.
	dropSpareBuffers()
	go debug.FreeOSMemory()
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
	decodedAt := f.clock.ticks()
	if unit.Received != 0 {
		f.decoding.add(time.Duration(decodedAt - unit.Received))
	}
	f.access.Lock()
	if f.session != s || f.screen == nil {
		f.access.Unlock()
		picture.recycle()
		return
	}
	made := &frame{picture: picture, time: unit.Arrival, received: unit.Received, decoded: decodedAt, turns: unit.Turns(), geometry: f.geometry, feed: f}
	made.totals = unit.totals
	made.info = FrameInfo{
		Unit: unit.Info, SincePrevious: unit.totals.since(f.framedTotals), Arrival: unit.Arrival, Decoded: f.clock.wall(decodedAt),
		Media: float64(picture.time) / 1e4,
	}
	f.framedTotals = unit.totals
	made.refs.Store(int32(1 + len(f.frameListeners)))
	f.replaceLatestLocked(made)
	f.sequence++
	f.broadcastLocked()
	listeners := make([]func(*Frame), 0, len(f.frameListeners))
	for _, listener := range f.frameListeners {
		listeners = append(listeners, listener)
	}
	f.access.Unlock()
	f.frames.Add(1)
	if f.observeDecoded != nil {
		f.observeDecoded(unit.Arrival, made.info.Decoded)
	}
	for _, listener := range listeners {
		listener(&Frame{frame: made})
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
	// What the lease knows of the frame it answered last, and the units since the one it
	// answered before; it keeps no pixels.
	hasAnswered    bool
	answered       FrameInfo
	answeredTotals unitTotals
	since          Changes
	// repeated is set when the units since the frame answered before tell that the last one
	// repeats it.
	repeated bool
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

// answer makes an image of a frame the caller holds, which the image then holds.
func (l *Lease) answer(ctx context.Context, answered *frame, sequence int64) (*raster.Image, error) {
	if ctx.Err() != nil {
		answered.release()
		return nil, &raster.Error{Name: raster.NameAbort, Message: "the operation was aborted"}
	}
	l.access.Lock()
	l.sequence = max(l.sequence, sequence)
	if l.hasAnswered {
		l.since = answered.totals.since(l.answeredTotals)
		l.repeated = l.since.Repeats
	} else {
		l.since = answered.info.SincePrevious
		l.repeated = false
	}
	l.hasAnswered, l.answered, l.answeredTotals = true, answered.info, answered.totals
	l.access.Unlock()
	return answered.image(), nil
}

// Repeated tells whether the units since the frame the lease answered before the last one tell
// that the last one repeats it pixel for pixel (raster.RepeatTeller).
func (l *Lease) Repeated() bool {
	l.access.Lock()
	defer l.access.Unlock()
	return l.repeated
}

// Answered is what the feed knows of the frame the lease answered last, and the units since
// the frame it answered before (since the frame decoded before it, for the lease's first);
// false before the lease answered a frame.
func (l *Lease) Answered() (FrameInfo, Changes, bool) {
	l.access.Lock()
	defer l.access.Unlock()
	return l.answered, l.since, l.hasAnswered
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
// epoch, the quarter turns that make it upright and the screen it belongs to (SWFrame.m). Every
// image a lease answers of it holds it and converts only what a call reads; the picture's bytes
// go back to the pool when no one holds the frame.
type frame struct {
	feed *Feed
	time float64
	// received and decoded are the clock's readings when the unit's first packet came and
	// when its picture came out of the decoder.
	received, decoded int64
	turns             int
	geometry          Geometry
	refs              atomic.Int32
	info              FrameInfo
	totals            unitTotals
	// answered is set once a conversion of the frame was timed from its arrival.
	answered atomic.Bool

	access  sync.Mutex
	picture *picture
	quarter *image.Gray
}

// Frame is a decoded frame that a frame listener gets (SWFrame.m); Release gives it back.
type Frame struct {
	frame    *frame
	released atomic.Bool
}

// Time is when the frame's first packet arrived, in milliseconds since the epoch.
func (fr *Frame) Time() float64 {
	return fr.frame.time
}

// Turns is the quarter turns clockwise that make the frame upright.
func (fr *Frame) Turns() int {
	return fr.frame.turns
}

// Info is what the feed knows of the frame: its unit, the units since the frame before it,
// and when it arrived and was decoded.
func (fr *Frame) Info() FrameInfo {
	return fr.frame.info
}

// Release gives the frame back; it may be called more than once.
func (fr *Frame) Release() {
	if fr.released.CompareAndSwap(false, true) {
		fr.frame.release()
	}
}

// Same tells whether two frames that are not released decoded to the same samples, which
// means the same screen pixel for pixel.
func (fr *Frame) Same(other *Frame) bool {
	if fr.released.Load() || other.released.Load() {
		return false
	}
	if fr.frame == other.frame {
		return true
	}
	a, b := fr.frame, other.frame
	return a.turns == b.turns && a.geometry == b.geometry && a.picture != nil && b.picture != nil && a.picture.same(b.picture)
}

// Quarter answers the frame cropped to the screen and shrunk to a quarter of its size, in gray
// and turned upright, which the change tracker of a recording compares (SWFrame.m quarter:).
// It is made once.
func (fr *Frame) Quarter(ctx context.Context) (*image.Gray, error) {
	if fr.released.Load() {
		return nil, &raster.Error{Name: raster.NameState, Message: "graphics: the frame was released"}
	}
	f := fr.frame
	f.access.Lock()
	defer f.access.Unlock()
	if f.quarter != nil {
		return f.quarter, nil
	}
	if f.picture == nil {
		return nil, &raster.Error{Name: raster.NameUnavailable, Message: "graphics: the frame is gone"}
	}
	width, height := f.cropSize(f.picture.width, f.picture.height)
	f.quarter = f.picture.quarterGray(width, height, f.turns)
	return f.quarter, nil
}

// cropSize is the part of a picture that shows the screen (SWFrame.m initWithPixelBuffer:).
func (fr *frame) cropSize(width, height int) (int, int) {
	if fr.geometry.Width > 0 {
		width = min(width, fr.geometry.Width)
	}
	if fr.geometry.Height > 0 {
		height = min(height, fr.geometry.Height)
	}
	return width, height
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

// image is the frame in points of the screen, cropped to the screen's size and turned upright,
// with the arrival as its timestamp (SWImageNode.m nodeWithFrame:). It takes over a hold on
// the frame, and makes its pixels as calls read them.
func (fr *frame) image() *raster.Image {
	width, height := fr.cropSize(fr.picture.width, fr.picture.height)
	width, height = fr.picture.uprightSize(width, height, fr.turns)
	return raster.FromSource(width, height, fr.geometry.Scale, fr.time, &frameSource{frame: fr})
}

// frameSource is a hold on a frame that makes an image's pixels (raster.Source).
type frameSource struct {
	frame    *frame
	released atomic.Bool
}

// Bytes is the picture's, which the source keeps whatever part of it an image shows.
func (s *frameSource) Bytes() int {
	if p := s.frame.picture; p != nil && p.buffer != nil {
		return cap(*p.buffer)
	}
	return 0
}

func (s *frameSource) Share() raster.Source {
	s.frame.refs.Add(1)
	return &frameSource{frame: s.frame}
}

func (s *frameSource) Release() {
	if s.released.CompareAndSwap(false, true) {
		s.frame.release()
	}
}

func (s *frameSource) picture() (*picture, int, int, error) {
	fr := s.frame
	if s.released.Load() || fr.picture == nil {
		return nil, 0, 0, &raster.Error{Name: raster.NameUnavailable, Message: "graphics: the frame is gone"}
	}
	width, height := fr.cropSize(fr.picture.width, fr.picture.height)
	return fr.picture, width, height, nil
}

func (s *frameSource) Pixels(ctx context.Context, rect image.Rectangle, out []byte) error {
	if ctx.Err() != nil {
		return &raster.Error{Name: raster.NameAbort, Message: "the operation was aborted"}
	}
	p, width, height, err := s.picture()
	if err != nil {
		return err
	}
	fr := s.frame
	f := fr.feed
	started := f.clock.ticks()
	p.rgbaInto(out, rect, width, height, fr.turns)
	f.converted.Add(1)
	finished := f.clock.ticks()
	f.converting.add(time.Duration(finished - started))
	if fr.received != 0 && fr.answered.CompareAndSwap(false, true) {
		f.answering.add(time.Duration(finished - fr.received))
	}
	return nil
}

func (s *frameSource) Gray(ctx context.Context, rect image.Rectangle, outWidth, outHeight int, out []byte) error {
	if ctx.Err() != nil {
		return &raster.Error{Name: raster.NameAbort, Message: "the operation was aborted"}
	}
	p, width, height, err := s.picture()
	if err != nil {
		return err
	}
	p.grayInto(out, rect, width, height, s.frame.turns, outWidth, outHeight)
	return nil
}

// session is one opening of the stream: its batches, and the goroutine that depacketizes and
// decodes them (SWScreenFeed.m's serial queue).
type session struct {
	feed      *Feed
	batches   chan delivery
	done      chan struct{}
	stopOnce  sync.Once
	finished  chan struct{}
	endedWith error
}

// delivery is a batch and the clock's reading when it reached the feed.
type delivery struct {
	batch    []byte
	received int64
}

func newSession(f *Feed) *session {
	return &session{feed: f, batches: make(chan delivery, batchBacklog), done: make(chan struct{}), finished: make(chan struct{})}
}

// deliver takes a batch from the device's reading goroutine; it never blocks it. The batch is
// stamped here, on the precise clock: the device stamps its packets with time.Now, which on
// Windows moves in steps of the system timer.
func (s *session) deliver(batch []byte) {
	received := s.feed.clock.ticks()
	select {
	case <-s.done:
		return
	default:
	}
	select {
	case s.batches <- delivery{batch: bytes.Clone(batch), received: received}:
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
	// Media Foundation's objects live on this thread, which runs above the others while it
	// decodes.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	f := s.feed
	priority, restore := raiseThreadPriority()
	defer restore()
	f.threadPriority.Store(&priority)
	decoder := newFrameDecoder(f.newCodec, f.options, f.logger, func(picture *picture, unit *AccessUnit) {
		f.decoded(s, picture, unit)
	})
	if reporter, ok := opened.(PixelFormatScreen); ok {
		decoder.hint = func() rangeHint {
			return hintOfPixelFormat(reporter.PixelFormat())
		}
	}
	decoder.onColor = func(color streamColor) {
		name := color.String()
		if previous := f.colorName.Swap(&name); previous == nil || *previous != name {
			f.logger.Debug("screen colors: ", name)
		}
	}
	var listeners []func(*AccessUnit)
	defer decoder.close()
	inspector := newUnitInspector()
	first := true
	var depacketizer Depacketizer
	var units []*AccessUnit
	var named codec
	for {
		select {
		case <-s.done:
			return
		case delivered := <-s.batches:
			depacketizer.received = delivered.received
			units = depacketizer.PushBatch(delivered.batch, units[:0])
			if len(units) > 0 {
				f.access.Lock()
				listeners = f.unitListenersLocked()
				f.access.Unlock()
			}
			for _, unit := range units {
				if unit.Received != 0 {
					unit.Arrival = f.clock.wall(unit.Received)
				}
				if unit.Damaged {
					f.damaged.Add(1)
				}
				f.count(inspector, unit, first)
				first = false
				for _, listener := range listeners {
					listener(unit)
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
