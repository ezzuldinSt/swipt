//go:build !darwin || !cgo

package graphics

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/automation"
	"github.com/swipt-automation/swipt/internal/graphics/feed"
	"github.com/swipt-automation/swipt/internal/graphics/movie"
	"github.com/swipt-automation/swipt/internal/graphics/raster"
)

const (
	// The recording asks for a keyframe this often, so its movies can be read from anywhere, and
	// closes a change that has been still without frames (SWRecordingSession.m SWKeyFrameInterval).
	keyFrameInterval = 2 * time.Second
	// The feed events count the frames of windows this long, in milliseconds.
	feedInterval = 250
	// The clock deltas kept before every other one is dropped.
	clockDeltaLimit = 8192
	// The device's RTP clock and the movies' timescale (SWHEVC.m SWRTPClock).
	recordingTimescale = 24000
	// The last sample of a movie lasts 1/60 s (SWSegmentWriter.m finish:).
	lastSampleDuration = 400
)

// How long the recording waits for its first keyframe (SWScreenFirstFrameLimit).
var firstFrameLimit = 5 * time.Second

// rtpClock unwraps the 32-bit RTP timestamps into ticks since the first one (SWHEVC.m SWRTPClock).
type rtpClock struct {
	hasLast bool
	last    uint32
	high    int64
	hasBase bool
	base    int64
}

func (c *rtpClock) time(timestamp uint32) int64 {
	if c.hasLast && timestamp < c.last && c.last-timestamp > 0x80000000 {
		c.high += 1 << 32
	}
	c.hasLast = true
	c.last = timestamp
	value := c.high + int64(timestamp)
	if !c.hasBase {
		c.hasBase = true
		c.base = value
	}
	return value - c.base
}

// segmentWriter writes the units of one movie of a recording without re-encoding them, each
// sample lasting until the next one's RTP time (SWSegmentWriter.m).
type segmentWriter struct {
	movie       *movie.Writer
	turns       int
	clock       rtpClock
	pending     *feed.AccessUnit
	pendingTime int64
	end         int64
	failure     string
}

func newSegmentWriter(path string, format *movie.Format, turns int) (*segmentWriter, error) {
	os.Remove(path)
	written, err := movie.Create(path, format, turns, recordingTimescale)
	if err != nil {
		return nil, E.Cause(err, "graphics: create ", filepath.Base(path))
	}
	return &segmentWriter{movie: written, turns: turns}, nil
}

// append queues a unit and writes the one before it; it answers the unit's time in ticks.
func (w *segmentWriter) append(unit *feed.AccessUnit) int64 {
	time := w.clock.time(unit.RTPTimestamp)
	if w.pending != nil {
		w.write(w.pending, w.pendingTime, time-w.pendingTime)
	}
	w.pending, w.pendingTime = unit, time
	return time
}

func (w *segmentWriter) write(unit *feed.AccessUnit, time, duration int64) {
	// A clock that stands still or goes back would make AVAssetWriter reject the sample; here
	// the sample keeps a tick, so the samples after it keep their order.
	duration = max(duration, 1)
	written, err := w.movie.Append(unit.NALs, duration, unit.IsKeyframe())
	if err != nil {
		if w.failure == "" {
			w.failure = err.Error()
		}
		return
	}
	if written {
		w.end = time + duration
	}
}

// finish writes the last unit and the movie's index, and answers a failure, or "".
func (w *segmentWriter) finish() string {
	if w.pending != nil {
		w.write(w.pending, w.pendingTime, lastSampleDuration)
		w.pending = nil
	}
	if err := w.movie.Close(); err != nil {
		return err.Error()
	}
	return w.failure
}

type recordingSegment struct {
	file   string
	start  float64
	turns  int
	writer *segmentWriter
}

// feedWindow counts the frames of a window of the feed and how late the latest was decoded.
type feedWindow struct {
	active  bool
	start   float64
	frames  int
	decoded float64
}

