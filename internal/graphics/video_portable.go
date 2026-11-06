//go:build !darwin || !cgo

package graphics

import (
	"context"
	"encoding/json"
	"image"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/graphics/feed"
	"github.com/swipt-automation/swipt/internal/graphics/movie"
	"github.com/swipt-automation/swipt/internal/graphics/raster"
)

// A segment's file and decoder close after this long unused, so the files can be deleted.
var readerIdle = 5 * time.Second

// AVAssetReader's output times compare with this slack (SWSegmentReader.m).
const timeSlack = 0.0005

type videoSegment struct {
	path     string
	start    float64
	duration float64
	turns    int
}

// videoModel is a recording opened for reading: its movies, the screen they show and what its
// metadata says happened (SWVideoModel.m).
type videoModel struct {
	path        string
	start       float64
	duration    float64
	scale       float64
	clockOffset float64
	width       int
	height      int
	segments    []videoSegment
	changes     []json.RawMessage
	events      []json.RawMessage

	access  sync.Mutex
	readers map[int]*segmentReader
}

func dataError(message ...any) *raster.Error {
	return &raster.Error{Name: raster.NameData, Message: E.New(message...).Error()}
}

// newVideoModel reads a recording's metadata; values of the wrong type count as absent, and
// segment files that would leave the recording's directory are skipped.
func newVideoModel(path string, metadata json.RawMessage) (*videoModel, error) {
	values, err := raster.ParseArguments(metadata)
	if err != nil {
		return nil, dataError("graphics: the recording's metadata is not an object")
	}
	m := &videoModel{path: path, start: math.NaN(), scale: 1, readers: make(map[int]*segmentReader)}
	if start, ok := values.Number("start"); ok {
		m.start = start
	}
	m.duration = values.Float("duration", 0)
	m.scale = values.Float("scale", 1)
	m.clockOffset = values.Float("clockOffset", 0)
	if screen, ok := values.Object("screen"); ok {
		m.width, m.height = screen.Integer("width", 0), screen.Integer("height", 0)
	}
	directory := filepath.Dir(path)
	listed, _ := values.Array("segments")
	for _, item := range listed {
		segment, err := raster.ParseArguments(item)
		if err != nil {
			continue
		}
		file, ok := segment.String("file")
		if !ok || file == "" || file == "." || file == ".." || strings.ContainsAny(file, `/\:`) {
			continue
		}
		m.segments = append(m.segments, videoSegment{
			path: filepath.Join(directory, file), start: segment.Float("start", 0), duration: segment.Float("duration", 0),
			turns: segment.Integer("turns", 0),
		})
	}
	if len(m.segments) == 0 {
		return nil, dataError("graphics: the recording lists no video")
	}
	m.changes, _ = values.Array("changes")
	m.events, _ = values.Array("events")
	return m, nil
}

// openVideo reads the recording's metadata, or analyzes a video without any (SWVideoModel.m
// SWVideoOpen).
func openVideo(ctx context.Context, path string, logError func(string)) (*videoModel, error) {
	if data, err := os.ReadFile(sidecarOf(path)); err == nil {
		var decoded any
		switch err := json.Unmarshal(data, &decoded); {
		case err != nil:
			logError("read the metadata of " + filepath.Base(path) + ": " + err.Error())
		default:
			if _, isObject := decoded.(map[string]any); isObject {
				return newVideoModel(path, data)
			}
		}
	}
	return analyzeVideo(ctx, path)
}

func (m *videoModel) segmentIndexAt(time float64) int {
	found := 0
	for index, segment := range m.segments {
		if segment.start <= time {
			found = index
		}
	}
	return found
}

func turnsOf(turns int) int {
	return ((turns % 4) + 4) % 4
}

// pixelSizeAt is the size of the frames at a time of the recording, upright.
func (m *videoModel) pixelSizeAt(time float64) (int, int) {
	segment := m.segments[m.segmentIndexAt(time-m.clockOffset)]
	if turnsOf(segment.turns)%2 == 0 {
		return m.width, m.height
	}
	return m.height, m.width
}

func (m *videoModel) reader(index int) (*segmentReader, error) {
	m.access.Lock()
	defer m.access.Unlock()
	if reader := m.readers[index]; reader != nil {
		return reader, nil
	}
	reader, err := newSegmentReader(m.segments[index].path)
	if err != nil {
		return nil, err
	}
	m.readers[index] = reader
	return reader, nil
}

