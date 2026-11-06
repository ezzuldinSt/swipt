package automation

import (
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
)

type FileInfo struct {
	Name      string
	Directory bool
	Size      int64
	Modified  time.Time
}

type fileRoot struct {
	root     *os.Root
	where    string
	readOnly bool
}

func openFileRoot(directory, where string) (*fileRoot, error) {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	return &fileRoot{root: root, where: where}, nil
}

func openPackageRoot(directory, where string) (*fileRoot, error) {
	root, err := os.OpenRoot(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return &fileRoot{where: where, readOnly: true}, nil
	}
	if err != nil {
		return nil, err
	}
	return &fileRoot{root: root, where: where, readOnly: true}, nil
}

func (r *fileRoot) Close() error {
	if r.root == nil {
		return nil
	}
	return r.root.Close()
}

func (r *fileRoot) readOnlyError() error {
	return E.Cause1(ErrNotAllowed, E.New(r.where, " is read-only"))
}

func cleanName(name string) (string, error) {
	if strings.HasPrefix(name, "/") {
		return "", E.Cause1(ErrInvalid, E.New("the path ", strconv.Quote(name), " is absolute"))
	}
	cleaned := path.Clean(name)
	if cleaned == "." || cleaned == "" {
		return ".", nil
	}
	if !fs.ValidPath(cleaned) {
		return "", E.Cause1(ErrInvalid, E.New("the path ", strconv.Quote(name), " leaves the directory"))
	}
	return filepath.FromSlash(cleaned), nil
}

func FileNotFound(where, name string) error {
	return E.Cause1(ErrNotFound, E.New("there is no ", strconv.Quote(filepath.ToSlash(name)), " in ", where))
}

func (r *fileRoot) error(name string, err error) error {
	pathError, isPath := errors.AsType[*fs.PathError](err)
	if !isPath {
		return err
	}
	shown := strconv.Quote(filepath.ToSlash(name))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return FileNotFound(r.where, name)
	case errors.Is(err, syscall.ENOTDIR):
		return E.Cause1(ErrInvalid, E.New("a file stands where ", shown, " needs a directory in ", r.where))
	case errors.Is(err, syscall.EISDIR):
		return E.Cause1(ErrInvalid, E.New(shown, " is a directory in ", r.where))
	case errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.EEXIST):
		return E.Cause1(ErrInvalid, E.New(shown, " already exists in ", r.where))
	}
	return E.New(shown, " in ", r.where, ": ", pathError.Err)
}

func (r *fileRoot) notRoot(name string) error {
	if name == "." {
		return E.Cause1(ErrInvalid, E.New("the path names ", r.where, " itself"))
	}
	return nil
}

func (r *fileRoot) stat(name string) (FileInfo, bool, error) {
	if r.root == nil {
		return FileInfo{}, false, nil
	}
	info, err := r.root.Stat(name)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return FileInfo{}, false, nil
	}
	if err != nil {
		return FileInfo{}, false, r.error(name, err)
	}
	return FileInfo{Name: info.Name(), Directory: info.IsDir(), Size: info.Size(), Modified: info.ModTime()}, true, nil
}

func (r *fileRoot) list(name string) ([]FileInfo, error) {
	if r.root == nil {
		return nil, FileNotFound(r.where, name)
	}
	directory, err := r.root.Open(name)
	if err != nil {
		return nil, r.error(name, err)
	}
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return nil, r.error(name, err)
	}
	files := make([]FileInfo, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		files = append(files, FileInfo{Name: entry.Name(), Directory: entry.IsDir(), Size: info.Size(), Modified: info.ModTime()})
	}
	return files, nil
}

func (r *fileRoot) open(name string) (*os.File, error) {
	if r.root == nil {
		return nil, FileNotFound(r.where, name)
	}
	file, err := r.root.Open(name)
	if err != nil {
		return nil, r.error(name, err)
	}
	info, err := file.Stat()
	if err == nil && info.IsDir() {
		err = &fs.PathError{Op: "open", Path: name, Err: syscall.EISDIR}
	}
	if err != nil {
		_ = file.Close()
		return nil, r.error(name, err)
	}
	return file, nil
}

func (r *fileRoot) read(name string) ([]byte, error) {
	if r.root == nil {
		return nil, FileNotFound(r.where, name)
	}
	data, err := r.root.ReadFile(name)
	if err != nil {
		return nil, r.error(name, err)
	}
	return data, nil
}

type FileWriter struct {
	*os.File
	root      *fileRoot
	directory *os.Root
	name      string
	temporary string
}

func (r *fileRoot) create(name string) (*FileWriter, error) {
	if r.readOnly {
		return nil, r.readOnlyError()
	}
	if err := r.notRoot(name); err != nil {
		return nil, err
	}
	parent := filepath.Dir(name)
	if err := r.root.MkdirAll(parent, 0o755); err != nil {
		return nil, r.error(parent, err)
	}
	directory, err := r.root.OpenRoot(parent)
	if err != nil {
		return nil, r.error(parent, err)
	}
	temporary := ".write-" + rand.Text()
	file, err := directory.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		_ = directory.Close()
		return nil, r.error(name, err)
	}
	return &FileWriter{File: file, root: r, directory: directory, name: name, temporary: temporary}, nil
}

