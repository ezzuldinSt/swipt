package raster

import (
	"encoding/binary"
	"image"
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
	c.convertRows(r.height, r.width, r.row)
}

// convertRows converts height rows of width RGBA pixels in place. A screen repeats its colors
// along a row, so a pixel like the one before it takes that one's result.
func (c *conversion) convertRows(height, width int, row func(y int) []byte) {
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
	parallel(height, width*4, func(start, end int) {
		for y := start; y < end; y++ {
			pixels := row(y)
			var before, after [3]byte
			repeated := false
			for x := 0; x+3 < len(pixels); x += 4 {
				p := pixels[x : x+3 : x+3]
				if repeated && p[0] == before[0] && p[1] == before[1] && p[2] == before[2] {
					p[0], p[1], p[2] = after[0], after[1], after[2]
					continue
				}
				before, repeated = [3]byte{p[0], p[1], p[2]}, true
				red, green, blue := c.linear[0][p[0]], c.linear[1][p[1]], c.linear[2][p[2]]
				p[0] = encode(m[0]*red + m[1]*green + m[2]*blue)
				p[1] = encode(m[3]*red + m[4]*green + m[5]*blue)
				p[2] = encode(m[6]*red + m[7]*green + m[8]*blue)
				after = [3]byte{p[0], p[1], p[2]}
			}
		}
	})
}

// The chromaticities of red, green, blue and white of the colour primaries of ITU-T H.273
// table 2 that conversions read; the others, and 2 (unspecified), are not converted.
var primariesOf = map[byte][4][2]float64{
	1:  {{0.640, 0.330}, {0.300, 0.600}, {0.150, 0.060}, whiteD65}, // BT.709, sRGB
	4:  {{0.670, 0.330}, {0.210, 0.710}, {0.140, 0.080}, whiteC},   // BT.470 M
	5:  {{0.640, 0.330}, {0.290, 0.600}, {0.150, 0.060}, whiteD65}, // BT.601 625
	6:  {{0.630, 0.340}, {0.310, 0.595}, {0.155, 0.070}, whiteD65}, // BT.601 525
	7:  {{0.630, 0.340}, {0.310, 0.595}, {0.155, 0.070}, whiteD65}, // SMPTE 240M
	8:  {{0.681, 0.319}, {0.243, 0.692}, {0.145, 0.049}, whiteC},   // generic film
	9:  {{0.708, 0.292}, {0.170, 0.797}, {0.131, 0.046}, whiteD65}, // BT.2020
	11: {{0.680, 0.320}, {0.265, 0.690}, {0.150, 0.060}, whiteDCI}, // DCI-P3
	12: {{0.680, 0.320}, {0.265, 0.690}, {0.150, 0.060}, whiteD65}, // Display P3
	22: {{0.630, 0.340}, {0.295, 0.605}, {0.155, 0.077}, whiteD65}, // EBU Tech. 3213
}

var (
	whiteD65 = [2]float64{0.3127, 0.3290}
	whiteC   = [2]float64{0.310, 0.316}
	whiteDCI = [2]float64{0.314, 0.351}
)

// linearOf answers the curve that takes values of the transfer characteristics of ITU-T H.273
// table 3 to linear light, or false for the ones it does not convert, such as PQ and HLG.
func linearOf(transfer byte) (func(float64) float64, bool) {
	switch transfer {
	case 1, 6, 14, 15: // BT.709 and the BT.601 and BT.2020 curves that equal it
		return func(x float64) float64 {
			if x < 0.081 {
				return x / 4.5
			}
			return math.Pow((x+0.099)/1.099, 1/0.45)
		}, true
	case 4:
		return func(x float64) float64 { return math.Pow(x, 2.2) }, true
	case 5:
		return func(x float64) float64 { return math.Pow(x, 2.8) }, true
	case 7: // SMPTE 240M
		return func(x float64) float64 {
			if x < 0.0913 {
				return x / 4
			}
			return math.Pow((x+0.1115)/1.1115, 1/0.45)
		}, true
	case 8:
		return func(x float64) float64 { return x }, true
	case 13: // IEC 61966-2-1, sRGB
		return sRGBLinear, true
	}
	return nil, false
}

