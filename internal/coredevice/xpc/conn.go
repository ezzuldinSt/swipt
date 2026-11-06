package xpc

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
)

const (
	frameData         = 0x0
	frameHeaders      = 0x1
	frameRSTStream    = 0x3
	frameSettings     = 0x4
	framePing         = 0x6
	frameGoAway       = 0x7
	frameWindowUpdate = 0x8

	flagEndStream  = 0x1
	flagAck        = 0x1
	flagEndHeaders = 0x4
	flagPadded     = 0x8

	settingMaxConcurrentStreams = 0x3
	settingInitialWindowSize    = 0x4
	settingMaxFrameSize         = 0x5

	errorCodeCancel = 0x8

	frameHeaderLength = 9
	preface           = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
	defaultWindow     = 65535
	defaultFrameSize  = 16384
	maxReadFrameSize  = 1<<24 - 1
)

// idevice uses stream 1 for requests and stream 3 for INIT_HANDSHAKE;
// recv_any accepts messages from either stream (idevice 3854a5d,
// idevice/src/xpc/mod.rs:18-19, 71-78, 116-130, 176-197).
const (
	rootStream  = 1
	replyStream = 3
)

// idevice grants a 1 MiB window and 100 concurrent streams;
// pymobiledevice3 grants a 16 MiB window (idevice 3854a5d,
// idevice/src/xpc/mod.rs:51-60; pymobiledevice3 9c33842,
// pymobiledevice3/remote/remotexpc.py:49-52). A window smaller than a
// screenshot's PNG makes the service wait a round trip for each grant.
const (
	receiveWindow        = 16 << 20
	maxConcurrentStreams = 100
)

const maxQueued = 32 << 20

const retainedFrameSize = 64 << 10

const sendStallTimeout = 5 * time.Second

type Conn struct {
	conn          net.Conn
	reader        *bufio.Reader
	logger        logger.ContextLogger
	nextMessageID uint64

	writeAccess sync.Mutex

	access            sync.Mutex
	changed           *sync.Cond
	peerInitialWindow int64
	peerMaxFrame      int
	sendWindow        int64
	streams           map[uint32]*stream
	nextStream        uint32
	messages          []Message
	queued            int
	unacked           int64
	settingsReceived  bool
	err               error
}

type stream struct {
	sendWindow int64
	unacked    int64
	pending    []byte
	reset      bool
}

func NewConn(ctx context.Context, conn net.Conn, logger logger.ContextLogger) (*Conn, error) {
	if err := ctx.Err(); err != nil {
		conn.Close()
		return nil, err
	}
	c := &Conn{
		conn:              conn,
		reader:            bufio.NewReaderSize(conn, 64<<10),
		logger:            logger,
		peerInitialWindow: defaultWindow,
		peerMaxFrame:      defaultFrameSize,
		sendWindow:        defaultWindow,
		streams: map[uint32]*stream{
			rootStream:  {sendWindow: defaultWindow},
			replyStream: {sendWindow: defaultWindow},
		},
		nextStream:    5,
		nextMessageID: 1,
	}
	c.changed = sync.NewCond(&c.access)

	stop := context.AfterFunc(ctx, func() { c.fail(ctx.Err()) })
	defer stop()
	err := c.writeHandshake()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		c.fail(err)
		return nil, E.Cause(err, "start RemoteXPC")
	}
	go c.readLoop()

	c.access.Lock()
	for !c.settingsReceived && c.err == nil && ctx.Err() == nil {
		c.changed.Wait()
	}
	err = c.err
	c.access.Unlock()
	if !stop() || ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		c.fail(err)
		return nil, E.Cause(err, "start RemoteXPC")
	}
	return c, nil
}

