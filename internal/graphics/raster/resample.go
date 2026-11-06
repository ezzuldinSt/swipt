package raster

import "math"

// The ObjC resizes with kvImageHighQualityResampling, which is a Lanczos-5 filter; vImage
// widens it when it shrinks, so every source pixel counts.
const lanczosLobes = 5

func lanczos(x float64) float64 {
	if x == 0 {
		return 1
	}
	if x <= -lanczosLobes || x >= lanczosLobes {
		return 0
	}
	px := math.Pi * x
	return lanczosLobes * math.Sin(px) * math.Sin(px/lanczosLobes) / (px * px)
}

// taps holds, for each output sample, the first source sample and the weights of the samples
// from it on. Weights of samples beyond the edges fold onto the edge, which extends it.
type taps struct {
	start   []int
	count   []int
	weights []float32
	stride  int
}

func tapsFor(source, target int) *taps {
	ratio := float64(source) / float64(target)
	widen := math.Max(ratio, 1)
	support := lanczosLobes * widen
	stride := int(math.Ceil(support))*2 + 2
	t := &taps{start: make([]int, target), count: make([]int, target), weights: make([]float32, target*stride), stride: stride}
	sums := make([]float64, source)
	for index := range target {
		center := (float64(index)+0.5)*ratio - 0.5
		low := int(math.Ceil(center - support))
		high := int(math.Floor(center + support))
		first, last := source, -1
		for sample := low; sample <= high; sample++ {
			weight := lanczos((float64(sample) - center) / widen)
			if weight == 0 {
				continue
			}
			clamped := min(max(sample, 0), source-1)
			if sums[clamped] == 0 {
				first, last = min(first, clamped), max(last, clamped)
			}
			sums[clamped] += weight
		}
		if last < first {
			nearest := min(max(int(math.Round(center)), 0), source-1)
			first, last = nearest, nearest
			sums[nearest] = 1
		}
		var total float64
		for sample := first; sample <= last; sample++ {
			total += sums[sample]
		}
		t.start[index] = first
		t.count[index] = min(last-first+1, stride)
		weights := t.weights[index*stride : index*stride+t.count[index]]
		for offset := range weights {
			weights[offset] = float32(sums[first+offset] / total)
		}
		for sample := first; sample <= last; sample++ {
			sums[sample] = 0
		}
	}
	return t
}

func (t *taps) of(index int) (int, []float32) {
	offset := index * t.stride
	return t.start[index], t.weights[offset : offset+t.count[index]]
}

// resized scales a raster with the Lanczos filter and rounds to the nearest byte.
func resized(r *raster, width, height int) *raster {
	if width == r.width && height == r.height {
		return r
	}
	channels := r.channels()
	across := tapsFor(r.width, width)
	down := tapsFor(r.height, height)
	rowLength := width * channels
	middle := make([]float32, rowLength*r.height)
	parallel(r.height, rowLength*across.stride, func(start, end int) {
		for y := start; y < end; y++ {
			source := r.row(y)
			target := middle[y*rowLength : (y+1)*rowLength]
			for x := range width {
				first, weights := across.of(x)
				if channels == 1 {
					var sum float32
					for offset, weight := range weights {
						sum += weight * float32(source[first+offset])
					}
					target[x] = sum
					continue
				}
				var red, green, blue, alpha float32
				pixels := source[first*4 : (first+len(weights))*4]
				for offset, weight := range weights {
					p := pixels[offset*4 : offset*4+4 : offset*4+4]
					red += weight * float32(p[0])
					green += weight * float32(p[1])
					blue += weight * float32(p[2])
					alpha += weight * float32(p[3])
				}
				target[x*4], target[x*4+1], target[x*4+2], target[x*4+3] = red, green, blue, alpha
			}
		}
	})
	out := newRaster(width, height, r.gray)
	parallel(height, rowLength*down.stride, func(start, end int) {
		sums := make([]float32, rowLength)
		for y := start; y < end; y++ {
			clear(sums)
			first, weights := down.of(y)
			for offset, weight := range weights {
				line := middle[(first+offset)*rowLength : (first+offset+1)*rowLength]
				for index, value := range line {
					sums[index] += weight * value
				}
			}
			target := out.row(y)
			for index, value := range sums {
				target[index] = clampByte(value)
			}
		}
	})
	return out
}

func clampByte(value float32) byte {
	if value <= 0 {
		return 0
	}
	if value >= 255 {
		return 255
	}
	return byte(value + 0.5)
}

// plane is a gray image of floats from 0 to 1 (SWMatchKernel.m).
type plane struct {
	values []float32
	width  int
	height int
}

func newPlane(width, height int) *plane {
	return &plane{values: make([]float32, width*height), width: width, height: height}
}

func planeOf(r *raster) *plane {
	gray := grayscale(r)
	out := newPlane(r.width, r.height)
	parallel(r.height, r.width, func(start, end int) {
		for y := start; y < end; y++ {
			target := out.values[y*out.width : (y+1)*out.width]
			for x, value := range gray.row(y) {
				target[x] = float32(value) * (1.0 / 255)
			}
		}
	})
	return out
}

func (p *plane) row(y int) []float32 {
	return p.values[y*p.width : (y+1)*p.width]
}

func (p *plane) squares() *plane {
	out := newPlane(p.width, p.height)
	for index, value := range p.values {
		out.values[index] = value * value
	}
	return out
}

// scaled resamples the plane like vImageScale_PlanarF, without clamping.
func (p *plane) scaled(width, height int) *plane {
	across := tapsFor(p.width, width)
	down := tapsFor(p.height, height)
	middle := make([]float32, width*p.height)
	parallel(p.height, width*across.stride, func(start, end int) {
		for y := start; y < end; y++ {
			source := p.row(y)
			target := middle[y*width : (y+1)*width]
			for x := range target {
				first, weights := across.of(x)
				var sum float32
				for offset, weight := range weights {
					sum += weight * source[first+offset]
				}
				target[x] = sum
			}
		}
	})
	out := newPlane(width, height)
	parallel(height, width*down.stride, func(start, end int) {
		for y := start; y < end; y++ {
			target := out.row(y)
			first, weights := down.of(y)
			for offset, weight := range weights {
				line := middle[(first+offset)*width : (first+offset+1)*width]
				for index, value := range line {
					target[index] += weight * value
				}
			}
		}
	})
	return out
}
