package usbmuxd

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"strconv"

	E "github.com/sagernet/sing/common/exceptions"

	"howett.net/plist"
)

const (
	protocolVersion = 1
	messagePlist    = 8
	progName        = "swipt"
)

type Device struct {
	ID         uint64
	UDID       string
	Connection string
	// Address is a Network device's address on the local network, when
	// usbmuxd lists one.
	Address netip.Addr
}

// usbmuxd lists a Network device's NetworkAddress as a raw sockaddr, beside
// its InterfaceIndex. Apple Mobile Device Service on Windows wrote a Windows
// sockaddr_in, a little-endian family first: 02 00, the port 00 00, then
// c0 a8 01 65 (iPhone18,2, iOS 27.0.1 24A446 on Wi-Fi). A BSD sockaddr, as on
// macOS, starts with its length and then a one-byte family.
const (
	familyInet       = 2
	familyInet6BSD   = 30
	familyInet6Linux = 10
	familyInet6Win   = 23
)

func parseNetworkAddress(data []byte, interfaceIndex uint64) netip.Addr {
	if len(data) < 2 {
		return netip.Addr{}
	}
	family := int(data[1])
	if family == 0 {
		family = int(data[0])
	}
	var address netip.Addr
	switch family {
	case familyInet:
		if len(data) < 8 {
			return netip.Addr{}
		}
		address = netip.AddrFrom4([4]byte(data[4:8]))
	case familyInet6BSD, familyInet6Linux, familyInet6Win:
		if len(data) < 24 {
			return netip.Addr{}
		}
		address = netip.AddrFrom16([16]byte(data[8:24])).Unmap()
		if address.Is6() && address.IsLinkLocalUnicast() {
			zone := interfaceIndex
			if len(data) >= 28 {
				if scope := binary.LittleEndian.Uint32(data[24:28]); scope != 0 {
					zone = uint64(scope)
				}
			}
			if zone != 0 {
				address = address.WithZone(strconv.FormatUint(zone, 10))
			}
		}
	default:
		return netip.Addr{}
	}
	if !address.IsValid() || address.IsUnspecified() || address.IsLoopback() {
		return netip.Addr{}
	}
	return address
}

type header struct {
	Length  uint32
	Version uint32
	Type    uint32
	Tag     uint32
}

func open(ctx context.Context, message map[string]any) (net.Conn, map[string]any, error) {
	conn, err := dial(ctx)
	if err != nil {
		return nil, nil, E.Cause(err, "usbmuxd: connect")
	}
	reply, err := request(ctx, conn, message)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, reply, nil
}

func request(ctx context.Context, conn net.Conn, message map[string]any) (map[string]any, error) {
	message["ProgName"] = progName
	message["ClientVersionString"] = progName
	body, err := plist.Marshal(message, plist.XMLFormat)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	var frame bytes.Buffer
	_ = binary.Write(&frame, binary.LittleEndian, header{Length: uint32(16 + len(body)), Version: protocolVersion, Type: messagePlist, Tag: 1})
	frame.Write(body)
	if _, err = conn.Write(frame.Bytes()); err != nil {
		return nil, E.Cause(err, "usbmuxd: send ", message["MessageType"])
	}
	var reply header
	if err = binary.Read(conn, binary.LittleEndian, &reply); err != nil {
		return nil, E.Cause(err, "usbmuxd: read ", message["MessageType"])
	}
	if reply.Length < 16 {
		return nil, E.New("usbmuxd: bad reply length ", reply.Length)
	}
	payload := make([]byte, reply.Length-16)
	if _, err = io.ReadFull(conn, payload); err != nil {
		return nil, E.Cause(err, "usbmuxd: read ", message["MessageType"])
	}
	var result map[string]any
	if _, err = plist.Unmarshal(payload, &result); err != nil {
		return nil, E.Cause(err, "usbmuxd: decode ", message["MessageType"])
	}
	if number, failed := result["Number"].(uint64); failed && number != 0 {
		return nil, E.New("usbmuxd: ", message["MessageType"], " failed with ", number)
	}
	return result, nil
}

func ListDevices(ctx context.Context) ([]Device, error) {
	conn, reply, err := open(ctx, map[string]any{"MessageType": "ListDevices"})
	if err != nil {
		return nil, err
	}
	_ = conn.Close()
	entries, _ := reply["DeviceList"].([]any)
	var devices []Device
	for _, entry := range entries {
		attached, _ := entry.(map[string]any)
		properties, _ := attached["Properties"].(map[string]any)
		id, _ := attached["DeviceID"].(uint64)
		udid, _ := properties["SerialNumber"].(string)
		connection, _ := properties["ConnectionType"].(string)
		if id == 0 || udid == "" {
			continue
		}
		networkAddress, _ := properties["NetworkAddress"].([]byte)
		interfaceIndex, _ := properties["InterfaceIndex"].(uint64)
		devices = append(devices, Device{
			ID: id, UDID: udid, Connection: connection,
			Address: parseNetworkAddress(networkAddress, interfaceIndex),
		})
	}
	return devices, nil
}

func ReadPairRecord(ctx context.Context, udid string) ([]byte, error) {
	conn, reply, err := open(ctx, map[string]any{"MessageType": "ReadPairRecord", "PairRecordID": udid})
	if err != nil {
		return nil, err
	}
	_ = conn.Close()
	data, _ := reply["PairRecordData"].([]byte)
	if len(data) == 0 {
		return nil, E.New("usbmuxd: no pair record for ", udid)
	}
	return data, nil
}

func ReadBUID(ctx context.Context) (string, error) {
	conn, reply, err := open(ctx, map[string]any{"MessageType": "ReadBUID"})
	if err != nil {
		return "", err
	}
	_ = conn.Close()
	buid, _ := reply["BUID"].(string)
	if buid == "" {
		return "", E.New("usbmuxd: no BUID")
	}
	return buid, nil
}

func SavePairRecord(ctx context.Context, device uint64, udid string, record []byte) error {
	conn, _, err := open(ctx, map[string]any{"MessageType": "SavePairRecord", "PairRecordID": udid, "DeviceID": device, "PairRecordData": record})
	if err != nil {
		return err
	}
	return conn.Close()
}

func Dial(ctx context.Context, device uint64, port uint16) (net.Conn, error) {
	conn, _, err := open(ctx, map[string]any{"MessageType": "Connect", "DeviceID": device, "PortNumber": port<<8 | port>>8})
	return conn, err
}
