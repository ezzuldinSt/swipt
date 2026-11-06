package display

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
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

// Info is one answer to getdisplayinfo, which reports the displays and their
// backlight together.
type Info struct {
	Displays []DisplayInfo
	output   map[string]any
}

// Backlight reports the primary integrated display's backlight state: off,
// inactiveOn, activeOn or dimmed.
func (i *Info) Backlight() (string, error) {
	return primaryBacklightState(i.output)
}

// Info asks the device for its displays on a kept connection. Callers that ask
// while a request is under way share the next one, and a caller gets the
// answer of a request made after it asked.
func (c *Client) Info(ctx context.Context) (*Info, error) {
	ctx, cancel := c.operationContext(ctx)
	defer cancel()
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	asked := time.Now()
	select {
	case c.infoTurn <- struct{}{}:
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
	defer func() { <-c.infoTurn }()
	if c.info != nil && c.infoAsked.After(asked) {
		return c.info, nil
	}
	started := time.Now()
	output, err := c.device.Service(deviceInfoServiceName).Invoke(ctx, c.deviceIdentifier, featureDisplayInfo, "", map[string]any{})
	if err != nil {
		return nil, err
	}
	info := &Info{Displays: displaysOf(output), output: output}
	c.info, c.infoAsked = info, started
	return info, nil
}

func (c *Client) BacklightState(ctx context.Context) (string, error) {
	info, err := c.Info(ctx)
	if err != nil {
		return "", err
	}
	return info.Backlight()
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
	info, err := c.Info(ctx)
	if err != nil {
		return nil, err
	}
	return slices.Clone(info.Displays), nil
}

func displaysOf(output map[string]any) []DisplayInfo {
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
	})
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
