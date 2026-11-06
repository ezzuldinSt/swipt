package raster

import (
	"context"
	"math"
)

// The ObjC compares Vision feature prints (VNGenerateImageFeaturePrintRequest), a neural
// embedding no pure-Go code reproduces. This print is a 16 by 16 color thumbnail of box
// averages, and the distance twice the RMS difference of two prints: 0 for the same image,
// 2 for black against white, about the range Vision's distances span.
const printSide = 16

type featurePrint []float32

func printOf(ctx context.Context, image *Image) (featurePrint, error) {
	r, err := image.raster(ctx)
	if err != nil {
		return nil, err
	}
	channels := r.channels()
	values := make(featurePrint, printSide*printSide*3)
	for row := range printSide {
		top, bottom := row*r.height/printSide, max((row+1)*r.height/printSide, row*r.height/printSide+1)
		for column := range printSide {
			left, right := column*r.width/printSide, max((column+1)*r.width/printSide, column*r.width/printSide+1)
			var sums [3]float64
			for y := top; y < min(bottom, r.height); y++ {
				line := r.row(y)
				for x := left; x < min(right, r.width); x++ {
					if channels == 1 {
						value := float64(line[x])
						sums[0], sums[1], sums[2] = sums[0]+value, sums[1]+value, sums[2]+value
						continue
					}
					sums[0] += float64(line[x*4])
					sums[1] += float64(line[x*4+1])
					sums[2] += float64(line[x*4+2])
				}
			}
			count := float64(max((min(bottom, r.height)-top)*(min(right, r.width)-left), 1))
			for channel := range 3 {
				values[(row*printSide+column)*3+channel] = float32(sums[channel] / count / 255)
			}
		}
	}
	if ctx.Err() != nil {
		return nil, aborted()
	}
	return values, nil
}

func (p featurePrint) distance(other featurePrint) float64 {
	var sum float64
	for index, value := range p {
		difference := float64(value - other[index])
		sum += difference * difference
	}
	return 2 * math.Sqrt(sum/float64(len(p)))
}