// pixelsAt answers the frame shown at a time of the recording, cropped to the screen and
// upright (SWVideoModel.m rasterAt:).
func (m *videoModel) pixelsAt(ctx context.Context, time float64) (image.Image, error) {
	stored := time - m.clockOffset
	index := m.segmentIndexAt(stored)
	segment := m.segments[index]
	reader, err := m.reader(index)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, &raster.Error{Name: raster.NameAbort, Message: "the operation was aborted"}
	}
	return reader.frameAt(ctx, max(stored-segment.start, 0)/1000, m.width, m.height, turnsOf(segment.turns))
}

// clamped keeps a time within the recording; NaN counts as 0, as MIN and MAX do.
func (m *videoModel) clamped(time float64) float64 {
	if !(time > 0) {
		return 0
	}
	if time > m.duration {
		return m.duration
	}
	return time
}

// nodeAt is the image of the frame at a time, made when a call needs its pixels (SWVideoModel.m
// nodeAt:).
func (m *videoModel) nodeAt(time float64) *raster.Image {
	time = m.clamped(time)
	width, height := m.pixelSizeAt(time)
	return raster.NewLazyImage(width, height, m.scale, time, m.start+time, func(ctx context.Context) (image.Image, error) {
		return m.pixelsAt(ctx, time)
	})
}

// scan visits the frames from one time of the recording to another with their times
// (SWVideoModel.m scanFrom:to:).
func (m *videoModel) scan(ctx context.Context, from, to float64, visit func(pixels *image.NRGBA, time float64) (bool, error)) error {
	first, last := from-m.clockOffset, to-m.clockOffset
	for index, segment := range m.segments {
		if segment.start+segment.duration < first || segment.start > last {
			continue
		}
		reader, err := m.reader(index)
		if err != nil {
			return err
		}
		start := max(first-segment.start, 0) / 1000
		end := (min(last, segment.start+segment.duration) - segment.start) / 1000
		going, err := reader.each(ctx, start, end, m.width, m.height, turnsOf(segment.turns), func(seconds float64, pixels *image.NRGBA) (bool, error) {
			return visit(pixels, segment.start+seconds*1000+m.clockOffset)
		})
		if err != nil {
			return err
		}
		if !going {
			return nil
		}
	}
	return nil
}

// infoJSON is what the video handle's info answers (SWVideoModel.m info).
func (m *videoModel) infoJSON() string {
	width, height := m.pixelSizeAt(0)
	var start any
	if !math.IsNaN(m.start) {
		start = m.start
	}
	changes, events := m.changes, m.events
	if changes == nil {
		changes = []json.RawMessage{}
	}
	if events == nil {
		events = []json.RawMessage{}
	}
	data, err := json.Marshal(map[string]any{
		"path": m.path, "start": start, "duration": raster.Rounded(m.duration),
		"width": raster.Rounded(float64(width) / m.scale), "height": raster.Rounded(float64(height) / m.scale),
		"scale": m.scale, "changes": changes, "events": events,
	})
	if err != nil {
		return "{}"
	}
	return string(data)
}

// defaultGridTimes are the start and the end of up to 15 changes, or 12 times across the
// recording without changes (SWVideoModel.m).
func (m *videoModel) defaultGridTimes() []float64 {
	changed := []float64{0}
	for _, item := range m.changes {
		change, err := raster.ParseArguments(item)
		if err != nil {
			continue
		}
		if after, ok := change.Number("after"); ok {
			changed = append(changed, after)
		}
	}
	var times []float64
	if len(changed) > 1 {
		step := max(float64(len(changed))/16, 1)
		for position := 0.0; position < float64(len(changed)); position += step {
			times = append(times, changed[int(position)])
		}
		return times
	}
	for index := range 12 {
		times = append(times, m.duration*float64(index)/12)
	}
	return times
}

// segmentReader reads the frames of one movie (SWSegmentReader.m). It answers frames as an
// AVAssetReader opened a second before the time would, without decoding the frames it skips:
// the sample tables tell which frame that is.
type segmentReader struct {
	path    string
	track   *movie.Track
	order   []int     // samples by presentation
	seconds []float64 // presentation times by presentation
	syncs   []int     // the keyframe at or before each sample, by decoding

	access   sync.Mutex
	current  int
	pending  int
	next     int
	file     *movie.File
	decoder  *feed.Decoder
	position int // the sample to decode next, or -1 for a decoder to start
	cached   *decodedFrame
	idle     *time.Timer
}

