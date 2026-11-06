package feed

import (
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/log"
)

// codec is a platform HEVC decoder: Media Foundation on Windows.
type codec interface {
	// Decode feeds one access unit in Annex B with its sample time and answers the pictures
	// that came out, which may belong to earlier units.
	Decode(data []byte, time int64, keyframe bool) ([]*picture, error)
	// Accepts tells whether the decoder can go on with these parameter sets.
	Accepts(sets *parameterSets) bool
	Name() string
	Close()
}

type codecOptions struct {
	// software keeps the decoding on the CPU, without hardware transforms or Direct3D.
	software bool
	// h264 decodes H.264 rather than HEVC.
	h264 bool
}

type codecFactory func(sets *parameterSets, options codecOptions) (codec, error)

// pendingUnit ties a sample time to the unit it came from, so a picture that comes out later
// keeps that unit's arrival and orientation.
type pendingUnit struct {
	time int64
	unit *AccessUnit
}

const pendingLimit = 64

// frameDecoder decodes access units in order (SWFrameDecoder.m): a damaged unit makes it wait
// for a keyframe, new parameter sets that the decoder cannot take make a new decoder, and a
// failure drops the decoder until the next keyframe. decode answers whether to ask the device
// for a keyframe.
type frameDecoder struct {
	newCodec codecFactory
	options  codecOptions
	logger   log.ContextLogger
	onFrame  func(*picture, *AccessUnit)

	codec    codec
	format   *parameterSets
	waiting  bool
	reported string
	clock    rtpClock
	pending  []pendingUnit
	buffer   []byte

	// hint, when set, answers the range the stream's pixel format gives; onColor hears of the
	// conversion each new format brings.
	hint    func() rangeHint
	onColor func(streamColor)
	color   streamColor
}

func newFrameDecoder(factory codecFactory, options codecOptions, logger log.ContextLogger, onFrame func(*picture, *AccessUnit)) *frameDecoder {
	return &frameDecoder{newCodec: factory, options: options, logger: logger, onFrame: onFrame, waiting: true}
}

// report logs a failure once until a different one comes (SWFrameDecoder.m reportStatus:).
func (d *frameDecoder) report(err error) {
	if message := err.Error(); message != d.reported {
		d.reported = message
		d.logger.Error(err)
	}
}

func (d *frameDecoder) closeCodec() {
	if d.codec != nil {
		d.codec.Close()
		d.codec = nil
	}
	d.pending = d.pending[:0]
}

// invalidate drops the decoder and waits for a keyframe; the parameter sets stay.
func (d *frameDecoder) invalidate() {
	d.closeCodec()
	d.waiting = true
}

func (d *frameDecoder) close() {
	d.closeCodec()
}

func (d *frameDecoder) makeCodec(sets *parameterSets) bool {
	d.closeCodec()
	made, err := d.newCodec(sets, d.options)
	if err != nil {
		d.report(E.Cause(err, "create the screen decoder"))
		return false
	}
	d.codec = made
	d.logger.Debug("decoding the screen at ", sets.width, "x", sets.height, " with ", made.Name())
	return true
}

func (d *frameDecoder) adoptParameterSets(unit *AccessUnit) bool {
	described, ok := readParameterSets(unit.NALs)
	if !ok {
		return true
	}
	if d.format == nil || !d.format.equal(described) || d.codec == nil {
		d.format = described
		d.color = streamColor{described: described.color}
		if d.hint != nil {
			d.color.hint = d.hint()
		}
		if d.onColor != nil {
			d.onColor(d.color)
		}
		if d.codec == nil || !d.codec.Accepts(described) {
			return d.makeCodec(described)
		}
	}
	return true
}

func (d *frameDecoder) decode(unit *AccessUnit) bool {
	if unit.Damaged {
		d.waiting = true
		return true
	}
	if unit.HasParameterSets() && !d.adoptParameterSets(unit) {
		return true
	}
	if d.waiting {
		if !unit.IsKeyframe() {
			return true
		}
		d.waiting = false
	}
	if d.codec == nil && d.format != nil && !d.makeCodec(d.format) {
		return true
	}
	if d.codec == nil || d.format == nil {
		return false
	}
	d.buffer = annexB(d.buffer, unit, d.format)
	if len(d.buffer) == 0 {
		return false
	}
	time := d.clock.time(unit.RTPTimestamp)
	if len(d.pending) >= pendingLimit {
		d.pending = append(d.pending[:0], d.pending[len(d.pending)-pendingLimit/2:]...)
	}
	d.pending = append(d.pending, pendingUnit{time: time, unit: unit})
	pictures, err := d.codec.Decode(d.buffer, time, unit.IsKeyframe())
	for _, picture := range pictures {
		d.color.apply(picture)
		d.onFrame(picture, d.unitOf(picture.time, unit))
	}
	if err != nil {
		d.report(E.Cause(err, "decode the screen"))
		d.invalidate()
		return true
	}
	return false
}

// unitOf finds the unit a picture came from by its sample time, forgetting the ones before it.
func (d *frameDecoder) unitOf(time int64, fallback *AccessUnit) *AccessUnit {
	for index, entry := range d.pending {
		if entry.time == time {
			d.pending = append(d.pending[:0], d.pending[index+1:]...)
			return entry.unit
		}
	}
	return fallback
}
