//go:build !unix

package media

import (
	"net"
	"net/netip"

	N "github.com/sagernet/sing/common/network"
)

// Without a connect on the socket, the leg has no batch writer, and
// serverLeg.send writes each datagram to its target.
func connectLeg(*net.UDPConn, netip.AddrPort) (N.ConnectedPacketBatchWriter, error) {
	return nil, nil
}
