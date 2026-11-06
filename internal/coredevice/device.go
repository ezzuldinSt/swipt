package coredevice

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/swipt-automation/swipt/internal/coredevice/pairing"
	"github.com/swipt-automation/swipt/internal/coredevice/rsd"
	"github.com/swipt-automation/swipt/internal/coredevice/tunnel"
	"github.com/swipt-automation/swipt/internal/coredevice/xpc"

	"github.com/google/uuid"
)

const checkinLabel = "swipt"

type DeviceInfo struct {
	UDID         string
	ProductType  string
	OSVersion    string
	BuildVersion string
	DeviceClass  string
	ECID         uint64
	Properties   map[string]any
}

type Device struct {
	tunnel    *tunnel.Tunnel
	session   *pairing.Session
	rsdConn   *xpc.Conn
	services  map[string]uint16
	info      DeviceInfo
	logger    logger.ContextLogger
	done      chan struct{}
	closeOnce sync.Once
	err       error

	servicesAccess sync.Mutex
	keptServices   map[string]*XPCService
}

func newDevice(ctx context.Context, tun *tunnel.Tunnel, session *pairing.Session, identity uuid.UUID, logger logger.ContextLogger) (*Device, error) {
	d := &Device{
		tunnel:  tun,
		session: session,
		logger:  logger,
		done:    make(chan struct{}),
	}
	err := d.handshake(ctx, identity)
	if err != nil {
		d.fail(err)
		return nil, err
	}
	go d.watch()
	return d, nil
}

func (d *Device) handshake(ctx context.Context, identity uuid.UUID) error {
	port := d.tunnel.Parameters().ServerRSDPort
	if port == 0 {
		port = rsd.Port
	}
	conn, err := d.tunnel.DialTCP(ctx, port)
	if err != nil {
		return E.Cause(err, "connect to RSD")
	}
	d.rsdConn, err = xpc.NewConn(ctx, conn, d.logger)
	if err != nil {
		return E.Cause(err, "connect to RSD")
	}
	peer, err := rsd.Handshake(ctx, d.rsdConn, identity)
	if err != nil {
		return err
	}
	d.services = peer.Services
	properties := peer.Properties
	d.info = DeviceInfo{Properties: properties}
	d.info.UDID, _ = properties["UniqueDeviceID"].(string)
	d.info.ProductType, _ = properties["ProductType"].(string)
	d.info.OSVersion, _ = properties["OSVersion"].(string)
	d.info.BuildVersion, _ = properties["BuildVersion"].(string)
	d.info.DeviceClass, _ = properties["DeviceClass"].(string)
	switch ecid := properties["UniqueChipID"].(type) {
	case uint64:
		d.info.ECID = ecid
	case int64:
		d.info.ECID = uint64(ecid)
	}
	d.logger.Debug("connected to device ", d.info.UDID, ": ", d.info.ProductType, " ", d.info.OSVersion,
		", protocol version ", peer.MessagingProtocolVersion, ", ", len(d.services), " services")
	return nil
}

func (d *Device) watch() {
	if d.session != nil {
		go func() {
			err := d.session.Wait()
			d.fail(E.Cause(err, "remote pairing connection closed"))
		}()
	}
	go func() {
		for {
			message, err := d.rsdConn.ReceiveMessage(context.Background())
			if err != nil {
				d.fail(E.Cause(err, "RSD connection closed"))
				return
			}
			// The device sent StartTls with YesNo=false and TlsPolicy=1 in a
			// captured RSD exchange (observed on iPad17,1, iPadOS 27.2 24B5099f).
			if message.Body["MessageType"] == "StartTls" {
				if message.Body["YesNo"] == true {
					d.logger.Warn("the device asks for TLS on the RSD connection, which is not supported: ", fmt.Sprint(message.Body))
					continue
				}
				err = rsd.DeclineTLS(d.rsdConn)
				if err != nil {
					d.fail(err)
					return
				}
				continue
			}
			d.logger.Debug("RSD message, flags ", message.Flags, ": ", fmt.Sprint(message.Body))
		}
	}()
	select {
	case <-d.tunnel.Done():
		d.fail(E.Cause(d.tunnel.Err(), "tunnel closed"))
	case <-d.done:
	}
}

func (d *Device) fail(err error) {
	d.closeOnce.Do(func() {
		d.logger.Debug("device connection closed: ", err)
		d.err = err
		close(d.done)
		d.closeServices()
		if d.rsdConn != nil {
			d.rsdConn.Close()
		}
		if d.session != nil {
			d.session.Close()
		}
		d.tunnel.Close()
	})
}

func (d *Device) Address() netip.Addr {
	return d.tunnel.Parameters().ServerAddress
}

func (d *Device) LocalAddress() netip.Addr {
	return d.tunnel.Parameters().ClientAddress
}

func (d *Device) TransportAddr() (local netip.AddrPort, remote netip.AddrPort) {
	return M.AddrPortFromNet(d.tunnel.LocalAddr()), M.AddrPortFromNet(d.tunnel.RemoteAddr())
}

func (d *Device) Info() DeviceInfo {
	return d.info
}

func (d *Device) HasService(name string) bool {
	_, found := d.services[name]
	return found
}

func (d *Device) DialService(ctx context.Context, name string) (net.Conn, error) {
	port, found := d.services[name]
	if !found {
		return nil, E.New("coredevice: the device does not offer ", name)
	}
	conn, err := d.tunnel.DialTCP(ctx, port)
	if err != nil {
		return nil, E.Cause(err, "connect to ", name)
	}
	return conn, nil
}

func (d *Device) DialXPC(ctx context.Context, name string) (*xpc.Conn, error) {
	conn, err := d.DialService(ctx, name)
	if err != nil {
		return nil, err
	}
	xpcConn, err := xpc.NewConn(ctx, conn, d.logger)
	if err != nil {
		return nil, E.Cause(err, "connect to ", name)
	}
	return xpcConn, nil
}

func (d *Device) DialShim(ctx context.Context, name string) (*rsd.PlistConn, error) {
	conn, err := d.DialService(ctx, name)
	if err != nil {
		return nil, err
	}
	plistConn := rsd.NewPlistConn(conn)
	err = plistConn.Checkin(ctx, checkinLabel)
	if err != nil {
		conn.Close()
		return nil, E.Cause(err, "connect to ", name)
	}
	return plistConn, nil
}

func (d *Device) ListenUDP(port uint16, options N.ReadWaitOptions) (*tunnel.UDPConn, error) {
	return d.tunnel.ListenUDP(port, options)
}

func (d *Device) Close() error {
	d.fail(net.ErrClosed)
	return nil
}

func (d *Device) Done() <-chan struct{} {
	return d.done
}

func (d *Device) Closed() bool {
	select {
	case <-d.tunnel.Done():
		return true
	default:
		return false
	}
}

func (d *Device) Err() error {
	select {
	case <-d.done:
		return d.err
	default:
		return nil
	}
}
