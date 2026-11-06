package feed

import "bytes"

// parameterSets is the VPS, SPS and PPS a decoder starts from, the first of each in a unit
// (SWHEVC.m SWHEVCFormat), and the picture size and colors the SPS gives.
type parameterSets struct {
	vps, sps, pps []byte
	width, height int
	color         colorDescription
}

func firstNAL(nals [][]byte, kind int) []byte {
	for _, nal := range nals {
		if NALType(nal) == kind {
			return nal
		}
	}
	return nil
}

// readParameterSets answers the unit's parameter sets, or false when one of the three is
// missing or the SPS does not parse.
func readParameterSets(nals [][]byte) (*parameterSets, bool) {
	vps, sps, pps := firstNAL(nals, nalVPS), firstNAL(nals, nalSPS), firstNAL(nals, nalPPS)
	if vps == nil || sps == nil || pps == nil {
		return nil, false
	}
	width, height, ok := pictureSize(sps)
	if !ok {
		return nil, false
	}
	sets := &parameterSets{
		vps: bytes.Clone(vps), sps: bytes.Clone(sps), pps: bytes.Clone(pps),
		width: width, height: height,
	}
	if parsed, ok := readSPS(sps); ok {
		sets.color = parsed.color
	}
	return sets, true
}

// describedColor reads the colors of the first SPS among a track's parameter sets.
func describedColor(nals [][]byte) colorDescription {
	if sps := firstNAL(nals, nalSPS); sps != nil {
		if parsed, ok := readSPS(sps); ok {
			return parsed.color
		}
	}
	return colorDescription{}
}

func (p *parameterSets) equal(other *parameterSets) bool {
	return other != nil && bytes.Equal(p.vps, other.vps) && bytes.Equal(p.sps, other.sps) && bytes.Equal(p.pps, other.pps)
}

var startCode = []byte{0, 0, 0, 1}

// annexB writes the unit as an Annex B byte stream, which Media Foundation's HEVC decoders
// read. VideoToolbox takes the parameter sets in a format description and length-prefixed
// NAL units instead (SWHEVC.m SWHEVCSample); here they stay in the stream, and a keyframe
// that arrives without them gets the last ones, so a decoder made after a loss can start.
func annexB(buffer []byte, unit *AccessUnit, sets *parameterSets) []byte {
	buffer = buffer[:0]
	if sets != nil && unit.IsKeyframe() && !unit.HasParameterSets() {
		for _, nal := range [][]byte{sets.vps, sets.sps, sets.pps} {
			buffer = append(append(buffer, startCode...), nal...)
		}
	}
	for _, nal := range unit.NALs {
		buffer = append(append(buffer, startCode...), nal...)
	}
	return buffer
}

// rtpClock unwraps the 32-bit RTP timestamps into a count from the first one (SWHEVC.m
// SWRTPClock). Media Foundation gets the count in 100 ns units at the ObjC's timescale of 24000;
// it only orders the samples and ties each picture back to its unit.
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
	return (value - c.base) * 10_000_000 / 24000
}

// bitReader reads an RBSP, the NAL payload with its emulation prevention bytes removed.
type bitReader struct {
	data []byte
	at   int
	bad  bool
}

func (r *bitReader) bit() uint32 {
	if r.at >= len(r.data)*8 {
		r.bad = true
		return 0
	}
	value := uint32(r.data[r.at/8]>>(7-r.at%8)) & 1
	r.at++
	return value
}

func (r *bitReader) bits(count int) uint32 {
	var value uint32
	for range count {
		value = value<<1 | r.bit()
	}
	return value
}

func (r *bitReader) skip(count int) {
	r.at += count
	if r.at > len(r.data)*8 {
		r.bad = true
	}
}

// ue reads an unsigned Exp-Golomb code.
func (r *bitReader) ue() uint32 {
	zeros := 0
	for r.bit() == 0 {
		if r.bad || zeros > 31 {
			r.bad = true
			return 0
		}
		zeros++
	}
	return 1<<zeros - 1 + r.bits(zeros)
}

func unescaped(nal []byte) []byte {
	out := make([]byte, 0, len(nal))
	zeros := 0
	for _, value := range nal {
		if zeros >= 2 && value == 3 {
			zeros = 0
			continue
		}
		if value == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, value)
	}
	return out
}

// pictureSize reads the luma size of an SPS less its conformance window (H.265 7.3.2.2).
func pictureSize(sps []byte) (width, height int, ok bool) {
	if len(sps) < 3 || NALType(sps) != nalSPS {
		return 0, 0, false
	}
	r := &bitReader{data: unescaped(sps[2:])}
	r.skip(4) // sps_video_parameter_set_id
	subLayers := int(r.bits(3))
	r.skip(1) // sps_temporal_id_nesting_flag
	// profile_tier_level(1, sps_max_sub_layers_minus1): the general part is 96 bits.
	r.skip(96)
	profilePresent := make([]bool, subLayers)
	levelPresent := make([]bool, subLayers)
	for index := range subLayers {
		profilePresent[index] = r.bit() == 1
		levelPresent[index] = r.bit() == 1
	}
	if subLayers > 0 {
		r.skip(2 * (8 - subLayers))
	}
	for index := range subLayers {
		if profilePresent[index] {
			r.skip(88)
		}
		if levelPresent[index] {
			r.skip(8)
		}
	}
	r.ue() // sps_seq_parameter_set_id
	chroma := r.ue()
	if chroma == 3 {
		r.skip(1) // separate_colour_plane_flag
	}
	width, height = int(r.ue()), int(r.ue())
	if r.bit() == 1 {
		left, right, top, bottom := int(r.ue()), int(r.ue()), int(r.ue()), int(r.ue())
		unitX, unitY := 1, 1
		switch chroma {
		case 1:
			unitX, unitY = 2, 2
		case 2:
			unitX = 2
		}
		width -= unitX * (left + right)
		height -= unitY * (top + bottom)
	}
	if r.bad || width <= 0 || height <= 0 || width > 16384 || height > 16384 {
		return 0, 0, false
	}
	return width, height, true
}
