package raster

import (
	"context"
	"math"
	"slices"
)

// Template matching scores zero-mean normalized cross-correlation weighted by the template's
// alpha, coarse to fine over a pyramid (SWTemplateMatching.m, SWMatchKernel.m).
const (
	coarsestSide = 8
	deepestLevel = 5
	refineRadius = 2
)

type match struct {
	rect  Rect
	score float64
	scale float64
}

// kernel is a template plane centered on its weighted mean.
type kernel struct {
	width     int
	height    int
	centered  []float32
	weights   []float32
	weightSum float32
	variance  float32
	uniform   bool
}

func newKernel(values, weights *plane) (*kernel, error) {
	var weightSum float32
	uniform := true
	for _, weight := range weights.values {
		weightSum += weight
		// Resampled opaque weights stay within rounding of 1, where box sums can stand in.
		uniform = uniform && math.Abs(float64(weight)-1) < 1e-5
	}
	if weightSum <= 0.5 {
		return nil, failure(NameRange, "the template is transparent")
	}
	var total float32
	for index, value := range values.values {
		total += value * weights.values[index]
	}
	mean := total / weightSum
	centered := make([]float32, len(values.values))
	var variance float32
	for index, value := range values.values {
		difference := value - mean
		centered[index] = difference * weights.values[index]
		variance += centered[index] * difference
	}
	return &kernel{width: values.width, height: values.height, centered: centered, weights: weights.values, weightSum: weightSum, variance: variance, uniform: uniform}, nil
}

// flat is the least weighted variance a window needs to be scored.
func flat(weightSum float32) float32 {
	return 1.6e-5 * weightSum
}

type level struct {
	image   *plane
	squared *plane
	kernel  *kernel
}

// score is the correlation of the kernel with the window at (x, y).
func (l *level) score(x, y int) float32 {
	k := l.kernel
	var correlation, sum, square float32
	for line := range k.height {
		offset := (y+line)*l.image.width + x
		values := l.image.values[offset : offset+k.width]
		squares := l.squared.values[offset : offset+k.width]
		centered := k.centered[line*k.width : (line+1)*k.width]
		weights := k.weights[line*k.width : (line+1)*k.width]
		var c, s, q float32
		for index, value := range values {
			c += value * centered[index]
			s += value * weights[index]
			q += squares[index] * weights[index]
		}
		correlation += c
		sum += s
		square += q
	}
	variance := square - sum*sum/k.weightSum
	if variance <= flat(k.weightSum) || k.variance <= 0 {
		return 0
	}
	return correlation / float32(math.Sqrt(float64(k.variance*variance)))
}

// correlate sums values·kernel over every placement of the kernel inside the image.
func correlate(values []float32, width, height int, kernel []float32, kernelWidth, kernelHeight int) []float32 {
	rows := height - kernelHeight + 1
	columns := width - kernelWidth + 1
	out := make([]float32, rows*columns)
	parallel(rows, columns*kernelWidth*kernelHeight, func(start, end int) {
		for y := start; y < end; y++ {
			target := out[y*columns : (y+1)*columns]
			for line := range kernelHeight {
				row := values[(y+line)*width:]
				for column := range kernelWidth {
					weight := kernel[line*kernelWidth+column]
					if weight == 0 {
						continue
					}
					source := row[column : column+columns]
					for x := range target {
						target[x] += weight * source[x]
					}
				}
			}
		}
	})
	return out
}

// boxSums sums every kernelWidth by kernelHeight window, which is the weighted sum when every
// weight is 1.
func boxSums(values []float32, width, height int, kernelWidth, kernelHeight int) []float32 {
	integral := make([]float64, (width+1)*(height+1))
	for y := range height {
		var running float64
		for x := range width {
			running += float64(values[y*width+x])
			integral[(y+1)*(width+1)+x+1] = integral[y*(width+1)+x+1] + running
		}
	}
	rows := height - kernelHeight + 1
	columns := width - kernelWidth + 1
	out := make([]float32, rows*columns)
	for y := range rows {
		for x := range columns {
			top, bottom := y*(width+1), (y+kernelHeight)*(width+1)
			out[y*columns+x] = float32(integral[bottom+x+kernelWidth] - integral[bottom+x] - integral[top+x+kernelWidth] + integral[top+x])
		}
	}
	return out
}

type position struct {
	x, y int
}

