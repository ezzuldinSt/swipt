package raster

import (
	"bytes"
	"context"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"slices"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// ImageIO leaves the default JPEG quality undocumented; 0.8 is close to what it writes.
const defaultQuality = 0.8

var heifBrands = []string{"heic", "heix", "heim", "heis", "hevc", "hevx", "hevm", "hevs", "mif1", "msf1", "avif", "avis"}

func isHEIF(data []byte) bool {
	return len(data) >= 12 && string(data[4:8]) == "ftyp" && slices.Contains(heifBrands, string(data[8:12]))
}

// Decode reads PNG, JPEG, GIF, BMP, TIFF or WebP bytes as an Image. Only the header is read
// now; the pixels are decoded when a call first needs them, as ImageIO does. A scale above 0
// overrides the density the file records; timestamp is in milliseconds, or NaN.
func Decode(data []byte, scale, timestamp float64) (*Image, error) {
	if isHEIF(data) {
		return nil, failure(NameData, "graphics: the bytes are a HEIF image, which this platform cannot decode")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return nil, failure(NameData, "graphics: the bytes are not an image")
	}
	found := readMetadata(data)
	width, height := config.Width, config.Height
	if found.orientation >= 5 {
		width, height = height, width
	}
	chosen := 1.0
	if scale > 0 {
		chosen = scale
	} else if found.dpi/72 >= 1 && found.dpi/72 <= 16 {
		chosen = found.dpi / 72
	}
	node := newImage(width, height, scaling(1/chosen, 1/chosen), false, math.NaN(), timestamp)
	node.source = func(ctx context.Context) (*raster, error) {
		return decodeRaster(data, found)
	}
	return node, nil
}

func decodeRaster(data []byte, found metadata) (*raster, error) {
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, failureCause(NameData, err, "graphics: the image could not be decoded")
	}
	r := colorRaster(decoded)
	if c := conversionOf(found.profile); c != nil {
		c.apply(r)
	}
	return oriented(r, found.orientation), nil
}

// encode writes PNG, or JPEG for "jpeg", recording 72·scale dots per inch so decoding the
// bytes restores the scale (SWRasterCodec.m).
func encode(r *raster, format string, quality *float64, scale float64) ([]byte, error) {
	var buffer bytes.Buffer
	switch format {
	case "heic":
		return nil, failure(NameError, "graphics: create the heic encoder: this platform cannot encode HEIC")
	case "jpeg":
		level := defaultQuality
		if quality != nil {
			level = *quality
		}
		err := jpeg.Encode(&buffer, jpegSource(r), &jpeg.Options{Quality: min(max(int(math.Round(level*100)), 1), 100)})
		if err != nil {
			return nil, failureCause(NameError, err, "graphics: encode the image as jpeg")
		}
		return withDensity(buffer.Bytes(), 72*scale), nil
	default:
		err := png.Encode(&buffer, r.image())
		if err != nil {
			return nil, failureCause(NameError, err, "graphics: encode the image as ", format)
		}
		return withPhysical(buffer.Bytes(), 72*scale), nil
	}
}

// jpegSource hands the JPEG encoder a type it converts without per-pixel interface calls.
// JPEG has no alpha, so a translucent pixel is composited over black.
func jpegSource(r *raster) image.Image {
	if r.gray {
		return r.image()
	}
	bounds := image.Rect(0, 0, r.width, r.height)
	if r.opaque() {
		return &image.RGBA{Pix: r.pix, Stride: r.stride, Rect: bounds}
	}
	out := image.NewRGBA(bounds)
	parallel(r.height, r.width, func(start, end int) {
		for y := start; y < end; y++ {
			source := r.row(y)
			target := out.Pix[y*out.Stride : y*out.Stride+r.width*4]
			for x := 0; x < len(source); x += 4 {
				alpha := uint32(source[x+3])
				target[x] = byte((uint32(source[x])*alpha + 127) / 255)
				target[x+1] = byte((uint32(source[x+1])*alpha + 127) / 255)
				target[x+2] = byte((uint32(source[x+2])*alpha + 127) / 255)
				target[x+3] = byte(alpha)
			}
		}
	})
	return out
}