type decodedFrame struct {
	index                int
	width, height, turns int
	pixels               *image.NRGBA
}

func readFailure(path string, err error) error {
	return &raster.Error{Name: raster.NameData, Message: "graphics: read " + filepath.Base(path) + ": " + err.Error()}
}

func newSegmentReader(path string) (*segmentReader, error) {
	file, err := movie.Open(path)
	if err != nil {
		return nil, readFailure(path, err)
	}
	defer file.Close()
	track := file.Video
	r := &segmentReader{path: path, track: track, order: track.PresentationOrder(), current: -1, pending: -1, position: -1}
	r.seconds = make([]float64, len(r.order))
	for index, sample := range r.order {
		r.seconds[index] = track.Seconds(track.Samples[sample].Time)
	}
	r.syncs = make([]int, len(track.Samples))
	key := 0
	for index, sample := range track.Samples {
		if sample.Sync {
			key = index
		}
		r.syncs[index] = key
	}
	return r, nil
}

// firstFrom is the first frame an AVAssetReader whose time range starts at start answers: the
// one shown at start, or the first after it.
func (r *segmentReader) firstFrom(start float64) int {
	index := sort.SearchFloat64s(r.seconds, start)
	if index > 0 && index <= len(r.seconds) {
		previous := r.track.Samples[r.order[index-1]]
		if r.seconds[index-1]+r.track.Seconds(previous.Duration) > start {
			index--
		}
	}
	return index
}

// frameAt answers the last frame at or before seconds, or the first after it when none is
// within the second before (SWSegmentReader.m frameAt:), cropped and turned upright.
func (r *segmentReader) frameAt(ctx context.Context, seconds float64, width, height, turns int) (*image.NRGBA, error) {
	r.access.Lock()
	defer r.access.Unlock()
	reusable := r.current >= 0 && r.seconds[r.current] <= seconds && seconds-r.seconds[r.current] < 2
	if !reusable {
		r.next = r.firstFrom(max(seconds-1, 0))
		r.current, r.pending = -1, -1
	}
	if r.pending >= 0 && r.seconds[r.pending] <= seconds {
		r.current, r.pending = r.pending, -1
	}
	for r.pending < 0 && r.next < len(r.order) {
		index := r.next
		r.next++
		if r.seconds[index] <= seconds+timeSlack {
			r.current = index
		} else {
			r.pending = index
		}
	}
	chosen := r.current
	if chosen < 0 {
		chosen = r.pending
	}
	if chosen < 0 {
		return nil, dataError("graphics: the video has no frame at ", int64(seconds*1000), " ms")
	}
	return r.decodeLocked(ctx, chosen, width, height, turns)
}

// touchLocked puts off closing the file and the decoder.
func (r *segmentReader) touchLocked() {
	if r.idle != nil {
		r.idle.Stop()
	}
	r.idle = time.AfterFunc(readerIdle, r.closeIdle)
}

func (r *segmentReader) openLocked() error {
	if r.file != nil {
		return nil
	}
	file, err := movie.Open(r.path)
	if err != nil {
		return readFailure(r.path, err)
	}
	r.file = file
	return nil
}

// closeIdle lets go of the file and the decoder of a reader no one has used for a while.
func (r *segmentReader) closeIdle() {
	if r.access.TryLock() {
		defer r.access.Unlock()
		r.closeLocked()
	}
}

func (r *segmentReader) closeLocked() {
	if r.decoder != nil {
		r.decoder.Close()
		r.decoder = nil
	}
	if r.file != nil {
		r.file.Close()
		r.file = nil
	}
	r.position = -1
}

func decoderOf(track *movie.Track) (*feed.Decoder, error) {
	return feed.NewDecoder(feed.VideoFormat{H264: track.Codec == movie.H264, ParameterSets: track.ParameterSets, Width: track.Width, Height: track.Height})
}

// time100 is a sample's presentation time in 100 ns units, which the decoder carries.
func (r *segmentReader) time100(sample int) int64 {
	return r.track.Samples[sample].Time * 10_000_000 / int64(r.track.Timescale)
}

