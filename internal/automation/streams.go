package automation

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"io"
	"strconv"
	"sync"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/jsc"

	"github.com/andybalholm/brotli"
)

const streamChunk = 64 << 10

var (
	errReleased     = E.New("the stream was released")
	errTrailingData = E.New("the compressed data continues after its end")
)

type resource struct {
	reader    io.Reader
	writer    io.Writer
	socket    *webSocket
	finish    func(abort error) error
	interrupt func(abort error)
	once      sync.Once
	err       error
}

func (r *resource) close(abort error) error {
	r.once.Do(func() { r.err = r.finish(abort) })
	return r.err
}

func (e *engine) addResource(it *resource) string {
	e.access.Lock()
	defer e.access.Unlock()
	e.resourceSequence++
	id := "r" + strconv.Itoa(e.resourceSequence)
	e.resources[id] = it
	if it.interrupt != nil {
		e.interrupts[id] = it.interrupt
		finish := it.finish
		it.finish = func(abort error) error {
			e.access.Lock()
			delete(e.interrupts, id)
			e.access.Unlock()
			return finish(abort)
		}
	}
	return id
}

func (e *engine) resource(id string) (*resource, error) {
	e.access.Lock()
	defer e.access.Unlock()
	it := e.resources[id]
	if it == nil {
		return nil, E.Cause1(ErrInvalid, E.New("the stream ", strconv.Quote(id), " is closed"))
	}
	return it, nil
}

func (e *engine) takeResource(id string) (*resource, error) {
	e.access.Lock()
	defer e.access.Unlock()
	it := e.resources[id]
	if it == nil {
		return nil, E.Cause1(ErrInvalid, E.New("the stream ", strconv.Quote(id), " is closed"))
	}
	delete(e.resources, id)
	return it, nil
}

func (e *engine) closeResources() {
	e.access.Lock()
	resources := e.resources
	e.resources = make(map[string]*resource)
	e.access.Unlock()
	for _, it := range resources {
		_ = it.close(errEnded)
	}
}

func pipeResources() (reader *resource, writer *resource) {
	pipeReader, pipeWriter := io.Pipe()
	reader = &resource{reader: pipeReader, finish: func(abort error) error {
		return pipeReader.CloseWithError(abort)
	}}
	writer = &resource{writer: pipeWriter, finish: func(abort error) error {
		return pipeWriter.CloseWithError(abort)
	}}
	return reader, writer
}

func (e *engine) release(arguments []jsc.Value) (any, error) {
	values, err := texts(arguments, 1)
	if err != nil {
		return nil, err
	}
	reason := ""
	if len(arguments) > 1 {
		reason, _ = arguments[1].ToString()
	}
	e.releaseResource(values[0], reason)
	return nil, nil
}

func (e *engine) releaseResource(id, reason string) {
	abort := errReleased
	if reason != "" {
		abort = failureError(reason)
	}
	if it, err := e.takeResource(id); err == nil {
		go it.close(abort)
		return
	}
	e.access.Lock()
	interrupt := e.interrupts[id]
	e.access.Unlock()
	if interrupt != nil {
		interrupt(abort)
	}
}

func (e *engine) compression(arguments []jsc.Value) (any, error) {
	values, err := texts(arguments, 2)
	if err != nil {
		return nil, err
	}
	return e.newCompression(values[0], values[1] == "decompress")
}

func (e *engine) newCompression(format string, decompress bool) ([]string, error) {
	switch format {
	case "brotli", "gzip", "deflate", "deflate-raw":
	default:
		return nil, E.Cause1(ErrInvalid, E.New("the compression format ", strconv.Quote(format), " is not one of brotli, gzip, deflate and deflate-raw"))
	}
	inputReader, inputWriter := pipeResources()
	input := inputReader.reader.(*io.PipeReader)
	inputPipe := inputWriter.writer.(*io.PipeWriter)
	output := newCodecOutput()
	done := make(chan error, 1)
	go func() {
		var err error
		if decompress {
			err = decompressData(format, input, output)
		} else {
			err = compressData(format, input, output)
		}
		_ = input.CloseWithError(err)
		output.closeWrite(err)
		done <- err
	}()
	// closing the input reports the codec's result, as a TransformStream's flush does, without waiting for a reader
	inputWriter.finish = func(abort error) error {
		if abort != nil {
			return inputPipe.CloseWithError(abort)
		}
		output.drain()
		_ = inputPipe.Close()
		return <-done
	}
	outputReader := &resource{reader: output, finish: func(abort error) error {
		output.closeRead(abort)
		return nil
	}}
	return []string{e.addResource(inputWriter), e.addResource(outputReader)}, nil
}

