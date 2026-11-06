package automation

import (
	"context"
	"os"
	"sync"

	"github.com/sagernet/sing/common"
	"github.com/swipt-automation/swipt/internal/jsc"
)

func (e *engine) fileRoot(kind string) (*fileRoot, error) {
	if kind == "cache" && e.caches != "" {
		if err := os.MkdirAll(e.caches, 0o755); err != nil {
			return nil, err
		}
		return openFileRoot(e.caches, "the cache directory")
	}
	return swipletRoot(e.swiplet, kind)
}

type fileLocation struct {
	root *fileRoot
	name string
}

func (e *engine) fileLocation(kind, name string) (*fileLocation, error) {
	cleaned, err := cleanName(name)
	if err != nil {
		return nil, err
	}
	root, err := e.fileRoot(kind)
	if err != nil {
		return nil, err
	}
	return &fileLocation{root: root, name: cleaned}, nil
}

func (o *operation) oneFileLocation() (*fileLocation, error) {
	var kind, name string
	if err := o.decode(2, &kind, &name); err != nil {
		return nil, err
	}
	return o.engine.fileLocation(kind, name)
}

type fileStat struct {
	Kind     string `json:"kind"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"`
}

type fileEntry struct {
	Name      string `json:"name"`
	Directory bool   `json:"directory"`
}

func fsStat(_ context.Context, call *operation) (any, error) {
	at, err := call.oneFileLocation()
	if err != nil {
		return nil, err
	}
	defer at.root.Close()
	info, found, err := at.root.stat(at.name)
	if err != nil || !found {
		return nil, err
	}
	kind := "file"
	if info.Directory {
		kind = "directory"
	}
	return fileStat{Kind: kind, Size: info.Size, Modified: info.Modified.UnixMilli()}, nil
}

func fsList(_ context.Context, call *operation) (any, error) {
	at, err := call.oneFileLocation()
	if err != nil {
		return nil, err
	}
	defer at.root.Close()
	files, err := at.root.list(at.name)
	if err != nil {
		return nil, err
	}
	return common.Map(files, func(file FileInfo) fileEntry { return fileEntry{Name: file.Name, Directory: file.Directory} }), nil
}

func fsRead(_ context.Context, call *operation) (any, error) {
	at, err := call.oneFileLocation()
	if err != nil {
		return nil, err
	}
	defer at.root.Close()
	data, err := at.root.read(at.name)
	if err != nil {
		return nil, err
	}
	return jsc.Bytes(data), nil
}

func fsWrite(_ context.Context, call *operation) (any, error) {
	var kind, name, key string
	if err := call.decode(3, &kind, &name, &key); err != nil {
		return nil, err
	}
	at, err := call.engine.fileLocation(kind, name)
	if err != nil {
		return nil, err
	}
	defer at.root.Close()
	data, err := call.engine.takeBlob(key)
	if err != nil {
		return nil, err
	}
	return nil, at.root.write(at.name, data)
}

func fsWriter(_ context.Context, call *operation) (any, error) {
	at, err := call.oneFileLocation()
	if err != nil {
		return nil, err
	}
	defer at.root.Close()
	file, err := at.root.create(at.name)
	if err != nil {
		return nil, err
	}
	return call.engine.addResource(&resource{writer: file, finish: file.Finish}), nil
}

type fileSource struct {
	access sync.Mutex
	open   func() (*os.File, error)
	file   *os.File
	closed bool
}

func (s *fileSource) opened() (*os.File, error) {
	s.access.Lock()
	defer s.access.Unlock()
	if s.closed {
		return nil, os.ErrClosed
	}
	if s.file == nil {
		file, err := s.open()
		if err != nil {
			return nil, err
		}
		s.file = file
	}
	return s.file, nil
}

func (s *fileSource) Read(buffer []byte) (int, error) {
	file, err := s.opened()
	if err != nil {
		return 0, err
	}
	return file.Read(buffer)
}

func (s *fileSource) Close() error {
	s.access.Lock()
	defer s.access.Unlock()
	s.closed = true
	if s.file == nil {
		return nil
	}
	return s.file.Close()
}

func (e *engine) openFile(arguments []jsc.Value) (any, error) {
	values, err := texts(arguments, 2)
	if err != nil {
		return nil, err
	}
	return e.newFileSource(values[0], values[1]), nil
}

func (e *engine) newFileSource(kind, name string) string {
	source := &fileSource{open: func() (*os.File, error) {
		at, err := e.fileLocation(kind, name)
		if err != nil {
			return nil, err
		}
		defer at.root.Close()
		return at.root.open(at.name)
	}}
	return e.addResource(&resource{reader: source, finish: func(error) error { return source.Close() }})
}

func fsMkdir(_ context.Context, call *operation) (any, error) {
	at, err := call.oneFileLocation()
	if err != nil {
		return nil, err
	}
	defer at.root.Close()
	return nil, at.root.mkdir(at.name)
}

func fsRemove(_ context.Context, call *operation) (any, error) {
	at, err := call.oneFileLocation()
	if err != nil {
		return nil, err
	}
	defer at.root.Close()
	return nil, at.root.remove(at.name)
}

func fsTransfer(move bool) operationFunc {
	return func(_ context.Context, call *operation) (any, error) {
		var fromKind, fromName, toKind, toName string
		if err := call.decode(4, &fromKind, &fromName, &toKind, &toName); err != nil {
			return nil, err
		}
		from, err := call.engine.fileLocation(fromKind, fromName)
		if err != nil {
			return nil, err
		}
		defer from.root.Close()
		to, err := call.engine.fileLocation(toKind, toName)
		if err != nil {
			return nil, err
		}
		defer to.root.Close()
		if move {
			return nil, from.root.moveTo(from.name, to.root, to.name)
		}
		return nil, from.root.copyTo(from.name, to.root, to.name)
	}
}
