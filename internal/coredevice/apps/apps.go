package apps

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"syscall"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/coredevice"
	"github.com/swipt-automation/swipt/internal/coredevice/xpc"

	"github.com/google/uuid"
	"howett.net/plist"
)

const (
	serviceName      = "com.apple.coredevice.appservice"
	streamFeature    = "com.apple.coredevice.feature.streamapplist"
	launchFeature    = "com.apple.coredevice.feature.launchapplication"
	processesFeature = "com.apple.coredevice.feature.listprocesses"
	signalFeature    = "com.apple.coredevice.feature.sendsignaltoprocess"

	errorCodeProcessUnknown = 10004
)

type App struct {
	BundleID      string
	Name          string
	Path          string
	Version       string
	BundleVersion string
}

// DDI 27B48 advertises both listapps and streamapplist on appservice (DDI
// 27B48 /Library/LaunchDaemons/com.apple.coredevice.dtappserviced.plist), and
// a DDI that offers streamapplist does not answer a non-streaming listapps
// request (pymobiledevice3 90b8d44,
// pymobiledevice3/remote/core_device/app_service.py:36-39). The input and the
// streaming side channel follow pymobiledevice3 90b8d44,
// pymobiledevice3/remote/core_device/app_service.py:41-54 and
// pymobiledevice3/remote/core_device/core_device_service.py:46-63, 95-135.
// An entry's version and bundleVersion carried CFBundleShortVersionString and
// CFBundleVersion, and some system apps had neither (iPhone18,2, iOS 27.2
// 24B115).
func List(ctx context.Context, device *coredevice.Device) ([]App, error) {
	conn, err := device.DialXPC(ctx, serviceName)
	if err != nil {
		return nil, E.Cause(err, "apps: list applications")
	}
	defer conn.Close()
	values, err := stream(ctx, conn, map[string]any{
		"includeAppClips":            false,
		"includeRemovableApps":       true,
		"includeHiddenApps":          false,
		"includeInternalApps":        false,
		"includeDefaultApps":         true,
		"requireContainerAccess":     false,
		"includeAppGroupIdentifiers": false,
		"includeContainerPaths":      false,
	})
	if err != nil {
		return nil, E.Cause(err, "apps: list applications")
	}
	result := make([]App, 0, len(values))
	for _, value := range values {
		entry, _ := value.(map[string]any)
		bundleID, hasBundleID := entry["bundleIdentifier"].(string)
		name, hasName := entry["name"].(string)
		if !hasBundleID || !hasName {
			return nil, E.New("apps: unexpected application entry: ", fmt.Sprint(value))
		}
		path, _ := entry["path"].(string)
		version, _ := entry["version"].(string)
		bundleVersion, _ := entry["bundleVersion"].(string)
		result = append(result, App{BundleID: bundleID, Name: name, Path: path, Version: version, BundleVersion: bundleVersion})
	}
	common.SortBy(result, func(app App) string { return strings.ToLower(app.Name) + "\x00" + app.BundleID })
	return result, nil
}

func stream(ctx context.Context, conn *xpc.Conn, input map[string]any) ([]any, error) {
	err := conn.SendContext(ctx, coredevice.FeatureRequest(uuid.NewString(), streamFeature, "", map[string]any{
		"actualInput": input,
		"streamProxy": map[string]any{"sideChannel": uuid.New()},
	}), true)
	if err != nil {
		return nil, err
	}
	var result []any
	for {
		message, err := conn.ReceiveMessage(ctx)
		if err != nil {
			return nil, err
		}
		if len(message.Body) == 0 {
			continue
		}
		status, ok := message.Body["CoreDevice.XPCMessageKey.sideChannelStatus"].(map[string]any)
		if !ok {
			if _, err = coredevice.FeatureOutput(streamFeature, message.Body); err != nil {
				return nil, err
			}
			return nil, E.New("missing stream status")
		}
		if failure, failed := status["receivedError"]; failed {
			return nil, E.New("stream failed: ", fmt.Sprint(failure))
		}
		if _, finished := status["finishStreaming"]; finished {
			return result, nil
		}
		pushing, _ := status["pushing"].(map[string]any)
		elements, ok := pushing["elements"].([]any)
		if !ok {
			return nil, E.New("unexpected stream status: ", fmt.Sprint(status))
		}
		result = append(result, elements...)
	}
}

