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
type router struct {
	udid   string
	relay  bool
	logger log.ContextLogger

	list       func(ctx context.Context) ([]usbmuxd.Device, error)
	pairRecord func(ctx context.Context, udid string) (*lockdown.PairRecord, error)
	open       func(ctx context.Context, options coredevice.LockdownOptions) (*coredevice.Device, error)
	dialDirect func(ctx context.Context, address netip.AddrPort) (net.Conn, error)
	dialRelay  func(id uint64) lockdown.Dialer
	now        func() time.Time

	access   sync.Mutex
	address  netip.Addr
	failed   netip.Addr
	failedAt time.Time
}

func newRouter(udid string, relay bool, logger log.ContextLogger) *router {
	return &router{
		udid:       udid,
		relay:      relay,
		logger:     logger,
		list:       usbmuxd.ListDevices,
		pairRecord: readPairRecord,
		open:       coredevice.ConnectLockdown,
		dialDirect: func(ctx context.Context, address netip.AddrPort) (net.Conn, error) {
			dialer := net.Dialer{Timeout: directDialTimeout, KeepAliveConfig: directKeepAlive}
			return dialer.DialContext(ctx, "tcp", address.String())
		},
		dialRelay: dialer,
		now:       time.Now,
	}
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

func (r *router) connect(ctx context.Context) (*coredevice.Device, error) {
	found, err := r.find(ctx)
	if err != nil {
		return nil, err
	}
	direct := r.directAddress(found)
	if found.ID == 0 && !direct.IsValid() {
		return nil, &device.Error{Problem: device.ProblemNoTunnel, Err: E.New("the device is not connected to ", thisComputer)}
	}
	record, err := r.pairRecord(ctx, r.udid)
	if err != nil {
		return nil, err
	}
	if direct.IsValid() {
		connected, err := r.connectDirect(ctx, direct, record)
		if err == nil {
			return connected, nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		r.access.Lock()
		r.failed, r.failedAt = direct, r.now()
		r.access.Unlock()
		if found.ID == 0 {
			return nil, &device.Error{Problem: device.ProblemNoTunnel, Err: E.Cause(err, "connect to the device at ", direct, "; usbmuxd does not list it")}
		}
		r.logger.Info("connect to the device at ", direct, ": ", err, "; connecting through usbmuxd instead")
	}
	r.logger.Debug("connecting through ", found.Connection, " as usbmuxd device ", found.ID)
	return r.open(ctx, coredevice.LockdownOptions{Record: record, Dial: r.dialRelay(found.ID), Logger: r.logger})
}

// directAddress is where to reach the device straight away, if anywhere: the
// address usbmuxd lists for it on the network or, while usbmuxd lists it on
// neither USB nor the network, the address it listed last, unless a
// connection there failed lately.
func (r *router) directAddress(found usbmuxd.Device) netip.Addr {
	if r.relay || found.Connection == connectionUSB {
		return netip.Addr{}
	}
	r.access.Lock()
	defer r.access.Unlock()
	if found.Address.IsValid() {
		r.address = found.Address
	} else if found.ID != 0 {
		return netip.Addr{}
	}
	if !r.address.IsValid() {
		return netip.Addr{}
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
