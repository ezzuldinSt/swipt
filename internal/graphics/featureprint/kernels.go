package featureprint

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// Below this many multiply-adds a job runs on the calling goroutine.
const parallelWork = 1 << 16

// spread runs work for each unit in [0, count) on up to GOMAXPROCS goroutines; cost is the
// multiply-adds of a unit. Each output is written by one unit, in a fixed order, so the
// results never depend on the schedule.
func spread(count, cost int, work func(unit int)) {
	workers := min(runtime.GOMAXPROCS(0), count)
	if workers <= 1 || count*cost < parallelWork {
		for unit := range count {
			work(unit)
		}
		return
	}
	var next atomic.Int64
	run := func() {
		for {
			unit := int(next.Add(1) - 1)
			if unit >= count {
				return
			}
			work(unit)
		}
	}
	var group sync.WaitGroup
	for range workers - 1 {
		group.Go(run)
	}
	run()
	group.Wait()
}

// A matrix product works on panels of panelRows weight rows, interleaved so that a step
// along the depth reads them together, and on tiles of up to tileColumns outputs a row.
const (
	panelRows   = 4
	tileColumns = 256
)

// packed interleaves the rows of each group's rows by depth weight matrix in panels.
func packed(weights []float32, groups, rows, depth int) []float32 {
	panels := (rows + panelRows - 1) / panelRows
	out := make([]float32, groups*panels*depth*panelRows)
	for group := range groups {
		for row := range rows {
			source := weights[(group*rows+row)*depth : (group*rows+row+1)*depth]
			panel := out[(group*panels+row/panelRows)*depth*panelRows:]
			for k, value := range source {
				panel[k*panelRows+row%panelRows] = value
			}
		}
	}
	return out
}

// multiply writes out = act(bias + weights × columns) for a rows by depth packed weight
// matrix and a depth by width column matrix.
func multiply(out, weights, bias, columns []float32, rows, depth, width int, act activation) {
	panels := (rows + panelRows - 1) / panelRows
	tiles := (width + tileColumns - 1) / tileColumns
	spread(panels*tiles, depth*panelRows*min(width, tileColumns), func(unit int) {
		panel, tile := unit%panels, unit/panels
		start := tile * tileColumns
		end := min(start+tileColumns, width)
		var lines [panelRows][]float32
		for index := range lines {
			row := panel*panelRows + index
			if row >= rows {
				lines[index] = make([]float32, end-start)
				continue
			}
			lines[index] = out[row*width+start : row*width+end]
			for column := range lines[index] {
				lines[index][column] = bias[row]
			}
		}
		multiplyPanel(lines, weights[panel*depth*panelRows:(panel+1)*depth*panelRows], columns, depth, width, start, end)
		for index, line := range lines {
			if panel*panelRows+index < rows {
				act.apply(line)
			}
		}
	})
}

func multiplyPanel(lines [panelRows][]float32, panel, columns []float32, depth, width, start, end int) {
	line0, line1, line2, line3 := lines[0], lines[1], lines[2], lines[3]
	k := 0
	// Two steps of the depth at a time halve the loads and stores of the outputs.
	for ; k+1 < depth; k += 2 {
		w := panel[k*panelRows : k*panelRows+2*panelRows : k*panelRows+2*panelRows]
		a0, a1, a2, a3, b0, b1, b2, b3 := w[0], w[1], w[2], w[3], w[4], w[5], w[6], w[7]
		x := columns[k*width+start : k*width+end]
		y := columns[(k+1)*width+start : (k+1)*width+end]
		y = y[:len(x)]
		out0, out1, out2, out3 := line0[:len(x)], line1[:len(x)], line2[:len(x)], line3[:len(x)]
		for column, value := range x {
			next := y[column]
			out0[column] += a0*value + b0*next
			out1[column] += a1*value + b1*next
			out2[column] += a2*value + b2*next
			out3[column] += a3*value + b3*next
		}
	}
	for ; k < depth; k++ {
		w := panel[k*panelRows : k*panelRows+panelRows : k*panelRows+panelRows]
		w0, w1, w2, w3 := w[0], w[1], w[2], w[3]
		x := columns[k*width+start : k*width+end]
		out0, out1, out2, out3 := line0[:len(x)], line1[:len(x)], line2[:len(x)], line3[:len(x)]
		for column, value := range x {
			out0[column] += w0 * value
			out1[column] += w1 * value
			out2[column] += w2 * value
			out3[column] += w3 * value
		}
	}
}

type convStep struct {
	in, out   int
	from, to  shape
	kernel    [2]int
	stride    [2]int
	pad       [2]int // top and left; the bottom and right follow from the shapes
	groups    int
	depthwise bool
	// direct is a 1 by 1 kernel over every pixel, whose input is already the column matrix.
	direct  bool
	weights []float32
	bias    []float32
	act     activation
}

func (s *convStep) uses() ([]int, int) {
	return []int{s.in}, s.out
}

