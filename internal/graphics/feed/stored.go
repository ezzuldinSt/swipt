package feed

import (
	"image"
	"runtime"
	"sync"

	E "github.com/sagernet/sing/common/exceptions"
)

var errClosed = E.New("graphics: the video decoder is closed")

// worker runs jobs on one goroutine locked to its thread, where Media Foundation's objects live.
type worker struct {
	jobs     chan func()
	quit     chan struct{}
	finished chan struct{}
	stopOnce sync.Once
}

// startWorker runs setup on the worker's thread; its cleanup runs there when the worker stops.
func startWorker(setup func() (cleanup func(), err error)) (*worker, error) {
	w := &worker{jobs: make(chan func()), quit: make(chan struct{}), finished: make(chan struct{})}
	started := make(chan error, 1)
	go func() {
		defer close(w.finished)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		cleanup, err := setup()
		started <- err
		if err != nil {
			return
		}
		defer cleanup()
		for {
			select {
			case job := <-w.jobs:
				job()
			case <-w.quit:
				return
			}
		}
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return w, nil
}

// do runs job on the worker and waits for it; it answers false once the worker stopped.
func (w *worker) do(job func()) bool {
	done := make(chan struct{})
	select {
	case w.jobs <- func() {
		defer close(done)
		job()
	}:
	case <-w.finished:
		return false
	}
	<-done
	return true
}

func (w *worker) stop() {
	w.stopOnce.Do(func() {
		close(w.quit)
	})
	<-w.finished
}

// VideoFormat describes a stored video track for NewDecoder.
type VideoFormat struct {
	// H264 decodes H.264 rather than HEVC.
	H264 bool
	// ParameterSets are the VPS, SPS and PPS of HEVC, or the SPS and PPS of H.264, which go
	// before every keyframe as a decoder made after a seek needs them.
	ParameterSets [][]byte
	Width, Height int
	// Software keeps the decoding on the CPU.
	Software bool
}

// storedCodec is a codec that decodes stored samples: it may skip pictures, drain and flush.
type storedCodec interface {
	codec
	setSkip(skip func(time int64) bool)
	Drain() ([]*picture, error)
	Flush() error
}

// Decoder decodes the samples of a stored track, such as a recording's, with Media Foundation
// on a thread of its own (SWSegmentReader.m reads them with AVAssetReader). Its methods may be
// called from any goroutine, one at a time.
type Decoder struct {
	worker *worker
	codec  storedCodec
	format VideoFormat
	buffer []byte
	// color is what the HEVC track's SPS says of its colors, as the live feed reads it.
	color streamColor
}

// NewDecoder makes a decoder of a track.
func NewDecoder(format VideoFormat) (*Decoder, error) {
	if !format.H264 {
		if err := available(); err != nil {
			return nil, err
		}
	}
	d := &Decoder{format: format}
	if !format.H264 {
		d.color = streamColor{described: describedColor(format.ParameterSets)}
	}
	w, err := startWorker(func() (func(), error) {
		made, err := newCodec(&parameterSets{width: format.Width, height: format.Height}, codecOptions{software: format.Software, h264: format.H264})
		if err != nil {
			return nil, err
		}
		stored, ok := made.(storedCodec)
		if !ok {
			made.Close()
			return nil, E.New("the decoder cannot read stored video")
		}
		d.codec = stored
		return made.Close, nil
	})
	if err != nil {
		return nil, E.Cause(err, "create the video decoder")
	}
	d.worker = w
	return d, nil
}

// Name names the decoder.
func (d *Decoder) Name() string {
	var name string
	d.worker.do(func() {
		name = d.codec.Name()
	})
	return name
}

func (d *Decoder) wrapPictures(pictures []*picture) []*Picture {
	wrapped := make([]*Picture, len(pictures))
	for index, it := range pictures {
		d.color.apply(it)
		wrapped[index] = &Picture{picture: it}
	}
	return wrapped
}

func (d *Decoder) annexB(nals [][]byte, keyframe bool) []byte {
	buffer := d.buffer[:0]
	if keyframe {
		for _, nal := range d.format.ParameterSets {
			buffer = append(append(buffer, startCode...), nal...)
		}
	}
	for _, nal := range nals {
		buffer = append(append(buffer, startCode...), nal...)
	}
	d.buffer = buffer
	return buffer
}

// Decode feeds a sample's NAL units with its time, in 100 ns units, and answers the pictures
// that came out; keep, when set, picks the times whose pictures to copy out, and the others
// are dropped on the GPU.
func (d *Decoder) Decode(nals [][]byte, time int64, keyframe bool, keep func(time int64) bool) ([]*Picture, error) {
	var pictures []*picture
	var err error
	if !d.worker.do(func() {
		d.codec.setSkip(skipOf(keep))
		defer d.codec.setSkip(nil)
		pictures, err = d.codec.Decode(d.annexB(nals, keyframe), time, keyframe)
	}) {
		return nil, errClosed
	}
	return d.wrapPictures(pictures), err
}

// Drain answers the pictures the decoder still holds, at the end of the samples.
func (d *Decoder) Drain(keep func(time int64) bool) ([]*Picture, error) {
	var pictures []*picture
	var err error
	if !d.worker.do(func() {
		d.codec.setSkip(skipOf(keep))
		defer d.codec.setSkip(nil)
		pictures, err = d.codec.Drain()
	}) {
		return nil, errClosed
	}
	return d.wrapPictures(pictures), err
}

// Flush drops what the decoder holds before a seek; the next sample must be a keyframe.
func (d *Decoder) Flush() error {
	var err error
	if !d.worker.do(func() {
		err = d.codec.Flush()
	}) {
		return errClosed
	}
	return err
}

// Close releases the decoder.
func (d *Decoder) Close() {
	d.worker.stop()
}

func skipOf(keep func(time int64) bool) func(time int64) bool {
	if keep == nil {
		return nil
	}
	return func(time int64) bool {
		return !keep(time)
	}
}

// Picture is a decoded picture of a stored track, in Go memory.
type Picture struct {
	picture *picture
}

// Time is the time the sample was decoded with.
func (p *Picture) Time() int64 {
	return p.picture.time
}

func (p *Picture) Width() int {
	return p.picture.width
}

func (p *Picture) Height() int {
	return p.picture.height
}

// RGBA converts the top-left width by height of the picture to opaque RGBA turned clockwise by
// quarter turns (SWFrame.m raster:).
func (p *Picture) RGBA(width, height, turns int) *image.NRGBA {
	return p.picture.rgba(width, height, turns)
}

// QuarterGray shrinks the top-left width by height to a quarter in gray, turned clockwise by
// quarter turns (SWVideoAnalysis.m reads the frames at a quarter of their size).
func (p *Picture) QuarterGray(width, height, turns int) *image.Gray {
	return p.picture.quarterGray(width, height, turns)
}

// Release gives the picture's bytes back; the picture must not be used afterwards.
func (p *Picture) Release() {
	p.picture.recycle()
}
