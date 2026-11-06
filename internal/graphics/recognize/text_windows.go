//go:build windows

package recognize

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unsafe"

	E "github.com/sagernet/sing/common/exceptions"

	"golang.org/x/image/draw"
)

// glyphPixels is the height SWRecognizeText samples text of minimumHeight at.
const glyphPixels = 24

// Windows reads no text that touches the bitmap's edges, so the region sits in a margin of this many
// pixels, a quarter of its shorter side within the bounds.
const (
	minimumMargin = 8
	maximumMargin = 64
)

const (
	bitmapPixelFormatBgra8       = 87
	bitmapAlphaModePremultiplied = 0
)

// Languages lists the tags of the OCR languages installed in Windows.
func Languages() ([]string, error) {
	leave, err := enter()
	if err != nil {
		return nil, err
	}
	defer leave()
	statics, err := activate("Windows.Media.Ocr.OcrEngine", &iidOcrEngineStatics)
	if err != nil {
		return nil, E.Cause(err, "graphics: load the OCR engine")
	}
	defer statics.release()
	return languages(statics)
}

// ReadText recognizes text with Windows.Media.Ocr, as SWRecognizeText does with Vision.
func ReadText(ctx context.Context, img Image, options TextOptions) (*Text, error) {
	s := img.space()
	if s.width == 0 || s.height == 0 {
		return &Text{Lines: []Line{}}, nil
	}
	region := image.Rect(0, 0, s.width, s.height)
	if options.Region != nil {
		var err error
		region, err = s.pixels(*options.Region)
		if err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	leave, err := enter()
	if err != nil {
		return nil, err
	}
	defer leave()
	statics, err := activate("Windows.Media.Ocr.OcrEngine", &iidOcrEngineStatics)
	if err != nil {
		return nil, E.Cause(err, "graphics: load the OCR engine")
	}
	defer statics.release()
	engine, err := newEngine(statics, options.Languages)
	if err != nil {
		return nil, err
	}
	defer engine.release()
	var limit uint32
	// IOcrEngineStatics::get_MaxImageDimension
	if err := statics.call(6, uintptr(unsafe.Pointer(&limit))); err != nil {
		return nil, E.Cause(err, "graphics: read the OCR size limit")
	}
	width, height := sampled(region.Dx(), region.Dy(), s.scale, options.MinimumHeight, int(limit)-2*maximumMargin)
	margin := min(max(min(width, height)/4, minimumMargin), maximumMargin)
	bitmap, err := newBitmap(img.Pixels, region, width, height, margin)
	if err != nil {
		return nil, E.Cause(err, "graphics: copy the pixels for OCR")
	}
	defer bitmap.release()
	result, err := recognize(ctx, engine, bitmap)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, E.Cause(err, "graphics: run OcrEngine")
	}
	defer result.release()
	angle, lines, err := readResult(result)
	if err != nil {
		return nil, E.Cause(err, "graphics: read the OCR result")
	}
	// OcrResult boxes words in the bitmap turned upright, by TextAngle clockwise around its centre.
	sin, cos := math.Sincos(angle * math.Pi / 180)
	centerX, centerY := float64(width)/2+float64(margin), float64(height)/2+float64(margin)
	unitX, unitY := float64(region.Dx())/float64(width), float64(region.Dy())/float64(height)
	text := &Text{Lines: []Line{}, place: func(x, y float64) [2]float64 {
		x, y = (x-centerX)*cos-(y-centerY)*sin+centerX, (x-centerX)*sin+(y-centerY)*cos+centerY
		return s.point(float64(region.Min.X)+(x-float64(margin))*unitX, float64(region.Min.Y)+(y-float64(margin))*unitY)
	}}
	for _, line := range lines {
		left, top := math.Inf(1), math.Inf(1)
		right, bottom := math.Inf(-1), math.Inf(-1)
		for _, w := range line.words {
			left, top = math.Min(left, w.rect[0]), math.Min(top, w.rect[1])
			right, bottom = math.Max(right, w.rect[0]+w.rect[2]), math.Max(bottom, w.rect[1]+w.rect[3])
		}
		box := text.box(left, top, right, bottom)
		if math.Hypot(box.Quad[3][0]-box.Quad[0][0], box.Quad[3][1]-box.Quad[0][1]) < options.MinimumHeight {
			continue
		}
		// Windows reads one candidate per line and gives no confidence.
		candidate := Candidate{Text: line.text, Confidence: 1}
		text.Lines = append(text.Lines, Line{Box: box, Candidate: candidate, Candidates: []Candidate{candidate}})
		text.words = append(text.words, slices.DeleteFunc(line.words, func(w word) bool { return w.start < 0 }))
	}
	return text, nil
}