func (s *convStep) run(w *workspace) {
	in, out := w.tensor(s.in), w.tensor(s.out)
	if s.depthwise {
		s.runDepthwise(in, out)
		return
	}
	rows := s.to.channels / s.groups
	channels := s.from.channels / s.groups
	depth := channels * s.kernel[0] * s.kernel[1]
	area := s.to.area()
	panels := (rows + panelRows - 1) / panelRows
	for group := range s.groups {
		columns := in
		if !s.direct {
			columns = w.scratch[:depth*area]
			s.lower(columns, in, group*channels, channels)
		}
		multiply(out[group*rows*area:(group+1)*rows*area], s.weights[group*panels*depth*panelRows:(group+1)*panels*depth*panelRows],
			s.bias[group*rows:(group+1)*rows], columns, rows, depth, area, s.act)
	}
}

// lower writes the column matrix of channels from first on (im2col): a row for each channel
// and kernel position, a column for each output pixel, zero where the kernel overhangs.
func (s *convStep) lower(columns, in []float32, first, channels int) {
	taps := s.kernel[0] * s.kernel[1]
	area := s.to.area()
	spread(channels*taps, area, func(row int) {
		channel, tap := first+row/taps, row%taps
		ky, kx := tap/s.kernel[1], tap%s.kernel[1]
		plane := in[channel*s.from.area() : (channel+1)*s.from.area()]
		target := columns[row*area : (row+1)*area]
		for y := range s.to.height {
			line := target[y*s.to.width : (y+1)*s.to.width]
			sourceY := y*s.stride[0] - s.pad[0] + ky
			if sourceY < 0 || sourceY >= s.from.height {
				clear(line)
				continue
			}
			source := plane[sourceY*s.from.width : (sourceY+1)*s.from.width]
			for x := range line {
				sourceX := x*s.stride[1] - s.pad[1] + kx
				if sourceX < 0 || sourceX >= s.from.width {
					line[x] = 0
				} else {
					line[x] = source[sourceX]
				}
			}
		}
	})
}

// runDepthwise convolves each channel with its own kernel (a Conv whose group is the
// channel count).
func (s *convStep) runDepthwise(in, out []float32) {
	taps := s.kernel[0] * s.kernel[1]
	fromArea, toArea := s.from.area(), s.to.area()
	spread(s.to.channels, toArea*taps, func(channel int) {
		plane := in[channel*fromArea : (channel+1)*fromArea]
		target := out[channel*toArea : (channel+1)*toArea]
		kernel := s.weights[channel*taps : (channel+1)*taps]
		for index := range target {
			target[index] = s.bias[channel]
		}
		for y := range s.to.height {
			line := target[y*s.to.width : (y+1)*s.to.width]
			for ky := range s.kernel[0] {
				sourceY := y*s.stride[0] - s.pad[0] + ky
				if sourceY < 0 || sourceY >= s.from.height {
					continue
				}
				source := plane[sourceY*s.from.width : (sourceY+1)*s.from.width]
				for kx := range s.kernel[1] {
					weight := kernel[ky*s.kernel[1]+kx]
					offset := kx - s.pad[1]
					// The outputs whose input x*stride+offset lies in the row.
					first := max(0, ceilDivide(-offset, s.stride[1]))
					last := min(s.to.width, ceilDivide(s.from.width-offset, s.stride[1]))
					if first >= last {
						continue
					}
					if s.stride[1] == 1 {
						inputs := source[first+offset : last+offset]
						outputs := line[first:last]
						outputs = outputs[:len(inputs)]
						for x, value := range inputs {
							outputs[x] += weight * value
						}
						continue
					}
					for x := first; x < last; x++ {
						line[x] += weight * source[x*s.stride[1]+offset]
					}
				}
			}
		}
		s.act.apply(target)
	})
}

// ceilDivide rounds up the quotient of a positive divisor, for numerators of either sign.
func ceilDivide(numerator, divisor int) int {
	if numerator <= 0 {
		return -(-numerator / divisor)
	}
	return (numerator + divisor - 1) / divisor
}

// addStep sums two tensors of the same shape, or with no second (a negative id) clamps the
// first, for a Relu or Clip no step before took.
type addStep struct {
	first, second, out int
	act                activation
}

func (s *addStep) uses() ([]int, int) {
	if s.second < 0 {
		return []int{s.first}, s.out
	}
	return []int{s.first, s.second}, s.out
}

func (s *addStep) run(w *workspace) {
	first, out := w.tensor(s.first), w.tensor(s.out)
	var second []float32
	if s.second >= 0 {
		second = w.tensor(s.second)
	}
	const chunk = 1 << 14
	spread((len(out)+chunk-1)/chunk, chunk, func(unit int) {
		start, end := unit*chunk, min((unit+1)*chunk, len(out))
		target := out[start:end]
		copy(target, first[start:end])
		if second != nil {
			addend := second[start:end]
			target = target[:len(addend)]
			for index, value := range addend {
				target[index] += value
			}
		}
		s.act.apply(target)
	})
}

type poolStep struct {
	in, out int
	from    shape
}

func (s *poolStep) uses() ([]int, int) {
	return []int{s.in}, s.out
}

// run averages each channel (Operators.md, GlobalAveragePool).
func (s *poolStep) run(w *workspace) {
	in, out := w.tensor(s.in), w.tensor(s.out)
	area := s.from.area()
	for channel := range out {
		var sum float64
		for _, value := range in[channel*area : (channel+1)*area] {
			sum += float64(value)
		}
		out[channel] = float32(sum / float64(area))
	}
}