// codecOutput carries a codec's output to its readable side. It holds back the codec while a chunk waits to be read,
// until the input closes; from then on it buffers, so the codec can finish and report its result.
type codecOutput struct {
	access   sync.Mutex
	changed  *sync.Cond
	chunks   [][]byte
	size     int
	draining bool
	writeErr error
	readErr  error
}

func newCodecOutput() *codecOutput {
	o := &codecOutput{}
	o.changed = sync.NewCond(&o.access)
	return o
}

func (o *codecOutput) Write(data []byte) (int, error) {
	o.access.Lock()
	defer o.access.Unlock()
	for !o.draining && o.size >= streamChunk && o.readErr == nil {
		o.changed.Wait()
	}
	if o.readErr != nil {
		return 0, o.readErr
	}
	if len(data) > 0 {
		o.chunks = append(o.chunks, append([]byte(nil), data...))
		o.size += len(data)
		o.changed.Broadcast()
	}
	return len(data), nil
}

func (o *codecOutput) Read(buffer []byte) (int, error) {
	o.access.Lock()
	defer o.access.Unlock()
	for len(o.chunks) == 0 && o.writeErr == nil && o.readErr == nil {
		o.changed.Wait()
	}
	if o.readErr != nil {
		return 0, io.ErrClosedPipe
	}
	if len(o.chunks) == 0 {
		return 0, o.writeErr
	}
	n := copy(buffer, o.chunks[0])
	if n == len(o.chunks[0]) {
		o.chunks = o.chunks[1:]
	} else {
		o.chunks[0] = o.chunks[0][n:]
	}
	o.size -= n
	o.changed.Broadcast()
	return n, nil
}

func (o *codecOutput) drain() {
	o.access.Lock()
	defer o.access.Unlock()
	o.draining = true
	o.changed.Broadcast()
}

func (o *codecOutput) closeWrite(err error) {
	o.access.Lock()
	defer o.access.Unlock()
	if o.writeErr == nil {
		o.writeErr = err
		if err == nil {
			o.writeErr = io.EOF
		}
	}
	o.changed.Broadcast()
}

func (o *codecOutput) closeRead(err error) {
	o.access.Lock()
	defer o.access.Unlock()
	if o.readErr == nil {
		o.readErr = err
		if err == nil {
			o.readErr = io.ErrClosedPipe
		}
		o.chunks = nil
		o.size = 0
	}
	o.changed.Broadcast()
}

func compressData(format string, input io.Reader, output io.Writer) error {
	var encoder io.WriteCloser
	switch format {
	case "brotli":
		encoder = brotli.NewWriter(output)
	case "gzip":
		encoder = gzip.NewWriter(output)
	case "deflate":
		encoder = zlib.NewWriter(output)
	default:
		encoder, _ = flate.NewWriter(output, flate.DefaultCompression)
	}
	_, err := io.Copy(encoder, input)
	return E.Errors(err, encoder.Close())
}

func decompressData(format string, input io.Reader, output io.Writer) error {
	buffered := bufio.NewReader(input)
	var decoder io.Reader
	var err error
	switch format {
	case "brotli":
		decoder = brotli.NewReader(buffered)
	case "gzip":
		var reader *gzip.Reader
		if reader, err = gzip.NewReader(buffered); err == nil {
			reader.Multistream(false)
			decoder = reader
		}
	case "deflate":
		decoder, err = zlib.NewReader(buffered)
	default:
		decoder = flate.NewReader(buffered)
	}
	if err == nil {
		_, err = io.Copy(output, decoder)
	}
	if err == nil {
		if _, peeked := buffered.Peek(1); peeked == nil {
			err = errTrailingData
		}
	}
	if err != nil && !E.IsMulti(err, io.ErrClosedPipe, errReleased, errEnded) {
		return E.Cause1(ErrInvalid, E.Cause(err, "decompress ", format))
	}
	return err
}

