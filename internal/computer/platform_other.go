//go:build !windows && !(darwin && cgo)

package computer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
)

const thisComputer = "this computer"

var appBuild = runtime.GOOS

func directories() (data, caches string, err error) {
	config, err := os.UserConfigDir()
	if err != nil {
		return "", "", err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(config, "swipt"), filepath.Join(cache, "swipt"), nil
}

// diskImages is the Developer Disk Image's directory without Xcode: a copy of
// Xcode's DeveloperDiskImages/iOS_DDI/Restore.
func diskImages(data string) string {
	return filepath.Join(data, "DeveloperDiskImage")
}

// The API takes no token from this server, so it serves this computer only,
// and there is nothing to advertise.
func listenAddress(port int) string {
	return "127.0.0.1:" + strconv.Itoa(port)
}

func serverHost() string {
	return "127.0.0.1"
}

func addressUnavailable(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

func advertise(context.Context, int) (interface{ Close() }, error) {
	return noAdvertisement{}, nil
}

type noAdvertisement struct{}

func (noAdvertisement) Close() {}