// pymobiledevice3 opens root HEADERS, sends the empty dictionary, opens
// reply HEADERS, then sends root flags 0x201 and reply INIT_HANDSHAKE
// (pymobiledevice3 9c33842, pymobiledevice3/remote/remotexpc.py:416-437).
// idevice sends INIT_HANDSHAKE before 0x201
// (idevice 3854a5d, idevice/src/xpc/mod.rs:61-82).
func (c *Conn) writeHandshake() error {
	out := []byte(preface)
	out = appendFrame(out, frameSettings, 0, 0,
		binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint16(
			binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint16(nil,
				settingMaxConcurrentStreams), maxConcurrentStreams),
			settingInitialWindowSize), receiveWindow))
	out = appendFrame(out, frameWindowUpdate, 0, 0, binary.BigEndian.AppendUint32(nil, receiveWindow-defaultWindow))
	out = appendFrame(out, frameHeaders, flagEndHeaders, rootStream, nil)
	empty, _ := Marshal(Message{Flags: FlagAlwaysSet, Body: map[string]any{}})
	out = c.appendInitialData(out, rootStream, empty)
	out = appendFrame(out, frameHeaders, flagEndHeaders, replyStream, nil)
	flagsOnly, _ := Marshal(Message{Flags: 0x201})
	out = c.appendInitialData(out, rootStream, flagsOnly)
	initHandshake, _ := Marshal(Message{Flags: FlagAlwaysSet | FlagInitHandshake})
	out = c.appendInitialData(out, replyStream, initHandshake)
	_, err := c.conn.Write(out)
	return err
}

func (c *Conn) appendInitialData(out []byte, id uint32, data []byte) []byte {
	c.sendWindow -= int64(len(data))
	c.streams[id].sendWindow -= int64(len(data))
	return appendFrame(out, frameData, 0, id, data)
}

func appendFrame(out []byte, kind, flags byte, id uint32, payload []byte) []byte {
	length := len(payload)
	out = append(out, byte(length>>16), byte(length>>8), byte(length), kind, flags)
	out = binary.BigEndian.AppendUint32(out, id&0x7fffffff)
	return append(out, payload...)
}

func (c *Conn) writeFrame(kind, flags byte, id uint32, payload []byte) error {
	frame := appendFrame(make([]byte, 0, frameHeaderLength+len(payload)), kind, flags, id, payload)
	c.writeAccess.Lock()
	_, err := c.conn.Write(frame)
	c.writeAccess.Unlock()
	if err != nil {
		err = E.Cause(err, "write to service")
		c.fail(err)
	}
	return err
}

func (c *Conn) readLoop() {
	header := make([]byte, frameHeaderLength)
	var payload []byte
	for {
		_, err := io.ReadFull(c.reader, header)
		if err != nil {
			c.fail(E.Cause(err, "read from service"))
			return
		}
		length := int(header[0])<<16 | int(header[1])<<8 | int(header[2])
		kind, flags := header[3], header[4]
		id := binary.BigEndian.Uint32(header[5:]) & 0x7fffffff
		if length > maxReadFrameSize {
			c.fail(E.New("xpc: frame of ", length, " bytes is too large, at most ", maxReadFrameSize))
			return
		}
		frame := payload
		if cap(frame) < length {
			frame = make([]byte, length)
			if length <= retainedFrameSize {
				payload = frame
			}
		}
		frame = frame[:length]
		_, err = io.ReadFull(c.reader, frame)
		if err != nil {
			c.fail(E.Cause(err, "read from service"))
			return
		}
		err = c.handleFrame(kind, flags, id, frame)
		if err != nil {
			c.fail(err)
			return
		}
	}
}

