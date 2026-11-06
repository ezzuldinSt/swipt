//go:build !darwin || !cgo

// Package movie writes and reads the files of screen recordings without the Objective-C
// engine: the QuickTime movie that AVAssetWriter writes from the device's HEVC stream without
// re-encoding it (SWSegmentWriter.m), and the video track of a QuickTime or MPEG-4 file with
// HEVC or H.264, which AVAssetReader reads in SWSegmentReader.m.
package movie

import (
	"bytes"
	"errors"
)

// HEVC NAL unit types (ITU-T H.265 table 7-1).
const (
	nalVPS = 32
	nalSPS = 33
	nalPPS = 34
)

func nalType(nal []byte) int {
	if len(nal) == 0 {
		return -1
	}
	return int(nal[0]>>1) & 0x3f
}

// Format is the parameter sets of an HEVC stream, which a QuickTime sample description keeps
// apart from the samples (CMVideoFormatDescriptionCreateFromHEVCParameterSets in SWHEVC.m).
type Format struct {
	VPS, SPS, PPS []byte
	// Width and Height are the luma size of the pictures less the conformance window.
	Width, Height int

	sps spsInfo
}

// HEVCFormat answers the first VPS, SPS and PPS among the NAL units, or false when one is
// missing or the SPS does not parse.
func HEVCFormat(nals [][]byte) (*Format, bool) {
	var vps, sps, pps []byte
	for _, nal := range nals {
		switch nalType(nal) {
		case nalVPS:
			if vps == nil {
				vps = nal
			}
		case nalSPS:
			if sps == nil {
				sps = nal
			}
		case nalPPS:
			if pps == nil {
				pps = nal
			}
		}
	}
	if vps == nil || sps == nil || pps == nil {
		return nil, false
	}
	info, err := parseSPS(sps)
	if err != nil {
		return nil, false
	}
	return &Format{
		VPS: bytes.Clone(vps), SPS: bytes.Clone(sps), PPS: bytes.Clone(pps),
		Width: info.width, Height: info.height, sps: info,
	}, true
}

// Equal tells whether two formats have the same parameter sets (CMFormatDescriptionEqual).
func (f *Format) Equal(other *Format) bool {
	return f != nil && other != nil && bytes.Equal(f.VPS, other.VPS) && bytes.Equal(f.SPS, other.SPS) && bytes.Equal(f.PPS, other.PPS)
}

// ChromaFormat is chroma_format_idc: 1 for 4:2:0, 3 for the device's 4:4:4 stream.
func (f *Format) ChromaFormat() int {
	return f.sps.chroma
}

// spsInfo is what an hvcC record repeats from the SPS (ISO/IEC 14496-15 8.3.3).
type spsInfo struct {
	profile        [12]byte // general_profile_space through general_level_idc
	subLayers      int
	nested         bool
	chroma         int
	bitDepthLuma   int
	bitDepthChroma int
	width, height  int
}

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

// unescaped removes the emulation prevention bytes of a NAL unit's payload.
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

var errBadSPS = errors.New("the SPS does not parse")

// parseSPS reads an SPS up to its bit depths (H.265 7.3.2.2).
func parseSPS(sps []byte) (spsInfo, error) {
	var info spsInfo
	if len(sps) < 3 || nalType(sps) != nalSPS {
		return info, errBadSPS
	}
	rbsp := unescaped(sps[2:])
	if len(rbsp) < 13 {
		return info, errBadSPS
	}
	r := &bitReader{data: rbsp}
	r.skip(4) // sps_video_parameter_set_id
	subLayers := int(r.bits(3))
	info.subLayers = subLayers + 1
	info.nested = r.bit() == 1
	copy(info.profile[:], rbsp[1:13])
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
	info.chroma = int(r.ue())
	if info.chroma == 3 {
		r.skip(1) // separate_colour_plane_flag
	}
	width, height := int(r.ue()), int(r.ue())
	if r.bit() == 1 {
		left, right, top, bottom := int(r.ue()), int(r.ue()), int(r.ue()), int(r.ue())
		unitX, unitY := 1, 1
		switch info.chroma {
		case 1:
			unitX, unitY = 2, 2
		case 2:
			unitX = 2
		}
		width -= unitX * (left + right)
		height -= unitY * (top + bottom)
	}
	info.bitDepthLuma = int(r.ue()) + 8
	info.bitDepthChroma = int(r.ue()) + 8
	if r.bad || width <= 0 || height <= 0 || width > 16384 || height > 16384 || info.chroma > 3 || info.bitDepthLuma > 16 || info.bitDepthChroma > 16 {
		return info, errBadSPS
	}
	info.width, info.height = width, height
	return info, nil
}

func (f *Format) description() description {
	return description{codec: HEVC, entry: "hvc1", box: "hvcC", name: "HEVC", configuration: f.configuration(), width: f.Width, height: f.Height}
}

// configuration is the HEVCDecoderConfigurationRecord of an hvcC box, with the VPS, SPS and
// PPS in arrays marked complete, and NAL units behind 4-byte lengths.
func (f *Format) configuration() []byte {
	out := []byte{1}
	out = append(out, f.sps.profile[:]...)
	out = append(out,
		0xf0, 0x00, // min_spatial_segmentation_idc 0
		0xfc,                            // parallelismType 0
		0xfc|byte(f.sps.chroma),         // chromaFormat
		0xf8|byte(f.sps.bitDepthLuma-8), // bitDepthLumaMinus8
		0xf8|byte(f.sps.bitDepthChroma-8),
		0, 0, // avgFrameRate
	)
	nested := byte(0)
	if f.sps.nested {
		nested = 1
	}
	out = append(out, byte(min(f.sps.subLayers, 7))<<3|nested<<2|3, 3)
	for _, nal := range [][]byte{f.VPS, f.SPS, f.PPS} {
		out = append(out, 0x80|byte(nalType(nal)), 0, 1, byte(len(nal)>>8), byte(len(nal)))
		out = append(out, nal...)
	}
	return out
}
