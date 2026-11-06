package raster

// grayscale weighs blue 29, green 150 and red 77 out of 256 and drops the remainder, as
// vImageMatrixMultiply_ARGB8888ToPlanar8 does with no post-bias (SWRasterFilter.m).
func grayscale(r *raster) *raster {
	if r.gray {
		return r
	}
	out := newRaster(r.width, r.height, true)
	parallel(r.height, r.width, func(start, end int) {
		for y := start; y < end; y++ {
			source := r.row(y)
			target := out.row(y)
			for x := range target {
				p := source[x*4 : x*4+3 : x*4+3]
				target[x] = byte((77*uint32(p[0]) + 150*uint32(p[1]) + 29*uint32(p[2])) >> 8)
			}
		}
	})
	return out
}

func lookUp(r *raster, table *[256]byte) *raster {
	gray := grayscale(r)
	out := newRaster(r.width, r.height, true)
	parallel(r.height, r.width, func(start, end int) {
		for y := start; y < end; y++ {
			source := gray.row(y)
			target := out.row(y)
			for x, value := range source {
				target[x] = table[value]
			}
		}
	})
	return out
}

func histogram(gray *raster) [256]int {
	var counts [256]int
	for y := range gray.height {
		for _, value := range gray.row(y) {
			counts[value]++
		}
	}
	return counts
}

// otsu answers the level that maximizes the variance between the classes at or below it and
// above it.
func otsu(gray *raster) int {
	counts := histogram(gray)
	var total, weighted float64
	for value, count := range counts {
		total += float64(count)
		weighted += float64(value) * float64(count)
	}
	var background, backgroundSum, best float64
	level := 0
	for value, count := range counts {
		background += float64(count)
		if background == 0 {
			continue
		}
		foreground := total - background
		if foreground == 0 {
			break
		}
		backgroundSum += float64(value) * float64(count)
		difference := backgroundSum/background - (weighted-backgroundSum)/foreground
		between := background * foreground * difference * difference
		if between > best {
			best = between
			level = value
		}
	}
	return level
}

// thresholded turns gray at or above level white and the rest black; a negative level asks
// for Otsu's level, and the pixels above it turn white.
func thresholded(r *raster, level int) *raster {
	gray := grayscale(r)
	cut := level
	if level < 0 {
		cut = otsu(gray) + 1
	}
	var table [256]byte
	for value := range table {
		if value >= cut {
			table[value] = 255
		}
	}
	return lookUp(gray, &table)
}

// masked turns white the pixels whose red, green and blue all lie in [low, high]. A gray
// image keeps the range all three channels share.
func masked(r *raster, low, high [3]byte) *raster {
	if r.gray {
		lower := max(low[0], low[1], low[2])
		upper := min(high[0], high[1], high[2])
		var table [256]byte
		for value := range table {
			if byte(value) >= lower && byte(value) <= upper {
				table[value] = 255
			}
		}
		return lookUp(r, &table)
	}
	var inside [3][256]bool
	for channel := range 3 {
		for value := int(low[channel]); value <= int(high[channel]); value++ {
			inside[channel][value] = true
		}
	}
	out := newRaster(r.width, r.height, true)
	parallel(r.height, r.width, func(start, end int) {
		for y := start; y < end; y++ {
			source := r.row(y)
			target := out.row(y)
			for x := range target {
				if inside[0][source[x*4]] && inside[1][source[x*4+1]] && inside[2][source[x*4+2]] {
					target[x] = 255
				}
			}
		}
	})
	return out
}

// inverted flips the color channels and keeps alpha.
func inverted(r *raster) *raster {
	out := newRaster(r.width, r.height, r.gray)
	parallel(r.height, r.width, func(start, end int) {
		for y := start; y < end; y++ {
			source := r.row(y)
			target := out.row(y)
			if r.gray {
				for x, value := range source {
					target[x] = 255 - value
				}
				continue
			}
			for x := 0; x < len(source); x += 4 {
				target[x], target[x+1], target[x+2], target[x+3] = 255-source[x], 255-source[x+1], 255-source[x+2], source[x+3]
			}
		}
	})
	return out
}

// morphed takes the maximum (dilate) or minimum (erode) of every channel, alpha included, over
// a square of side 2·radius+1 clipped to the image, like vImageMax and vImageMin.
func morphed(r *raster, radius int, dilate bool) *raster {
	if radius <= 0 {
		out := newRaster(r.width, r.height, r.gray)
		for y := range r.height {
			copy(out.row(y), r.row(y))
		}
		return out
	}
	channels := r.channels()
	across := newRaster(r.width, r.height, r.gray)
	parallel(r.height, r.width*channels, func(start, end int) {
		line := newExtremes(r.width, radius, dilate)
		for y := start; y < end; y++ {
			source := r.row(y)
			target := across.row(y)
			for channel := range channels {
				line.run(target[channel:], source[channel:], channels, r.width)
			}
		}
	})
	out := newRaster(r.width, r.height, r.gray)
	columns := r.width * channels
	parallel(columns, r.height, func(start, end int) {
		line := newExtremes(r.height, radius, dilate)
		for column := start; column < end; column++ {
			line.run(out.pix[column:], across.pix[column:], out.stride, r.height)
		}
	})
	return out
}

// extremes is the van Herk/Gil-Werman running maximum or minimum: three comparisons a sample
// whatever the window.
type extremes struct {
	radius  int
	window  int
	dilate  bool
	padded  []byte
	forward []byte
	reverse []byte
}

func newExtremes(length, radius int, dilate bool) *extremes {
	size := length + 2*radius
	return &extremes{radius: radius, window: 2*radius + 1, dilate: dilate, padded: make([]byte, size), forward: make([]byte, size), reverse: make([]byte, size)}
}

func (e *extremes) pick(a, b byte) byte {
	if e.dilate == (a > b) {
		return a
	}
	return b
}

// run filters count samples step bytes apart from source into target.
func (e *extremes) run(target, source []byte, step, count int) {
	outside := byte(255)
	if e.dilate {
		outside = 0
	}
	size := count + 2*e.radius
	padded, forward, reverse := e.padded[:size], e.forward[:size], e.reverse[:size]
	for index := range padded {
		padded[index] = outside
	}
	for index := range count {
		padded[e.radius+index] = source[index*step]
	}
	for index := range size {
		if index%e.window == 0 {
			forward[index] = padded[index]
		} else {
			forward[index] = e.pick(forward[index-1], padded[index])
		}
	}
	for index := size - 1; index >= 0; index-- {
		if index == size-1 || index%e.window == e.window-1 {
			reverse[index] = padded[index]
		} else {
			reverse[index] = e.pick(reverse[index+1], padded[index])
		}
	}
	for index := range count {
		target[index*step] = e.pick(reverse[index], forward[index+e.window-1])
	}
}