// The request follows pymobiledevice3 90b8d44,
// pymobiledevice3/remote/core_device/app_service.py:57-84. LaunchOptions
// encodes payloadURL with the Encodable conformance of Foundation.URL (DDI
// 27B5028f CoreDeviceUtilities 0x396e18..0x396e50), which writes a keyed
// container of relative and an optional base (swift-foundation e577ad6,
// Sources/FoundationEssentials/URL/URL.swift:2048-2073).
//
// Launch answers the PID in the output's processToken, a token like those of
// the process list, or 0 when there is none; that token was not checked on a
// device.
func Launch(ctx context.Context, device *coredevice.Device, bundleID, payloadURL string, terminateExisting, animated bool) (uint64, error) {
	open := map[string]any{}
	if !animated {
		// platformSpecificOptions reach FrontBoard as open options, and SpringBoard
		// switches without its zoom for this key (SpringBoardServices, iOS 27 24A446;
		// iPhone18,2, iOS 27.2 24B115).
		open["__LaunchWithoutAnimation"] = true
	}
	platformOptions, _ := plist.Marshal(open, plist.XMLFormat)
	options := map[string]any{
		"arguments":                     []any{},
		"environmentVariables":          map[string]any{},
		"standardIOUsesPseudoterminals": true,
		"startStopped":                  false,
		"terminateExisting":             terminateExisting,
		"user":                          map[string]any{"shortName": "mobile"},
		"platformSpecificOptions":       platformOptions,
	}
	if payloadURL != "" {
		options["payloadURL"] = map[string]any{"relative": payloadURL}
	}
	input := map[string]any{
		"applicationSpecifier": map[string]any{
			"bundleIdentifier": map[string]any{"_0": bundleID},
		},
		"options":               options,
		"standardIOIdentifiers": map[string]any{},
	}
	var output map[string]any
	// A launch repeated on a new connection ends with the same app in front.
	err := device.Service(serviceName).Do(ctx, true, func(conn *xpc.Conn) error {
		var err error
		output, err = coredevice.InvokeFeature(ctx, conn, uuid.NewString(), launchFeature, "", input)
		// Relaunching an app whose previous process is still exiting fails with CoreDeviceError 10004, "The process
		// identifier of the launched application could not be determined" (iPhone18,2, iOS 27.2 24B115).
		if featureError, isFeatureError := errors.AsType[*coredevice.FeatureError](err); isFeatureError && terminateExisting && featureError.Code == errorCodeProcessUnknown {
			output, err = coredevice.InvokeFeature(ctx, conn, uuid.NewString(), launchFeature, "", input)
		}
		return err
	})
	if err != nil {
		return 0, E.Cause(err, "apps: launch ", bundleID)
	}
	return launchedPID(output), nil
}

func launchedPID(output map[string]any) uint64 {
	token, _ := output["processToken"].(map[string]any)
	return processIdentifier(token["processIdentifier"])
}

func processIdentifier(value any) uint64 {
	switch pid := value.(type) {
	case int64:
		if pid > 0 {
			return uint64(pid)
		}
	case uint64:
		return pid
	}
	return 0
}

type Process struct {
	PID        uint64
	Executable string
}

// idevice reads processIdentifier and an optional executableURL from each
// process token (idevice d32c818, idevice/src/services/core_device/app_service.rs:70-76,
// 302-322).
func Processes(ctx context.Context, device *coredevice.Device) ([]Process, error) {
	output, err := device.Service(serviceName).Invoke(ctx, uuid.NewString(), processesFeature, "", map[string]any{})
	if err != nil {
		return nil, E.Cause(err, "apps: list processes")
	}
	tokens, ok := output["processTokens"].([]any)
	if !ok {
		return nil, E.New("apps: list processes: unexpected output: ", fmt.Sprint(output))
	}
	result := make([]Process, 0, len(tokens))
	for _, value := range tokens {
		token, _ := value.(map[string]any)
		var process Process
		switch pid := token["processIdentifier"].(type) {
		case int64:
			process.PID = uint64(pid)
		case uint64:
			process.PID = pid
		default:
			return nil, E.New("apps: unexpected process token: ", fmt.Sprint(value))
		}
		if executableURL, ok := token["executableURL"].(map[string]any); ok {
			relative, _ := executableURL["relative"].(string)
			parsed, err := url.Parse(relative)
			if err != nil {
				return nil, E.Cause(err, "apps: parse the executable URL of process ", process.PID)
			}
			process.Executable = parsed.Path
		}
		result = append(result, process)
	}
	return result, nil
}

// idevice sends the signal number with a process token holding only
// processIdentifier (idevice d32c818,
// idevice/src/services/core_device/app_service.rs:343-359).
func Kill(ctx context.Context, device *coredevice.Device, pid uint64) error {
	_, err := device.Service(serviceName).Invoke(ctx, uuid.NewString(), signalFeature, "", map[string]any{
		"process": map[string]any{"processIdentifier": int64(pid)},
		"signal":  int64(syscall.SIGKILL),
	})
	if featureError, isFeatureError := errors.AsType[*coredevice.FeatureError](err); isFeatureError && featureError.CausedBy("NSPOSIXErrorDomain", int64(syscall.ESRCH)) {
		return nil
	}
	if err != nil {
		return E.Cause(err, "apps: kill process ", pid)
	}
	return nil
}
