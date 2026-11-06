//go:build !windows || (!amd64 && !arm64)

package feed

import (
	"image"
	"time"
)

// Encoder writes a movie with Media Foundation, which only 64-bit Windows has.
type Encoder struct{}

func NewEncoder(EncoderOptions) (*Encoder, error) {
	return nil, &UnavailableError{Reason: errNoDecoder.Reason}
}

func (*Encoder) HEVC() bool {
	return false
}

func (*Encoder) Write(*image.NRGBA, time.Duration, time.Duration) error {
	return errClosed
}

func (*Encoder) Finish() error {
	return errClosed
}

func (*Encoder) Close() {}
