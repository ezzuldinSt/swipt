package raster

import "math"

// remapped copies the pixel at column across·x + down·y + origin of the source into (x, y) of
// a width by height raster; the offsets are in pixels, so every quarter turn and mirror is one.
func remapped(r *raster, width, height int, origin, across, down [2]int) *raster {
	out := newRaster(width, height, r.gray)
	channels := r.channels()
	stepX := across[0]*channels + across[1]*r.stride
	stepY := down[0]*channels + down[1]*r.stride
	base := origin[0]*channels + origin[1]*r.stride
	parallel(height, width, func(start, end int) {
		for y := start; y < end; y++ {
			target := out.row(y)
			offset := base + y*stepY
			if channels == 1 {
				for x := range target {
					target[x] = r.pix[offset]
					offset += stepX
				}
				continue
			}
			for x := 0; x < len(target); x += 4 {
				copy(target[x:x+4], r.pix[offset:offset+4])
				offset += stepX
			}
		}
	})
	return out
}

// rotatedQuarters turns the raster clockwise by quarter turns, like vImageRotate90.
func rotatedQuarters(r *raster, quarterTurns int) *raster {
	w, h := r.width, r.height
	switch ((quarterTurns % 4) + 4) % 4 {
	case 1:
		return remapped(r, h, w, [2]int{0, h - 1}, [2]int{0, -1}, [2]int{1, 0})
	case 2:
		return remapped(r, w, h, [2]int{w - 1, h - 1}, [2]int{-1, 0}, [2]int{0, -1})
	case 3:
		return remapped(r, h, w, [2]int{w - 1, 0}, [2]int{0, 1}, [2]int{-1, 0})
	}
	return r
}

// oriented applies an EXIF orientation, as ImageIO does when it decodes (SWRasterCodec.m).
func oriented(r *raster, orientation int) *raster {
	w, h := r.width, r.height
	switch orientation {
	case 2:
		return remapped(r, w, h, [2]int{w - 1, 0}, [2]int{-1, 0}, [2]int{0, 1})
	case 3:
		return rotatedQuarters(r, 2)
	case 4:
		return remapped(r, w, h, [2]int{0, h - 1}, [2]int{1, 0}, [2]int{0, -1})
	case 5:
		return remapped(r, h, w, [2]int{0, 0}, [2]int{0, 1}, [2]int{1, 0})
	case 6:
		return rotatedQuarters(r, 1)
	case 7:
		return remapped(r, h, w, [2]int{w - 1, h - 1}, [2]int{0, -1}, [2]int{-1, 0})
	case 8:
		return rotatedQuarters(r, 3)
	}
	return r
}

// rotatedDegrees turns the raster clockwise about its center onto a width by height canvas.
// Core Graphics draws it with high-quality interpolation over transparent black, in
// premultiplied alpha (SWRasterTransform.m); this samples bilinearly the same way.
func rotatedDegrees(r *raster, degrees float64, width, height int) *raster {
	out := newRaster(width, height, r.gray)
	sin, cos := math.Sincos(degrees * math.Pi / 180)
	centerX, centerY := float64(width)/2, float64(height)/2
	sourceX, sourceY := float64(r.width)/2, float64(r.height)/2
	channels := r.channels()
	parallel(height, width*channels*4, func(start, end int) {
		var sample [4]float64
		for y := start; y < end; y++ {
			target := out.row(y)
			dy := float64(y) + 0.5 - centerY
			for x := range width {
				dx := float64(x) + 0.5 - centerX
				// The inverse of the drawing's rotation, in the image's downward y.
				u := cos*dx + sin*dy + sourceX - 0.5
				v := -sin*dx + cos*dy + sourceY - 0.5
				left, top := math.Floor(u), math.Floor(v)
				fx, fy := u-left, v-top
				sample = [4]float64{}
				for corner := range 4 {
					px, py := int(left)+corner%2, int(top)+corner/2
					if px < 0 || py < 0 || px >= r.width || py >= r.height {
						continue
					}
					weight := (1 - fx) * (1 - fy)
					switch corner {
					case 1:
						weight = fx * (1 - fy)
					case 2:
						weight = (1 - fx) * fy
					case 3:
						weight = fx * fy
					}
					p := r.pix[py*r.stride+px*channels:]
					if channels == 1 {
						sample[0] += weight * float64(p[0])
						continue
					}
					alpha := weight * float64(p[3])
					sample[0] += alpha * float64(p[0])
					sample[1] += alpha * float64(p[1])
					sample[2] += alpha * float64(p[2])
					sample[3] += alpha
				}
				if channels == 1 {
					target[x] = clampByte(float32(sample[0]))
					continue
				}
				if sample[3] < 0.5 {
					continue
				}
				pixel := target[x*4 : x*4+4]
				pixel[0] = clampByte(float32(sample[0] / sample[3]))
				pixel[1] = clampByte(float32(sample[1] / sample[3]))
				pixel[2] = clampByte(float32(sample[2] / sample[3]))
				pixel[3] = clampByte(float32(sample[3]))
			}
		}
	})
	return out
}
