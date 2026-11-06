//go:build !darwin || !cgo

package graphics

import (
	"bufio"
	"compress/lzw"
	"image"
	"image/color"
	"io"
	"slices"
)

// gifWriter writes an endlessly looping GIF a frame at a time, so a long export never holds
// all its frames (ImageIO's CGImageDestination in SWVideoExport.m).
type gifWriter struct {
	output *bufio.Writer
	opened bool
}

func newGIFWriter(output io.Writer) *gifWriter {
	return &gifWriter{output: bufio.NewWriter(output)}
}

func (g *gifWriter) u16(value int) {
	g.output.WriteByte(byte(value))
	g.output.WriteByte(byte(value >> 8))
}

// frame adds a frame shown for delay hundredths of a second; the first sets the canvas size.
func (g *gifWriter) frame(pixels *image.NRGBA, delay int) error {
	bounds := pixels.Bounds()
	width, height := min(bounds.Dx(), 0xffff), min(bounds.Dy(), 0xffff)
	if !g.opened {
		g.opened = true
		g.output.WriteString("GIF89a")
		g.u16(width)
		g.u16(height)
		g.output.Write([]byte{0, 0, 0})
		g.output.Write([]byte{0x21, 0xff, 0x0b})
		g.output.WriteString("NETSCAPE2.0")
		g.output.Write([]byte{3, 1, 0, 0, 0})
	}
	palette, indexes := quantize(pixels)
	bits := 1
	for 1<<bits < len(palette) {
		bits++
	}
	g.output.Write([]byte{0x21, 0xf9, 4, 0})
	g.u16(min(max(delay, 0), 0xffff))
	g.output.Write([]byte{0, 0})
	g.output.WriteByte(0x2c)
	g.u16(0)
	g.u16(0)
	g.u16(width)
	g.u16(height)
	g.output.WriteByte(0x80 | byte(bits-1))
	for index := range 1 << bits {
		var entry color.NRGBA
		if index < len(palette) {
			entry = palette[index]
		}
		g.output.Write([]byte{entry.R, entry.G, entry.B})
	}
	literal := max(bits, 2)
	g.output.WriteByte(byte(literal))
	blocks := &subBlocks{output: g.output}
	compressor := lzw.NewWriter(blocks, lzw.LSB, literal)
	for y := range height {
		if _, err := compressor.Write(indexes[y*bounds.Dx() : y*bounds.Dx()+width]); err != nil {
			return err
		}
	}
	if err := compressor.Close(); err != nil {
		return err
	}
	blocks.flush()
	return g.output.WriteByte(0)
}

func (g *gifWriter) close() error {
	g.output.WriteByte(0x3b)
	return g.output.Flush()
}

// subBlocks cuts the compressed data into GIF sub-blocks of up to 255 bytes.
type subBlocks struct {
	output  *bufio.Writer
	pending []byte
}

func (b *subBlocks) Write(data []byte) (int, error) {
	b.pending = append(b.pending, data...)
	for len(b.pending) >= 255 {
		b.output.WriteByte(255)
		b.output.Write(b.pending[:255])
		b.pending = b.pending[255:]
	}
	return len(data), nil
}

func (b *subBlocks) flush() {
	if len(b.pending) > 0 {
		b.output.WriteByte(byte(len(b.pending)))
		b.output.Write(b.pending)
		b.pending = nil
	}
}

// colorBox is a box of the 5-bit color cube that median cut splits.
type colorBox struct {
	bins  []int
	count int
}

// quantize picks up to 256 colors by median cut over the colors at 5 bits a channel, and maps
// every pixel to the nearest of them.
func quantize(pixels *image.NRGBA) ([]color.NRGBA, []byte) {
	bounds := pixels.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	var counts [1 << 15]int
	var sums [1 << 15][3]int
	binOf := func(p []byte) int {
		return int(p[0]>>3)<<10 | int(p[1]>>3)<<5 | int(p[2]>>3)
	}
	for y := range height {
		row := pixels.Pix[pixels.PixOffset(bounds.Min.X, bounds.Min.Y+y):]
		for x := range width {
			p := row[x*4 : x*4+3]
			bin := binOf(p)
			counts[bin]++
			sums[bin][0] += int(p[0])
			sums[bin][1] += int(p[1])
			sums[bin][2] += int(p[2])
		}
	}
	var all []int
	for bin, count := range counts {
		if count > 0 {
			all = append(all, bin)
		}
	}
	boxes := []colorBox{{bins: all, count: width * height}}
	channel := func(bin, axis int) int {
		return bin >> (10 - 5*axis) & 31
	}
	for len(boxes) < 256 {
		chosen, axis, best := -1, 0, 0
		for index, box := range boxes {
			if len(box.bins) < 2 {
				continue
			}
			for candidate := range 3 {
				low, high := 31, 0
				for _, bin := range box.bins {
					value := channel(bin, candidate)
					low, high = min(low, value), max(high, value)
				}
				if score := (high - low) * box.count; high > low && score > best {
					chosen, axis, best = index, candidate, score
				}
			}
		}
		if chosen < 0 {
			break
		}
		box := boxes[chosen]
		slices.SortFunc(box.bins, func(a, b int) int {
			return channel(a, axis) - channel(b, axis)
		})
		half, seen, split := box.count/2, 0, 1
		for index, bin := range box.bins[:len(box.bins)-1] {
			seen += counts[bin]
			split = index + 1
			if seen >= half {
				break
			}
		}
		lower, upper := box.bins[:split], box.bins[split:]
		lowerCount := 0
		for _, bin := range lower {
			lowerCount += counts[bin]
		}
		boxes[chosen] = colorBox{bins: lower, count: lowerCount}
		boxes = append(boxes, colorBox{bins: upper, count: box.count - lowerCount})
	}
	palette := make([]color.NRGBA, len(boxes))
	for index, box := range boxes {
		var red, green, blue, count int
		for _, bin := range box.bins {
			red, green, blue, count = red+sums[bin][0], green+sums[bin][1], blue+sums[bin][2], count+counts[bin]
		}
		if count > 0 {
			palette[index] = color.NRGBA{R: uint8(red / count), G: uint8(green / count), B: uint8(blue / count), A: 255}
		}
	}
	var nearest [1 << 15]int16
	for bin := range nearest {
		nearest[bin] = -1
	}
	indexes := make([]byte, width*height)
	for y := range height {
		row := pixels.Pix[pixels.PixOffset(bounds.Min.X, bounds.Min.Y+y):]
		out := indexes[y*width : (y+1)*width]
		for x := range width {
			bin := binOf(row[x*4 : x*4+3])
			if nearest[bin] < 0 {
				r, g, b := int(row[x*4]), int(row[x*4+1]), int(row[x*4+2])
				bestIndex, bestDistance := 0, 1<<30
				for index, entry := range palette {
					dr, dg, db := r-int(entry.R), g-int(entry.G), b-int(entry.B)
					if distance := dr*dr + dg*dg + db*db; distance < bestDistance {
						bestIndex, bestDistance = index, distance
					}
				}
				nearest[bin] = int16(bestIndex)
			}
			out[x] = byte(nearest[bin])
		}
	}
	return palette, indexes
}