func (w feedWindow) event() map[string]any {
	return map[string]any{"time": w.start, "type": "feed", "frames": w.frames, "decodedMs": raster.Rounded(w.decoded)}
}

// recordingSession records the screen's stream into movies without re-encoding it, starting a
// movie whenever the orientation or the parameter sets change, and tracks the changes of the
// screen on quarter-size frames (SWRecordingSession.m).
type recordingSession struct {
	path     string
	geometry feed.Geometry
	feed     *feed.Feed
	lease    *feed.Lease
	timeline *automation.TimelineWatch
	report   func(message string)

	listeners []int
	wake      chan struct{}
	quit      chan struct{}
	tracking  sync.WaitGroup
	finishing sync.WaitGroup

	trackerAccess sync.Mutex
	tracker       raster.ChangeTracker

	access      sync.Mutex
	began       chan struct{}
	hasOrigin   bool
	origin      float64
	last        float64
	segments    []*recordingSegment
	writer      *segmentWriter
	format      *movie.Format
	waiting     bool
	stopped     bool
	failures    []string
	queued      *feed.Frame
	clockDeltas []float64
	window      feedWindow
	feedEvents  []map[string]any
}

// sidecarOf is the metadata file beside a recording (SWVideoSidecar).
func sidecarOf(path string) string {
	return strings.TrimSuffix(path, filepath.Ext(path)) + ".json"
}

// startRecording records the screen into path once its first keyframe comes, within 5 seconds
// (SWRecordingSession.m startWithURL:). report hears the failure of a recording abandoned
// without being stopped.
func startRecording(ctx context.Context, screen *feed.Feed, path string, timeline *automation.TimelineWatch, report func(string)) (*recordingSession, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		timeline.Stop()
		return nil, E.Cause(err, "graphics: create the directory of ", path)
	}
	os.Remove(sidecarOf(path))
	lease, err := screen.Acquire(ctx)
	if err != nil {
		timeline.Stop()
		return nil, err
	}
	s := &recordingSession{
		path: path, geometry: lease.Geometry(), feed: screen, lease: lease, timeline: timeline, report: report,
		wake: make(chan struct{}, 1), quit: make(chan struct{}), began: make(chan struct{}), waiting: true,
	}
	s.listeners = []int{screen.AddUnitListener(s.append), screen.AddFrameListener(s.observe)}
	s.tracking.Add(1)
	go s.track()
	screen.RequestKeyFrame()
	timer := time.NewTimer(firstFrameLimit)
	defer timer.Stop()
	var failure error
	select {
	case <-s.began:
		return s, nil
	case <-ctx.Done():
		failure = &raster.Error{Name: raster.NameAbort, Message: "the operation was aborted"}
	case <-timer.C:
		failure = &raster.Error{Name: raster.NameUnavailable, Message: fmt.Sprint("graphics: the screen sent no frame within ", int(firstFrameLimit/time.Second), " s; it may be off")}
	}
	s.stop(context.Background())
	return nil, failure
}

// Start is when the first keyframe arrived, in milliseconds since the epoch.
func (s *recordingSession) Start() float64 {
	s.access.Lock()
	defer s.access.Unlock()
	return s.origin
}

func (s *recordingSession) append(unit *feed.AccessUnit) {
	s.access.Lock()
	defer s.access.Unlock()
	s.appendLocked(unit)
}

func (s *recordingSession) segmentFile(index int) string {
	name := filepath.Base(s.path)
	if index == 0 {
		return name
	}
	extension := filepath.Ext(name)
	return fmt.Sprintf("%s-%d.%s", strings.TrimSuffix(name, extension), index+1, strings.TrimPrefix(extension, "."))
}