func (c *Conn) handleFrame(kind, flags byte, id uint32, payload []byte) error {
	switch kind {
	case frameData:
		return c.handleData(flags, id, payload)
	case frameRSTStream:
		if len(payload) < 4 {
			return E.New("xpc: short RST_STREAM frame of ", len(payload), " bytes, expected 4")
		}
		code := binary.BigEndian.Uint32(payload)
		if id == rootStream || id == replyStream {
			return E.New("xpc: service reset stream ", id, " (error code ", code, ")")
		}
		c.access.Lock()
		if s := c.streams[id]; s != nil {
			s.reset = true
		}
		c.changed.Broadcast()
		c.access.Unlock()
	case frameSettings:
		if flags&flagAck != 0 {
			return nil
		}
		if len(payload)%6 != 0 {
			return E.New("xpc: SETTINGS frame of ", len(payload), " bytes is not a multiple of 6")
		}
		c.access.Lock()
		for i := 0; i < len(payload); i += 6 {
			value := binary.BigEndian.Uint32(payload[i+2:])
			switch binary.BigEndian.Uint16(payload[i:]) {
			case settingInitialWindowSize:
				delta := int64(value) - c.peerInitialWindow
				c.peerInitialWindow = int64(value)
				for _, s := range c.streams {
					s.sendWindow += delta
				}
			case settingMaxFrameSize:
				if value >= defaultFrameSize && value <= maxReadFrameSize {
					c.peerMaxFrame = int(value)
				}
			}
		}
		c.settingsReceived = true
		c.changed.Broadcast()
		c.access.Unlock()
		return c.writeFrame(frameSettings, flagAck, 0, nil)
	case framePing:
		if flags&flagAck == 0 {
			return c.writeFrame(framePing, flagAck, 0, payload)
		}
	case frameGoAway:
		if len(payload) < 8 {
			return E.New("xpc: service closed the connection (GOAWAY)")
		}
		return E.New("xpc: service closed the connection (GOAWAY, error code ",
			binary.BigEndian.Uint32(payload[4:]), ": ", strconv.Quote(string(payload[8:])), ")")
	case frameWindowUpdate:
		if len(payload) < 4 {
			return E.New("xpc: short WINDOW_UPDATE frame of ", len(payload), " bytes, expected 4")
		}
		increment := int64(binary.BigEndian.Uint32(payload) & 0x7fffffff)
		c.access.Lock()
		if id == 0 {
			c.sendWindow += increment
		} else if s := c.streams[id]; s != nil {
			s.sendWindow += increment
		}
		c.changed.Broadcast()
		c.access.Unlock()
	}
	return nil
}

func (c *Conn) handleData(flags byte, id uint32, payload []byte) error {
	length := int64(len(payload))
	data := payload
	if flags&flagPadded != 0 {
		if len(data) == 0 || int(data[0]) >= len(data) {
			return E.New("xpc: bad DATA padding in a ", len(data), "-byte frame")
		}
		data = data[1 : len(data)-int(data[0])]
	}
	c.access.Lock()
	defer c.access.Unlock()
	var connectionIncrement, streamIncrement int64
	c.unacked += length
	if c.unacked >= receiveWindow/2 {
		connectionIncrement, c.unacked = c.unacked, 0
	}
	s := c.streams[id]
	if s != nil && flags&flagEndStream == 0 {
		s.unacked += length
		if s.unacked >= receiveWindow/2 {
			streamIncrement, s.unacked = s.unacked, 0
		}
	}
	if s != nil && (id == rootStream || id == replyStream) {
		s.pending = append(s.pending, data...)
		for {
			message, used, err := Unmarshal(s.pending)
			if errors.Is(err, errShort) {
				break
			}
			if err != nil {
				return err
			}
			message.size = used
			s.pending = s.pending[used:]
			c.messages = append(c.messages, message)
			c.queued += used
			if c.queued > maxQueued {
				return E.New("xpc: too many unread messages: ", c.queued, " bytes queued, at most ", maxQueued)
			}
			c.changed.Broadcast()
		}
		if len(s.pending) == 0 {
			s.pending = nil
		} else if len(s.pending) > maxQueued {
			return E.New("xpc: incomplete message of ", len(s.pending), " bytes exceeds ", maxQueued, " bytes")
		}
	}
	if connectionIncrement > 0 || streamIncrement > 0 {
		go c.grant(id, connectionIncrement, streamIncrement)
	}
	return nil
}

func (c *Conn) grant(id uint32, connectionIncrement, streamIncrement int64) {
	if connectionIncrement > 0 {
		if c.writeFrame(frameWindowUpdate, 0, 0, binary.BigEndian.AppendUint32(nil, uint32(connectionIncrement))) != nil {
			return
		}
	}
	if streamIncrement > 0 {
		c.writeFrame(frameWindowUpdate, 0, id, binary.BigEndian.AppendUint32(nil, uint32(streamIncrement)))
	}
}

