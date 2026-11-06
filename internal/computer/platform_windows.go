package computer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/windows"
)

const (
	thisComputer = "this PC"
	appBuild     = "Windows"
)

func directories() (data, caches string, err error) {
	local, err := os.UserCacheDir()
	if err != nil {
		return "", "", err
	}
	data = filepath.Join(local, "Swipt")
	return data, filepath.Join(data, "Caches"), nil
}

// diskImages is the Developer Disk Image's directory without Xcode: a copy of
// Xcode's DeveloperDiskImages/iOS_DDI/Restore.
func diskImages(data string) string {
	return filepath.Join(data, "DeveloperDiskImage")
}

// The API takes no token from this server, so it serves this PC only, and
// there is nothing to advertise.
func listenAddress(port int) string {
	return "127.0.0.1:" + strconv.Itoa(port)
}

func serverHost() string {
	return "127.0.0.1"
}

// A port in a range that Windows reserves (netsh interface ipv4 show
// excludedportrange protocol=tcp) is refused with WSAEACCES.
func addressUnavailable(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE) || errors.Is(err, windows.WSAEACCES)
}

func advertise(context.Context, int) (interface{ Close() }, error) {
	return noAdvertisement{}, nil
}

type noAdvertisement struct{}

func (noAdvertisement) Close() {}
