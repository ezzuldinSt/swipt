package computer

import (
	"context"
	"net"
	"slices"
	"strings"
	"unicode"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/coredevice"
	"github.com/swipt-automation/swipt/internal/coredevice/lockdown"
	"github.com/swipt-automation/swipt/internal/device"
	"github.com/swipt-automation/swipt/internal/log"
	"github.com/swipt-automation/swipt/internal/rfb"
	"github.com/swipt-automation/swipt/internal/usbmuxd"
)

const connectionUSB = "USB"

var ErrDeviceNotListed = E.New("device not listed by usbmuxd")

type pairedDevice struct {
	UDID string
	Name string
}

func preferUSB(devices []usbmuxd.Device) []usbmuxd.Device {
	var result []usbmuxd.Device
	for _, it := range devices {
		index := slices.IndexFunc(result, func(known usbmuxd.Device) bool { return known.UDID == it.UDID })
		switch {
		case index < 0:
			result = append(result, it)
		case it.Connection == connectionUSB:
			result[index] = it
		}
	}
	return result
}

func dialer(id uint64) lockdown.Dialer {
	return func(ctx context.Context, port uint16) (net.Conn, error) {
		return usbmuxd.Dial(ctx, id, port)
	}
}

func chooseDevice(ctx context.Context, given string) (pairedDevice, error) {
	listed, err := usbmuxd.ListDevices(ctx)
	if err != nil {
		return pairedDevice{}, err
	}
	var devices []pairedDevice
	var unread []error
	for _, found := range preferUSB(listed) {
		value, err := lockdown.GetValue(ctx, dialer(found.ID), "DeviceName")
		if err != nil {
			if ctx.Err() != nil {
				return pairedDevice{}, err
			}
			err = E.Cause(err, "read the name of ", found.UDID)
			if strings.EqualFold(found.UDID, given) {
				return pairedDevice{}, err
			}
			unread = append(unread, err)
			continue
		}
		name, _ := value.(string)
		devices = append(devices, pairedDevice{UDID: found.UDID, Name: name})
	}
	if given != "" {
		devices = common.Filter(devices, func(it pairedDevice) bool {
			return strings.EqualFold(it.UDID, given) || strings.EqualFold(label(it.Name), label(given))
		})
	}
	switch len(devices) {
	case 1:
		return devices[0], nil
	case 0:
		if len(unread) > 0 {
			return pairedDevice{}, E.Errors(unread...)
		}
		if given != "" {
			return pairedDevice{}, E.Extend(ErrDeviceNotListed, given)
		}
		return pairedDevice{}, ErrDeviceNotListed
	}
	names := common.Map(devices, func(it pairedDevice) string { return it.Name + " (" + it.UDID + ")" })
	return pairedDevice{}, E.New("more than one device is connected to ", thisComputer, ": ", strings.Join(names, ", "), "; choose one with -d NAME|UDID")
}

func label(name string) string {
	var parts []string
	var part strings.Builder
	for _, character := range strings.NewReplacer("'", "", "’", "").Replace(name) {
		if character < unicode.MaxASCII && (unicode.IsLetter(character) || unicode.IsDigit(character)) {
			part.WriteRune(character)
			continue
		}
		if part.Len() > 0 {
			parts = append(parts, part.String())
			part.Reset()
		}
	}
	if part.Len() > 0 {
		parts = append(parts, part.String())
	}
	if len(parts) == 0 {
		return name
	}
	return strings.Join(parts, "-")
}

type lockdownPeer struct {
	device usbmuxd.Device
	record *lockdown.PairRecord
}

func findLockdownPeer(ctx context.Context, udid string) (lockdownPeer, error) {
	devices, err := usbmuxd.ListDevices(ctx)
	if err != nil {
		return lockdownPeer{}, err
	}
	found := common.Find(preferUSB(devices), func(it usbmuxd.Device) bool { return it.UDID == udid })
	if found.ID == 0 {
		return lockdownPeer{}, &device.Error{Problem: device.ProblemNoTunnel, Err: E.New("the device is not connected to ", thisComputer)}
	}
	data, err := usbmuxd.ReadPairRecord(ctx, udid)
	if err != nil {
		return lockdownPeer{}, E.Cause(err, "read the pair record")
	}
	record, err := lockdown.ParsePairRecord(data)
	if err != nil {
		return lockdownPeer{}, err
	}
	record.UDID = udid
	return lockdownPeer{device: found, record: record}, nil
}

func connect(udid string, logger log.ContextLogger) func(ctx context.Context) (*coredevice.Device, error) {
	return func(ctx context.Context) (*coredevice.Device, error) {
		peer, err := findLockdownPeer(ctx, udid)
		if err != nil {
			return nil, err
		}
		logger.Debug("connecting through ", peer.device.Connection, " as usbmuxd device ", peer.device.ID)
		return coredevice.ConnectLockdown(ctx, coredevice.LockdownOptions{Record: peer.record, Dial: dialer(peer.device.ID), Logger: logger})
	}
}

type observer struct{}

func (observer) StateChanged(device.State, error) {}

func (observer) DeviceConnected(coredevice.DeviceInfo) {}

func (observer) ViewerConnected(*rfb.ServerSession) {}

func (observer) ViewerDisconnected(*rfb.ServerSession, error) {}

func (observer) StreamingChanged(bool) {}
