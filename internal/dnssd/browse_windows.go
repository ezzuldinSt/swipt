package dnssd

import (
	"context"
	"net/netip"
	"runtime"
	"strconv"
	"sync"
	"unsafe"

	E "github.com/sagernet/sing/common/exceptions"

	"golang.org/x/sys/windows"
)

// dnsapi.dll browses and resolves DNS-SD services since Windows 10 1809
// (windns.h DnsServiceBrowse, DnsServiceResolve).
var (
	dnsapi            = windows.NewLazySystemDLL("dnsapi.dll")
	procBrowse        = dnsapi.NewProc("DnsServiceBrowse")
	procBrowseCancel  = dnsapi.NewProc("DnsServiceBrowseCancel")
	procResolve       = dnsapi.NewProc("DnsServiceResolve")
	procResolveCancel = dnsapi.NewProc("DnsServiceResolveCancel")
	procFreeInstance  = dnsapi.NewProc("DnsServiceFreeInstance")
)

const (
	queryRequestVersion1 = 1
	requestPending       = 9506 // DNS_REQUEST_PENDING
	freeRecordList       = 1    // DnsFreeRecordList
)

// DNS_SERVICE_BROWSE_REQUEST and DNS_SERVICE_RESOLVE_REQUEST share this
// layout in version 1.
type serviceRequest struct {
	Version        uint32
	InterfaceIndex uint32
	QueryName      *uint16
	Callback       uintptr
	Context        uintptr
}

type serviceCancel struct {
	reserved uintptr
}

// DNS_SERVICE_INSTANCE; its IP4_ADDRESS holds the address in network order.
type serviceInstance struct {
	InstanceName   *uint16
	HostName       *uint16
	IP4Address     *[4]byte
	IP6Address     *[16]byte
	Port           uint16
	Priority       uint16
	Weight         uint16
	PropertyCount  uint32
	Keys           **uint16
	Values         **uint16
	InterfaceIndex uint32
}

type resolved struct {
	instance Instance
	err      error
}

var (
	callbacksOnce   sync.Once
	browseCallback  uintptr
	resolveCallback uintptr

	waitersAccess sync.Mutex
	waiters       = make(map[uintptr]func(value any))
	nextWaiter    uintptr
)

// Callbacks made by windows.NewCallback are never freed, so the two are made
// once and tell their requests apart by the context they carry.
func setupCallbacks() {
	callbacksOnce.Do(func() {
		browseCallback = windows.NewCallback(func(status uint32, context uintptr, records *windows.DNSRecord) uintptr {
			var names []string
			for record := records; record != nil; record = record.Next {
				if record.Type == windows.DNS_TYPE_PTR {
					if host := (*windows.DNSPTRData)(unsafe.Pointer(&record.Data[0])).Host; host != nil {
						names = append(names, windows.UTF16PtrToString(host))
					}
				}
			}
			if records != nil {
				windows.DnsRecordListFree(records, freeRecordList)
			}
			if status == 0 {
				deliver(context, names)
			}
			return 0
		})
		resolveCallback = windows.NewCallback(func(status uint32, context uintptr, instance *serviceInstance) uintptr {
			var result resolved
			if status != 0 || instance == nil {
				result.err = E.New("dnssd: resolve: error ", status)
			} else {
				result.instance = copyInstance(instance)
			}
			if instance != nil {
				_, _, _ = procFreeInstance.Call(uintptr(unsafe.Pointer(instance)))
			}
			deliver(context, result)
			return 0
		})
	})
}

func copyInstance(instance *serviceInstance) Instance {
	out := Instance{
		Name:      windows.UTF16PtrToString(instance.InstanceName),
		Host:      windows.UTF16PtrToString(instance.HostName),
		Port:      instance.Port,
		Interface: instance.InterfaceIndex,
	}
	if instance.IP4Address != nil {
		out.Addresses = append(out.Addresses, netip.AddrFrom4(*instance.IP4Address))
	}
	if instance.IP6Address != nil {
		address := netip.AddrFrom16(*instance.IP6Address)
		if address.IsLinkLocalUnicast() && instance.InterfaceIndex != 0 {
			address = address.WithZone(strconv.FormatUint(uint64(instance.InterfaceIndex), 10))
		}
		out.Addresses = append(out.Addresses, address)
	}
	return out
}

func register(receive func(value any)) uintptr {
	waitersAccess.Lock()
	defer waitersAccess.Unlock()
	nextWaiter++
	waiters[nextWaiter] = receive
	return nextWaiter
}

func unregister(id uintptr) {
	waitersAccess.Lock()
	delete(waiters, id)
	waitersAccess.Unlock()
}

func deliver(id uintptr, value any) {
	waitersAccess.Lock()
	receive := waiters[id]
	waitersAccess.Unlock()
	if receive != nil {
		receive(value)
	}
}

func available() error {
	for _, proc := range []*windows.LazyProc{procBrowse, procBrowseCancel, procResolve, procResolveCancel, procFreeInstance} {
		if proc.Find() != nil {
			return ErrUnsupported
		}
	}
	setupCallbacks()
	return nil
}

// Browse calls found with the name of each instance of service, such as
// "_apple-mobdev2._tcp.local", that answers until ctx ends.
func Browse(ctx context.Context, service string, found func(name string)) error {
	if err := available(); err != nil {
		return err
	}
	query, err := windows.UTF16PtrFromString(service)
	if err != nil {
		return err
	}
	results := make(chan []string, 64)
	id := register(func(value any) {
		select {
		case results <- value.([]string):
		default:
		}
	})
	defer unregister(id)
	request := &serviceRequest{Version: queryRequestVersion1, QueryName: query, Callback: browseCallback, Context: id}
	cancel := new(serviceCancel)
	code, _, _ := procBrowse.Call(uintptr(unsafe.Pointer(request)), uintptr(unsafe.Pointer(cancel)))
	if code != requestPending {
		return E.New("dnssd: browse ", service, ": error ", code)
	}
	defer func() {
		_, _, _ = procBrowseCancel.Call(uintptr(unsafe.Pointer(cancel)))
		runtime.KeepAlive(request)
		runtime.KeepAlive(query)
	}()
	seen := make(map[string]bool)
	for {
		select {
		case <-ctx.Done():
			return nil
		case names := <-results:
			for _, name := range names {
				if !seen[name] {
					seen[name] = true
					found(name)
				}
			}
		}
	}
}

// Resolve answers the host, addresses and port of a service instance, such
// as "name._apple-mobdev2._tcp.local".
func Resolve(ctx context.Context, name string) (Instance, error) {
	if err := available(); err != nil {
		return Instance{}, err
	}
	query, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return Instance{}, err
	}
	results := make(chan resolved, 1)
	id := register(func(value any) {
		select {
		case results <- value.(resolved):
		default:
		}
	})
	defer unregister(id)
	request := &serviceRequest{Version: queryRequestVersion1, QueryName: query, Callback: resolveCallback, Context: id}
	cancel := new(serviceCancel)
	code, _, _ := procResolve.Call(uintptr(unsafe.Pointer(request)), uintptr(unsafe.Pointer(cancel)))
	if code != requestPending {
		return Instance{}, E.New("dnssd: resolve ", name, ": error ", code)
	}
	defer func() {
		runtime.KeepAlive(request)
		runtime.KeepAlive(query)
	}()
	select {
	case result := <-results:
		return result.instance, result.err
	case <-ctx.Done():
		_, _, _ = procResolveCancel.Call(uintptr(unsafe.Pointer(cancel)))
		return Instance{}, ctx.Err()
	}
}