// toXYZ is the matrix from linear RGB of these primaries to CIE XYZ, white at Y 1.
func toXYZ(primaries [4][2]float64) ([9]float64, bool) {
	var columns [9]float64
	for column := range 3 {
		x, y := primaries[column][0], primaries[column][1]
		columns[column], columns[3+column], columns[6+column] = x/y, 1, (1-x-y)/y
	}
	inverse, ok := inverted3(columns)
	if !ok {
		return columns, false
	}
	x, y := primaries[3][0], primaries[3][1]
	white := [3]float64{x / y, 1, (1 - x - y) / y}
	var out [9]float64
	for column := range 3 {
		scale := inverse[column*3]*white[0] + inverse[column*3+1]*white[1] + inverse[column*3+2]*white[2]
		for row := range 3 {
			out[row*3+column] = columns[row*3+column] * scale
		}
	}
	return out, true
}

func multiplied3(a, b [9]float64) [9]float64 {
	var out [9]float64
	for row := range 3 {
		for column := range 3 {
			for k := range 3 {
				out[row*3+column] += a[row*3+k] * b[k*3+column]
			}
		}
	}
	return out
}

// bradford adapts XYZ from one white to another (ICC.1 annex E).
func bradford(from, to [2]float64) [9]float64 {
	cone := [9]float64{0.8951, 0.2664, -0.1614, -0.7502, 1.7135, 0.0367, 0.0389, -0.0685, 1.0296}
	inverse, _ := inverted3(cone)
	response := func(white [2]float64) [3]float64 {
		xyz := [3]float64{white[0] / white[1], 1, (1 - white[0] - white[1]) / white[1]}
		var out [3]float64
		for row := range 3 {
			out[row] = cone[row*3]*xyz[0] + cone[row*3+1]*xyz[1] + cone[row*3+2]*xyz[2]
		}
		return out
	}
	source, destination := response(from), response(to)
	var scale [9]float64
	for index := range 3 {
		scale[index*4] = destination[index] / source[index]
	}
	return multiplied3(inverse, multiplied3(scale, cone))
}

// cicpConversion converts the colors of H.273 code points to sRGB in linear light, clipping
// what sRGB cannot show, as conversionOf does for ICC profiles. It answers nil when they are
// sRGB already, and false when it cannot convert them.
func cicpConversion(primaries, transfer byte, fullRange bool) (*conversion, bool) {
	source, known := primariesOf[primaries]
	curve, linear := linearOf(transfer)
	if !known || !linear {
		return nil, false
	}
	if primaries == 1 && transfer == 13 && fullRange {
		return nil, true
	}
	into, ok := toXYZ(source)
	sRGB, ok2 := toXYZ(primariesOf[1])
	if !ok || !ok2 {
		return nil, false
	}
	if source[3] != whiteD65 {
		into = multiplied3(bradford(source[3], whiteD65), into)
	}
	from, ok := inverted3(sRGB)
	if !ok {
		return nil, false
	}
	matrix := multiplied3(from, into)
	c := &conversion{}
	for index := range matrix {
		c.matrix[index] = float32(matrix[index])
	}
	for value := range 256 {
		x := float64(value) / 255
		if !fullRange {
			x = min(max((float64(value)-16)/219, 0), 1)
		}
		level := float32(curve(x))
		c.linear[0][value], c.linear[1][value], c.linear[2][value] = level, level, level
	}
	return c, true
}

// ColorConversion converts the colors of another color space to sRGB, as decoding a PNG does.
type ColorConversion struct {
	conversion *conversion
}

var colorConversions sync.Map

// ConversionToSRGB answers the conversion of colors described by H.273 code points, as a PNG's
// cICP chunk or a video's VUI gives them, to sRGB; nil when they are sRGB already or are not
// ones it converts.
func ConversionToSRGB(primaries, transfer byte, fullRange bool) *ColorConversion {
	key := [3]byte{primaries, transfer, 0}
	if fullRange {
		key[2] = 1
	}
	if found, ok := colorConversions.Load(key); ok {
		return found.(*ColorConversion)
	}
	var made *ColorConversion
	if c, ok := cicpConversion(primaries, transfer, fullRange); ok && c != nil {
		made = &ColorConversion{conversion: c}
	}
	colorConversions.Store(key, made)
	return made
}

// ConvertNRGBA converts opaque pixels in place.
func (c *ColorConversion) ConvertNRGBA(pixels *image.NRGBA) {
	width, height := pixels.Rect.Dx(), pixels.Rect.Dy()
	c.conversion.convertRows(height, width, func(y int) []byte {
		start := pixels.PixOffset(pixels.Rect.Min.X, pixels.Rect.Min.Y+y)
		return pixels.Pix[start : start+width*4]
	})
}
