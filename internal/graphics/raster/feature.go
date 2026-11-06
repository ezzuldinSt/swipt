package raster

import (
	"context"

	"github.com/swipt-automation/swipt/internal/graphics/featureprint"
)

// printOf answers the feature print distance and nearest compare, which the ObjC asks Vision
// for (VNGenerateImageFeaturePrintRequest). The image is squeezed to the network's square
// whatever its shape, as Vision's default VNImageCropAndScaleOptionScaleFill does, so a tall
// screen keeps all of its content at the cost of its proportions. An image never changes, so
// it keeps its print and nearest over n candidates makes at most n+1.
func printOf(ctx context.Context, image *Image) (featureprint.Print, error) {
	image.printAccess.Lock()
	defer image.printAccess.Unlock()
	if image.printed != nil {
		return image.printed, nil
	}
	r, err := image.raster(ctx)
	if err != nil {
		return nil, err
	}
	made, err := featureprint.Of(ctx, resized(r, featureprint.Size, featureprint.Size).image())
	if err != nil {
		if ctx.Err() != nil {
			return nil, aborted()
		}
		return nil, failureCause(NameError, err, "graphics: make a feature print")
	}
	image.printed = made
	return made, nil
}
