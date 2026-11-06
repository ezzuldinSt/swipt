package feed

import (
	"strconv"

	"github.com/swipt-automation/swipt/internal/graphics/raster"
)

// The pixel formats a device stream starts with (CoreVideo's kCVPixelFormatType FourCCs): the
// last letter tells full ('f') or video ('v') range.
const (
	pixelFormat420Video = 0x34323076 // 420v
	pixelFormat420Full  = 0x34323066 // 420f
	pixelFormat444Video = 0x34343476 // 444v
	pixelFormat444Full  = 0x34343466 // 444f
)

// PixelFormatScreen is a Screen that knows the pixel format its stream started with, such as
// the 444f of the device's remote desktop stream; the feed takes the range from it when the
// stream's VUI does not give one.
type PixelFormatScreen interface {
	PixelFormat() uint32
}

// rangeHint is a range known from outside the stream.
type rangeHint struct {
	known, fullRange bool
}

func hintOfPixelFormat(format uint32) rangeHint {
	switch format {
	case pixelFormat420Full, pixelFormat444Full:
		return rangeHint{known: true, fullRange: true}
	case pixelFormat420Video, pixelFormat444Video:
		return rangeHint{known: true}
	}
	return rangeHint{}
}

// matrixOf is the matrix of H.273 MatrixCoefficients, false for 2 (unspecified) and the ones
// it does not know.
func matrixOf(code uint8) (colorMatrix, bool) {
	switch code {
	case 0:
		return matrixIdentity, true
	case 1:
		return matrixBT709, true
	case 4:
		return matrixFCC, true
	case 5, 6:
		return matrixBT601, true
	case 7:
		return matrixSMPTE240, true
	case 8:
		return matrixYCgCo, true
	case 9, 10:
		// The constant-luminance 10 is read as the non-constant one.
		return matrixBT2020, true
	}
	return 0, false
}

// streamColor is how the pictures of a stream are converted: the stream's own VUI first, then
// the range its pixel format gives, then what Media Foundation says (which defaults to video
// range and BT.709).
type streamColor struct {
	described colorDescription
	hint      rangeHint
}

func (c streamColor) apply(p *picture) {
	switch {
	case c.described.signal:
		p.fullRange = c.described.fullRange
	case c.hint.known:
		p.fullRange = c.hint.fullRange
	}
	if c.described.described {
		if matrix, ok := matrixOf(c.described.matrix); ok {
			p.matrix = matrix
		}
		p.primaries, p.transfer = c.described.primaries, c.described.transfer
	}
}

// String describes the conversion for Stats.
func (c streamColor) String() string {
	text := "range "
	switch {
	case c.described.signal && c.described.fullRange:
		text += "full (VUI)"
	case c.described.signal:
		text += "video (VUI)"
	case c.hint.known && c.hint.fullRange:
		text += "full (pixel format)"
	case c.hint.known:
		text += "video (pixel format)"
	default:
		text += "from the decoder"
	}
	if !c.described.described {
		return text + ", no colour description"
	}
	return text + ", primaries " + strconv.Itoa(int(c.described.primaries)) + ", transfer " + strconv.Itoa(int(c.described.transfer)) +
		", matrix " + strconv.Itoa(int(c.described.matrix))
}

// conversionToSRGB is the conversion of a picture's RGB to sRGB, for the wide-gamut primaries
// of Display P3, DCI-P3 and BT.2020, as screenshots in Display P3 are read in sRGB. iOS draws
// with the sRGB curve whatever transfer its video is tagged with, so the BT.709 curves count as
// sRGB's; the primaries of BT.709 and BT.601 pass as sRGB, as video players show them.
func (p *picture) conversionToSRGB() *raster.ColorConversion {
	switch p.primaries {
	case 9, 11, 12:
	default:
		return nil
	}
	transfer := p.transfer
	switch transfer {
	case 1, 2, 6, 14, 15:
		transfer = 13
	}
	return raster.ConversionToSRGB(p.primaries, transfer, true)
}
