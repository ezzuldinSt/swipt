//go:build !windows

package usbmuxd

import (
	"context"
	"net"
)

const socketPath = "/var/run/usbmuxd"

func dial(ctx context.Context) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "unix", socketPath)
}
