//go:build !darwin || !cgo

package movie

import (
	"bufio"
	"encoding/binary"
	"os"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
)

// The movie's own timescale; the track keeps the timescale it is given.
const movieTimescale = 1000

// Seconds from 1904, when QuickTime's clock starts, to 1970.
const macEpoch = 2082844800

// The header of the mdat box: size 1, then the 64-bit size, so it may pass 4 GB.
const mdatHeaderSize = 16

// Writer writes one HEVC track of a QuickTime movie as samples come, like AVAssetWriter with
// an input that passes them through (SWSegmentWriter.m): an 'hvc1' sample description with the
// parameter sets, length-prefixed samples in the mdat box, and the moov box at the end. Its
// track matrix turns the pictures clockwise by quarter turns, as the writer input's transform.
type Writer struct {
	file      *os.File
	output    *bufio.Writer
	format    *Format
	turns     int
	timescale uint32
	created   uint32

	written   int64
	sizes     []uint32
	durations []uint32
	syncs     []uint32
	duration  int64
	failure   error
	closed    bool
	scratch   []byte
}

// Create starts a movie at path, replacing any file there.
func Create(path string, format *Format, turns int, timescale uint32) (*Writer, error) {
	file, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	w := &Writer{
		file: file, output: bufio.NewWriterSize(file, 1<<20), format: format, turns: ((turns % 4) + 4) % 4, timescale: timescale,
		created: uint32(time.Now().Unix() + macEpoch),
	}
	header := []byte{0, 0, 0, 20, 'f', 't', 'y', 'p', 'q', 't', ' ', ' ', 0, 0, 0, 0, 'q', 't', ' ', ' '}
	header = append(header, 0, 0, 0, 1, 'm', 'd', 'a', 't', 0, 0, 0, 0, 0, 0, 0, mdatHeaderSize)
	if _, err = w.output.Write(header); err != nil {
		file.Close()
		os.Remove(path)
		return nil, err
	}
	return w, nil
}

// Turns is the quarter turns of the track's matrix.
func (w *Writer) Turns() int {
	return w.turns
}

// Format is the parameter sets of the sample description.
func (w *Writer) Format() *Format {
	return w.format
}

// Samples is how many samples were written.
func (w *Writer) Samples() int {
	return len(w.sizes)
}

// Append writes an access unit lasting duration in the track's timescale; sync marks a
// keyframe. The parameter sets stay in the sample description, so a unit of nothing else
// writes no sample and answers false (SWHEVC.m SWHEVCSample).
func (w *Writer) Append(nals [][]byte, duration int64, sync bool) (bool, error) {
	if w.failure != nil {
		return false, w.failure
	}
	if w.closed {
		return false, E.New("the movie is closed")
	}
	size := 0
	for _, nal := range nals {
		if kind := nalType(nal); kind < nalVPS || kind > nalPPS {
			size += 4 + len(nal)
		}
	}
	if size == 0 {
		return false, nil
	}
	if duration <= 0 || duration > 1<<31 {
		return false, E.New("a sample of ", duration, " ticks")
	}
	w.scratch = w.scratch[:0]
	for _, nal := range nals {
		if kind := nalType(nal); kind < nalVPS || kind > nalPPS {
			w.scratch = binary.BigEndian.AppendUint32(w.scratch, uint32(len(nal)))
			w.scratch = append(w.scratch, nal...)
		}
	}
	if _, err := w.output.Write(w.scratch); err != nil {
		w.failure = err
		return false, err
	}
	w.written += int64(size)
	w.sizes = append(w.sizes, uint32(size))
	w.durations = append(w.durations, uint32(duration))
	if sync {
		w.syncs = append(w.syncs, uint32(len(w.sizes)))
	}
	w.duration += duration
	return true, nil
}

// Duration is the time the samples cover in the track's timescale.
func (w *Writer) Duration() int64 {
	return w.duration
}

// Close writes the moov box and closes the file; a movie without samples is still a movie.
func (w *Writer) Close() error {
	if w.closed {
		return w.failure
	}
	w.closed = true
	err := w.failure
	if err == nil {
		err = w.output.Flush()
	}
	if err == nil {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(mdatHeaderSize+w.written))
		_, err = w.file.WriteAt(size[:], 28)
	}
	if err == nil {
		_, err = w.file.Seek(0, 2)
	}
	if err == nil {
		_, err = w.file.Write(w.movie())
	}
	if closeErr := w.file.Close(); err == nil {
		err = closeErr
	}
	if err != nil && w.failure == nil {
		w.failure = err
	}
	return err
}

// builder writes nested boxes.
type builder struct {
	data   []byte
	starts []int
}

func (b *builder) open(kind string) {
	b.starts = append(b.starts, len(b.data))
	b.data = append(b.data, 0, 0, 0, 0)
	b.data = append(b.data, kind...)
}

func (b *builder) full(kind string, version byte, flags uint32) {
	b.open(kind)
	b.u32(uint32(version)<<24 | flags)
}

func (b *builder) close() {
	start := b.starts[len(b.starts)-1]
	b.starts = b.starts[:len(b.starts)-1]
	binary.BigEndian.PutUint32(b.data[start:], uint32(len(b.data)-start))
}

func (b *builder) u8(value byte) {
	b.data = append(b.data, value)
}

func (b *builder) u16(value uint16) {
	b.data = binary.BigEndian.AppendUint16(b.data, value)
}

func (b *builder) u32(value uint32) {
	b.data = binary.BigEndian.AppendUint32(b.data, value)
}

