//go:build !windows

package computer

import "net"

func acknowledgeEach(net.Conn) {}
