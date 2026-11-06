package usbmuxd

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"

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
		devices = append(devices, Device{ID: id, UDID: udid, Connection: connection})
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
