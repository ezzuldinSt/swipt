package screencapture

import (
	"context"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/coredevice"

	"github.com/google/uuid"
)

const (
	serviceName = "com.apple.coredevice.screencaptureservice"
	feature     = "com.apple.coredevice.feature.capturescreenshot"
)

// CaptureScreenshotInput decodes an optional displayUniqueID and a
// requestedFormat, a ScreenshotImageFormat whose only case is "png", and
// CaptureScreenshotOutput encodes image, displayUniqueID and imageFormat (DDI
// 27B5028f CoreDeviceUtilities 0x108444, 0x3efa44, 0x108db8); image arrived as
// PNG data (iPhone18,2, iOS 27.2 24B5110a). dtscreencaptured renders the
// primary display at its bounds times its point scale and turns the image to
// the current orientation, but answers it unturned for an unknown orientation
// or when the turned bitmap cannot be made (DDI 27B48 dtscreencaptured
// 0x10000fca8, 0x100010f38, 0x1000135cc, 0x100014bd0).
//
// Captures take turns on a kept connection, through the device's tunnel for
// transfers (see coredevice.Device.Bulk).
func Capture(ctx context.Context, device *coredevice.Device) ([]byte, error) {
	bulk, release := device.Bulk(ctx, false)
	defer release()
	input := map[string]any{"requestedFormat": "png"}
	output, err := bulk.Service(serviceName).Invoke(ctx, uuid.NewString(), feature, "", input)
	if err != nil && bulk != device && bulk.Err() != nil && ctx.Err() == nil {
		output, err = device.Service(serviceName).Invoke(ctx, uuid.NewString(), feature, "", input)
	}
	if err != nil {
		return nil, err
	}
	image, _ := output["image"].([]byte)
	if len(image) == 0 {
		return nil, E.New("screencapture: no image in reply")
	}
	return image, nil
}
