package automation

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"slices"
	"sync"
	"unicode/utf8"

	E "github.com/sagernet/sing/common/exceptions"
)

var webOperations = []string{
	"stream.read", "stream.readAll", "stream.write", "stream.close", "stream.pipe", "stream.create",
	"fetch", "cookies.get", "cookies.set", "cookies.clear", "websocket.connect", "websocket.read", "websocket.send", "websocket.close",
	"fs.stat", "fs.list", "fs.read", "fs.write", "fs.writer", "fs.mkdir", "fs.remove", "fs.copy", "fs.move",
	"crypto.encrypt", "crypto.decrypt", "crypto.sign", "crypto.verify", "crypto.derive", "crypto.generate", "crypto.import", "crypto.export",
}

const randomLimit = 65536

type WebSession struct {
	engine     *engine
	access     sync.Mutex
	operations map[string]context.CancelFunc
}

func (h *Host) OpenWeb(id string) (*WebSession, error) {
	swiplet, err := h.Swiplet(id)
	if err != nil {
		return nil, err
	}
	e := &engine{host: h, swiplet: swiplet, caches: h.cacheDirectory(id), blobs: make(map[string][]byte), resources: make(map[string]*resource)}
	e.ctx, e.cancel = context.WithCancelCause(h.ctx)
	return &WebSession{engine: e, operations: make(map[string]context.CancelFunc)}, nil
}

func (s *WebSession) Start(id, name, encoded string, done func(value any, err error)) {
	run := operations[name]
	if !slices.Contains(webOperations, name) {
		go done(nil, E.Cause1(ErrInvalid, E.New("unknown operation ", name)))
		return
	}
	var args []json.RawMessage
	if err := json.Unmarshal([]byte(encoded), &args); err != nil {
		go done(nil, E.Cause1(ErrInvalid, E.Cause(err, "decode the arguments of ", name)))
		return
	}
	ctx, cancel := context.WithCancel(s.engine.ctx)
	s.access.Lock()
	s.operations[id] = cancel
	s.access.Unlock()
	go func() {
		value, err := run(ctx, &operation{engine: s.engine, name: name, args: args})
		s.access.Lock()
		delete(s.operations, id)
		s.access.Unlock()
		cancel()
		done(value, err)
	}()
}

func (s *WebSession) Cancel(id string) {
	s.access.Lock()
	cancel := s.operations[id]
	s.access.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *WebSession) Put(data []byte) string {
	return s.engine.putBlob(data)
}

func (s *WebSession) Release(id, reason string) {
	s.engine.releaseResource(id, reason)
}

func (s *WebSession) OpenFile(kind, name string) string {
	return s.engine.newFileSource(kind, name)
}

func (s *WebSession) Compression(format, mode string) ([]string, error) {
	return s.engine.newCompression(format, mode == "decompress")
}

func (s *WebSession) Random(length int) ([]byte, error) {
	if length < 0 || length > randomLimit {
		return nil, E.Cause1(ErrInvalid, E.New("the length of random bytes must be between 0 and ", randomLimit))
	}
	data := make([]byte, length)
	_, _ = rand.Read(data)
	return data, nil
}

func (s *WebSession) Digest(algorithm string, data []byte) ([]byte, error) {
	return digestOf(algorithm, data)
}

func DecodeText(data []byte, fatal bool) (text string, restored int, ok bool) {
	if utf8.Valid(data) {
		return string(data), 0, true
	}
	return decodeUTF8(data, fatal)
}

func (s *WebSession) Close() {
	s.engine.cancel(errEnded)
	s.access.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.operations))
	for _, cancel := range s.operations {
		cancels = append(cancels, cancel)
	}
	clear(s.operations)
	s.access.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	s.engine.closeResources()
	s.engine.access.Lock()
	clear(s.engine.blobs)
	s.engine.access.Unlock()
}
