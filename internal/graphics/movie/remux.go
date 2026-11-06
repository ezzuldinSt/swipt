//go:build !darwin || !cgo

package movie

import (
	E "github.com/sagernet/sing/common/exceptions"
)

// H.264 NAL unit types (ITU-T H.264 table 7-1).
const (
	avcSPS = 7
	avcPPS = 8
)

func avcNALType(nal []byte) int {
	if len(nal) == 0 {
		return -1
	}
	return int(nal[0] & 0x1f)
}

// avcConfiguration is the AVCDecoderConfigurationRecord of an avcC box (ISO/IEC 14496-15
// 5.3.3.1) for the SPS and PPS among the parameter sets, NAL units behind 4-byte lengths.
func avcConfiguration(parameterSets [][]byte) ([]byte, error) {
	var sps, pps [][]byte
	for _, nal := range parameterSets {
		switch avcNALType(nal) {
		case avcSPS:
			sps = append(sps, nal)
		case avcPPS:
			pps = append(pps, nal)
		}
	}
	if len(sps) == 0 || len(pps) == 0 || len(sps[0]) < 4 || len(sps) > 31 || len(pps) > 255 {
		return nil, E.New("the H.264 track has no usable SPS and PPS")
	}
	out := []byte{1, sps[0][1], sps[0][2], sps[0][3], 0xfc | 3, 0xe0 | byte(len(sps))}
	for _, nal := range sps {
		out = append(out, byte(len(nal)>>8), byte(len(nal)))
		out = append(out, nal...)
	}
	out = append(out, byte(len(pps)))
	for _, nal := range pps {
		out = append(out, byte(len(nal)>>8), byte(len(nal)))
		out = append(out, nal...)
	}
	return out, nil
}

// Remux copies the video track of a QuickTime or MPEG-4 file into a QuickTime movie at target
// without decoding it: the same samples, times, keyframes and turn, as an exported .mov is a
// QuickTime movie (AVFileTypeQuickTimeMovie in SWVideoExport.m).
func Remux(source, target string) error {
	file, err := Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	track := file.Video
	writer, err := CreateTrack(target, track.Codec, track.ParameterSets, track.Width, track.Height, track.Turns, track.Timescale)
	if err != nil {
		return err
	}
	var decoded int64
	for _, sample := range track.Samples {
		nals, err := file.ReadSample(track, sample)
		if err == nil {
			_, err = writer.AppendAt(nals, sample.Duration, sample.Time-decoded, sample.Sync)
		}
		if err != nil {
			writer.Close()
			return E.Cause(err, "copy the video")
		}
		decoded += sample.Duration
	}
	return writer.Close()
}
