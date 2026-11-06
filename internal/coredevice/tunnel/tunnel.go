package tunnel

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/buf"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
)

// idevice sends CDTunnel JSON after this magic and a big-endian uint16
// length (idevice 3854a5d, idevice/src/tunnel.rs:14, 47-71).
const magic = "CDTunnel"

// idevice requests MTU 16000; go-ios requests 1280
// (idevice 3854a5d, idevice/src/tunnel.rs:16;
// go-ios ca2c56d, ios/tunnel/tunnel.go:266-270).
const DefaultMTU = 16000

// A vphone guest stops acknowledging a transfer over a tunnel of DefaultMTU
// (iPhone18,2, iOS 27.0.1 24A446, vphone-cli 2.15.2).
const GuestMTU = 1280

const ipv6HeaderLength = 40

type Parameters struct {
	ClientAddress netip.Addr
	MTU           int
	ServerAddress netip.Addr
	ServerRSDPort uint16
}

type handshakeResponse struct {
	ClientParameters struct {
		Address string `json:"address"`
		MTU     int    `json:"mtu"`
	} `json:"clientParameters"`
	ServerAddress string `json:"serverAddress"`
	ServerRSDPort uint16 `json:"serverRSDPort"`
}

func handshake(conn net.Conn, reader *bufio.Reader, mtu int) (*Parameters, error) {
	request, err := json.Marshal(map[string]any{"type": "clientHandshakeRequest", "mtu": mtu})
	if err != nil {
		return nil, err
	}
	frame := append([]byte(magic), 0, 0)
	binary.BigEndian.PutUint16(frame[len(magic):], uint16(len(request)))
	_, err = conn.Write(append(frame, request...))
	if err != nil {
		return nil, E.Cause(err, "send CDTunnel request")
	}
	header := make([]byte, len(magic)+2)
	_, err = io.ReadFull(reader, header)
	if err != nil {
		return nil, E.Cause(err, "read CDTunnel response")
	}
	if string(header[:len(magic)]) != magic {
		return nil, E.New("CDTunnel: bad magic ", strconv.Quote(string(header[:len(magic)])))
	}
	body := make([]byte, binary.BigEndian.Uint16(header[len(magic):]))
	_, err = io.ReadFull(reader, body)
	if err != nil {
		return nil, E.Cause(err, "read CDTunnel response")
	}
	var response handshakeResponse
	err = json.Unmarshal(body, &response)
	if err != nil {
		return nil, E.Cause(err, "parse CDTunnel response ", string(body))
	}
	clientAddress, err := netip.ParseAddr(response.ClientParameters.Address)
	if err != nil {
		return nil, E.Cause(err, "parse CDTunnel client address")
	}
	serverAddress, err := netip.ParseAddr(response.ServerAddress)
	if err != nil {
		return nil, E.Cause(err, "parse CDTunnel server address")
	}
	parameters := &Parameters{
		ClientAddress: clientAddress,
		MTU:           response.ClientParameters.MTU,
		ServerAddress: serverAddress,
		ServerRSDPort: response.ServerRSDPort,
	}
	if parameters.MTU <= ipv6HeaderLength || parameters.MTU > 65535 {
		parameters.MTU = 1280
	}
	return parameters, nil
}

const batchSize = 64

// sing-tun MemoryTun.WritePackets skips packets larger than 65535 bytes
// without incrementing its accepted count
// (sing-tun v0.9.7-0.20261002083955-3f8acd9da65b, tun_memory.go:13, 115-118).
const maxPacket = 65535

const maxWrite = 256 << 10

const retryDelay = time.Millisecond

// observer, set only by tests, sees each packet the tunnel carries.
var observer atomic.Pointer[func(inbound bool, packet []byte)]

type Tunnel struct {
	conn          net.Conn
	reader        *bufio.Reader
	parameters    *Parameters
	clientAddress [16]byte
	logger        logger.ContextLogger
	device        *tun.MemoryTun
	stack         *tun.Go
	udpAccess     sync.RWMutex
	udpConns      map[uint16]*UDPConn
	// sing-tun calls the Outbound callback serially from one goroutine
	// (sing-tun v0.9.7-0.20261002083955-3f8acd9da65b, tun_memory.go:59-63;
	// tun_memory_queue.go:27-39, 80-95).
	writeBuffer []byte
	done        chan struct{}
	closeOnce   sync.Once
	err         error
}

func Open(ctx context.Context, conn net.Conn, mtu int, logger logger.ContextLogger) (*Tunnel, error) {
	if err := ctx.Err(); err != nil {
		conn.Close()
		return nil, err
	}
	reader := bufio.NewReaderSize(conn, 2*(ipv6HeaderLength+65535))
	stopAfter := context.AfterFunc(ctx, func() { conn.Close() })
	parameters, err := handshake(conn, reader, mtu)
	if !stopAfter() || ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	logger.Debug("tunnel client ", parameters.ClientAddress, ", server ", parameters.ServerAddress,
		", RSD port ", parameters.ServerRSDPort, ", MTU ", parameters.MTU)
	return start(conn, reader, parameters, logger)
}

