package recognize

import (
	"cmp"
	"context"
	"image"
	"image/draw"
	"slices"

	E "github.com/sagernet/sing/common/exceptions"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/aztec"
	"github.com/makiuchi-d/gozxing/common"
	"github.com/makiuchi-d/gozxing/datamatrix"
	multidetector "github.com/makiuchi-d/gozxing/multi/qrcode/detector"
	"github.com/makiuchi-d/gozxing/oned"
	"github.com/makiuchi-d/gozxing/oned/rss"
	qrdecoder "github.com/makiuchi-d/gozxing/qrcode/decoder"
	qrdetector "github.com/makiuchi-d/gozxing/qrcode/detector"
)

// Barcode is a detected code, as the barcodes call reports one.
type Barcode struct {
	Box
	Payload   string `json:"payload"`
	Symbology string `json:"symbology"`
}

// symbologies are Vision's names for gozxing's formats: VNBarcodeSymbology without its prefix.
var symbologies = map[gozxing.BarcodeFormat]string{
	gozxing.BarcodeFormat_AZTEC:       "Aztec",
	gozxing.BarcodeFormat_CODABAR:     "Codabar",
	gozxing.BarcodeFormat_CODE_39:     "Code39",
	gozxing.BarcodeFormat_CODE_93:     "Code93",
	gozxing.BarcodeFormat_CODE_128:    "Code128",
	gozxing.BarcodeFormat_DATA_MATRIX: "DataMatrix",
	gozxing.BarcodeFormat_EAN_8:       "EAN8",
	gozxing.BarcodeFormat_EAN_13:      "EAN13",
	gozxing.BarcodeFormat_QR_CODE:     "QR",
	gozxing.BarcodeFormat_RSS_14:      "GS1DataBar",
	gozxing.BarcodeFormat_UPC_E:       "UPCE",
}

func symbologyOf(format gozxing.BarcodeFormat, payload string) string {
	if format == gozxing.BarcodeFormat_ITF {
		if len(payload) == 14 {
			return "ITF14"
		}
		return "I2of5"
	}
	return symbologies[format]
}

// ReadBarcodes detects barcodes with gozxing, as SWDetectBarcodes does with Vision: every QR code,
// a Data Matrix and an Aztec code, and every one-dimensional code along the rows or the columns.
// Quads of one-dimensional codes are upright rectangles.
func ReadBarcodes(ctx context.Context, img Image) ([]Barcode, error) {
	s := img.space()
	codes := []Barcode{}
	if s.width == 0 || s.height == 0 {
		return codes, nil
	}
	gray := luminance(img.Pixels)
	bitmap, err := binarized(gray, s.width, s.height)
	if err != nil {
		return nil, E.Cause(err, "graphics: binarize the image")
	}
	hints := map[gozxing.DecodeHintType]any{gozxing.DecodeHintType_TRY_HARDER: true}
	place := func(quad [4][2]float64, format gozxing.BarcodeFormat, payload string) {
		for index, corner := range quad {
			quad[index] = s.point(corner[0], corner[1])
		}
		codes = append(codes, Barcode{Box: boxOf(quad), Payload: payload, Symbology: symbologyOf(format, payload)})
	}
	if matrix, err := bitmap.GetBlackMatrix(); err == nil {
		for _, code := range readQR(matrix, hints) {
			place(code.quad, gozxing.BarcodeFormat_QR_CODE, code.payload)
		}
	}
	// Data Matrix lists its corners top left, bottom left, bottom right, top right; Aztec top right,
	// bottom right, bottom left, top left.
	if result, err := datamatrix.NewDataMatrixReader().Decode(bitmap, hints); err == nil && len(result.GetResultPoints()) == 4 {
		p := result.GetResultPoints()
		place([4][2]float64{xy(p[0]), xy(p[3]), xy(p[2]), xy(p[1])}, gozxing.BarcodeFormat_DATA_MATRIX, result.GetText())
	}
	if result, err := aztec.NewAztecReader().Decode(bitmap, hints); err == nil && len(result.GetResultPoints()) == 4 {
		p := result.GetResultPoints()
		place([4][2]float64{xy(p[3]), xy(p[0]), xy(p[1]), xy(p[2])}, gozxing.BarcodeFormat_AZTEC, result.GetText())
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := scanLinear(ctx, bitmap, s.width, s.height)
	if err != nil {
		return nil, err
	}
	for _, code := range rows {
		place(rectQuad(float64(code.left), float64(code.top), float64(code.right+1), float64(code.bottom+1)), code.format, code.text)
	}
	turned, err := binarized(transposed(gray, s.width, s.height), s.height, s.width)
	if err != nil {
		return nil, E.Cause(err, "graphics: binarize the image")
	}
	columns, err := scanLinear(ctx, turned, s.height, s.width)
	if err != nil {
		return nil, err
	}
	for _, code := range columns {
		place(rectQuad(float64(code.top), float64(code.left), float64(code.bottom+1), float64(code.right+1)), code.format, code.text)
	}
	slices.SortStableFunc(codes, func(a, b Barcode) int {
		return cmp.Or(cmp.Compare(a.Frame[1], b.Frame[1]), cmp.Compare(a.Frame[0], b.Frame[0]))
	})
	return codes, nil
}

func xy(point gozxing.ResultPoint) [2]float64 {
	return [2]float64{point.GetX(), point.GetY()}
}

// luminance is the gray plane gozxing's image source makes, (r + 2g + b) / 4 over white, without
// an interface call per pixel.
func luminance(pixels image.Image) []byte {
	bounds := pixels.Bounds()
	rgba, isRGBA := pixels.(*image.RGBA)
	if !isRGBA {
		rgba = image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
		draw.Draw(rgba, rgba.Bounds(), pixels, bounds.Min, draw.Src)
		bounds = rgba.Bounds()
	}
	gray := make([]byte, 0, bounds.Dx()*bounds.Dy())
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		row := rgba.Pix[rgba.PixOffset(bounds.Min.X, y):][:bounds.Dx()*4]
		for x := 0; x < len(row); x += 4 {
			white := 255 - int(row[x+3])
			gray = append(gray, byte((int(row[x])+2*int(row[x+1])+int(row[x+2])+4*white)/4))
		}
	}
	return gray
}

