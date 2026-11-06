// Package featureprint makes image feature prints where Vision's
// VNGenerateImageFeaturePrintRequest is missing: the pooled features of a bundled MobileNetV2,
// run by a small ONNX interpreter in pure Go and scaled to unit length, so that the distance of
// two prints spans 0 to 2 like that of Vision's revision 2 prints. The values differ from
// Vision's; what is near stays near.
package featureprint

import (
	"context"
	_ "embed"
	"image"
	"image/color"
	"math"
	"sync"

	E "github.com/sagernet/sing/common/exceptions"
)

// The model is mobilenetv2-12.onnx of the ONNX Model Zoo, torchvision's MobileNetV2 trained on
// ImageNet (NOTICE has its source and licenses), from
// https://github.com/onnx/models/raw/main/validated/vision/classification/mobilenet/model/mobilenetv2-12.onnx
// SHA-256 c0c3f76d93fa3fd6580652a45618618a220fced18babf65774ed169de0432ad5, 13964571 bytes.
//
//go:embed mobilenetv2-12.onnx
var modelData []byte

// Size is the side of the square image the network sees.
const Size = 224

// The statistics of ImageNet the network was trained with, for red, green and blue.
var (
	mean      = [3]float32{0.485, 0.456, 0.406}
	deviation = [3]float32{0.229, 0.224, 0.225}
)

var loaded = sync.OnceValues(func() (*network, error) {
	m, err := readModel(modelData)
	if err != nil {
		return nil, err
	}
	return compile(m, Size)
})

// Print is a feature print: 1280 features of unit length.
type Print []float32

// Of makes the print of an image of Size by Size pixels. Transparent pixels count as black.
func Of(ctx context.Context, picture image.Image) (Print, error) {
	bounds := picture.Bounds()
	if bounds.Dx() != Size || bounds.Dy() != Size {
		return nil, E.New("featureprint: the image is ", bounds.Dx(), "x", bounds.Dy(), " pixels, not ", Size, "x", Size)
	}
	n, err := loaded()
	if err != nil {
		return nil, E.Cause(err, "featureprint: load the model")
	}
	features, err := n.pooled(ctx, func(input []float32) {
		normalize(input, picture)
	})
	if err != nil {
		return nil, err
	}
	var sum float64
	for _, value := range features {
		sum += float64(value) * float64(value)
	}
	if sum > 0 {
		scale := 1 / math.Sqrt(sum)
		for index, value := range features {
			features[index] = float32(float64(value) * scale)
		}
	}
	return features, nil
}

// Distance is the Euclidean distance of two prints: 0 for the same image. The features follow
// a ReLU6, so none is negative and two prints are at most √2 apart, where Vision's reach 2.
func (p Print) Distance(other Print) float64 {
	var sum float64
	for index, value := range p {
		difference := float64(value) - float64(other[index])
		sum += difference * difference
	}
	return math.Sqrt(sum)
}

// normalize writes the planes of red, green and blue scaled to [0, 1] and standardized.
func normalize(input []float32, picture image.Image) {
	var levels [3][256]float32
	for channel := range levels {
		for value := range levels[channel] {
			levels[channel][value] = (float32(value)/255 - mean[channel]) / deviation[channel]
		}
	}
	red, green, blue := input[:Size*Size], input[Size*Size:2*Size*Size], input[2*Size*Size:3*Size*Size]
	put := func(index int, r, g, b, alpha byte) {
		if alpha != 255 {
			r, g, b = premultiplied(r, alpha), premultiplied(g, alpha), premultiplied(b, alpha)
		}
		red[index], green[index], blue[index] = levels[0][r], levels[1][g], levels[2][b]
	}
	bounds := picture.Bounds()
	switch source := picture.(type) {
	case *image.NRGBA:
		for y := range Size {
			row := source.Pix[source.PixOffset(bounds.Min.X, bounds.Min.Y+y):][:Size*4]
			for x := range Size {
				put(y*Size+x, row[x*4], row[x*4+1], row[x*4+2], row[x*4+3])
			}
		}
	case *image.Gray:
		for y := range Size {
			row := source.Pix[source.PixOffset(bounds.Min.X, bounds.Min.Y+y):][:Size]
			for x, value := range row {
				put(y*Size+x, value, value, value, 255)
			}
		}
	default:
		for y := range Size {
			for x := range Size {
				c := color.NRGBAModel.Convert(picture.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.NRGBA)
				put(y*Size+x, c.R, c.G, c.B, c.A)
			}
		}
	}
}

func premultiplied(value, alpha byte) byte {
	return byte((uint32(value)*uint32(alpha) + 127) / 255)
}
