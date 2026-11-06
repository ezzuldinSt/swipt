//go:build !darwin || !cgo

package movie

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"slices"

	E "github.com/sagernet/sing/common/exceptions"
)

// Codec is how the video track is compressed.
type Codec int

const (
	HEVC Codec = iota + 1
	H264
)

func (c Codec) String() string {
	switch c {
	case HEVC:
		return "HEVC"
	case H264:
		return "H.264"
	}
	return "unknown"
}

// Sample is a sample of a track: where its bytes are, its presentation time and duration in
// the track's timescale, and whether it is a keyframe.
type Sample struct {
	Offset   int64
	Size     int
	Time     int64
	Duration int64
	Sync     bool
}

// Track is the first video track of a movie, its samples in decoding order.
type Track struct {
	Codec Codec
	// Width and Height are the size of the sample description: the stored, unturned pictures.
	Width, Height int
	// Turns is the quarter turns clockwise that the track's matrix applies to show them.
	Turns     int
	Timescale uint32
	// Duration is the media's duration in the track's timescale.
	Duration int64
	// ParameterSets are the VPS, SPS and PPS of HEVC, or the SPS and PPS of H.264, from the
	// sample description; LengthSize is the size of the length before each NAL unit.
	ParameterSets [][]byte
	LengthSize    int
	Samples       []Sample
}

// Seconds converts a time in the track's timescale.
func (t *Track) Seconds(value int64) float64 {
	return float64(value) / float64(t.Timescale)
}

// File is an open movie.
type File struct {
	file *os.File
	// Duration is the movie's duration in seconds (AVAsset duration).
	Duration float64
	Video    *Track
}

// Open reads the sample tables of the first video track of a QuickTime or MPEG-4 file.
func Open(path string) (*File, error) {
	file, err := openShared(path)
	if err != nil {
		return nil, err
	}
	opened := &File{file: file}
	if err = opened.read(); err != nil {
		file.Close()
		return nil, err
	}
	return opened, nil
}

func (f *File) Close() error {
	return f.file.Close()
}

// ReadSample reads a sample's NAL units.
func (f *File) ReadSample(track *Track, sample Sample) ([][]byte, error) {
	data := make([]byte, sample.Size)
	if _, err := f.file.ReadAt(data, sample.Offset); err != nil {
		return nil, E.Cause(err, "read a sample")
	}
	var nals [][]byte
	for at := 0; at < len(data); {
		if at+track.LengthSize > len(data) {
			return nil, E.New("a sample ends inside a NAL unit length")
		}
		var size int
		for index := range track.LengthSize {
			size = size<<8 | int(data[at+index])
		}
		at += track.LengthSize
		if size == 0 {
			continue
		}
		if at+size > len(data) {
			return nil, E.New("a NAL unit runs past its sample")
		}
		nals = append(nals, data[at:at+size:at+size])
		at += size
	}
	return nals, nil
}

var errTruncated = E.New("a box runs past its parent")

// box is a box's type and body.
type box struct {
	kind string
	body []byte
}

// children splits a body into boxes.
func children(data []byte) ([]box, error) {
	var boxes []box
	for len(data) >= 8 {
		size := uint64(binary.BigEndian.Uint32(data))
		kind := string(data[4:8])
		header := uint64(8)
		switch size {
		case 0:
			size = uint64(len(data))
		case 1:
			if len(data) < 16 {
				return nil, errTruncated
			}
			size = binary.BigEndian.Uint64(data[8:])
			header = 16
		}
		if size < header || size > uint64(len(data)) {
			return nil, errTruncated
		}
		boxes = append(boxes, box{kind: kind, body: data[header:size]})
		data = data[size:]
	}
	return boxes, nil
}

func find(boxes []box, kind string) []byte {
	for _, it := range boxes {
		if it.kind == kind {
			return it.body
		}
	}
	return nil
}

// childrenOf answers the boxes inside the box of a kind among the boxes of data.
func childrenOf(data []byte, kind string) ([]box, error) {
	body := find(mustChildren(data), kind)
	if body == nil {
		return nil, E.New("the movie has no ", kind, " box")
	}
	return children(body)
}

func mustChildren(data []byte) []box {
	boxes, _ := children(data)
	return boxes
}