func transposed(gray []byte, width, height int) []byte {
	out := make([]byte, len(gray))
	for y := range height {
		for x := range width {
			out[x*height+y] = gray[y*width+x]
		}
	}
	return out
}

// binarized wraps a gray plane as the luma plane of a YUV source, which gozxing reads as is.
func binarized(gray []byte, width, height int) (*gozxing.BinaryBitmap, error) {
	source, err := gozxing.NewPlanarYUVLuminanceSource(gray, width, height, 0, 0, width, height, false)
	if err != nil {
		return nil, err
	}
	return gozxing.NewBinaryBitmap(gozxing.NewHybridBinarizer(source))
}

type qrCode struct {
	quad    [4][2]float64
	payload string
}

// readQR decodes every QR code the multiple-code detector finds, else the one the single-code
// detector finds. It runs the detectors itself because only their results carry the dimension that
// places the corners.
func readQR(matrix *gozxing.BitMatrix, hints map[gozxing.DecodeHintType]any) []qrCode {
	found, _ := multidetector.NewMultiDetector(matrix).DetectMulti(hints)
	codes := decodeQR(found, hints)
	if len(codes) == 0 {
		if single, err := qrdetector.NewDetector(matrix).Detect(hints); err == nil {
			codes = decodeQR([]*common.DetectorResult{single}, hints)
		}
	}
	return codes
}

func decodeQR(found []*common.DetectorResult, hints map[gozxing.DecodeHintType]any) []qrCode {
	var codes []qrCode
	decoder := qrdecoder.NewDecoder()
	for _, result := range found {
		decoded, err := decoder.Decode(result.GetBits(), hints)
		points := result.GetPoints()
		if err != nil || len(points) < 3 {
			continue
		}
		if metadata, isQR := decoded.GetOther().(*qrdecoder.QRCodeDecoderMetaData); isQR {
			metadata.ApplyMirroredCorrection(points)
		}
		codes = append(codes, qrCode{quad: qrQuad(points, result.GetBits().GetWidth()), payload: decoded.GetText()})
	}
	return codes
}

// qrQuad finds a QR code's corners from the centres of its finder patterns, bottom left, top left and
// top right, which sit 3.5 modules inside the corners and dimension - 7 modules apart.
func qrQuad(points []gozxing.ResultPoint, dimension int) [4][2]float64 {
	bottomLeft, topLeft, topRight := xy(points[0]), xy(points[1]), xy(points[2])
	span := float64(dimension - 7)
	var right, down [2]float64
	for axis := range 2 {
		right[axis] = (topRight[axis] - topLeft[axis]) / span * 3.5
		down[axis] = (bottomLeft[axis] - topLeft[axis]) / span * 3.5
	}
	var quad [4][2]float64
	for axis := range 2 {
		quad[0][axis] = topLeft[axis] - right[axis] - down[axis]
		quad[1][axis] = topRight[axis] + right[axis] - down[axis]
		quad[2][axis] = topRight[axis] + bottomLeft[axis] - topLeft[axis] + right[axis] + down[axis]
		quad[3][axis] = bottomLeft[axis] - right[axis] + down[axis]
	}
	return quad
}

