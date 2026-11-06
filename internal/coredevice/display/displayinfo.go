package display

import (
	"context"
	"strconv"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/coredevice"
)

// pymobiledevice3 sends getdisplayinfo to deviceinfo with no action
// identifier (pymobiledevice3 1146717,
// pymobiledevice3/remote/core_device/device_info.py:12, 23-27).
const (
	deviceInfoServiceName = "com.apple.coredevice.deviceinfo"
	featureDisplayInfo    = "com.apple.coredevice.feature.getdisplayinfo"
)

type DisplayInfo struct {
	ID            int
	Primary       bool
	Width, Height int
	// devicectl 651.13.2 reported nativeSize=[2420,1668] and
	// nativeOrientation=rot270 (observed on iPad17,1).
	NativeWidth, NativeHeight int
	CurrentOrientation        string
	PointScale                int
}

func (c *Client) BacklightState(ctx context.Context) (string, error) {
	ctx, cancel := c.operationContext(ctx)
	defer cancel()
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
	conn, err := c.device.DialXPC(ctx, deviceInfoServiceName)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	output, err := coredevice.InvokeFeature(ctx, conn, c.deviceIdentifier, featureDisplayInfo, "", map[string]any{})
	if err != nil {
		return "", err
	}
	return primaryBacklightState(output)
}

func primaryBacklightState(output map[string]any) (string, error) {
	var selected map[string]any
	for _, value := range array(output["displays"]) {
		display := dictionary(value)
		primary, ok := boolValue(display["primary"])
		if !ok {
			return "", E.New("the device did not identify its primary display")
		}
		if !primary {
			continue
		}
		external, ok := boolValue(display["external"])
		if !ok {
			return "", E.New("the device did not report whether its primary display is external")
		}
		if external {
			continue
		}
		if selected != nil {
			return "", E.New("the device reported multiple primary integrated displays")
		}
		selected = display
	}
	if selected == nil {
		return "", E.New("the device did not report a primary integrated display")
	}
	// The aggregate state can remain activeOn while the integrated display is
	// inactiveOn (Always-On Display). CoreDeviceUtilities names
	// BLSBacklightState 3 activeDimmed, and answers unknown for any other state
	// or a display it has no backlight of (DDI 27B5028f CoreDeviceUtilities
	// 0x37298c, 0x35aecc, 0x372bbc).
	state, _ := selected["backlightState"].(string)
	if state == "" {
		// iOS 27.0 reports the aggregate state only (iPhone18,2, iOS 27.0.1
		// 24A446).
		state, _ = output["backlightState"].(string)
	}
	switch state {
	case "off", "inactiveOn", "activeOn":
		return state, nil
	case "activeDimmed":
		return "dimmed", nil
	case "":
		return "", E.New("the device did not report its primary integrated display's backlight state")
	}
	return "", E.New("the device reported the backlight state ", strconv.Quote(state), " for its primary integrated display")
}

func (c *Client) Displays(ctx context.Context) ([]DisplayInfo, error) {
	ctx, cancel := c.operationContext(ctx)
	defer cancel()
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	conn, err := c.device.DialXPC(ctx, deviceInfoServiceName)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	output, err := coredevice.InvokeFeature(ctx, conn, c.deviceIdentifier, featureDisplayInfo, "", map[string]any{})
	if err != nil {
		return nil, err
	}
	return common.Map(array(output["displays"]), func(value any) DisplayInfo {
		display := dictionary(value)
		info := DisplayInfo{
			ID:                 intField(display, "displayId"),
			CurrentOrientation: stringValue(display["currentOrientation"]),
			PointScale:         intField(display, "pointScale"),
		}
		info.Primary, _ = boolValue(display["primary"])
		info.Width, info.Height = sizeValue(dictionary(display["currentMode"])["size"])
		info.NativeWidth, info.NativeHeight = sizeValue(display["nativeSize"])
		return info
	}), nil
}

func sizeValue(value any) (int, int) {
	size := array(value)
	if len(size) != 2 {
		return 0, 0
	}
	width, _ := floatValue(size[0])
	height, _ := floatValue(size[1])
	return int(width), int(height)
}
