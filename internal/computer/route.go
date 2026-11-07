package computer

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/coredevice"
	"github.com/swipt-automation/swipt/internal/coredevice/lockdown"
	"github.com/swipt-automation/swipt/internal/device"
	"github.com/swipt-automation/swipt/internal/log"
	"github.com/swipt-automation/swipt/internal/usbmuxd"
)

// RelayVariable, set to 1, keeps a device on Wi-Fi behind usbmuxd's relay.
const RelayVariable = "SWIPT_WIFI_RELAY"

const (
	// A direct connection answered lockdown and opened the tunnel in
	// 0.5-0.6 s, while an idle phone answered pings in up to 128 ms
	// (iPhone18,2, iOS 27.0.1 24A446 on Wi-Fi).
	directDialTimeout = 3 * time.Second
	directPatience    = 8 * time.Second
	// After a direct connection to an address fails, connections go through
	// the relay for this long before trying that address again.
	directRetry = 2 * time.Minute
)

// usbmuxd notices a device that left the network; a direct connection learns
// of it from keepalives, here within about 15 s of silence.
var directKeepAlive = net.KeepAliveConfig{Enable: true, Idle: 5 * time.Second, Interval: 2 * time.Second, Count: 5}

// router connects to a device listed by usbmuxd. A Network device is reached
// straight at the address usbmuxd lists, with usbmuxd's pair record, instead
// of through Apple Mobile Device Service's relay: a round trip took 11-13 ms
// that way instead of 20-25 ms, and a screenshot 2.1-3.1 s instead of
// 2.7-4.8 s (iPhone18,2, iOS 27.0.1 24A446 on Wi-Fi). It falls back to the
// relay when that fails, and moves to a new address when usbmuxd lists one.
// While usbmuxd does not list the device, whose pair record it still reads,
// it is reached at the address it answered at last and the one Bonjour finds
// for it, which also serve the next start.
type router struct {
	udid   string
	name   string
	relay  bool
	logger log.ContextLogger
	known  *knownDevices

	list       func(ctx context.Context) ([]usbmuxd.Device, error)
	pairRecord func(ctx context.Context, udid string) (*lockdown.PairRecord, error)
	open       func(ctx context.Context, options coredevice.LockdownOptions) (*coredevice.Device, error)
	dialDirect func(ctx context.Context, address netip.AddrPort) (net.Conn, error)
	dialRelay  func(id uint64) lockdown.Dialer
	lookup     func(ctx context.Context, mac string) (netip.Addr, error)
	now        func() time.Time

	access   sync.Mutex
	address  netip.Addr
	failed   netip.Addr
	failedAt time.Time
}

func newRouter(udid, name string, relay bool, known *knownDevices, logger log.ContextLogger) *router {
	r := &router{
		udid:       udid,
		name:       name,
		relay:      relay,
		logger:     logger,
		known:      known,
		list:       usbmuxd.ListDevices,
		pairRecord: readPairRecord,
		open:       coredevice.ConnectLockdown,
		dialDirect: dialDirect,
		dialRelay:  dialer,
		lookup:     lookupBonjour,
		now:        time.Now,
	}
	if device, found := known.find(udid); found {
		r.address = device.address()
	}
	return r
}

func dialDirect(ctx context.Context, address netip.AddrPort) (net.Conn, error) {
	dialer := net.Dialer{Timeout: directDialTimeout, KeepAliveConfig: directKeepAlive}
	return dialer.DialContext(ctx, "tcp", address.String())
}

func readPairRecord(ctx context.Context, udid string) (*lockdown.PairRecord, error) {
	data, err := usbmuxd.ReadPairRecord(ctx, udid)
	if err != nil {
		return nil, E.Cause(err, "read the pair record")
	}
	record, err := lockdown.ParsePairRecord(data)
	if err != nil {
		return nil, err
	}
	record.UDID = udid
	return record, nil
}

func (r *router) find(ctx context.Context) (usbmuxd.Device, error) {
	devices, err := r.list(ctx)
	if err != nil {
		return usbmuxd.Device{}, err
	}
	return common.Find(preferUSB(devices), func(it usbmuxd.Device) bool { return it.UDID == r.udid }), nil
}

var errNotConnected = E.New("the device is not connected to ", thisComputer)