func (e *engine) releaseOnCancel(ctx context.Context, id string) func() bool {
	return context.AfterFunc(ctx, func() {
		if it, err := e.takeResource(id); err == nil {
			_ = it.close(context.Cause(ctx))
		}
	})
}

func streamRead(ctx context.Context, call *operation) (any, error) {
	var id string
	if err := call.decode(1, &id); err != nil {
		return nil, err
	}
	e := call.engine
	it, err := e.resource(id)
	if err != nil {
		return nil, err
	}
	if it.reader == nil {
		return nil, E.Cause1(ErrInvalid, E.New("the stream ", strconv.Quote(id), " is not readable"))
	}
	defer e.releaseOnCancel(ctx, id)()
	buffer := make([]byte, streamChunk)
	for {
		n, err := it.reader.Read(buffer)
		if n > 0 {
			return jsc.Bytes(buffer[:n]), nil
		}
		if err == nil {
			continue
		}
		if taken, takeErr := e.takeResource(id); takeErr == nil {
			if err == io.EOF {
				return nil, taken.close(nil)
			}
			_ = taken.close(err)
		}
		if err == io.EOF {
			return nil, nil
		}
		return nil, err
	}
}

func streamReadAll(ctx context.Context, call *operation) (any, error) {
	var id string
	if err := call.decode(1, &id); err != nil {
		return nil, err
	}
	it, err := call.engine.takeResource(id)
	if err != nil {
		return nil, err
	}
	defer context.AfterFunc(ctx, func() { _ = it.close(context.Cause(ctx)) })()
	if it.reader == nil {
		_ = it.close(errReleased)
		return nil, E.Cause1(ErrInvalid, E.New("the stream ", strconv.Quote(id), " is not readable"))
	}
	data, err := io.ReadAll(it.reader)
	if err != nil {
		_ = it.close(err)
		return nil, err
	}
	if err = it.close(nil); err != nil {
		return nil, err
	}
	return jsc.Bytes(data), nil
}

func streamWrite(_ context.Context, call *operation) (any, error) {
	var id, key string
	if err := call.decode(2, &id, &key); err != nil {
		return nil, err
	}
	e := call.engine
	data, err := e.takeBlob(key)
	if err != nil {
		return nil, err
	}
	it, err := e.resource(id)
	if err != nil {
		return nil, err
	}
	if it.writer == nil {
		return nil, E.Cause1(ErrInvalid, E.New("the stream ", strconv.Quote(id), " is not writable"))
	}
	if _, err = it.writer.Write(data); err != nil {
		if taken, takeErr := e.takeResource(id); takeErr == nil {
			_ = taken.close(err)
		}
		return nil, err
	}
	return nil, nil
}

func streamClose(_ context.Context, call *operation) (any, error) {
	var id string
	if err := call.decode(1, &id); err != nil {
		return nil, err
	}
	it, err := call.engine.takeResource(id)
	if err != nil {
		return nil, err
	}
	return nil, it.close(nil)
}

func streamPipe(ctx context.Context, call *operation) (any, error) {
	var from, to string
	if err := call.decode(2, &from, &to); err != nil {
		return nil, err
	}
	e := call.engine
	source, err := e.takeResource(from)
	if err != nil {
		return nil, err
	}
	destination, err := e.takeResource(to)
	if err != nil {
		_ = source.close(errReleased)
		return nil, err
	}
	if source.reader == nil || destination.writer == nil {
		_ = source.close(errReleased)
		_ = destination.close(errReleased)
		return nil, E.Cause1(ErrInvalid, E.New("the streams can't be piped"))
	}
	stop := context.AfterFunc(ctx, func() {
		_ = source.close(context.Cause(ctx))
		_ = destination.close(context.Cause(ctx))
	})
	defer stop()
	_, err = io.CopyBuffer(destination.writer, source.reader, make([]byte, streamChunk))
	if err != nil {
		_ = source.close(err)
		_ = destination.close(err)
		return nil, err
	}
	return nil, E.Errors(source.close(nil), destination.close(nil))
}