// candidates scores every placement on the coarsest level and keeps the best ones that are not
// within half a template of a better one.
func (l *level) candidates(cutoff float32, wanted int) []position {
	k := l.kernel
	image := l.image
	rows := image.height - k.height + 1
	columns := image.width - k.width + 1
	correlation := correlate(image.values, image.width, image.height, k.centered, k.width, k.height)
	var sums, squares []float32
	if k.uniform {
		sums = boxSums(image.values, image.width, image.height, k.width, k.height)
		squares = boxSums(l.squared.values, image.width, image.height, k.width, k.height)
	} else {
		sums = correlate(image.values, image.width, image.height, k.weights, k.width, k.height)
		squares = correlate(l.squared.values, image.width, image.height, k.weights, k.width, k.height)
	}
	minimum := flat(k.weightSum)
	type candidate struct {
		score float32
		x, y  int
	}
	var found []candidate
	for y := range rows {
		for x := range columns {
			index := y*columns + x
			variance := squares[index] - sums[index]*sums[index]/k.weightSum
			if variance <= minimum {
				continue
			}
			value := correlation[index] / float32(math.Sqrt(float64(k.variance*variance)))
			if value >= cutoff {
				found = append(found, candidate{score: value, x: x, y: y})
			}
		}
	}
	slices.SortStableFunc(found, func(a, b candidate) int {
		switch {
		case a.score > b.score:
			return -1
		case a.score < b.score:
			return 1
		}
		return 0
	})
	var kept []position
	for _, c := range found {
		if len(kept) >= wanted {
			break
		}
		near := false
		for _, other := range kept {
			if abs(other.x-c.x)*2 < k.width && abs(other.y-c.y)*2 < k.height {
				near = true
				break
			}
		}
		if !near {
			kept = append(kept, position{x: c.x, y: c.y})
		}
	}
	return kept
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// refine follows a coarse position down the pyramid, searching two pixels about it on each
// finer level, and answers the score on the finest.
func refine(levels []*level, p position) (position, float32) {
	var best float32
	for index := len(levels) - 2; index >= 0; index-- {
		current := levels[index]
		limitX := current.image.width - current.kernel.width
		limitY := current.image.height - current.kernel.height
		centerX, centerY := p.x*2, p.y*2
		best = -2
		for dy := -refineRadius; dy <= refineRadius; dy++ {
			for dx := -refineRadius; dx <= refineRadius; dx++ {
				px := min(max(centerX+dx, 0), limitX)
				py := min(max(centerY+dy, 0), limitY)
				if value := current.score(px, py); value > best {
					best = value
					p = position{x: px, y: py}
				}
			}
		}
	}
	if len(levels) == 1 {
		best = levels[0].score(p.x, p.y)
	}
	return p, best
}

// colorKernel scores color templates on all three channels together.
type colorKernel struct {
	width     int
	height    int
	centered  [3][]float32
	weights   []float32
	weightSum float32
	variance  float32
}

func newColorKernel(template *raster, alpha *plane) (*colorKernel, error) {
	c := &colorKernel{width: template.width, height: template.height, weights: alpha.values}
	for channel := range 3 {
		values := newPlane(template.width, template.height)
		for y := range template.height {
			row := template.row(y)
			target := values.row(y)
			for x := range target {
				target[x] = float32(row[x*4+channel]) * (1.0 / 255)
			}
		}
		k, err := newKernel(values, alpha)
		if err != nil {
			return nil, err
		}
		c.centered[channel] = k.centered
		c.variance += k.variance
	}
	for _, weight := range alpha.values {
		c.weightSum += weight
	}
	return c, nil
}

func (c *colorKernel) score(target *raster, x, y int) float32 {
	var correlation, spread float32
	for channel := range 3 {
		centered := c.centered[channel]
		var sum, square float32
		for line := range c.height {
			row := target.pix[(y+line)*target.stride+x*4:]
			kernel := centered[line*c.width : (line+1)*c.width]
			weights := c.weights[line*c.width : (line+1)*c.width]
			var part, s, q float32
			for index := range kernel {
				value := float32(row[index*4+channel]) * (1.0 / 255)
				part += value * kernel[index]
				s += value * weights[index]
				q += value * value * weights[index]
			}
			correlation += part
			sum += s
			square += q
		}
		spread += max(square-sum*sum/c.weightSum, 0)
	}
	if spread <= 1.6e-5*c.weightSum*3 || c.variance <= 0 {
		return 0
	}
	return correlation / float32(math.Sqrt(float64(c.variance*spread)))
}

// templateWeights is the template's alpha, or all ones for a gray template.
func templateWeights(template *raster) *plane {
	weights := newPlane(template.width, template.height)
	if template.gray {
		for index := range weights.values {
			weights.values[index] = 1
		}
		return weights
	}
	for y := range template.height {
		row := template.row(y)
		target := weights.row(y)
		for x := range target {
			target[x] = float32(row[x*4+3]) * (1.0 / 255)
		}
	}
	return weights
}

// pyramid keeps the target's levels, which every scale of the template shares.
type pyramid struct {
	base   *plane
	levels map[int]*level
}

func (p *pyramid) level(index int) *level {
	if found, ok := p.levels[index]; ok {
		return found
	}
	image := p.base.scaled(max(p.base.width>>index, 1), max(p.base.height>>index, 1))
	found := &level{image: image, squared: image.squares()}
	p.levels[index] = found
	return found
}

func search(ctx context.Context, target *pyramid, targetRaster, template *raster, threshold float32, limit int, factor float64) ([]match, error) {
	gray := planeOf(template)
	alpha := templateWeights(template)
	full, err := newKernel(gray, alpha)
	if err != nil {
		return nil, err
	}
	if full.variance/full.weightSum <= 1e-6 {
		return nil, failure(NameRange, "the template has no contrast to match")
	}
	depth := 0
	for depth < deepestLevel && min(template.width, template.height)>>(depth+1) >= coarsestSide {
		depth++
	}
	levels := []*level{{image: target.base, squared: target.level(0).squared, kernel: full}}
	for index := 1; index <= depth; index++ {
		if ctx.Err() != nil {
			return nil, aborted()
		}
		shared := target.level(index)
		width := max(template.width>>index, 1)
		height := max(template.height>>index, 1)
		if width > shared.image.width || height > shared.image.height {
			break
		}
		k, err := newKernel(gray.scaled(width, height), alpha.scaled(width, height))
		if err != nil {
			return nil, err
		}
		levels = append(levels, &level{image: shared.image, squared: shared.squared, kernel: k})
	}
	coarse := levels[len(levels)-1]
	if coarse.image.height-coarse.kernel.height+1 <= 0 || coarse.image.width-coarse.kernel.width+1 <= 0 {
		return []match{}, nil
	}
	if ctx.Err() != nil {
		return nil, aborted()
	}
	wanted := min(max(limit*3, 20), 300)
	allowance := float32(0.1)
	if len(levels) > 1 {
		allowance = 0.25
	}
	kept := coarse.candidates(max(threshold-allowance, 0), wanted)
	if ctx.Err() != nil {
		return nil, aborted()
	}
	var colors *colorKernel
	if !targetRaster.gray && !template.gray {
		colors, err = newColorKernel(template, alpha)
		if err != nil {
			return nil, err
		}
	}
	scores := make([]float32, len(kept))
	places := make([]position, len(kept))
	parallel(len(kept), template.width*template.height*refineRadius*refineRadius*len(levels), func(start, end int) {
		for index := start; index < end; index++ {
			if ctx.Err() != nil {
				return
			}
			place, best := refine(levels, kept[index])
			if colors != nil {
				best = colors.score(targetRaster, place.x, place.y)
			}
			scores[index], places[index] = best, place
		}
	})
	if ctx.Err() != nil {
		return nil, aborted()
	}
	matches := []match{}
	for index, score := range scores {
		if score >= threshold {
			matches = append(matches, match{rect: Rect{float64(places[index].x), float64(places[index].y), float64(template.width), float64(template.height)}, score: float64(score), scale: factor})
		}
	}
	return matches, nil
}

func intersectionOverUnion(a, b Rect) float64 {
	overlap, ok := intersect(a, b)
	if !ok {
		return 0
	}
	shared := overlap[2] * overlap[3]
	return shared / (a[2]*a[3] + b[2]*b[3] - shared)
}

// findTemplate searches every scale factor of the template, best scores first, and drops a
// match that overlaps a better one by more than overlap.
func findTemplate(ctx context.Context, target, template *raster, factors []float64, threshold float64, limit int, overlap float64) ([]match, error) {
	shared := &pyramid{base: planeOf(target), levels: map[int]*level{}}
	shared.levels[0] = &level{image: shared.base, squared: shared.base.squares()}
	var found []match
	for _, factor := range factors {
		width := int(math.Round(float64(template.width) * factor))
		height := int(math.Round(float64(template.height) * factor))
		if width < 3 || height < 3 || width > target.width || height > target.height {
			continue
		}
		matches, err := search(ctx, shared, target, resized(template, width, height), float32(threshold), limit, factor)
		if err != nil {
			return nil, err
		}
		found = append(found, matches...)
	}
	slices.SortStableFunc(found, func(a, b match) int {
		switch {
		case a.score > b.score:
			return -1
		case a.score < b.score:
			return 1
		}
		return 0
	})
	kept := []match{}
	for _, m := range found {
		if len(kept) >= limit {
			break
		}
		overlapping := false
		for _, other := range kept {
			if intersectionOverUnion(other.rect, m.rect) > overlap {
				overlapping = true
				break
			}
		}
		if !overlapping {
			kept = append(kept, m)
		}
	}
	return kept, nil
}
