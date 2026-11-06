package raster

import (
	"encoding/binary"
	"math"
	"sync"
)

// ImageIO decodes into sRGB (SWRasterCodec.m), so a screenshot tagged Display P3 reads in
// sRGB values. conversion does the same for matrix/TRC ICC profiles, the kind displays and
// screenshots carry; a profile it cannot read leaves the values as they are.
type conversion struct {
	linear [3][256]float32
	matrix [9]float32
}

// The sRGB primaries adapted to the D50 white of the ICC connection space (sRGB IEC61966-2.1).
var sRGBPrimaries = [9]float64{
	0.4360747, 0.3850649, 0.1430804,
	0.2225045, 0.7168786, 0.0606169,
	0.0139322, 0.0971045, 0.7141733,
}

func conversionOf(profile []byte) *conversion {
	if len(profile) < 132 || string(profile[16:20]) != "RGB " || string(profile[20:24]) != "XYZ " {
		return nil
	}
	tags := map[string][]byte{}
	count := int(binary.BigEndian.Uint32(profile[128:]))
	for index := range count {
		entry := 132 + index*12
		if entry+12 > len(profile) {
			return nil
		}
		offset := int(binary.BigEndian.Uint32(profile[entry+4:]))
		size := int(binary.BigEndian.Uint32(profile[entry+8:]))
		if offset < 0 || size < 0 || offset+size > len(profile) {
			return nil
		}
		tags[string(profile[entry:entry+4])] = profile[offset : offset+size]
	}
	var primaries [9]float64
	for column, name := range []string{"rXYZ", "gXYZ", "bXYZ"} {
		value := tags[name]
		if len(value) < 20 || string(value[:4]) != "XYZ " {
			return nil
		}
		for row := range 3 {
			primaries[row*3+column] = float64(int32(binary.BigEndian.Uint32(value[8+row*4:]))) / 65536
		}
	}
	c := &conversion{}
	sRGB := true
	for channel, name := range []string{"rTRC", "gTRC", "bTRC"} {
		curve, ok := curveOf(tags[name])
		if !ok {
			return nil
		}
		for value := range 256 {
			linear := curve(float64(value) / 255)
			c.linear[channel][value] = float32(linear)
			if math.Abs(linear-sRGBLinear(float64(value)/255)) > 1e-3 {
				sRGB = false
			}
		}
	}
	for index := range primaries {
		if math.Abs(primaries[index]-sRGBPrimaries[index]) > 2e-3 {
			sRGB = false
		}
	}
	if sRGB {
		return nil
	}
	inverse, ok := inverted3(sRGBPrimaries)
	if !ok {
		return nil
	}
	for row := range 3 {
		for column := range 3 {
			var sum float64
			for k := range 3 {
				sum += inverse[row*3+k] * primaries[k*3+column]
			}
			c.matrix[row*3+column] = float32(sum)
		}
	}
	return c
}