// decodeLocked decodes one frame, from the samples already decoded when no keyframe is closer,
// copying only that frame out of the decoder.
func (r *segmentReader) decodeLocked(ctx context.Context, index, width, height, turns int) (*image.NRGBA, error) {
	r.touchLocked()
	if cached := r.cached; cached != nil && cached.index == index && cached.width == width && cached.height == height && cached.turns == turns {
		return cached.pixels, nil
	}
	if err := r.openLocked(); err != nil {
		return nil, err
	}
	if r.decoder == nil {
		decoder, err := decoderOf(r.track)
		if err != nil {
			return nil, err
		}
		r.decoder, r.position = decoder, -1
	}
	sample := r.order[index]
	if r.position < 0 || r.position > sample || r.syncs[sample] > r.position {
		if r.position >= 0 {
			if err := r.decoder.Flush(); err != nil {
				r.closeLocked()
				return nil, err
			}
		}
		r.position = r.syncs[sample]
	}
	target := r.time100(sample)
	keep := func(time int64) bool { return time == target }
	var found *feed.Picture
	collect := func(pictures []*feed.Picture) {
		for _, picture := range pictures {
			if found == nil && picture.Time() == target {
				found = picture
			} else {
				picture.Release()
			}
		}
	}
	for found == nil && r.position < len(r.track.Samples) {
		if ctx.Err() != nil {
			return nil, &raster.Error{Name: raster.NameAbort, Message: "the operation was aborted"}
		}
		stored := r.track.Samples[r.position]
		nals, err := r.file.ReadSample(r.track, stored)
		if err != nil {
			r.closeLocked()
			return nil, readFailure(r.path, err)
		}
		pictures, err := r.decoder.Decode(nals, r.time100(r.position), stored.Sync, keep)
		r.position++
		collect(pictures)
		if err != nil {
			r.closeLocked()
			return nil, E.Cause(err, "graphics: decode ", filepath.Base(r.path))
		}
	}
	if found == nil {
		pictures, err := r.decoder.Drain(keep)
		collect(pictures)
		r.position = len(r.track.Samples) + 1
		if err != nil {
			r.closeLocked()
			return nil, E.Cause(err, "graphics: decode ", filepath.Base(r.path))
		}
	}
	if found == nil {
		return nil, dataError("graphics: ", filepath.Base(r.path), " has no frame at ", int64(r.seconds[index]*1000), " ms")
	}
	pixels := convertPicture(found, width, height, turns)
	found.Release()
	r.cached = &decodedFrame{index: index, width: width, height: height, turns: turns, pixels: pixels}
	return pixels, nil
}

// convertPicture crops a picture to the screen, when the screen's size is known, and turns it
// upright (SWFrame.m raster:).
func convertPicture(picture *feed.Picture, width, height, turns int) *image.NRGBA {
	cropWidth, cropHeight := picture.Width(), picture.Height()
	if width > 0 {
		cropWidth = min(cropWidth, width)
	}
	if height > 0 {
		cropHeight = min(cropHeight, height)
	}
	return picture.RGBA(cropWidth, cropHeight, turns)
}