// pixels covers rect, in points, with whole pixels, as -[SWImageNode pixelRectInSpace:into:error:].
func (s space) pixels(rect [4]float64) (image.Rectangle, error) {
	unitX, unitY := float64(s.width)/s.frame[2], float64(s.height)/s.frame[3]
	left := math.Floor(math.Max((rect[0]-s.frame[0])*unitX, 0) + 1e-6)
	top := math.Floor(math.Max((rect[1]-s.frame[1])*unitY, 0) + 1e-6)
	right := math.Ceil(math.Min((rect[0]+rect[2]-s.frame[0])*unitX, float64(s.width)) - 1e-6)
	bottom := math.Ceil(math.Min((rect[1]+rect[3]-s.frame[1])*unitY, float64(s.height)) - 1e-6)
	if !(right-left >= 1 && bottom-top >= 1) {
		return image.Rectangle{}, newError("RangeError", "[%s] does not overlap the image [%s]", describe(rect), describe(s.frame))
	}
	return image.Rect(int(left), int(top), int(right), int(bottom)), nil
}

func describe(rect [4]float64) string {
	parts := make([]string, len(rect))
	for index, value := range rect {
		parts[index] = strconv.FormatFloat(rounded(value), 'g', 6, 64)
	}
	return strings.Join(parts, ", ")
}

func newError(name string, format string, args ...any) *Error {
	return &Error{Name: name, Message: fmt.Sprintf(format, args...)}
}

// sampled sizes a region for the recognizer: text of minimumHeight at glyphPixels, as SWRecognizeText
// resamples for Vision, within limit, OcrEngine.MaxImageDimension.
func sampled(width, height int, scale, minimumHeight float64, limit int) (int, int) {
	w, h := float64(width), float64(height)
	if minimumHeight > 0 {
		if density := glyphPixels / minimumHeight; density < scale {
			w, h = math.Max(math.Floor(w*density/scale), 1), math.Max(math.Floor(h*density/scale), 1)
		}
	}
	if longest := math.Max(w, h); limit > 0 && longest > float64(limit) {
		w, h = math.Max(math.Floor(w*float64(limit)/longest), 1), math.Max(math.Floor(h*float64(limit)/longest), 1)
	}
	return int(w), int(h)
}

// newEngine creates an OcrEngine for the first of preferred that Windows reads, or without
// preferences for the user's profile languages, as SWDeviceLanguages picks the device's. Windows
// recognizes one language at a time.
func newEngine(statics *object, preferred []string) (*object, error) {
	if len(preferred) == 0 {
		var engine *object
		// IOcrEngineStatics::TryCreateFromUserProfileLanguages
		if err := statics.call(10, uintptr(unsafe.Pointer(&engine))); err != nil {
			return nil, E.Cause(err, "graphics: create the OCR engine")
		}
		if engine != nil {
			return engine, nil
		}
	}
	available, err := languages(statics)
	if err != nil {
		return nil, err
	}
	wanted := preferred
	if len(wanted) == 0 {
		wanted = append([]string{"en-US"}, available...)
	}
	for _, language := range wanted {
		if tag := matchLanguage(language, available); tag != "" {
			return engineFor(statics, tag)
		}
	}
	if len(available) == 0 {
		return nil, newError("NotSupportedError", "graphics: Windows has no OCR language installed")
	}
	return nil, newError("NotSupportedError", "graphics: Windows has no OCR language for %s, only %s", strings.Join(preferred, ", "), strings.Join(available, ", "))
}

func engineFor(statics *object, tag string) (*object, error) {
	factory, err := activate("Windows.Globalization.Language", &iidLanguageFactory)
	if err != nil {
		return nil, E.Cause(err, "graphics: create the OCR engine")
	}
	defer factory.release()
	name, err := newHString(tag)
	if err != nil {
		return nil, E.Cause(err, "graphics: create the OCR engine")
	}
	defer name.delete()
	var language *object
	// ILanguageFactory::CreateLanguage
	if err := factory.call(6, uintptr(name), uintptr(unsafe.Pointer(&language))); err != nil {
		return nil, E.Cause(err, "graphics: create the language ", tag)
	}
	defer language.release()
	var engine *object
	// IOcrEngineStatics::TryCreateFromLanguage
	if err := statics.call(9, uintptr(unsafe.Pointer(language)), uintptr(unsafe.Pointer(&engine))); err != nil {
		return nil, E.Cause(err, "graphics: create the OCR engine for ", tag)
	}
	if engine == nil {
		return nil, newError("NotSupportedError", "graphics: Windows cannot recognize %s", tag)
	}
	return engine, nil
}