func (s *recordingSession) appendLocked(unit *feed.AccessUnit) {
	if s.stopped {
		return
	}
	if unit.Damaged {
		s.waiting = true
		return
	}
	if unit.HasParameterSets() {
		if described, ok := movie.HEVCFormat(unit.NALs); ok {
			s.format = described
		}
	}
	changed := true
	if s.writer != nil && s.format != nil {
		changed = s.writer.turns != unit.Turns() || !s.format.Equal(s.writer.movie.Format())
	}
	if changed {
		if s.writer != nil {
			s.closeWriter(s.writer)
			s.writer = nil
		}
		if !unit.IsKeyframe() || s.format == nil {
			s.waiting = true
			return
		}
		if !s.hasOrigin {
			s.hasOrigin = true
			s.origin = unit.Arrival
			close(s.began)
		}
		file := s.segmentFile(len(s.segments))
		created, err := newSegmentWriter(filepath.Join(filepath.Dir(s.path), file), s.format, unit.Turns())
		if err != nil {
			s.failures = append(s.failures, err.Error())
			return
		}
		s.writer = created
		s.segments = append(s.segments, &recordingSegment{file: file, start: unit.Arrival - s.origin, turns: unit.Turns(), writer: created})
		s.waiting = false
	}
	if s.waiting {
		if !unit.IsKeyframe() {
			return
		}
		s.waiting = false
	}
	if s.writer != nil && s.hasOrigin && len(s.segments) > 0 {
		stored := s.segments[len(s.segments)-1].start + float64(s.writer.append(unit))*1000/recordingTimescale
		s.clockDeltas = append(s.clockDeltas, unit.Arrival-s.origin-stored)
		if len(s.clockDeltas) > clockDeltaLimit {
			kept := s.clockDeltas[:0]
			for index := 0; index < len(s.clockDeltas); index += 2 {
				kept = append(kept, s.clockDeltas[index])
			}
			s.clockDeltas = kept
		}
	}
	s.last = unit.Arrival
}

// closeWriter finishes a movie in the background; stop waits for it.
func (s *recordingSession) closeWriter(writer *segmentWriter) {
	s.finishing.Go(func() {
		if failure := writer.finish(); failure != "" {
			s.access.Lock()
			s.failures = append(s.failures, failure)
			s.access.Unlock()
		}
	})
}

// observe keeps the newest frame for the change tracker and counts it in the feed's window.
func (s *recordingSession) observe(frame *feed.Frame) {
	s.access.Lock()
	if s.stopped || !s.hasOrigin || frame.Time() < s.origin {
		s.access.Unlock()
		frame.Release()
		return
	}
	if s.window.active && frame.Time() >= s.window.start+feedInterval {
		s.feedEvents = append(s.feedEvents, s.window.event())
		s.window.active = false
	}
	if !s.window.active {
		s.window = feedWindow{active: true, start: frame.Time()}
	}
	s.window.frames++
	s.window.decoded = max(s.window.decoded, raster.NowMilliseconds()-frame.Time())
	if s.queued != nil {
		s.queued.Release()
	}
	s.queued = frame
	s.access.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// track compares the frames on a goroutine of its own, skipping those that come while it
// compares, and asks for a keyframe every 2 seconds.
func (s *recordingSession) track() {
	defer s.tracking.Done()
	ticker := time.NewTicker(keyFrameInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-s.wake:
			s.drain()
		case <-ticker.C:
			s.feed.RequestKeyFrame()
			s.tick()
		}
	}
}

func (s *recordingSession) fail(err error) {
	s.access.Lock()
	s.failures = append(s.failures, err.Error())
	s.access.Unlock()
}

func (s *recordingSession) drain() {
	for {
		s.access.Lock()
		frame, origin := s.queued, s.origin
		if frame == nil || !s.hasOrigin {
			s.access.Unlock()
			return
		}
		s.queued = nil
		s.access.Unlock()
		plane, err := frame.Quarter(context.Background())
		frame.Release()
		if err != nil {
			s.fail(err)
			continue
		}
		s.trackerAccess.Lock()
		s.tracker.Push(plane, frame.Time()-origin)
		s.trackerAccess.Unlock()
	}
}