// read finds the moov box among the top-level boxes, wherever it is.
func (f *File) read() error {
	info, err := f.file.Stat()
	if err != nil {
		return err
	}
	end := info.Size()
	var header [16]byte
	for offset := int64(0); offset+8 <= end; {
		if _, err = f.file.ReadAt(header[:8], offset); err != nil {
			return E.Cause(err, "read a box")
		}
		size := int64(binary.BigEndian.Uint32(header[:]))
		kind := string(header[4:8])
		length := int64(8)
		switch size {
		case 0:
			size = end - offset
		case 1:
			if _, err = f.file.ReadAt(header[8:16], offset+8); err != nil {
				return E.Cause(err, "read a box")
			}
			size = int64(binary.BigEndian.Uint64(header[8:]))
			length = 16
		}
		if size < length || offset+size > end {
			return E.New("the file is not a movie, or it was cut short")
		}
		if kind == "moov" {
			if size > 1<<30 {
				return E.New("the movie's index is too large")
			}
			data := make([]byte, size-length)
			if _, err = f.file.ReadAt(data, offset+length); err != nil && !errors.Is(err, io.EOF) {
				return E.Cause(err, "read the movie's index")
			}
			return f.parseMovie(data)
		}
		offset += size
	}
	return E.New("the file has no movie index; it may not be a movie or may not have been finished")
}

func (f *File) parseMovie(data []byte) error {
	boxes, err := children(data)
	if err != nil {
		return err
	}
	var movieScale uint32
	if mvhd := find(boxes, "mvhd"); len(mvhd) >= 20 {
		var duration uint64
		if mvhd[0] == 1 && len(mvhd) >= 32 {
			movieScale = binary.BigEndian.Uint32(mvhd[20:])
			duration = binary.BigEndian.Uint64(mvhd[24:])
		} else {
			movieScale = binary.BigEndian.Uint32(mvhd[12:])
			duration = uint64(binary.BigEndian.Uint32(mvhd[16:]))
		}
		if movieScale > 0 {
			f.Duration = float64(duration) / float64(movieScale)
		}
	}
	var failure error
	for _, it := range boxes {
		if it.kind != "trak" {
			continue
		}
		track, err := parseTrack(it.body, movieScale)
		if err != nil {
			failure = err
			continue
		}
		if track != nil {
			f.Video = track
			if f.Duration == 0 {
				f.Duration = track.Seconds(track.Duration)
			}
			return nil
		}
	}
	if failure != nil {
		return failure
	}
	return E.New("the movie has no video track")
}

// parseTrack answers nil for a track that is not video.
func parseTrack(data []byte, movieScale uint32) (*Track, error) {
	boxes, err := children(data)
	if err != nil {
		return nil, err
	}
	mdia, err := children(find(boxes, "mdia"))
	if err != nil {
		return nil, err
	}
	if handler := find(mdia, "hdlr"); len(handler) < 12 || string(handler[8:12]) != "vide" {
		return nil, nil
	}
	track := &Track{}
	if tkhd := find(boxes, "tkhd"); len(tkhd) > 0 {
		at := 40 // version 0: flags, times, ID, reserved, duration, reserved, layer, group, volume
		if tkhd[0] == 1 {
			at = 52
		}
		if len(tkhd) >= at+36 {
			a := float64(int32(binary.BigEndian.Uint32(tkhd[at:])))
			b := float64(int32(binary.BigEndian.Uint32(tkhd[at+4:])))
			turns := int(math.Round(math.Atan2(b, a) / (math.Pi / 2)))
			track.Turns = ((turns % 4) + 4) % 4
		}
	}
	mdhd := find(mdia, "mdhd")
	if len(mdhd) < 20 {
		return nil, E.New("the video track has no media header")
	}
	if mdhd[0] == 1 && len(mdhd) >= 32 {
		track.Timescale = binary.BigEndian.Uint32(mdhd[20:])
		track.Duration = int64(binary.BigEndian.Uint64(mdhd[24:]))
	} else {
		track.Timescale = binary.BigEndian.Uint32(mdhd[12:])
		track.Duration = int64(binary.BigEndian.Uint32(mdhd[16:]))
	}
	if track.Timescale == 0 {
		return nil, E.New("the video track has no timescale")
	}
	stbl, err := childrenOf(find(mdia, "minf"), "stbl")
	if err != nil {
		return nil, err
	}
	if err = track.parseDescription(find(stbl, "stsd")); err != nil {
		return nil, err
	}
	if err = track.parseSamples(stbl); err != nil {
		return nil, err
	}
	if edts := find(boxes, "edts"); edts != nil {
		track.applyEdits(find(mustChildren(edts), "elst"), movieScale)
	}
	return track, nil
}

