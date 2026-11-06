package raster

import (
	"bytes"
	"context"
	"slices"
	"sync/atomic"
)

// The most components a call returns (SWRegions.m).
const largestComponentCount = 10000

type component struct {
	minX, minY, maxX, maxY int
	count                  int
	sumX, sumY             float64
}

func (c component) rect() Rect {
	return Rect{float64(c.minX), float64(c.minY), float64(c.maxX - c.minX + 1), float64(c.maxY - c.minY + 1)}
}

// centroid is the mean of the pixel centers.
func (c component) centroid() Point {
	return Point{c.sumX/float64(c.count) + 0.5, c.sumY/float64(c.count) + 0.5}
}

type run struct {
	y, start, end int
}

func (c *component) merge(r run) {
	count := r.end - r.start + 1
	sumX := float64(count) * float64(r.start+r.end) / 2
	sumY := float64(count * r.y)
	if c.count == 0 {
		*c = component{minX: r.start, minY: r.y, maxX: r.end, maxY: r.y, count: count, sumX: sumX, sumY: sumY}
		return
	}
	c.minX, c.maxX = min(c.minX, r.start), max(c.maxX, r.end)
	c.minY, c.maxY = min(c.minY, r.y), max(c.maxY, r.y)
	c.count += count
	c.sumX += sumX
	c.sumY += sumY
}

func binarized(r *raster) *raster {
	var table [256]byte
	for value := 128; value < 256; value++ {
		table[value] = 255
	}
	return lookUp(r, &table)
}

func root(parents []int, label int) int {
	for parents[label] != label {
		parents[label] = parents[parents[label]]
		label = parents[label]
	}
	return label
}

func join(parents []int, a, b int) {
	first, second := root(parents, a), root(parents, b)
	if first != second {
		parents[max(first, second)] = min(first, second)
	}
}

// components labels the 8-connected runs of 255 in a binary raster and answers the parts of
// at least minimumPixels pixels, top to bottom and then left to right.
func components(ctx context.Context, binary *raster, minimumPixels int) ([]component, error) {
	var runs []run
	var parents []int
	previousStart := 0
	for y := range binary.height {
		if y%256 == 0 && ctx.Err() != nil {
			return nil, aborted()
		}
		row := binary.row(y)
		currentStart := len(runs)
		for x := 0; x < binary.width; {
			found := bytes.IndexByte(row[x:], 255)
			if found < 0 {
				break
			}
			start := x + found
			end := binary.width - 1
			if rest := bytes.IndexByte(row[start:], 0); rest >= 0 {
				end = start + rest - 1
			}
			parents = append(parents, len(runs))
			runs = append(runs, run{y: y, start: start, end: end})
			x = end + 1
		}
		above := previousStart
		for current := currentStart; current < len(runs); current++ {
			for above < currentStart && runs[above].end < runs[current].start-1 {
				above++
			}
			for index := above; index < currentStart && runs[index].start <= runs[current].end+1; index++ {
				join(parents, current, index)
			}
		}
		previousStart = currentStart
	}
	merged := make([]component, len(runs))
	for index, r := range runs {
		merged[root(parents, index)].merge(r)
	}
	kept := merged[:0]
	for _, c := range merged {
		if c.count > 0 && c.count >= minimumPixels {
			kept = append(kept, c)
		}
	}
	slices.SortStableFunc(kept, func(a, b component) int {
		if a.minY != b.minY {
			return a.minY - b.minY
		}
		return a.minX - b.minX
	})
	return kept[:min(len(kept), largestComponentCount)], nil
}

// changedCount counts the pixels whose gray levels differ by threshold or more.
func changedCount(first, second *raster, threshold int) int {
	a, b := grayscale(first), grayscale(second)
	var total atomic.Int64
	parallel(a.height, a.width, func(start, end int) {
		count := 0
		for y := start; y < end; y++ {
			left, right := a.row(y), b.row(y)[:a.width]
			for x, value := range left {
				difference := int(value) - int(right[x])
				if difference >= threshold || -difference >= threshold {
					count++
				}
			}
		}
		total.Add(int64(count))
	})
	return int(total.Load())
}

// changeMask marks white the pixels whose gray levels differ by threshold or more, and counts
// them (SWRegionsDifference).
func changeMask(first, second *raster, threshold int) (*raster, int) {
	a, b := grayscale(first), grayscale(second)
	mask := newRaster(a.width, a.height, true)
	var total atomic.Int64
	parallel(a.height, a.width, func(start, end int) {
		count := 0
		for y := start; y < end; y++ {
			left, right, target := a.row(y), b.row(y)[:a.width], mask.row(y)
			for x, value := range left {
				difference := int(value) - int(right[x])
				if difference >= threshold || -difference >= threshold {
					target[x] = 255
					count++
				}
			}
		}
		total.Add(int64(count))
	})
	return mask, int(total.Load())
}

// merged grows the changes by radius so nearby ones join, then shrinks each joined box back by
// radius about its center.
func merged(ctx context.Context, mask *raster, radius int) ([]Rect, error) {
	grown := mask
	if radius > 0 {
		grown = morphed(mask, radius, true)
	}
	parts, err := components(ctx, grown, 1)
	if err != nil {
		return nil, err
	}
	rects := make([]Rect, len(parts))
	for index, part := range parts {
		r := part.rect()
		width := max(r[2]-float64(radius*2), 1)
		height := max(r[3]-float64(radius*2), 1)
		center := r.center()
		rects[index] = Rect{center[0] - width/2, center[1] - height/2, width, height}
	}
	return rects, nil
}

// minimumChange is the least number of changed pixels that counts as a change.
func minimumChange(total int) int {
	return max(4, total/5000)
}