func (b *builder) u64(value uint64) {
	b.data = binary.BigEndian.AppendUint64(b.data, value)
}

func (b *builder) zeros(count int) {
	b.data = append(b.data, make([]byte, count)...)
}

// matrix writes the track's transform: a clockwise quarter turn of a width by height picture,
// moved back into view, in QuickTime's 16.16 and 2.30 fixed point.
func (b *builder) matrix(turns, width, height int) {
	const one = 0x10000
	a, bb, c, d, tx, ty := int32(one), int32(0), int32(0), int32(one), int32(0), int32(0)
	switch turns {
	case 1:
		a, bb, c, d, tx = 0, one, -one, 0, int32(height)*one
	case 2:
		a, d, tx, ty = -one, -one, int32(width)*one, int32(height)*one
	case 3:
		a, bb, c, d, ty = 0, -one, one, 0, int32(width)*one
	}
	for _, value := range []int32{a, bb, 0, c, d, 0, tx, ty, 0x40000000} {
		b.u32(uint32(value))
	}
}

func (w *Writer) movie() []byte {
	b := &builder{}
	movieDuration := uint32((w.duration*movieTimescale + int64(w.timescale)/2) / int64(w.timescale))
	width, height := w.format.Width, w.format.Height
	b.open("moov")
	b.full("mvhd", 0, 0)
	b.u32(w.created)
	b.u32(w.created)
	b.u32(movieTimescale)
	b.u32(movieDuration)
	b.u32(0x00010000) // preferred rate
	b.u16(0x0100)     // preferred volume
	b.zeros(10)
	b.matrix(0, 0, 0)
	b.zeros(24) // preview, poster, selection and current times
	b.u32(2)    // next track ID
	b.close()

	b.open("trak")
	b.full("tkhd", 0, 3) // enabled, in the movie
	b.u32(w.created)
	b.u32(w.created)
	b.u32(1) // track ID
	b.u32(0)
	b.u32(movieDuration)
	b.zeros(8)
	b.u16(0) // layer
	b.u16(0) // alternate group
	b.u16(0) // volume
	b.u16(0)
	b.matrix(w.turns, width, height)
	b.u32(uint32(width) << 16)
	b.u32(uint32(height) << 16)
	b.close()

	b.open("mdia")
	b.full("mdhd", 0, 0)
	b.u32(w.created)
	b.u32(w.created)
	b.u32(w.timescale)
	b.u32(uint32(w.duration))
	b.u16(0x55c4) // und
	b.u16(0)
	b.close()
	b.full("hdlr", 0, 0)
	b.data = append(b.data, "mhlrvide"...)
	b.zeros(12)
	b.u8(0) // an empty name, as a Pascal and a C string
	b.close()

	b.open("minf")
	b.full("vmhd", 0, 1)
	b.zeros(8)
	b.close()
	b.full("hdlr", 0, 0)
	b.data = append(b.data, "dhlrurl "...)
	b.zeros(12)
	b.u8(0)
	b.close()
	b.open("dinf")
	b.full("dref", 0, 0)
	b.u32(1)
	b.full("url ", 0, 1) // in this file
	b.close()
	b.close()
	b.close()

	b.open("stbl")
	b.full("stsd", 0, 0)
	b.u32(1)
	b.open("hvc1")
	b.zeros(6)
	b.u16(1)    // data reference index
	b.zeros(16) // version, revision, vendor, temporal and spatial quality
	b.u16(uint16(width))
	b.u16(uint16(height))
	b.u32(0x00480000) // 72 dpi
	b.u32(0x00480000)
	b.u32(0)
	b.u16(1) // frames a sample
	name := make([]byte, 32)
	name[0] = 4
	copy(name[1:], "HEVC")
	b.data = append(b.data, name...)
	b.u16(24)
	b.u16(0xffff)
	b.open("hvcC")
	b.data = append(b.data, w.format.configuration()...)
	b.close()
	b.close()
	b.close()

	b.full("stts", 0, 0)
	countAt := len(b.data)
	b.u32(0)
	runs := uint32(0)
	for index := 0; index < len(w.durations); {
		end := index
		for end < len(w.durations) && w.durations[end] == w.durations[index] {
			end++
		}
		b.u32(uint32(end - index))
		b.u32(w.durations[index])
		runs++
		index = end
	}
	binary.BigEndian.PutUint32(b.data[countAt:], runs)
	b.close()

	if len(w.syncs) < len(w.sizes) {
		b.full("stss", 0, 0)
		b.u32(uint32(len(w.syncs)))
		for _, sample := range w.syncs {
			b.u32(sample)
		}
		b.close()
	}

	b.full("stsc", 0, 0)
	if len(w.sizes) > 0 {
		b.u32(1)
		b.u32(1) // from the first chunk on,
		b.u32(1) // one sample a chunk,
		b.u32(1) // of the first description
	} else {
		b.u32(0)
	}
	b.close()

	b.full("stsz", 0, 0)
	b.u32(0)
	b.u32(uint32(len(w.sizes)))
	for _, size := range w.sizes {
		b.u32(size)
	}
	b.close()

	b.full("co64", 0, 0)
	b.u32(uint32(len(w.sizes)))
	offset := uint64(20 + mdatHeaderSize)
	for _, size := range w.sizes {
		b.u64(offset)
		offset += uint64(size)
	}
	b.close()

	b.close() // stbl
	b.close() // minf
	b.close() // mdia
	b.close() // trak
	b.close() // moov
	return b.data
}