// linearCode is a one-dimensional code found along rows: it spans pixels left to right of rows top to
// bottom, read on rows of them.
type linearCode struct {
	format      gozxing.BarcodeFormat
	text        string
	left, right int
	top, bottom int
	rows        int
}

func (c linearCode) matches(other linearCode) bool {
	return c.format == other.format && c.text == other.text && c.left <= other.right && other.left <= c.right
}

// scanLinear reads one-dimensional codes on every few rows, both ways and repeatedly along each row,
// gathers the rows reading the same code into one, and walks its top and bottom out to the rows
// between the scanned ones. A code read on one row only is dropped as noise.
func scanLinear(ctx context.Context, bitmap *gozxing.BinaryBitmap, width, height int) ([]linearCode, error) {
	readers := []oned.RowDecoder{
		oned.NewMultiFormatUPCEANReader(nil).(oned.RowDecoder),
		oned.NewCode128Reader().(oned.RowDecoder),
		oned.NewCode39Reader().(oned.RowDecoder),
		oned.NewCode93Reader().(oned.RowDecoder),
		oned.NewCodaBarReader().(oned.RowDecoder),
		oned.NewITFReader().(oned.RowDecoder),
		rss.NewRSS14Reader().(oned.RowDecoder),
	}
	row := gozxing.NewBitArray(width)
	scan := gozxing.NewBitArray(width)
	read := func(y int) []linearCode {
		var found []linearCode
		if _, err := bitmap.GetBlackRow(y, row); err != nil {
			return nil
		}
		for _, reader := range readers {
			for _, reversed := range []bool{false, true} {
				copy(scan.GetBitArray(), row.GetBitArray())
				if reversed {
					scan.Reverse()
				}
				for last := -1; ; {
					result, err := reader.DecodeRow(y, scan, nil)
					if err != nil || len(result.GetResultPoints()) < 2 {
						break
					}
					left, right := width, -1
					for _, point := range result.GetResultPoints() {
						left, right = min(left, int(point.GetX())), max(right, int(point.GetX()))
					}
					if right <= last {
						break
					}
					left, right = edges(scan, max(left, 0), min(right, width-1))
					for x := 0; x <= right; x++ {
						if scan.Get(x) {
							scan.Flip(x)
						}
					}
					last = right
					if reversed {
						left, right = width-1-right, width-1-left
					}
					found = append(found, linearCode{format: result.GetBarcodeFormat(), text: result.GetText(), left: left, right: right, top: y, bottom: y, rows: 1})
				}
			}
		}
		return found
	}
	reads := func(y int, code linearCode) bool {
		return slices.ContainsFunc(read(y), code.matches)
	}
	step := max(1, height>>8)
	var codes []linearCode
	for y := 0; y < height; y += step {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, found := range read(y) {
			index := slices.IndexFunc(codes, func(code linearCode) bool {
				return code.matches(found) && y-code.bottom <= 2*step
			})
			if index < 0 {
				codes = append(codes, found)
				continue
			}
			code := &codes[index]
			code.left, code.right = min(code.left, found.left), max(code.right, found.right)
			if code.bottom != y {
				code.bottom = y
				code.rows++
			}
		}
	}
	codes = slices.DeleteFunc(codes, func(code linearCode) bool { return code.rows < 2 })
	for index := range codes {
		code := &codes[index]
		for y := code.top - 1; y > code.top-step && y >= 0 && reads(y, *code); y-- {
			code.top = y
		}
		for y := code.bottom + 1; y < code.bottom+step && y < height && reads(y, *code); y++ {
			code.bottom = y
		}
	}
	return codes, nil
}

// edges widens [left, right], where a reader places a code (the middle of its guard patterns, or
// their inner edges), to its outermost bars: a white run wider than any inside the code is its quiet
// zone.
func edges(row *gozxing.BitArray, left, right int) (int, int) {
	widest, run := 0, 0
	for x := left; x <= right; x++ {
		if row.Get(x) {
			run = 0
		} else {
			run++
			widest = max(widest, run)
		}
	}
	for x, run := left-1, 0; x >= 0 && run <= widest; x-- {
		if row.Get(x) {
			left, run = x, 0
		} else {
			run++
		}
	}
	for x, run := right+1, 0; x < row.GetSize() && run <= widest; x++ {
		if row.Get(x) {
			right, run = x, 0
		} else {
			run++
		}
	}
	return left, right
}
