// Package recognize reads text and barcodes where Vision is missing, in the shapes the text and
// barcodes calls return on macOS (SWRecognition.m).
package recognize

import (
	"image"
	"math"
	"unicode/utf16"
)

// Image is pixels placed in a space of points, as an image node places its raster.
type Image struct {
	Pixels image.Image
	// Scale is pixels per point; zero takes it from Frame.
	Scale float64
	// Frame is where Pixels lie, in points; zero places them at the origin at Scale.
	Frame [4]float64
}

type space struct {
	frame         [4]float64
	scale         float64
	width, height int
}

func (i Image) space() space {
	bounds := i.Pixels.Bounds()
	s := space{frame: i.Frame, scale: i.Scale, width: bounds.Dx(), height: bounds.Dy()}
	width, height := float64(s.width), float64(s.height)
	if s.frame[2] <= 0 || s.frame[3] <= 0 {
		if s.scale <= 0 {
			s.scale = 1
		}
		s.frame = [4]float64{0, 0, width / s.scale, height / s.scale}
	} else if s.scale <= 0 {
		s.scale = math.Sqrt(width * height / (s.frame[2] * s.frame[3]))
	}
	return s
}

// point places a position in pixels, from the top left of the pixels, in points.
func (s space) point(x, y float64) [2]float64 {
	return [2]float64{s.frame[0] + x*s.frame[2]/float64(s.width), s.frame[1] + y*s.frame[3]/float64(s.height)}
}

// rounded keeps two decimals and zeroes what JSON cannot carry, as SWRounded.
func rounded(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return math.Round(value*100) / 100
}

// Error is a failure a script sees as a DOMException named Name, as SWMakeError makes.
type Error struct {
	Name    string
	Message string
}

func (e *Error) Error() string {
	return e.Message
}

// Box is a quadrilateral in points and its bounds, as SWShape: the quad runs top left, top right,
// bottom right, bottom left.
type Box struct {
	Quad   [4][2]float64 `json:"quad"`
	Frame  [4]float64    `json:"frame"`
	Center [2]float64    `json:"center"`
}

func boxOf(quad [4][2]float64) Box {
	var box Box
	minX, minY, maxX, maxY := quad[0][0], quad[0][1], quad[0][0], quad[0][1]
	for index, corner := range quad {
		minX, minY = math.Min(minX, corner[0]), math.Min(minY, corner[1])
		maxX, maxY = math.Max(maxX, corner[0]), math.Max(maxY, corner[1])
		box.Quad[index] = [2]float64{rounded(corner[0]), rounded(corner[1])}
	}
	box.Frame = [4]float64{rounded(minX), rounded(minY), rounded(maxX - minX), rounded(maxY - minY)}
	box.Center = [2]float64{rounded((minX + maxX) / 2), rounded((minY + maxY) / 2)}
	return box
}

func rectQuad(left, top, right, bottom float64) [4][2]float64 {
	return [4][2]float64{{left, top}, {right, top}, {right, bottom}, {left, bottom}}
}

// TextOptions are the options of the text call that Windows' recognizer honours. It has no equivalent
// of detectLanguage, level, words or correction.
type TextOptions struct {
	Region        *[4]float64 `json:"region,omitempty"`
	Languages     []string    `json:"languages,omitempty"`
	MinimumHeight float64     `json:"minimumHeight,omitempty"`
}

// Candidate is one reading of a line.
type Candidate struct {
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`
}

// Line is a recognized line: its box, its best reading and every reading.
type Line struct {
	Box
	Candidate
	Candidates []Candidate `json:"candidates"`
}

// Text is a recognition: Lines marshals to the lines of the text call, and BoundingBox serves the
// text handle.
type Text struct {
	Lines []Line `json:"lines"`
	words [][]word
	// place maps the recognizer's space, where words are upright rects, to points.
	place func(x, y float64) [2]float64
}

// word is a recognized word: its UTF-16 range in its line's text, and its rect in the recognizer's
// space.
type word struct {
	start, end int
	rect       [4]float64
}

func (t *Text) box(left, top, right, bottom float64) Box {
	quad := rectQuad(left, top, right, bottom)
	for index, corner := range quad {
		quad[index] = t.place(corner[0], corner[1])
	}
	return boxOf(quad)
}

// BoundingBox boxes the UTF-16 range [start, end) of a line's candidate, as the text handle's
// boundingBox; nil stands for null. The recognizer boxes words, so a range inside a word takes its
// share of the word's width.
func (t *Text) BoundingBox(line, candidate, start, end int) *Box {
	if line < 0 || line >= len(t.Lines) || candidate < 0 || candidate >= len(t.Lines[line].Candidates) {
		return nil
	}
	if start < 0 || start > end || end > utf16Length(t.Lines[line].Candidates[candidate].Text) {
		return nil
	}
	left, top, right, bottom := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, w := range t.words[line] {
		from, to := max(start, w.start), min(end, w.end)
		if from >= to {
			continue
		}
		length := float64(w.end - w.start)
		left = math.Min(left, w.rect[0]+w.rect[2]*float64(from-w.start)/length)
		right = math.Max(right, w.rect[0]+w.rect[2]*float64(to-w.start)/length)
		top, bottom = math.Min(top, w.rect[1]), math.Max(bottom, w.rect[1]+w.rect[3])
	}
	if left > right {
		return nil
	}
	box := t.box(left, top, right, bottom)
	return &box
}

func utf16Length(text string) int {
	length := 0
	for _, r := range text {
		length += utf16.RuneLen(r)
	}
	return length
}
