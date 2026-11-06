package raster

import (
	"math"
	"sync"
)

// Stats is the result of stats: one entry per channel, red, green, blue and alpha, or a
// single gray one (SWRasterStatistics.m).
type Stats struct {
	Mean      []float64 `json:"mean"`
	Min       []float64 `json:"min"`
	Max       []float64 `json:"max"`
	Deviation []float64 `json:"deviation"`
}

type channelTotals struct {
	sum     uint64
	squares uint64
	low     byte
	high    byte
}

func statistics(r *raster) *Stats {
	channels := r.channels()
	totals := make([]channelTotals, channels)
	for channel := range totals {
		totals[channel].low = 255
	}
	var access sync.Mutex
	parallel(r.height, r.width*channels, func(start, end int) {
		band := make([]channelTotals, channels)
		for channel := range band {
			band[channel].low = 255
		}
		for y := start; y < end; y++ {
			row := r.row(y)
			for channel := range channels {
				t := &band[channel]
				var sum, squares uint64
				low, high := t.low, t.high
				for x := channel; x < len(row); x += channels {
					value := row[x]
					sum += uint64(value)
					squares += uint64(value) * uint64(value)
					low, high = min(low, value), max(high, value)
				}
				t.sum += sum
				t.squares += squares
				t.low, t.high = low, high
			}
		}
		access.Lock()
		for channel, t := range band {
			totals[channel].sum += t.sum
			totals[channel].squares += t.squares
			totals[channel].low = min(totals[channel].low, t.low)
			totals[channel].high = max(totals[channel].high, t.high)
		}
		access.Unlock()
	})
	count := float64(max(r.width*r.height, 1))
	stats := &Stats{}
	for _, t := range totals {
		mean := float64(t.sum) / count
		square := float64(t.squares) / count
		stats.Mean = append(stats.Mean, rounded(mean))
		stats.Min = append(stats.Min, float64(t.low))
		stats.Max = append(stats.Max, float64(t.high))
		stats.Deviation = append(stats.Deviation, rounded(math.Sqrt(math.Max(square-mean*mean, 0))))
	}
	return stats
}
