package coredevice

import (
	"context"
	"net"
	"sync"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/coredevice/xpc"
)

type keptConn interface {
	Err() error
	Close() error
}

// kept holds one connection to a service for a run of requests, taken one at a
// time, so that only the first pays the connect and the protocol's hello: two
// round trips over Wi-Fi.
type kept[C keptConn] struct {
	dial func(ctx context.Context) (C, error)
	turn chan struct{}

	access sync.Mutex
	conn   C
	open   bool
	closed bool
}

func newKept[C keptConn](dial func(ctx context.Context) (C, error)) *kept[C] {
	return &kept[C]{dial: dial, turn: make(chan struct{}, 1)}
}

// do runs use on the kept connection, connecting first if there is none or it
// has ended. A connection that ends while in use is dropped; when it had served
// an earlier request, the service may have closed it while it sat idle, so a
// retryable use runs once more on a new connection. An error that leaves the
// connection open, such as a feature's, is the service's answer.
func (k *kept[C]) do(ctx context.Context, retry bool, use func(conn C) error) error {
	select {
	case k.turn <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-k.turn }()
	for attempt := 0; ; attempt++ {
		conn, reused, err := k.take(ctx)
		if err != nil {
			return err
		}
		err = use(conn)
		if err == nil || conn.Err() == nil {
			return err
		}
		k.drop(conn)
		if !reused || !retry || attempt > 0 || ctx.Err() != nil {
			return err
		}
	}
}

func (k *kept[C]) take(ctx context.Context) (conn C, reused bool, err error) {
	k.access.Lock()
	if k.closed {
		k.access.Unlock()
		return conn, false, E.Cause(net.ErrClosed, "the device is disconnected")
	}
	if k.open && k.conn.Err() == nil {
		conn = k.conn
		k.access.Unlock()
		return conn, true, nil
	}
	if k.open {
		k.conn.Close()
		k.open = false
	}
	k.access.Unlock()
	conn, err = k.dial(ctx)
	if err != nil {
		return conn, false, err
	}
	k.access.Lock()
	defer k.access.Unlock()
	if k.closed {
		conn.Close()
		return conn, false, E.Cause(net.ErrClosed, "the device is disconnected")
	}
	k.conn, k.open = conn, true
	return conn, false, nil
}

func (k *kept[C]) drop(conn C) {
	conn.Close()
	k.access.Lock()
	defer k.access.Unlock()
	if k.open && any(k.conn) == any(conn) {
		var none C
		k.conn, k.open = none, false
	}
}

func (k *kept[C]) close() {
	k.access.Lock()
	defer k.access.Unlock()
	k.closed = true
	if k.open {
		k.conn.Close()
		var none C
		k.conn, k.open = none, false
	}
}

// XPCService keeps a RemoteXPC connection to one of the device's services for
// as long as the device is connected.
type XPCService struct {
	kept *kept[*xpc.Conn]
}

// Service returns the device's kept connection to the named RemoteXPC service,
// made on its first request.
func (d *Device) Service(name string) *XPCService {
	d.servicesAccess.Lock()
	defer d.servicesAccess.Unlock()
	if service := d.keptServices[name]; service != nil {
		return service
	}
	service := &XPCService{kept: newKept(func(ctx context.Context) (*xpc.Conn, error) {
		return d.DialXPC(ctx, name)
	})}
	if d.keptServices == nil {
		d.keptServices = make(map[string]*XPCService)
	}
	d.keptServices[name] = service
	if d.Err() != nil {
		service.kept.close()
	}
	return service
}

func (d *Device) closeServices() {
	d.servicesAccess.Lock()
	defer d.servicesAccess.Unlock()
	for _, service := range d.keptServices {
		service.kept.close()
	}
}

// Do runs use on the connection, which nothing else uses meanwhile. With
// retry, a use that loses a connection kept from an earlier request runs again
// on a new one; it must be safe to repeat.
func (s *XPCService) Do(ctx context.Context, retry bool, use func(conn *xpc.Conn) error) error {
	return s.kept.do(ctx, retry, use)
}

// Invoke invokes a feature whose request is safe to repeat.
func (s *XPCService) Invoke(ctx context.Context, deviceIdentifier, feature, action string, input map[string]any) (output map[string]any, err error) {
	err = s.Do(ctx, true, func(conn *xpc.Conn) error {
		output, err = InvokeFeature(ctx, conn, deviceIdentifier, feature, action, input)
		return err
	})
	return output, err
}
