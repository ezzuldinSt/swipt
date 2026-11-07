// The lockdownd session follows go-ios (ios/lockdown.go,
// ios/startsession.go; MIT, Copyright (c) 2019 danielpaulus).

package lockdown

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	"github.com/swipt-automation/swipt/internal/coredevice/rsd"

	"howett.net/plist"
)

const Port = 62078

const (
	// go-ios opens CoreDeviceProxy through lockdownd, then exchanges CDTunnel
	// parameters over that connection (go-ios ca2c56d,
	// ios/tunnel/tunnel_lockdown.go:15-28; ios/tunnel/tunnel.go:266-305).
	CoreDeviceProxy = "com.apple.internal.devicecompute.CoreDeviceProxy"

	// pymobiledevice3 reaches remotepairingdeviced's RPPairing control channel
	// through this lockdownd service, where pair-setup needs no consent on the
	// device (pymobiledevice3 9c33842,
	// pymobiledevice3/remote/tunnel_service.py:1398-1431).
	RemotePairing = "com.apple.dt.remotepairingdeviced.lockdown"
)

const progName = "swipt"

var ErrNotPairRecord = E.New("lockdown: not a pair record")

type PairRecord struct {
	HostID      string
	SystemBUID  string
	UDID        string
	Certificate tls.Certificate
	// WiFiMACAddress is the device's Wi-Fi address, such as
	// "66:83:3b:04:28:0f", which names its _apple-mobdev2._tcp instance.
	WiFiMACAddress string
}

// jitterbugpair writes the usbmuxd pair record with the device's UDID added
// (Jitterbug b0449f1, JitterbugPair/main.c:162-184; libimobiledevice c6f89de,
// common/userpref.c:322-341).
type pairRecordFile struct {
	HostID          string `plist:"HostID"`
	SystemBUID      string `plist:"SystemBUID"`
	UDID            string `plist:"UDID"`
	HostCertificate []byte `plist:"HostCertificate"`
	HostPrivateKey  []byte `plist:"HostPrivateKey"`
	WiFiMACAddress  string `plist:"WiFiMACAddress"`
}

func ParsePairRecord(data []byte) (*PairRecord, error) {
	var file pairRecordFile
	_, err := plist.Unmarshal(data, &file)
	if err != nil || file.HostID == "" || len(file.HostCertificate) == 0 {
		return nil, ErrNotPairRecord
	}
	certificate, err := tls.X509KeyPair(file.HostCertificate, file.HostPrivateKey)
	if err != nil {
		return nil, E.Cause(err, "load pair record certificate")
	}
	return &PairRecord{
		HostID:         file.HostID,
		SystemBUID:     file.SystemBUID,
		UDID:           file.UDID,
		Certificate:    certificate,
		WiFiMACAddress: file.WiFiMACAddress,
	}, nil
}

type Dialer func(ctx context.Context, port uint16) (net.Conn, error)

func GetValue(ctx context.Context, dial Dialer, key string) (any, error) {
	conn, err := dial(ctx, Port)
	if err != nil {
		return nil, E.Cause(err, "connect to lockdown")
	}
	defer conn.Close()
	reply, err := rsd.NewPlistConn(conn).Request(ctx, map[string]any{
		"Label":   progName,
		"Request": "GetValue",
		"Key":     key,
	})
	if err != nil {
		return nil, E.Cause(err, "lockdown GetValue ", key)
	}
	if message, failed := reply["Error"]; failed {
		return nil, E.New("lockdown GetValue ", key, ": ", fmt.Sprint(message))
	}
	return reply["Value"], nil
}

func DialService(ctx context.Context, record *PairRecord, dial Dialer, service string, logger logger.ContextLogger) (net.Conn, error) {
	tlsConfig := &tls.Config{
		Certificates:       []tls.Certificate{record.Certificate},
		InsecureSkipVerify: true,
	}
	conn, err := dial(ctx, Port)
	if err != nil {
		return nil, E.Cause(err, "connect to lockdown")
	}
	defer conn.Close()
	lockdown := rsd.NewPlistConn(conn)
	reply, err := lockdown.Request(ctx, map[string]any{
		"Label":           progName,
		"ProtocolVersion": "2",
		"Request":         "StartSession",
		"HostID":          record.HostID,
		"SystemBUID":      record.SystemBUID,
	})
	if err != nil {
		return nil, E.Cause(err, "lockdown StartSession")
	}
	if message, failed := reply["Error"]; failed {
		return nil, E.New("lockdown StartSession: ", fmt.Sprint(message))
	}
	if reply["EnableSessionSSL"] == true {
		tlsConn := tls.Client(conn, tlsConfig)
		err = tlsConn.HandshakeContext(ctx)
		if err != nil {
			return nil, E.Cause(err, "lockdown TLS")
		}
		lockdown = rsd.NewPlistConn(tlsConn)
	}
	reply, err = lockdown.Request(ctx, map[string]any{
		"Label":           progName,
		"ProtocolVersion": "2",
		"Request":         "StartService",
		"Service":         service,
	})
	if err != nil {
		return nil, E.Cause(err, "lockdown StartService ", service)
	}
	if message, failed := reply["Error"]; failed {
		return nil, E.New("lockdown StartService ", service, ": ", fmt.Sprint(message))
	}
	port, _ := reply["Port"].(uint64)
	if port == 0 || port > 65535 {
		return nil, E.New("lockdown StartService ", service, ": no port in ", fmt.Sprint(reply))
	}
	logger.Debug("started lockdown service ", service, " on port ", port)
	serviceConn, err := dial(ctx, uint16(port))
	if err != nil {
		return nil, E.Cause(err, "connect to ", service)
	}
	if reply["EnableServiceSSL"] == true {
		tlsConn := tls.Client(serviceConn, tlsConfig)
		err = tlsConn.HandshakeContext(ctx)
		if err != nil {
			serviceConn.Close()
			return nil, E.Cause(err, service, " TLS")
		}
		return tlsConn, nil
	}
	return serviceConn, nil
}