func languages(statics *object) ([]string, error) {
	var vector *object
	// IOcrEngineStatics::get_AvailableRecognizerLanguages
	if err := statics.call(7, uintptr(unsafe.Pointer(&vector))); err != nil {
		return nil, E.Cause(err, "graphics: list the OCR languages")
	}
	defer vector.release()
	count, err := vector.size()
	if err != nil {
		return nil, E.Cause(err, "graphics: list the OCR languages")
	}
	tags := make([]string, 0, count)
	for index := range count {
		language, err := vector.at(index)
		if err != nil {
			return nil, E.Cause(err, "graphics: list the OCR languages")
		}
		// ILanguage::get_LanguageTag
		tag, err := language.text(6)
		language.release()
		if err != nil {
			return nil, E.Cause(err, "graphics: list the OCR languages")
		}
		tags = append(tags, tag)
	}
	return tags, nil
}

// matchLanguage finds preferred among available tags, else a tag of its language and script, else
// one of its language, as SWMatchLanguage.
func matchLanguage(preferred string, available []string) string {
	for _, tag := range available {
		if strings.EqualFold(tag, preferred) {
			return tag
		}
	}
	language, script := subtags(preferred)
	fallback := ""
	for _, tag := range available {
		candidateLanguage, candidateScript := subtags(tag)
		if candidateLanguage != language {
			continue
		}
		if candidateScript == script {
			return tag
		}
		if fallback == "" {
			fallback = tag
		}
	}
	return fallback
}

// subtags reads the language and script of a BCP 47 tag, as NSLocale's languageCode and scriptCode.
func subtags(tag string) (language, script string) {
	parts := strings.FieldsFunc(strings.ToLower(tag), func(r rune) bool { return r == '-' || r == '_' })
	if len(parts) == 0 {
		return "", ""
	}
	if len(parts) > 1 && len(parts[1]) == 4 {
		script = parts[1]
	}
	return parts[0], script
}

// newBitmap copies a region of pixels, scaled to width x height inside a margin of its border's mean
// colour, into a SoftwareBitmap of premultiplied Bgra8 through a Windows.Storage.Streams.Buffer.
func newBitmap(pixels image.Image, region image.Rectangle, width, height, margin int) (*object, error) {
	canvas := image.NewRGBA(image.Rect(0, 0, width+2*margin, height+2*margin))
	inside := image.Rect(margin, margin, margin+width, margin+height)
	source := region.Add(pixels.Bounds().Min)
	if source.Dx() == width && source.Dy() == height {
		draw.Draw(canvas, inside, pixels, source.Min, draw.Src)
	} else {
		draw.BiLinear.Scale(canvas, inside, pixels, source, draw.Src, nil)
	}
	var sum [4]int
	border := 0
	for y := inside.Min.Y; y < inside.Max.Y; y++ {
		step := 1
		if y != inside.Min.Y && y != inside.Max.Y-1 {
			step = max(width-1, 1)
		}
		for x := inside.Min.X; x < inside.Max.X; x += step {
			for channel, value := range canvas.Pix[canvas.PixOffset(x, y):][:4] {
				sum[channel] += int(value)
			}
			border++
		}
	}
	background := image.NewUniform(color.RGBA{uint8(sum[0] / border), uint8(sum[1] / border), uint8(sum[2] / border), uint8(sum[3] / border)})
	for _, strip := range []image.Rectangle{
		image.Rect(0, 0, canvas.Rect.Dx(), margin),
		image.Rect(0, inside.Max.Y, canvas.Rect.Dx(), canvas.Rect.Dy()),
		image.Rect(0, margin, margin, inside.Max.Y),
		image.Rect(inside.Max.X, margin, canvas.Rect.Dx(), inside.Max.Y),
	} {
		draw.Draw(canvas, strip, background, image.Point{}, draw.Src)
	}
	for index := 0; index < len(canvas.Pix); index += 4 {
		canvas.Pix[index], canvas.Pix[index+2] = canvas.Pix[index+2], canvas.Pix[index]
	}
	factory, err := activate("Windows.Storage.Streams.Buffer", &iidBufferFactory)
	if err != nil {
		return nil, err
	}
	defer factory.release()
	var buffer *object
	// IBufferFactory::Create
	if err := factory.call(6, uintptr(len(canvas.Pix)), uintptr(unsafe.Pointer(&buffer))); err != nil {
		return nil, err
	}
	defer buffer.release()
	// IBuffer::put_Length
	if err := buffer.call(8, uintptr(len(canvas.Pix))); err != nil {
		return nil, err
	}
	access, err := buffer.query(&iidBufferByteAccess)
	if err != nil {
		return nil, err
	}
	defer access.release()
	var data *byte
	// IBufferByteAccess::Buffer, the only method after IUnknown's
	if err := access.call(3, uintptr(unsafe.Pointer(&data))); err != nil {
		return nil, err
	}
	copy(unsafe.Slice(data, len(canvas.Pix)), canvas.Pix)
	statics, err := activate("Windows.Graphics.Imaging.SoftwareBitmap", &iidSoftwareBitmapStatics)
	if err != nil {
		return nil, err
	}
	defer statics.release()
	var bitmap *object
	// ISoftwareBitmapStatics::CreateCopyWithAlphaFromBuffer
	err = statics.call(10, uintptr(unsafe.Pointer(buffer)), bitmapPixelFormatBgra8, uintptr(canvas.Rect.Dx()), uintptr(canvas.Rect.Dy()), bitmapAlphaModePremultiplied, uintptr(unsafe.Pointer(&bitmap)))
	return bitmap, err
}