func (s *recordingSession) tick() {
	s.access.Lock()
	hasOrigin, origin := s.hasOrigin, s.origin
	s.access.Unlock()
	if hasOrigin {
		s.trackerAccess.Lock()
		s.tracker.Tick(raster.NowMilliseconds() - origin)
		s.trackerAccess.Unlock()
	}
}

// stop finishes the movies and writes the metadata beside them (SWRecordingSession.m stop:).
func (s *recordingSession) stop(ctx context.Context) (*videoModel, error) {
	s.access.Lock()
	if s.stopped {
		s.access.Unlock()
		return nil, &raster.Error{Name: raster.NameState, Message: "the recording already stopped"}
	}
	s.stopped = true
	writer := s.writer
	s.writer = nil
	s.access.Unlock()
	for _, listener := range s.listeners {
		s.feed.RemoveListener(listener)
	}
	close(s.quit)
	if writer != nil {
		s.closeWriter(writer)
	}
	s.lease.Release()
	events := s.timeline.Stop()
	finished := make(chan struct{})
	go func() {
		s.finishing.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-ctx.Done():
		return nil, &raster.Error{Name: raster.NameAbort, Message: "the operation was aborted"}
	}
	s.tracking.Wait()
	s.drain()
	s.trackerAccess.Lock()
	s.tracker.Finish()
	s.trackerAccess.Unlock()
	return s.save(events)
}

// abandon stops a recording that its handle let go of without stopping it, and reports when
// that fails (SWRecordingSession.m abandon).
func (s *recordingSession) abandon() {
	s.access.Lock()
	running := !s.stopped
	s.access.Unlock()
	if !running {
		return
	}
	go func() {
		if _, err := s.stop(context.Background()); err != nil && s.report != nil {
			s.report(named(err).Message)
		}
	}()
}

func (s *recordingSession) medianClockDelta() float64 {
	if len(s.clockDeltas) == 0 {
		return 0
	}
	sorted := slices.Clone(s.clockDeltas)
	slices.Sort(sorted)
	return sorted[len(sorted)/2]
}

// segmentRecord is a movie of a recording as its metadata lists it.
type segmentRecord struct {
	File     string  `json:"file"`
	Start    float64 `json:"start"`
	Duration float64 `json:"duration"`
	Turns    int     `json:"turns"`
}

// recordingMetadata is the JSON beside a recording (SWRecordingSession.m save:).
type recordingMetadata struct {
	Version     int                   `json:"version"`
	Start       float64               `json:"start"`
	Duration    float64               `json:"duration"`
	Scale       float64               `json:"scale"`
	ClockOffset float64               `json:"clockOffset"`
	Screen      map[string]int        `json:"screen"`
	Segments    []segmentRecord       `json:"segments"`
	Changes     []raster.ChangeRecord `json:"changes"`
	Events      []map[string]any      `json:"events"`
}

// timedEvent is an event of the timeline or the feed with its time read out.
type timedEvent struct {
	time   float64
	fields map[string]any
}

func eventsOf(timeline []automation.TimelineEvent, feedEvents []map[string]any) []timedEvent {
	var events []timedEvent
	if data, err := json.Marshal(timeline); err == nil {
		var decoded []map[string]any
		if json.Unmarshal(data, &decoded) == nil {
			for _, fields := range decoded {
				events = append(events, timedEvent{fields: fields})
			}
		}
	}
	for _, fields := range feedEvents {
		events = append(events, timedEvent{fields: fields})
	}
	for index := range events {
		switch value := events[index].fields["time"].(type) {
		case float64:
			events[index].time = value
		case int:
			events[index].time = float64(value)
		default:
			events[index].time = -1
		}
	}
	return events
}