func (c *Conn) fail(err error) {
	c.access.Lock()
	defer c.access.Unlock()
	if c.err != nil {
		return
	}
	c.logger.Debug("RemoteXPC connection closed: ", err)
	c.err = err
	c.conn.Close()
	c.changed.Broadcast()
}

func (c *Conn) wakeOnDone(ctx context.Context) func() bool {
	return context.AfterFunc(ctx, func() {
		c.access.Lock()
		c.changed.Broadcast()
		c.access.Unlock()
	})
}

func (c *Conn) writeData(ctx context.Context, id uint32, data []byte, endStream bool) error {
	stop := c.wakeOnDone(ctx)
	defer stop()
	for {
		n, err := c.reserve(ctx, id, len(data))
		if err != nil {
			return err
		}
		chunk := data[:n]
		data = data[n:]
		var flags byte
		if endStream && len(data) == 0 {
			flags = flagEndStream
		}
		err = c.writeFrame(frameData, flags, id, chunk)
		if err != nil || len(data) == 0 {
			return err
		}
	}
}

func (c *Conn) reserve(ctx context.Context, id uint32, want int) (int, error) {
	c.access.Lock()
	defer c.access.Unlock()
	var stallTimer *time.Timer
	var stalledAt time.Time
	defer func() {
		if stallTimer != nil {
			stallTimer.Stop()
		}
	}()
	for {
		if c.err != nil {
			return 0, c.err
		}
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		s := c.streams[id]
		if s == nil || s.reset {
			return 0, E.New("xpc: stream ", id, " was reset by the service")
		}
		if want == 0 {
			return 0, nil
		}
		n := min(int64(want), int64(c.peerMaxFrame), s.sendWindow, c.sendWindow)
		if n > 0 {
			s.sendWindow -= n
			c.sendWindow -= n
			return int(n), nil
		}
		if stallTimer == nil {
			stalledAt = time.Now()
			stallTimer = time.AfterFunc(sendStallTimeout, func() {
				c.access.Lock()
				c.changed.Broadcast()
				c.access.Unlock()
			})
		} else if time.Since(stalledAt) >= sendStallTimeout {
			return 0, E.New("xpc: the service accepted no data for ", sendStallTimeout)
		}
		c.changed.Wait()
	}
}

func (c *Conn) Send(body map[string]any, wantReply bool) error {
	return c.SendContext(context.Background(), body, wantReply)
}

func (c *Conn) SendContext(ctx context.Context, body map[string]any, wantReply bool) error {
	_, err := c.send(ctx, body, wantReply)
	return err
}

// RemoteXPC numbers each side's messages with its own parity, the
// initiator's odd and the listener's even, and aborts with "Attempted to send
// non-reply msg on the reply channel" when asked to reply to an ID of the
// wrong parity (24B5110a RemoteXPC _xpc_remote_connection_send_reply_message
// 0x2a4bb8080, 0x2a4bb81d4; pymobiledevice3 cae0191,
// pymobiledevice3/remote/remotexpc.py:312-316), as dtuhidd did for an even ID
// (iPad17,1, iPadOS 27.2 24B115). A reply carries the reply flag and the ID of
// the message it answers (24B5110a RemoteXPC 0x2a4bb8110).
func (c *Conn) send(ctx context.Context, body map[string]any, wantReply bool) (id uint64, err error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer func() {
		if !stop() {
			c.Close()
			id, err = 0, ctx.Err()
		}
	}()
	flags := uint32(FlagAlwaysSet)
	if len(body) > 0 {
		flags |= FlagData
	}
	if wantReply {
		flags |= FlagWantingReply
	}
	id = c.nextMessageID
	data, err := Marshal(Message{Flags: flags, ID: id, Body: body})
	if err != nil {
		return 0, err
	}
	c.nextMessageID += 2
	err = c.writeData(ctx, rootStream, data, false)
	if ctx.Err() != nil {
		c.Close()
		return 0, ctx.Err()
	}
	return id, err
}

