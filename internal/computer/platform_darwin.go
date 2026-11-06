//go:build darwin && cgo

package computer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/swipt-automation/swipt/internal/api/discovery"
	"github.com/swipt-automation/swipt/internal/dnssd"
)

const (
	thisComputer = "this Mac"
	appBuild     = "macOS"
)

func directories() (data, caches string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(home, "Library/Application Support/Swipt"), filepath.Join(home, "Library/Caches/Swipt"), nil
}

// diskImages is where Xcode keeps the Developer Disk Image.
func diskImages(string) string {
	return "/Library/Developer/DeveloperDiskImages/iOS_DDI/Restore"
}

func listenAddress(port int) string {
	return ":" + strconv.Itoa(port)
}

func serverHost() string {
	return LocalHostName() + ".local"
}

func addressUnavailable(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

func advertise(ctx context.Context, port int) (interface{ Close() }, error) {
	advertisement, err := dnssd.Advertise(ctx, Name(), discovery.ServiceType, uint16(port), nil)
	if err != nil {
		return nil, err
	}
	return advertisement, nil
}
