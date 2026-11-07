package computer

import (
	"net"
	"unsafe"

	"golang.org/x/sys/windows"
)

// mstcpip.h defines SIO_TCP_SET_ACK_FREQUENCY as _WSAIOW(IOC_VENDOR, 23).
const sioTCPSetAckFrequency = windows.IOC_IN | windows.IOC_VENDOR | 23

// The device sends the next small part of a reply through a tunnel only once
// this side has acknowledged the part before, which Windows delays while
// nothing else arrives on that connection. A lock-state read took 66-81 ms
// that way on a tunnel of its own, and 27-39 ms only while the screen streamed
// through the same one; with each segment acknowledged at once, it took 20-28
// ms either way, as did getdisplayinfo 13-19 ms instead of 21-112 ms
// (iPhone18,2, iOS 27.0.1 24A446, connected straight through a Wi-Fi access
// point).
func acknowledgeEach(conn net.Conn) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return
	}
	_ = raw.Control(func(fd uintptr) {
		frequency := uint32(1)
		var returned uint32
		_ = windows.WSAIoctl(windows.Handle(fd), sioTCPSetAckFrequency, (*byte)(unsafe.Pointer(&frequency)), 4, nil, 0, &returned, nil, 0)
	})
}