func (c *Conn) ReceiveMessage(ctx context.Context) (Message, error) {
	stop := c.wakeOnDone(ctx)
	defer stop()
	c.access.Lock()
	defer c.access.Unlock()
	for len(c.messages) == 0 {
		if c.err != nil {
			return Message{}, c.err
		}
		if err := ctx.Err(); err != nil {
			return Message{}, err
		}
		c.changed.Wait()
	}
	message := c.messages[0]
	c.messages[0] = Message{}
	c.messages = c.messages[1:]
	if len(c.messages) == 0 {
		c.messages = nil
	}
	c.queued -= message.size
	message.size = 0
	return message, nil
}

// idevice skips bodyless messages and empty dictionaries when receiving
// (idevice 3854a5d, idevice/src/xpc/mod.rs:151-174).
func (c *Conn) Receive() (map[string]any, error) {
	for {
		message, err := c.ReceiveMessage(context.Background())
		if err != nil {
			return nil, err
		}
		if len(message.Body) > 0 {
			return message.Body, nil
		}
	}
}

func (c *Conn) Request(ctx context.Context, body map[string]any) (reply map[string]any, err error) {
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer func() {
		if !stop() {
			c.Close()
			reply = nil
			err = ctx.Err()
		}
	}()
	id, err := c.send(ctx, body, true)
	for err == nil {
		var message Message
		message, err = c.ReceiveMessage(ctx)
		if err == nil && message.Flags&FlagReply != 0 && message.ID == id {
			return message.Body, nil
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, err
}

// pymobiledevice3 sends the file-transfer preamble, payload and END_STREAM
// without waiting for a transfer acknowledgement
// (pymobiledevice3 9c33842, pymobiledevice3/remote/remotexpc.py:500-531).
func (c *Conn) SendFile(ctx context.Context, id uint64, content io.Reader, size int64) (err error) {
	if err = ctx.Err(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer func() {
		if !stop() {
			c.Close()
			err = ctx.Err()
		}
	}()
	c.access.Lock()
	if c.err != nil {
		c.access.Unlock()
		return c.err
	}
	streamID := c.nextStream
	c.nextStream += 2
	c.streams[streamID] = &stream{sendWindow: c.peerInitialWindow}
	c.access.Unlock()
	defer func() {
		c.access.Lock()
		delete(c.streams, streamID)
		c.access.Unlock()
	}()
	err = c.writeFrame(frameHeaders, flagEndHeaders, streamID, nil)
	if err != nil {
		return err
	}
	opening, err := Marshal(Message{Flags: FlagAlwaysSet | FlagFileTransferRequest, ID: id})
	if err != nil {
		return err
	}
	err = c.writeData(ctx, streamID, opening, false)
	if err == nil {
		err = c.copyFile(ctx, streamID, content, size)
	}
	if err == nil {
		err = c.writeData(ctx, streamID, nil, true)
	}
	if err != nil {
		if ctx.Err() == nil {
			c.writeFrame(frameRSTStream, 0, streamID, binary.BigEndian.AppendUint32(nil, errorCodeCancel))
		}
		return E.Cause(err, "send file ", id)
	}
	return nil
}

func (c *Conn) copyFile(ctx context.Context, streamID uint32, content io.Reader, size int64) error {
	buffer := make([]byte, 64<<10)
	var sent int64
	for sent < size {
		n, err := content.Read(buffer[:min(int64(len(buffer)), size-sent)])
		if n > 0 {
			writeErr := c.writeData(ctx, streamID, buffer[:n], false)
			if writeErr != nil {
				return writeErr
			}
			sent += int64(n)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if sent != size {
		return E.New("xpc: file has ", sent, " bytes, ", size, " announced")
	}
	return nil
}

// Err reports why the connection ended, or nil while it is open.
func (c *Conn) Err() error {
	c.access.Lock()
	defer c.access.Unlock()
	return c.err
}

func (c *Conn) Close() error {
	c.fail(net.ErrClosed)
	return nil
}
