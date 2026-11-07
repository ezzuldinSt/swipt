package computer

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/coredevice/lockdown"
	"github.com/swipt-automation/swipt/internal/log"
)

// chooseReachable chooses the device as chooseDevice does or, when usbmuxd
// lists none that fits, a device on Wi-Fi this computer reached before, or
// the one given by UDID, that answers at the address it had or at the one
// Bonjour finds for it. A forced relay needs usbmuxd's listing.
func chooseReachable(ctx context.Context, given string, known *knownDevices, relay bool, logger log.ContextLogger) (pairedDevice, error) {
	paired, err := chooseDevice(ctx, given)
	if err == nil {
		if rememberErr := known.remember(knownDevice{UDID: paired.UDID, Name: paired.Name, Connected: time.Now()}); rememberErr != nil {
			logger.Debug("remember the device: ", rememberErr)
		}
		return paired, nil
	}
	if relay || !errors.Is(err, ErrDeviceNotListed) {
		return paired, err
	}
	for _, candidate := range unlistedCandidates(given, known.all()) {
		reached, reachErr := reachUnlisted(ctx, candidate, known, lookupBonjour)
		if reachErr == nil {
			logger.Info("usbmuxd does not list ", reached.Name, " (", reached.UDID, "), which answers on the network")
			return reached, nil
		}
		if ctx.Err() != nil {
			return pairedDevice{}, ctx.Err()
		}
		logger.Info("usbmuxd does not list ", candidate.UDID, ", and it does not answer on the network: ", reachErr)
	}
	return pairedDevice{}, err
}

var udidPattern = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{16}$|^[0-9A-Fa-f]{40}$`)

// unlistedCandidates are the devices given names, by UDID or name, among
// those reached before, or by its UDID alone; with none given, the only one
// reached before.
func unlistedCandidates(given string, known []knownDevice) []knownDevice {
	if given == "" {
		if len(known) == 1 {
			return known
		}
		return nil
	}
	var out []knownDevice
	for _, device := range known {
		if strings.EqualFold(device.UDID, given) || device.Name != "" && (strings.EqualFold(device.Name, given) || strings.EqualFold(label(device.Name), label(given))) {
			out = append(out, device)
		}
	}
	if len(out) == 0 && udidPattern.MatchString(given) {
		out = append(out, knownDevice{UDID: given})
	}
	return out
}

// reachUnlisted reads a device's pair record, which usbmuxd answers for a
// device it does not list, and its name over lockdown at the address it had
// or the one Bonjour finds for it, which is then remembered.
func reachUnlisted(ctx context.Context, candidate knownDevice, known *knownDevices, lookup func(ctx context.Context, mac string) (netip.Addr, error)) (pairedDevice, error) {
	record, err := readPairRecord(ctx, candidate.UDID)
	if err != nil {
		return pairedDevice{}, err
	}
	var failures []error
	try := func(address netip.Addr) (pairedDevice, bool) {
		asking, cancel := context.WithTimeout(ctx, directPatience)
		defer cancel()
		dial := func(ctx context.Context, port uint16) (net.Conn, error) {
			return dialDirect(ctx, netip.AddrPortFrom(address, port))
		}
		value, err := lockdown.GetValue(asking, dial, "DeviceName")
		if err != nil {
			failures = append(failures, E.Cause(err, address))
			return pairedDevice{}, false
		}
		name, _ := value.(string)
		if name == "" {
			name = candidate.Name
		}
		_ = known.remember(knownDevice{UDID: candidate.UDID, Name: name, Address: address.String(), Connected: time.Now()})
		return pairedDevice{UDID: candidate.UDID, Name: name}, true
	}
	last := candidate.address()
	if last.IsValid() {
		if reached, ok := try(last); ok {
			return reached, nil
		}
	}
	address, err := lookup(ctx, record.WiFiMACAddress)
	if err != nil {
		failures = append(failures, err)
	} else if address != last {
		if reached, ok := try(address); ok {
			return reached, nil
		}
	}
	return pairedDevice{}, E.Errors(failures...)
}
