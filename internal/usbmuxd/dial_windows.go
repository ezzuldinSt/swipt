package usbmuxd

import (
	"context"
	"net"
)

// Apple Mobile Device Service, installed with iTunes or Apple Devices, serves
// usbmuxd on this port of the loopback interface.
const serviceAddress = "127.0.0.1:27015"

func dial(ctx context.Context) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", serviceAddress)
}
