//go:build !windows || (!amd64 && !arm64)

package feed

import E "github.com/sagernet/sing/common/exceptions"

var errNoDecoder = &UnavailableError{Reason: E.New("this platform has no HEVC decoder for it; it uses Media Foundation on 64-bit Windows")}

func available() error {
	return errNoDecoder
}

func newCodec(*parameterSets, codecOptions) (codec, error) {
	return nil, errNoDecoder
}
