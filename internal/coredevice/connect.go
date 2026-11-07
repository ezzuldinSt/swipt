package coredevice

import (
	"context"
	"net"
	"net/netip"
	"strings"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	"github.com/swipt-automation/swipt/internal/coredevice/lockdown"
	"github.com/swipt-automation/swipt/internal/coredevice/pairing"
	"github.com/swipt-automation/swipt/internal/coredevice/tlspsk"
	"github.com/swipt-automation/swipt/internal/coredevice/tunnel"

	"github.com/google/uuid"
)

// vphone guests are built with chip ID 0xFE01, which their UDID starts with
// (vphone-cli 8c3630c7, Research/Guest/xcode_install_signature_gate.md).
func IsGuest(udid string) bool {
	return strings.HasPrefix(udid, "0000FE01-")
}

func tunnelMTU(udid string) int {
	if IsGuest(udid) {
		return tunnel.GuestMTU
	}
	return tunnel.DefaultMTU
}

type RemotePairingOptions struct {
	Address netip.AddrPort
	Record  *pairing.Record
	Logger  logger.ContextLogger
}

var ErrLoopbackAddress = E.New("remote pairing through a loopback address is refused: " +
	"it leaves the device's remotepairingd unable to set up tunnels until Developer Mode is turned off and on again; " +
	"use the VPN's loopback address (such as 10.7.0.1) instead")

func ConnectRemotePairing(ctx context.Context, options RemotePairingOptions) (*Device, error) {
	if address := options.Address.Addr().Unmap(); address.IsLoopback() || address.IsUnspecified() {
		return nil, ErrLoopbackAddress
	}
	if options.Logger == nil {
		options.Logger = logger.NOP()
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", options.Address.String())
	if err != nil {
		return nil, E.Cause(err, "connect to remote pairing")
	}
	session, err := pairing.Verify(ctx, conn, options.Record, options.Logger)
	if err != nil {
		return nil, err
	}
	port, err := session.CreateListener(ctx)
	if err != nil {
		session.Close()
		return nil, err
	}
	options.Logger.Debug("remote pairing listener: ", port)
	tunnelConn, err := dialer.DialContext(ctx, "tcp", netip.AddrPortFrom(options.Address.Addr(), port).String())
	if err != nil {
		session.Close()
		return nil, E.Cause(err, "connect to tunnel port")
	}
	secureConn, err := tlspsk.Client(ctx, tunnelConn, session.Key())
	if err != nil {
		session.Close()
		return nil, E.Cause(err, "tunnel TLS")
	}
	tun, err := tunnel.Open(ctx, secureConn, tunnelMTU(options.Record.DeviceUDID), options.Logger)
	if err != nil {
		session.Close()
		return nil, err
	}
	return newDevice(ctx, tun, session, handshakeIdentity(options.Record.Identifier), options.Logger, nil)
}

type LockdownOptions struct {
	Address netip.Addr
	Dial    lockdown.Dialer
	Record  *lockdown.PairRecord
	Logger  logger.ContextLogger
	// Bulk lets the display stream and screenshots go through a second
	// tunnel, made the same way when they need it (see Device.Bulk).
	Bulk bool
}

func ConnectLockdown(ctx context.Context, options LockdownOptions) (*Device, error) {
	if options.Logger == nil {
		options.Logger = logger.NOP()
	}
	dial := options.Dial
	if dial == nil {
		dial = func(ctx context.Context, port uint16) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", netip.AddrPortFrom(options.Address, port).String())
		}
	}
	identity := handshakeIdentity(options.Record.HostID)
	return connectLockdown(ctx, options.Record, dial, identity, options.Logger, options.Bulk)
}

func connectLockdown(ctx context.Context, record *lockdown.PairRecord, dial lockdown.Dialer, identity uuid.UUID, logger logger.ContextLogger, bulk bool) (*Device, error) {
	conn, err := lockdown.DialService(ctx, record, dial, lockdown.CoreDeviceProxy, logger)
	if err != nil {
		return nil, err
	}
	tun, err := tunnel.Open(ctx, conn, tunnelMTU(record.UDID), logger)
	if err != nil {
		return nil, err
	}
	var connectBulk func(ctx context.Context) (*Device, error)
	if bulk {
		connectBulk = func(ctx context.Context) (*Device, error) {
			return connectLockdown(ctx, record, dial, identity, logger, false)
		}
	}
	return newDevice(ctx, tun, nil, identity, logger, connectBulk)
}

// pymobiledevice3 sends a stable host UUID in RSD handshakes
// (pymobiledevice3 9c33842, pymobiledevice3/remote/remotexpc.py:82-110).
func handshakeIdentity(name string) uuid.UUID {
	identity, err := uuid.Parse(name)
	if err != nil {
		identity = uuid.NewSHA1(uuid.NameSpaceOID, []byte("swipt:"+name))
	}
	return identity
}
