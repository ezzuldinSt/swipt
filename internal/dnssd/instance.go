package dnssd

import (
	"net/netip"

	E "github.com/sagernet/sing/common/exceptions"
)

// Instance is a resolved service instance.
type Instance struct {
	Name      string
	Host      string
	Addresses []netip.Addr
	Port      uint16
	Interface uint32
}

var ErrUnsupported = E.New("dnssd: this system cannot browse for services")
