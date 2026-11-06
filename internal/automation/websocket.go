package automation

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/jsc"

	"github.com/coder/websocket"
)

const webSocketReadLimit = 64 << 20

type webSocket struct {
	conn    *websocket.Conn
	closing atomic.Bool
	once    sync.Once
	closed  chan struct{}
	result  map[string]any
}

func closeResult(code websocket.StatusCode, reason string, clean bool, err error) map[string]any {
	result := map[string]any{"code": int(code), "reason": reason, "clean": clean}
	if err != nil {
		result["error"] = Message(networkError(err, "read from the WebSocket"))
	}
	return result
}

func (s *webSocket) close(code websocket.StatusCode, reason string) {
	s.once.Do(func() {
		s.closing.Store(true)
		err := s.conn.Close(code, reason)
		closeError, isClose := errors.AsType[websocket.CloseError](err)
		switch {
		case isClose:
			s.result = closeResult(closeError.Code, closeError.Reason, true, nil)
		case err != nil:
			s.result = closeResult(websocket.StatusAbnormalClosure, "", false, err)
		default:
			s.result = closeResult(code, reason, true, nil)
		}
		close(s.closed)
	})
}

func webSocketConnect(ctx context.Context, call *operation) (any, error) {
	var address string
	var protocols []string
	var fields [][2]string
	var options clientOptions
	if err := call.decode(1, &address, &protocols, &fields, &options); err != nil {
		return nil, err
	}
	e := call.engine
	target, err := url.Parse(address)
	if err != nil {
		return nil, E.Cause1(ErrInvalid, err)
	}
	client, err := e.httpClient(options, true)
	if err != nil {
		return nil, err
	}
	client.Transport = handshake{transport: client.Transport, protocols: protocols}
	var jar *cookieJar
	if options.Cookies != nil {
		if jar, err = e.cookieJar(*options.Cookies); err != nil {
			return nil, err
		}
	}
	header := make(http.Header)
	host := ""
	for _, field := range fields {
		name, value := byteString(field[0]), byteString(field[1])
		if strings.EqualFold(name, "host") {
			host = value
		} else {
			header.Add(name, value)
		}
	}
	if _, set := header["User-Agent"]; !set {
		header.Set("User-Agent", userAgent)
	}
	if jar != nil {
		cookies, err := jar.header(target)
		if err != nil {
			return nil, err
		}
		attachCookies(header, cookies)
	}
	dialCtx, cancel := context.WithCancelCause(e.ctx)
	stop := context.AfterFunc(ctx, func() { cancel(context.Cause(ctx)) })
	conn, response, err := websocket.Dial(dialCtx, address, &websocket.DialOptions{
		HTTPClient:   client,
		HTTPHeader:   header,
		Host:         host,
		Subprotocols: protocols,
	})
	if !stop() {
		if err == nil {
			_ = conn.CloseNow()
		}
		return nil, ctx.Err()
	}
	if jar != nil && response != nil {
		if storeErr := jar.store(target, response.Header.Values("Set-Cookie")); storeErr != nil && err == nil {
			_ = conn.CloseNow()
			err = storeErr
		}
	}
	if err != nil {
		cancel(nil)
		return nil, networkError(err, "connect to ", address)
	}
	conn.SetReadLimit(webSocketReadLimit)
	id := e.addResource(&resource{socket: &webSocket{conn: conn, closed: make(chan struct{})}, finish: func(error) error { return conn.CloseNow() }})
	return map[string]any{"socket": id, "protocol": conn.Subprotocol(), "extensions": response.Header.Get("Sec-WebSocket-Extensions")}, nil
}

type handshake struct {
	transport http.RoundTripper
	protocols []string
}

func (h handshake) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := h.transport.RoundTrip(request)
	if err != nil || response.StatusCode != http.StatusSwitchingProtocols {
		return response, err
	}
	protocol := response.Header.Get("Sec-WebSocket-Protocol")
	if protocol == "" && len(h.protocols) == 0 || protocol != "" && slices.Contains(h.protocols, protocol) {
		return response, nil
	}
	_ = response.Body.Close()
	if protocol == "" {
		return nil, E.New("the server selected none of the protocols")
	}
	return nil, E.New("the server selected the protocol ", strconv.Quote(protocol), ", which wasn't requested")
}

func (o *operation) socket(id string) (*webSocket, error) {
	it, err := o.engine.resource(id)
	if err != nil {
		return nil, err
	}
	if it.socket == nil {
		return nil, E.Cause1(ErrInvalid, E.New("the resource is not a WebSocket"))
	}
	return it.socket, nil
}

func webSocketRead(ctx context.Context, call *operation) (any, error) {
	var id string
	if err := call.decode(1, &id); err != nil {
		return nil, err
	}
	socket, err := call.socket(id)
	if err != nil {
		return nil, err
	}
	kind, data, err := socket.conn.Read(ctx)
	if err == nil {
		if kind == websocket.MessageText {
			return string(data), nil
		}
		return jsc.Bytes(data), nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	result := closeResult(websocket.StatusAbnormalClosure, "", false, err)
	if closed, isClose := errors.AsType[websocket.CloseError](err); isClose {
		result = closeResult(closed.Code, closed.Reason, true, nil)
	} else if socket.closing.Load() {
		select {
		case <-socket.closed:
			result = socket.result
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if taken, takeErr := call.engine.takeResource(id); takeErr == nil {
		_ = taken.close(nil)
	}
	return result, nil
}

func webSocketSend(ctx context.Context, call *operation) (any, error) {
	var id, data string
	var binary bool
	if err := call.decode(3, &id, &data, &binary); err != nil {
		return nil, err
	}
	kind, payload := websocket.MessageText, []byte(data)
	if binary {
		var err error
		if payload, err = call.engine.takeBlob(data); err != nil {
			return nil, err
		}
		kind = websocket.MessageBinary
	}
	socket, err := call.socket(id)
	if err != nil {
		return nil, err
	}
	return nil, socket.conn.Write(ctx, kind, payload)
}

func webSocketClose(_ context.Context, call *operation) (any, error) {
	var id, reason string
	var code int
	if err := call.decode(3, &id, &code, &reason); err != nil {
		return nil, err
	}
	if socket, err := call.socket(id); err == nil {
		socket.close(websocket.StatusCode(code), reason)
	}
	return nil, nil
}
