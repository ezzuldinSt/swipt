package computer

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// knownFile names, in the data directory, the devices this computer reached,
// with the address each answered at last, so that a device on Wi-Fi is
// reached again while usbmuxd does not list it: Apple Mobile Device Service
// stopped listing an iPhone18,2 on Wi-Fi for 30 and 45 minutes, while it
// answered pings and advertised _apple-mobdev2._tcp (iOS 27.0.1 24A446).
const knownFile = "devices.json"

type knownDevice struct {
	UDID      string    `json:"udid"`
	Name      string    `json:"name,omitempty"`
	Address   string    `json:"address,omitempty"`
	Connected time.Time `json:"connected"`
}

func (d knownDevice) address() netip.Addr {
	address, _ := netip.ParseAddr(d.Address)
	return address
}

type knownDevices struct {
	path   string
	access sync.Mutex
}

// newKnownDevices keeps the devices in directory; without one, it keeps none.
func newKnownDevices(directory string) *knownDevices {
	if directory == "" {
		return &knownDevices{}
	}
	return &knownDevices{path: filepath.Join(directory, knownFile)}
}

type knownFileContent struct {
	Devices []knownDevice `json:"devices"`
}

func (k *knownDevices) load() []knownDevice {
	if k == nil || k.path == "" {
		return nil
	}
	data, err := os.ReadFile(k.path)
	if err != nil {
		return nil
	}
	var content knownFileContent
	if json.Unmarshal(data, &content) != nil {
		return nil
	}
	return slices.DeleteFunc(content.Devices, func(it knownDevice) bool { return it.UDID == "" })
}

func (k *knownDevices) all() []knownDevice {
	if k == nil {
		return nil
	}
	k.access.Lock()
	defer k.access.Unlock()
	return k.load()
}

func (k *knownDevices) find(udid string) (knownDevice, bool) {
	for _, known := range k.all() {
		if strings.EqualFold(known.UDID, udid) {
			return known, true
		}
	}
	return knownDevice{}, false
}

// remember records a device reached now, keeping the name and address known
// before where it brings none.
func (k *knownDevices) remember(device knownDevice) error {
	if k == nil || k.path == "" || device.UDID == "" {
		return nil
	}
	k.access.Lock()
	defer k.access.Unlock()
	devices := k.load()
	index := slices.IndexFunc(devices, func(it knownDevice) bool { return strings.EqualFold(it.UDID, device.UDID) })
	if index >= 0 {
		previous := devices[index]
		if device.Name == "" {
			device.Name = previous.Name
		}
		if device.Address == "" {
			device.Address = previous.Address
		}
		devices[index] = device
	} else {
		devices = append(devices, device)
	}
	data, err := json.MarshalIndent(knownFileContent{Devices: devices}, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(k.path), 0o755); err != nil {
		return err
	}
	temporary := k.path + ".tmp"
	if err = os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err = os.Rename(temporary, k.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		os.Remove(temporary)
		return err
	}
	return nil
}
