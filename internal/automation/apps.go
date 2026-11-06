package automation

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/coredevice/apps"
	"github.com/swipt-automation/swipt/internal/device"
)

type appInfo struct {
	BundleID string `json:"bundleId"`
	Name     string `json:"name"`
	Version  string `json:"version,omitempty"`
	Build    string `json:"build,omitempty"`
}

func fileURL(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

func (h *Host) listApps(ctx context.Context, phone *device.Phone) ([]apps.App, error) {
	list, err := phone.Apps(ctx)
	if err != nil {
		return nil, err
	}
	for _, it := range list {
		h.appVersions.Store(it.BundleID, it.Version+"+"+it.BundleVersion)
	}
	return list, nil
}

func deviceApps(ctx context.Context, call *operation) (any, error) {
	phone, err := call.phone()
	if err != nil {
		return nil, err
	}
	list, err := call.engine.host.listApps(ctx, phone)
	if err != nil {
		return nil, err
	}
	return common.Map(list, func(it apps.App) appInfo {
		return appInfo{BundleID: it.BundleID, Name: it.Name, Version: it.Version, Build: it.BundleVersion}
	}), nil
}

func deviceAppIcon(ctx context.Context, call *operation) (any, error) {
	var bundleID string
	if err := call.decode(0, &bundleID); err != nil {
		return nil, err
	}
	phone, err := call.phone()
	if err != nil {
		return nil, err
	}
	host := call.engine.host
	version, known := host.appVersions.Load(bundleID)
	if !known {
		if _, err = host.listApps(ctx, phone); err != nil {
			return nil, err
		}
		if version, known = host.appVersions.Load(bundleID); !known {
			return nil, E.Cause1(ErrNotFound, E.New("there is no app ", strconv.Quote(bundleID)))
		}
	}
	directory := filepath.Join(call.engine.caches, "AppIcons")
	path := filepath.Join(directory, strings.NewReplacer("/", "_", ":", "_").Replace(bundleID+"@"+version.(string)+".png"))
	if _, err = os.Stat(path); err == nil {
		return fileURL(path), nil
	}
	data, err := phone.AppIcon(ctx, bundleID)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(directory, 0o755); err != nil {
		return nil, err
	}
	if err = writeAtomically(path, data); err != nil {
		return nil, err
	}
	return fileURL(path), nil
}