func (s *recordingSession) save(timeline []automation.TimelineEvent) (*videoModel, error) {
	s.access.Lock()
	hasOrigin, origin, last := s.hasOrigin, s.origin, s.last
	segments := slices.Clone(s.segments)
	failures := slices.Clone(s.failures)
	clockOffset := s.medianClockDelta()
	feedEvents := slices.Clone(s.feedEvents)
	if s.window.active {
		feedEvents = append(feedEvents, s.window.event())
	}
	s.access.Unlock()
	if !hasOrigin || len(segments) == 0 {
		return nil, &raster.Error{Name: raster.NameUnavailable, Message: "graphics: the recording got no frames; the screen may be off or another app may hold it"}
	}
	if len(failures) > 0 {
		nothingWritten := true
		for _, segment := range segments {
			nothingWritten = nothingWritten && segment.writer.end == 0
		}
		if nothingWritten {
			return nil, &raster.Error{Name: raster.NameError, Message: "graphics: " + failures[0]}
		}
	}
	events := eventsOf(timeline, feedEvents)
	duration := max(last-origin, 0) + 1000.0/60
	metadata := recordingMetadata{
		Version: 1, Start: origin, Duration: raster.Rounded(duration), Scale: s.geometry.Scale, ClockOffset: raster.Rounded(clockOffset),
		Screen:   map[string]int{"width": s.geometry.Width, "height": s.geometry.Height},
		Segments: make([]segmentRecord, len(segments)),
		Changes:  s.changes(segments, events, origin),
		Events:   relativeEvents(events, origin),
	}
	for index, segment := range segments {
		following := duration
		if index+1 < len(segments) {
			following = segments[index+1].start
		}
		metadata.Segments[index] = segmentRecord{File: segment.file, Start: raster.Rounded(segment.start), Duration: raster.Rounded(following - segment.start), Turns: segment.turns}
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		return nil, &raster.Error{Name: raster.NameError, Message: "graphics: encode the recording's metadata"}
	}
	if err = writeAtomically(sidecarOf(s.path), data); err != nil {
		return nil, E.Cause(err, "graphics: write the recording's metadata")
	}
	return newVideoModel(s.path, data)
}

// changes describes the tracker's changes in points of the screen, with the app in front when
// each began.
func (s *recordingSession) changes(segments []*recordingSegment, events []timedEvent, origin float64) []raster.ChangeRecord {
	var foreground []timedEvent
	for _, event := range events {
		if _, isApp := event.fields["app"].(string); isApp && event.fields["type"] == "foreground" && event.time >= 0 {
			foreground = append(foreground, event)
		}
	}
	s.trackerAccess.Lock()
	changes := slices.Clone(s.tracker.Changes())
	s.trackerAccess.Unlock()
	described := make([]raster.ChangeRecord, 0, len(changes))
	for _, change := range changes {
		at := origin + change.Start
		var app *string
		if len(foreground) > 0 {
			first := foreground[0].fields["app"].(string)
			app = &first
		}
		for _, event := range foreground {
			if event.time <= at {
				name := event.fields["app"].(string)
				app = &name
			}
		}
		turns := 0
		for _, segment := range segments {
			if segment.start <= change.Start {
				turns = segment.turns
			}
		}
		width := s.geometry.Width
		if turns%2 != 0 {
			width = s.geometry.Height
		}
		described = append(described, change.Record(float64(width)/float64(max(change.Width, 1)), s.geometry.Scale, app))
	}
	return described
}

// relativeEvents keeps the events from the origin on, their times since it, in order.
func relativeEvents(events []timedEvent, origin float64) []map[string]any {
	var kept []timedEvent
	for _, event := range events {
		if event.time < 0 || event.time < origin {
			continue
		}
		shifted := maps.Clone(event.fields)
		shifted["time"] = raster.Rounded(event.time - origin)
		kept = append(kept, timedEvent{time: event.time, fields: shifted})
	}
	slices.SortStableFunc(kept, func(a, b timedEvent) int {
		return cmp.Compare(a.time, b.time)
	})
	relative := make([]map[string]any, len(kept))
	for index, event := range kept {
		relative[index] = event.fields
	}
	return relative
}

func writeAtomically(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), path)
	}
	if err != nil {
		os.Remove(file.Name())
	}
	return err
}