func (r *router) connect(ctx context.Context) (*coredevice.Device, error) {
	found, err := r.find(ctx)
	if err != nil {
		return nil, err
	}
	listed := found.ID != 0
	if !listed && r.relay {
		return nil, &device.Error{Problem: device.ProblemNoTunnel, Err: errNotConnected}
	}
	record, err := r.pairRecord(ctx, r.udid)
	if err != nil {
		if !listed {
			return nil, &device.Error{Problem: device.ProblemNoTunnel, Err: E.Cause(err, errNotConnected)}
		}
		return nil, err
	}
	// Bonjour is asked alongside the address known, for an unlisted device.
	var bonjour func() (netip.Addr, error)
	if !listed {
		bonjour = r.lookupAhead(ctx, record.WiFiMACAddress)
	}
	var failures []error
	tried := make(map[netip.Addr]bool)
	direct := func(address netip.Addr) *coredevice.Device {
		tried[address] = true
		connected, err := r.connectDirect(ctx, address, record)
		if err == nil {
			r.reached(address)
			return connected
		}
		r.access.Lock()
		r.failed, r.failedAt = address, r.now()
		r.access.Unlock()
		failures = append(failures, E.Cause(err, "connect to the device at ", address))
		return nil
	}
	if address := r.directAddress(found); address.IsValid() {
		if connected := direct(address); connected != nil {
			return connected, nil
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if bonjour != nil {
		address, err := bonjour()
		switch {
		case err != nil:
			failures = append(failures, E.Cause(err, "find the device by Bonjour"))
		case !tried[address]:
			r.logger.Info("Bonjour found the device at ", address)
			if connected := direct(address); connected != nil {
				return connected, nil
			}
		}
	}
	if !listed {
		failures = append([]error{E.New(errNotConnected, ", and usbmuxd does not list it")}, failures...)
		return nil, &device.Error{Problem: device.ProblemNoTunnel, Err: E.Errors(failures...)}
	}
	if len(failures) > 0 {
		r.logger.Info(E.Errors(failures...), "; connecting through usbmuxd instead")
	}
	r.logger.Debug("connecting through ", found.Connection, " as usbmuxd device ", found.ID)
	connected, err := r.open(ctx, coredevice.LockdownOptions{Record: record, Dial: r.dialRelay(found.ID), Logger: r.logger})
	if err == nil {
		r.remember(knownDevice{UDID: r.udid, Name: r.name, Connected: r.now()})
	}
	return connected, err
}

func (r *router) lookupAhead(ctx context.Context, mac string) func() (netip.Addr, error) {
	done := make(chan struct{})
	var address netip.Addr
	var err error
	go func() {
		defer close(done)
		address, err = r.lookup(ctx, mac)
	}()
	return func() (netip.Addr, error) {
		<-done
		return address, err
	}
}

func (r *router) reached(address netip.Addr) {
	r.access.Lock()
	r.address = address
	r.access.Unlock()
	r.remember(knownDevice{UDID: r.udid, Name: r.name, Address: address.String(), Connected: r.now()})
}

func (r *router) remember(device knownDevice) {
	if err := r.known.remember(device); err != nil {
		r.logger.Debug("remember the device: ", err)
	}
}

// directAddress is where to reach the device straight away, if anywhere: the
// address usbmuxd lists for it on the network, unless a connection there
// failed lately, or, while usbmuxd lists it on neither USB nor the network,
// the address it answered at last.
func (r *router) directAddress(found usbmuxd.Device) netip.Addr {
	if r.relay || found.Connection == connectionUSB {
		return netip.Addr{}
	}
	r.access.Lock()
	defer r.access.Unlock()
	switch {
	case found.Address.IsValid():
		r.address = found.Address
	case found.ID != 0:
		return netip.Addr{}
	default:
		return r.address
	}
	if r.address == r.failed && r.now().Sub(r.failedAt) < directRetry {
		return netip.Addr{}
	}
	return r.address
}

func (r *router) connectDirect(ctx context.Context, address netip.Addr, record *lockdown.PairRecord) (*coredevice.Device, error) {
	r.logger.Debug("connecting straight to ", address)
	ctx, cancel := context.WithTimeout(ctx, directPatience)
	defer cancel()
	dial := func(ctx context.Context, port uint16) (net.Conn, error) {
		return r.dialDirect(ctx, netip.AddrPortFrom(address, port))
	}
	connected, err := r.open(ctx, coredevice.LockdownOptions{Record: record, Dial: dial, Logger: r.logger})
	if err != nil && errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		err = E.Cause(err, "no answer within ", directPatience)
	}
	return connected, err
}

// moved reports whether a new connection would reach the device another way
// than the direct one to remote: by USB, or at another address.
func (r *router) moved(ctx context.Context, remote netip.AddrPort) bool {
	if !remote.IsValid() || remote.Addr().Unmap().IsLoopback() || r.relay {
		return false
	}
	found, err := r.find(ctx)
	if err != nil || found.ID == 0 {
		return false
	}
	if found.Connection == connectionUSB {
		r.logger.Info("the device is connected by USB now")
		return true
	}
	if found.Address.IsValid() && found.Address.WithZone("") != remote.Addr().Unmap().WithZone("") {
		r.logger.Info("the device moved from ", remote.Addr(), " to ", found.Address)
		return true
	}
	return false
}

func relayForced(option bool, value string) bool {
	if option {
		return true
	}
	forced, _ := strconv.ParseBool(value)
	return forced
}
