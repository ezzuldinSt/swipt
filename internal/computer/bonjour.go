package computer

import (
	"context"
	"net/netip"
	"strings"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/dnssd"
)

// An iPhone on Wi-Fi advertises _apple-mobdev2._tcp as
// "66:83:3b:04:28:0f@fe80::6483:3bff:fe04:280f-supportsRP-26", its Wi-Fi MAC
// address first, the one its usbmuxd pair record holds as WiFiMACAddress.
// Windows answered the browse in 4 ms and the resolve in 105 ms, with an IPv4
// address that answered lockdown and a link-local IPv6 one that did not
// (iPhone18,2, iOS 27.0.1 24A446 as a hotspot).
const (
	mobileDeviceService = "_apple-mobdev2._tcp.local"
	bonjourPatience     = 3 * time.Second
)

// lookupBonjour finds the address of the device whose Wi-Fi MAC address names
// a _apple-mobdev2._tcp instance.
func lookupBonjour(ctx context.Context, mac string) (netip.Addr, error) {
	if mac == "" {
		return netip.Addr{}, E.New("the pair record holds no Wi-Fi address to look for")
	}
	browsing, cancel := context.WithTimeout(ctx, bonjourPatience)
	defer cancel()
	prefix := strings.ToLower(mac) + "@"
	var name string
	err := dnssd.Browse(browsing, mobileDeviceService, func(found string) {
		if name == "" && strings.HasPrefix(strings.ToLower(found), prefix) {
			name = found
			cancel()
		}
	})
	if err != nil {
		return netip.Addr{}, err
	}
	if name == "" {
		if ctx.Err() != nil {
			return netip.Addr{}, ctx.Err()
		}
		return netip.Addr{}, E.New("no ", mobileDeviceService, " instance answered for ", mac, " within ", bonjourPatience)
	}
	resolving, cancelResolve := context.WithTimeout(ctx, bonjourPatience)
	defer cancelResolve()
	instance, err := dnssd.Resolve(resolving, name)
	if err != nil {
		return netip.Addr{}, E.Cause(err, "resolve ", name)
	}
	if address := preferredAddress(instance.Addresses); address.IsValid() {
		return address, nil
	}
	return netip.Addr{}, E.New(name, " resolved to no address")
}

// preferredAddress takes IPv4 before IPv6, and a link-local IPv6 address last.
func preferredAddress(addresses []netip.Addr) netip.Addr {
	rank := func(address netip.Addr) int {
		switch {
		case address.Unmap().Is4():
			return 0
		case !address.IsLinkLocalUnicast():
			return 1
		}
		return 2
	}
	var best netip.Addr
	for _, address := range addresses {
		if !address.IsValid() || address.IsUnspecified() || address.IsLoopback() {
			continue
		}
		if !best.IsValid() || rank(address) < rank(best) {
			best = address.Unmap()
		}
	}
	return best
}