func (t *Track) parseDescription(stsd []byte) error {
	if len(stsd) < 8 || binary.BigEndian.Uint32(stsd[4:]) == 0 {
		return E.New("the video track has no sample description")
	}
	entries, err := children(stsd[8:])
	if err != nil || len(entries) == 0 {
		return E.New("the video track's sample description does not parse")
	}
	entry := entries[0]
	switch entry.kind {
	case "hvc1", "hev1":
		t.Codec = HEVC
	case "avc1", "avc3":
		t.Codec = H264
	default:
		return E.New("the video is ", entry.kind, ", not HEVC or H.264")
	}
	if len(entry.body) < 78 {
		return E.New("the video's sample description is cut short")
	}
	t.Width = int(binary.BigEndian.Uint16(entry.body[24:]))
	t.Height = int(binary.BigEndian.Uint16(entry.body[26:]))
	configurations, err := children(entry.body[78:])
	if err != nil {
		return err
	}
	if t.Codec == HEVC {
		return t.parseHEVCConfiguration(find(configurations, "hvcC"))
	}
	return t.parseAVCConfiguration(find(configurations, "avcC"))
}

func (t *Track) parseHEVCConfiguration(data []byte) error {
	if len(data) < 23 {
		return E.New("the video has no HEVC configuration")
	}
	t.LengthSize = int(data[21]&3) + 1
	count := int(data[22])
	at := 23
	for range count {
		if at+3 > len(data) {
			return E.New("the HEVC configuration is cut short")
		}
		nals := int(binary.BigEndian.Uint16(data[at+1:]))
		at += 3
		for range nals {
			if at+2 > len(data) {
				return E.New("the HEVC configuration is cut short")
			}
			size := int(binary.BigEndian.Uint16(data[at:]))
			at += 2
			if at+size > len(data) {
				return E.New("the HEVC configuration is cut short")
			}
			t.ParameterSets = append(t.ParameterSets, data[at:at+size:at+size])
			at += size
		}
	}
	return nil
}

func (t *Track) parseAVCConfiguration(data []byte) error {
	if len(data) < 7 {
		return E.New("the video has no H.264 configuration")
	}
	t.LengthSize = int(data[4]&3) + 1
	at := 5
	for pass := range 2 {
		if at >= len(data) {
			return E.New("the H.264 configuration is cut short")
		}
		count := int(data[at])
		if pass == 0 {
			count &= 0x1f
		}
		at++
		for range count {
			if at+2 > len(data) {
				return E.New("the H.264 configuration is cut short")
			}
			size := int(binary.BigEndian.Uint16(data[at:]))
			at += 2
			if at+size > len(data) {
				return E.New("the H.264 configuration is cut short")
			}
			t.ParameterSets = append(t.ParameterSets, data[at:at+size:at+size])
			at += size
		}
	}
	return nil
}

func uint32s(data []byte, at, count int) ([]uint32, bool) {
	if count < 0 || at+4*count > len(data) {
		return nil, false
	}
	values := make([]uint32, count)
	for index := range values {
		values[index] = binary.BigEndian.Uint32(data[at+4*index:])
	}
	return values, true
}