func start(conn net.Conn, reader *bufio.Reader, parameters *Parameters, logger logger.ContextLogger) (*Tunnel, error) {
	t := &Tunnel{
		conn:          conn,
		reader:        reader,
		parameters:    parameters,
		clientAddress: parameters.ClientAddress.As16(),
		logger:        logger,
		udpConns:      make(map[uint16]*UDPConn),
		done:          make(chan struct{}),
	}
	t.device = tun.NewMemoryTun(tun.MemoryTunOptions{
		MTU:       parameters.MTU,
		BatchSize: batchSize,
		Outbound:  t.writePackets,
	})
	var err error
	t.stack, err = tun.NewGo(tun.StackOptions{
		Context:    context.Background(),
		Tun:        t.device,
		TunOptions: tun.Options{MTU: uint32(parameters.MTU)},
		Logger:     logger,
	})
	if err == nil {
		err = t.stack.Start()
	}
	if err != nil {
		t.device.Close()
		conn.Close()
		return nil, E.Cause(err, "start network stack")
	}
	go t.readLoop()
	return t, nil
}

func (t *Tunnel) Parameters() *Parameters {
	return t.parameters
}

func (t *Tunnel) LocalAddr() net.Addr {
	return t.conn.LocalAddr()
}

func (t *Tunnel) RemoteAddr() net.Addr {
	return t.conn.RemoteAddr()
}

// sing-tun waits for its outbound callback; a full data queue returns
// errGoTransmitBlocked (sing-tun v0.9.7-0.20261002083955-3f8acd9da65b,
// tun_memory_queue.go:80-95, 155-156).
func (t *Tunnel) writePackets(packets []*buf.Buffer) {
	defer buf.ReleaseMulti(packets)
	data := t.writeBuffer[:0]
	observe := observer.Load()
	for _, packet := range packets {
		if observe != nil {
			(*observe)(false, packet.Bytes())
		}
		if len(data) > 0 && len(data)+packet.Len() > maxWrite {
			if !t.write(data) {
				return
			}
			data = data[:0]
		}
		data = append(data, packet.Bytes()...)
	}
	if len(data) > 0 {
		t.write(data)
	}
	if cap(data) <= 2*maxWrite {
		t.writeBuffer = data[:0]
	}
}

func (t *Tunnel) write(data []byte) bool {
	_, err := t.conn.Write(data)
	if err != nil {
		t.fail(E.Cause(err, "write to tunnel"))
		return false
	}
	return true
}

func (t *Tunnel) readLoop() {
	packets := make([][]byte, 0, batchSize)
	lengths := make([]int, 0, batchSize)
	retry := time.NewTimer(retryDelay)
	retry.Stop()
	var tooLong uint64
	for {
		length, err := t.peekPacket(0, true)
		if err != nil {
			t.fail(err)
			return
		}
		total := length
		lengths = append(lengths[:0], length)
		for len(lengths) < batchSize {
			length, err = t.peekPacket(total, false)
			if err != nil || length == 0 {
				break
			}
			lengths = append(lengths, length)
			total += length
		}
		data, _ := t.reader.Peek(total)
		arrived := time.Now()
		observe := observer.Load()
		packets = packets[:0]
		offset := 0
		for _, length := range lengths {
			packet := data[offset : offset+length]
			offset += length
			if observe != nil {
				(*observe)(true, packet)
			}
			if t.deliverDirect(packet, arrived) {
				continue
			}
			if length > maxPacket {
				tooLong++
				t.logger.Debug("dropped a packet of ", length, " bytes, too long for the network stack, ", tooLong, " in total")
				continue
			}
			packets = append(packets, packet)
		}
		err = t.deliver(packets, retry)
		if err != nil {
			t.fail(err)
			return
		}
		t.reader.Discard(total)
	}
}

func (t *Tunnel) peekPacket(offset int, wait bool) (int, error) {
	if !wait && t.reader.Buffered() < offset+ipv6HeaderLength {
		return 0, nil
	}
	data, err := t.reader.Peek(offset + ipv6HeaderLength)
	if err != nil {
		return 0, E.Cause(err, "read from tunnel")
	}
	header := data[offset:]
	if header[0]>>4 != 6 {
		return 0, E.New("read from tunnel: not an IPv6 packet (version ", header[0]>>4, ")")
	}
	length := ipv6HeaderLength + int(binary.BigEndian.Uint16(header[4:6]))
	if !wait && t.reader.Buffered() < offset+length {
		return 0, nil
	}
	_, err = t.reader.Peek(offset + length)
	if err != nil {
		return 0, E.Cause(err, "read from tunnel")
	}
	return length, nil
}

// sing-tun MemoryTun.WritePackets copies accepted packets and reports
// their count; a full queue accepts none
// (sing-tun v0.9.7-0.20261002083955-3f8acd9da65b, tun_memory.go:101-133).
func (t *Tunnel) deliver(packets [][]byte, retry *time.Timer) error {
	for len(packets) > 0 {
		accepted, err := t.device.WritePackets(packets)
		if err != nil {
			return err
		}
		packets = packets[accepted:]
		if len(packets) == 0 {
			return nil
		}
		retry.Reset(retryDelay)
		select {
		case <-t.done:
			retry.Stop()
			return net.ErrClosed
		case <-retry.C:
		}
	}
	return nil
}

func (t *Tunnel) fail(err error) {
	t.closeOnce.Do(func() {
		t.logger.Debug("tunnel closed: ", err)
		t.err = err
		close(t.done)
		t.closeUDP(err)
		t.conn.Close()
		t.stack.Close()
		t.device.Close()
	})
}

func (t *Tunnel) DialTCP(ctx context.Context, port uint16) (net.Conn, error) {
	conn, err := t.stack.DialTCP(ctx, t.parameters.ClientAddress, netip.AddrPortFrom(t.parameters.ServerAddress, port))
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (t *Tunnel) Done() <-chan struct{} {
	return t.done
}

func (t *Tunnel) Err() error {
	select {
	case <-t.done:
		return t.err
	default:
		return nil
	}
}

func (t *Tunnel) Close() error {
	t.fail(net.ErrClosed)
	return nil
}