func curveOf(tag []byte) (func(float64) float64, bool) {
	if len(tag) < 12 {
		return nil, false
	}
	switch string(tag[:4]) {
	case "curv":
		count := int(binary.BigEndian.Uint32(tag[8:]))
		switch {
		case count == 0:
			return func(x float64) float64 { return x }, true
		case count == 1 && len(tag) >= 14:
			gamma := float64(binary.BigEndian.Uint16(tag[12:])) / 256
			return func(x float64) float64 { return math.Pow(x, gamma) }, true
		case len(tag) >= 12+count*2:
			table := make([]float64, count)
			for index := range table {
				table[index] = float64(binary.BigEndian.Uint16(tag[12+index*2:])) / 65535
			}
			return func(x float64) float64 {
				position := x * float64(count-1)
				low := int(math.Floor(position))
				if low >= count-1 {
					return table[count-1]
				}
				fraction := position - float64(low)
				return table[low]*(1-fraction) + table[low+1]*fraction
			}, true
		}
	case "para":
		kind := int(binary.BigEndian.Uint16(tag[8:]))
		need := [5]int{1, 3, 4, 5, 7}
		if kind > 4 || len(tag) < 12+need[kind]*4 {
			return nil, false
		}
		var p [7]float64
		for index := range need[kind] {
			p[index] = float64(int32(binary.BigEndian.Uint32(tag[12+index*4:]))) / 65536
		}
		g, a, b, c, d, e, f := p[0], p[1], p[2], p[3], p[4], p[5], p[6]
		power := func(x float64) float64 {
			if x <= 0 {
				return 0
			}
			return math.Pow(x, g)
		}
		switch kind {
		case 0:
			return power, true
		case 1:
			return func(x float64) float64 {
				if x >= -b/a {
					return power(a*x + b)
				}
				return 0
			}, true
		case 2:
			return func(x float64) float64 {
				if x >= -b/a {
					return power(a*x+b) + c
				}
				return c
			}, true
		case 3:
			return func(x float64) float64 {
				if x >= d {
					return power(a*x + b)
				}
				return c * x
			}, true
		case 4:
			return func(x float64) float64 {
				if x >= d {
					return power(a*x+b) + e
				}
				return c*x + f
			}, true
		}
	}
	return nil, false
}

func inverted3(m [9]float64) ([9]float64, bool) {
	determinant := m[0]*(m[4]*m[8]-m[5]*m[7]) - m[1]*(m[3]*m[8]-m[5]*m[6]) + m[2]*(m[3]*m[7]-m[4]*m[6])
	if determinant == 0 {
		return m, false
	}
	return [9]float64{
		(m[4]*m[8] - m[5]*m[7]) / determinant, (m[2]*m[7] - m[1]*m[8]) / determinant, (m[1]*m[5] - m[2]*m[4]) / determinant,
		(m[5]*m[6] - m[3]*m[8]) / determinant, (m[0]*m[8] - m[2]*m[6]) / determinant, (m[2]*m[3] - m[0]*m[5]) / determinant,
		(m[3]*m[7] - m[4]*m[6]) / determinant, (m[1]*m[6] - m[0]*m[7]) / determinant, (m[0]*m[4] - m[1]*m[3]) / determinant,
	}, true
}

func sRGBLinear(x float64) float64 {
	if x <= 0.04045 {
		return x / 12.92
	}
	return math.Pow((x+0.055)/1.055, 2.4)
}

const encodeSteps = 1 << 16

// sRGBEncoded maps linear light, in steps of 1/65535, to sRGB bytes.
var sRGBEncoded = sync.OnceValue(func() []byte {
	table := make([]byte, encodeSteps)
	for index := range table {
		x := float64(index) / (encodeSteps - 1)
		var encoded float64
		if x <= 0.0031308 {
			encoded = x * 12.92
		} else {
			encoded = 1.055*math.Pow(x, 1/2.4) - 0.055
		}
		table[index] = byte(math.Round(min(max(encoded, 0), 1) * 255))
	}
	return table
})

// apply converts a color raster in place, clipping colors sRGB cannot show.
func (c *conversion) apply(r *raster) {
	table := sRGBEncoded()
	m := c.matrix
	encode := func(value float32) byte {
		if value <= 0 {
			return table[0]
		}
		if value >= 1 {
			return table[encodeSteps-1]
		}
		return table[int(value*(encodeSteps-1)+0.5)]
	}
	parallel(r.height, r.width*4, func(start, end int) {
		for y := start; y < end; y++ {
			row := r.row(y)
			for x := 0; x < len(row); x += 4 {
				p := row[x : x+3 : x+3]
				red, green, blue := c.linear[0][p[0]], c.linear[1][p[1]], c.linear[2][p[2]]
				p[0] = encode(m[0]*red + m[1]*green + m[2]*blue)
				p[1] = encode(m[3]*red + m[4]*green + m[5]*blue)
				p[2] = encode(m[6]*red + m[7]*green + m[8]*blue)
			}
		}
	})
}