func (t *Track) parseSamples(stbl []box) error {
	stsz := find(stbl, "stsz")
	if len(stsz) < 12 {
		return E.New("the video track has no sample sizes")
	}
	count := int(binary.BigEndian.Uint32(stsz[8:]))
	if count > 1<<24 {
		return E.New("the video track lists too many samples")
	}
	sizes := make([]int, count)
	if fixed := binary.BigEndian.Uint32(stsz[4:]); fixed != 0 {
		for index := range sizes {
			sizes[index] = int(fixed)
		}
	} else {
		listed, ok := uint32s(stsz, 12, count)
		if !ok {
			return E.New("the sample sizes are cut short")
		}
		for index, size := range listed {
			sizes[index] = int(size)
		}
	}
	var chunks []int64
	if stco := find(stbl, "stco"); len(stco) >= 8 {
		listed, ok := uint32s(stco, 8, int(binary.BigEndian.Uint32(stco[4:])))
		if !ok {
			return E.New("the chunk offsets are cut short")
		}
		for _, offset := range listed {
			chunks = append(chunks, int64(offset))
		}
	} else if co64 := find(stbl, "co64"); len(co64) >= 8 {
		entries := int(binary.BigEndian.Uint32(co64[4:]))
		if 8+8*entries > len(co64) {
			return E.New("the chunk offsets are cut short")
		}
		for index := range entries {
			chunks = append(chunks, int64(binary.BigEndian.Uint64(co64[8+8*index:])))
		}
	} else {
		return E.New("the video track has no chunk offsets")
	}
	stsc := find(stbl, "stsc")
	if len(stsc) < 8 {
		return E.New("the video track has no chunk map")
	}
	entries := int(binary.BigEndian.Uint32(stsc[4:]))
	runs, ok := uint32s(stsc, 8, entries*3)
	if !ok {
		return E.New("the chunk map is cut short")
	}
	t.Samples = make([]Sample, count)
	sample := 0
	for entry := 0; entry < entries && sample < count; entry++ {
		first, perChunk := int(runs[entry*3]), int(runs[entry*3+1])
		last := len(chunks)
		if entry+1 < entries {
			last = int(runs[(entry+1)*3]) - 1
		}
		for chunk := first; chunk <= last && chunk >= 1 && chunk <= len(chunks) && sample < count; chunk++ {
			offset := chunks[chunk-1]
			for range perChunk {
				if sample >= count {
					break
				}
				t.Samples[sample].Offset = offset
				t.Samples[sample].Size = sizes[sample]
				offset += int64(sizes[sample])
				sample++
			}
		}
	}
	if sample < count {
		return E.New("the chunk map covers ", sample, " of ", count, " samples")
	}
	stts := find(stbl, "stts")
	if len(stts) < 8 {
		return E.New("the video track has no sample times")
	}
	timing, ok := uint32s(stts, 8, int(binary.BigEndian.Uint32(stts[4:]))*2)
	if !ok {
		return E.New("the sample times are cut short")
	}
	var clock int64
	sample = 0
	for entry := 0; entry+1 < len(timing) && sample < count; entry += 2 {
		for range timing[entry] {
			if sample >= count {
				break
			}
			t.Samples[sample].Time = clock
			t.Samples[sample].Duration = int64(timing[entry+1])
			clock += int64(timing[entry+1])
			sample++
		}
	}
	if ctts := find(stbl, "ctts"); len(ctts) >= 8 {
		offsets, ok := uint32s(ctts, 8, int(binary.BigEndian.Uint32(ctts[4:]))*2)
		if ok {
			sample = 0
			for entry := 0; entry+1 < len(offsets) && sample < count; entry += 2 {
				for range offsets[entry] {
					if sample >= count {
						break
					}
					t.Samples[sample].Time += int64(int32(offsets[entry+1]))
					sample++
				}
			}
		}
	}
	if stss := find(stbl, "stss"); len(stss) >= 8 {
		syncs, ok := uint32s(stss, 8, int(binary.BigEndian.Uint32(stss[4:])))
		if !ok {
			return E.New("the keyframe list is cut short")
		}
		for _, number := range syncs {
			if number >= 1 && int(number) <= count {
				t.Samples[number-1].Sync = true
			}
		}
	} else {
		for index := range t.Samples {
			t.Samples[index].Sync = true
		}
	}
	if t.Duration == 0 {
		t.Duration = clock
	}
	return nil
}

// applyEdits shifts the samples by the edit list as AVFoundation presents them: empty edits
// delay the media, and the first other edit starts it at its media time.
func (t *Track) applyEdits(elst []byte, movieScale uint32) {
	if len(elst) < 8 || movieScale == 0 {
		return
	}
	version := elst[0]
	count := int(binary.BigEndian.Uint32(elst[4:]))
	entrySize := 12
	if version == 1 {
		entrySize = 20
	}
	var delay, start int64
	for index := range count {
		at := 8 + index*entrySize
		if at+entrySize > len(elst) {
			return
		}
		var duration, mediaTime int64
		if version == 1 {
			duration = int64(binary.BigEndian.Uint64(elst[at:]))
			mediaTime = int64(binary.BigEndian.Uint64(elst[at+8:]))
		} else {
			duration = int64(binary.BigEndian.Uint32(elst[at:]))
			mediaTime = int64(int32(binary.BigEndian.Uint32(elst[at+4:])))
		}
		if mediaTime < 0 {
			delay += duration * int64(t.Timescale) / int64(movieScale)
			continue
		}
		start = mediaTime
		break
	}
	if delay == 0 && start == 0 {
		return
	}
	for index := range t.Samples {
		t.Samples[index].Time += delay - start
	}
}

// PresentationOrder answers the samples' indexes sorted by presentation time.
func (t *Track) PresentationOrder() []int {
	order := make([]int, len(t.Samples))
	for index := range order {
		order[index] = index
	}
	slices.SortStableFunc(order, func(a, b int) int {
		switch {
		case t.Samples[a].Time < t.Samples[b].Time:
			return -1
		case t.Samples[a].Time > t.Samples[b].Time:
			return 1
		}
		return 0
	})
	return order
}
