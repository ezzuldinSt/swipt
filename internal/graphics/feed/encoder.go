package feed

// EncoderOptions describes a movie for NewEncoder.
type EncoderOptions struct {
	Path string
	// HEVC encodes HEVC, as AVFoundation does for a .mov export, and falls back to H.264 where
	// no HEVC encoder works; otherwise H.264, as for an .mp4 one (SWVideoExport.m).
	HEVC bool
	// Width and Height are even.
	Width, Height int
	FrameRate     float64
}