func (w *FileWriter) Finish(abort error) error {
	err := w.File.Close()
	if abort == nil && err == nil {
		err = w.directory.Rename(w.temporary, filepath.Base(w.name))
	}
	if abort != nil || err != nil {
		_ = w.directory.Remove(w.temporary)
	}
	_ = w.directory.Close()
	if err != nil {
		return w.root.error(w.name, err)
	}
	return nil
}

func (r *fileRoot) write(name string, data []byte) error {
	file, err := r.create(name)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err != nil {
		_ = file.Finish(err)
		return r.error(name, err)
	}
	return file.Finish(nil)
}

func (r *fileRoot) mkdir(name string) error {
	if r.readOnly {
		return r.readOnlyError()
	}
	if err := r.root.MkdirAll(name, 0o755); err != nil {
		return r.error(name, err)
	}
	return nil
}

func (r *fileRoot) remove(name string) error {
	if r.readOnly {
		return r.readOnlyError()
	}
	if err := r.notRoot(name); err != nil {
		return err
	}
	if err := r.root.RemoveAll(name); err != nil {
		return r.error(name, err)
	}
	return nil
}

func (r *fileRoot) copyTo(name string, destination *fileRoot, target string) error {
	if destination.readOnly {
		return destination.readOnlyError()
	}
	if err := destination.notRoot(target); err != nil {
		return err
	}
	if r.root == nil {
		return FileNotFound(r.where, name)
	}
	return fs.WalkDir(r.root.FS(), filepath.ToSlash(name), func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return r.error(current, err)
		}
		relative, err := filepath.Rel(name, filepath.FromSlash(current))
		if err != nil {
			return err
		}
		to := filepath.Join(target, relative)
		if entry.IsDir() {
			return destination.mkdir(to)
		}
		in, err := r.root.Open(filepath.FromSlash(current))
		if err != nil {
			return r.error(current, err)
		}
		defer in.Close()
		if err = destination.root.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return destination.error(filepath.Dir(to), err)
		}
		out, err := destination.root.Create(to)
		if err != nil {
			return destination.error(to, err)
		}
		_, err = io.Copy(out, in)
		return E.Errors(err, out.Close())
	})
}

func (r *fileRoot) moveTo(name string, destination *fileRoot, target string) error {
	if r.readOnly {
		return r.readOnlyError()
	}
	if destination.readOnly {
		return destination.readOnlyError()
	}
	if err := E.Errors(r.notRoot(name), destination.notRoot(target)); err != nil {
		return err
	}
	if r.root.Name() == destination.root.Name() {
		if err := destination.root.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return destination.error(filepath.Dir(target), err)
		}
		if err := r.root.Rename(name, target); err != nil {
			return r.error(name, err)
		}
		return nil
	}
	if err := r.copyTo(name, destination, target); err != nil {
		return err
	}
	return r.remove(name)
}

func swipletRoot(swiplet Swiplet, kind string) (*fileRoot, error) {
	switch kind {
	case DataFolder:
		return openFileRoot(filepath.Join(swiplet.Directory, DataFolder), "the data directory of "+swiplet.ID)
	case AssetsFolder:
		return openPackageRoot(filepath.Join(swiplet.Directory, AssetsFolder), "the assets of "+swiplet.ID)
	}
	return nil, E.Cause1(ErrInvalid, E.New("unknown file root ", strconv.Quote(kind)))
}

func (h *Host) swipletRoot(id, kind string) (*fileRoot, error) {
	if id == REPLDirectory && kind == DataFolder {
		return openFileRoot(filepath.Join(h.options.Directory, REPLDirectory, DataFolder), "the data directory of the REPL")
	}
	swiplet, err := h.Swiplet(id)
	if err != nil {
		return nil, err
	}
	return swipletRoot(swiplet, kind)
}

func (h *Host) withRoot(id, kind, name string, work func(root *fileRoot, name string) error) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	root, err := h.swipletRoot(id, kind)
	if err != nil {
		return err
	}
	defer root.Close()
	return work(root, name)
}

func (h *Host) ListFiles(id, kind, name string) (files []FileInfo, err error) {
	err = h.withRoot(id, kind, name, func(root *fileRoot, name string) error {
		files, err = root.list(name)
		return err
	})
	return
}

func (h *Host) ReadFile(id, kind, name string) (data []byte, err error) {
	err = h.withRoot(id, kind, name, func(root *fileRoot, name string) error {
		data, err = root.read(name)
		return err
	})
	return
}

func (h *Host) WriteFile(id, kind, name string, data []byte) error {
	return h.withRoot(id, kind, name, func(root *fileRoot, name string) error { return root.write(name, data) })
}

func (h *Host) RemoveFile(id, kind, name string) error {
	return h.withRoot(id, kind, name, func(root *fileRoot, name string) error { return root.remove(name) })
}