// each visits the frames from start to end seconds as an AVAssetReader opened a second before
// start answers them: the last frame at or before start counts as at start (SWSegmentReader.m
// eachFrom:through:). It answers false when the visitor stopped.
func (r *segmentReader) each(ctx context.Context, start, end float64, width, height, turns int, visit func(seconds float64, pixels *image.NRGBA) (bool, error)) (bool, error) {
	type planned struct {
		index   int
		seconds float64
	}
	var visits []planned
	held := -1
	stopped := false
	for index := r.firstFrom(max(start-1, 0)); index < len(r.order); index++ {
		if r.seconds[index] <= start+timeSlack {
			held = index
			continue
		}
		if held >= 0 {
			visits = append(visits, planned{held, start})
			held = -1
		}
		if r.seconds[index] > end+timeSlack {
			stopped = true
			break
		}
		visits = append(visits, planned{index, r.seconds[index]})
	}
	if !stopped && held >= 0 {
		visits = append(visits, planned{held, start})
	}
	if len(visits) == 0 {
		return true, nil
	}
	file, err := movie.Open(r.path)
	if err != nil {
		return false, readFailure(r.path, err)
	}
	defer file.Close()
	decoder, err := decoderOf(r.track)
	if err != nil {
		return false, err
	}
	defer decoder.Close()
	wanted := make(map[int64]int, len(visits))
	for position, it := range visits {
		wanted[r.time100(r.order[it.index])] = position
	}
	keep := func(time int64) bool {
		_, found := wanted[time]
		return found
	}
	visited := 0
	going := true
	var failure error
	deliver := func(pictures []*feed.Picture) {
		for _, picture := range pictures {
			position, found := wanted[picture.Time()]
			if !going || failure != nil || !found || position < visited {
				picture.Release()
				continue
			}
			pixels := convertPicture(picture, width, height, turns)
			picture.Release()
			visited = position + 1
			going, failure = visit(visits[position].seconds, pixels)
		}
	}
	firstSample := r.order[visits[0].index]
	lastSample := 0
	for _, it := range visits {
		lastSample = max(lastSample, r.order[it.index])
	}
	for position := r.syncs[firstSample]; position < len(r.track.Samples) && going && failure == nil && visited < len(visits); position++ {
		if ctx.Err() != nil {
			return false, &raster.Error{Name: raster.NameAbort, Message: "the operation was aborted"}
		}
		stored := r.track.Samples[position]
		nals, err := file.ReadSample(r.track, stored)
		if err != nil {
			return false, readFailure(r.path, err)
		}
		pictures, err := decoder.Decode(nals, r.time100(position), stored.Sync, keep)
		deliver(pictures)
		if err != nil {
			return false, E.Cause(err, "graphics: decode ", filepath.Base(r.path))
		}
		if position >= lastSample && visited >= len(visits) {
			break
		}
	}
	if going && failure == nil && visited < len(visits) {
		pictures, err := decoder.Drain(keep)
		deliver(pictures)
		if err != nil {
			return false, E.Cause(err, "graphics: decode ", filepath.Base(r.path))
		}
	}
	return going, failure
}

// analyzeVideo finds the changes of a video without metadata from its frames at a quarter of
// their size, as one movie that starts at 0 (SWVideoAnalysis.m SWVideoAnalyze).
func analyzeVideo(ctx context.Context, path string) (*videoModel, error) {
	file, err := movie.Open(path)
	if err != nil {
		return nil, readFailure(path, err)
	}
	defer file.Close()
	track := file.Video
	width, height, turns := track.Width, track.Height, track.Turns
	decoder, err := decoderOf(track)
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	var tracker raster.ChangeTracker
	lastTime := 0.0
	push := func(pictures []*feed.Picture) {
		for _, picture := range pictures {
			at := float64(picture.Time()) / 10_000
			tracker.Push(picture.QuarterGray(width, height, turns), at)
			lastTime = at
			picture.Release()
		}
	}
	for index, sample := range track.Samples {
		if ctx.Err() != nil {
			return nil, &raster.Error{Name: raster.NameAbort, Message: "the operation was aborted"}
		}
		nals, err := file.ReadSample(track, sample)
		if err != nil {
			return nil, readFailure(path, err)
		}
		pictures, err := decoder.Decode(nals, sample.Time*10_000_000/int64(track.Timescale), sample.Sync, nil)
		push(pictures)
		if err != nil {
			return nil, E.Cause(err, "graphics: decode ", filepath.Base(path), " at sample ", index)
		}
	}
	pictures, err := decoder.Drain(nil)
	push(pictures)
	if err != nil {
		return nil, E.Cause(err, "graphics: decode ", filepath.Base(path))
	}
	tracker.Finish()
	duration := max(file.Duration*1000, lastTime)
	uprightWidth := width
	if turns%2 != 0 {
		uprightWidth = height
	}
	changes := make([]raster.ChangeRecord, 0, len(tracker.Changes()))
	for _, change := range tracker.Changes() {
		changes = append(changes, change.Record(float64(uprightWidth)/float64(max(change.Width, 1)), 1, nil))
	}
	data, err := json.Marshal(map[string]any{
		"duration": duration, "scale": 1, "screen": map[string]int{"width": width, "height": height},
		"segments": []segmentRecord{{File: filepath.Base(path), Start: 0, Duration: duration, Turns: turns}},
		"changes":  changes,
	})
	if err != nil {
		return nil, err
	}
	return newVideoModel(path, data)
}