func recognize(ctx context.Context, engine *object, bitmap *object) (*object, error) {
	var operation *object
	// IOcrEngine::RecognizeAsync
	if err := engine.call(6, uintptr(unsafe.Pointer(bitmap)), uintptr(unsafe.Pointer(&operation))); err != nil {
		return nil, err
	}
	defer operation.release()
	if err := wait(ctx, operation); err != nil {
		return nil, err
	}
	var result *object
	// IAsyncOperation<OcrResult>::GetResults
	return result, operation.call(8, uintptr(unsafe.Pointer(&result)))
}

type ocrLine struct {
	text  string
	words []word
}

// readResult reads an OcrResult: its text angle, in degrees clockwise, and its lines with words.
func readResult(result *object) (float64, []ocrLine, error) {
	var reference *object
	// IOcrResult::get_TextAngle, an IReference<double> that is null without text
	if err := result.call(7, uintptr(unsafe.Pointer(&reference))); err != nil {
		return 0, nil, err
	}
	var angle float64
	if reference != nil {
		// IReference<double>::get_Value
		err := reference.call(6, uintptr(unsafe.Pointer(&angle)))
		reference.release()
		if err != nil {
			return 0, nil, err
		}
	}
	var vector *object
	// IOcrResult::get_Lines
	if err := result.call(6, uintptr(unsafe.Pointer(&vector))); err != nil {
		return 0, nil, err
	}
	defer vector.release()
	count, err := vector.size()
	if err != nil {
		return 0, nil, err
	}
	lines := make([]ocrLine, 0, count)
	for index := range count {
		item, err := vector.at(index)
		if err != nil {
			return 0, nil, err
		}
		line, err := readLine(item)
		item.release()
		if err != nil {
			return 0, nil, err
		}
		if len(line.words) > 0 {
			lines = append(lines, line)
		}
	}
	return angle, lines, nil
}

// readLine reads a line's text and words, finding each word in the text to learn its UTF-16 range;
// a word it cannot find keeps a negative start and only shapes the line's box.
func readLine(item *object) (ocrLine, error) {
	// IOcrLine::get_Text
	content, err := item.text(7)
	if err != nil {
		return ocrLine{}, err
	}
	var vector *object
	// IOcrLine::get_Words
	if err := item.call(6, uintptr(unsafe.Pointer(&vector))); err != nil {
		return ocrLine{}, err
	}
	defer vector.release()
	count, err := vector.size()
	if err != nil {
		return ocrLine{}, err
	}
	units := utf16.Encode([]rune(content))
	cursor := 0
	line := ocrLine{text: content, words: make([]word, 0, count)}
	for index := range count {
		element, err := vector.at(index)
		if err != nil {
			return ocrLine{}, err
		}
		label, rect, err := readWord(element)
		element.release()
		if err != nil {
			return ocrLine{}, err
		}
		w := word{start: -1, rect: [4]float64{float64(rect.X), float64(rect.Y), float64(rect.Width), float64(rect.Height)}}
		labelUnits := utf16.Encode([]rune(label))
		if offset := indexUnits(units[cursor:], labelUnits); offset >= 0 && len(labelUnits) > 0 {
			w.start = cursor + offset
			w.end = w.start + len(labelUnits)
			cursor = w.end
		}
		line.words = append(line.words, w)
	}
	return line, nil
}

// rect is a Windows.Foundation.Rect.
type rect struct {
	X, Y, Width, Height float32
}

func readWord(item *object) (string, rect, error) {
	var bounds rect
	// IOcrWord::get_BoundingRect
	if err := item.call(6, uintptr(unsafe.Pointer(&bounds))); err != nil {
		return "", rect{}, err
	}
	// IOcrWord::get_Text
	label, err := item.text(7)
	return label, bounds, err
}

func indexUnits(units, sub []uint16) int {
	for index := 0; index+len(sub) <= len(units); index++ {
		if slices.Equal(units[index:index+len(sub)], sub) {
			return index
		}
	}
	return -1
}
