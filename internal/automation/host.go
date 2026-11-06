package automation

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/device"
	"github.com/swipt-automation/swipt/internal/log"
)

type Caller string

const (
	CallerApp       Caller = "app"
	CallerShortcuts Caller = "shortcuts"
	CallerAPI       Caller = "api"
)

var (
	ErrNotFound   = E.New("not found")
	ErrNotAllowed = E.New("not allowed")
	ErrInvalid    = device.ErrInvalid

	errClosed  = E.New("the host closed")
	errRemoved = E.New("the swiplet was removed")
)

var packageFiles = []string{SourceFolder, AssetsFolder, ManifestFile, IconFile}

type Options struct {
	Device            *device.Phone
	Directory         string
	Caches            string
	Languages         []string
	LogFactory        *log.Factory
	Dialog            Dialog
	Graphics          Graphics
	OnSwipletsChanged func()
}

type Host struct {
	options    Options
	logFactory *log.Factory
	logger     log.ContextLogger
	ctx        context.Context
	cancel     context.CancelCauseFunc
	lease      *foregroundLease

	installAccess sync.Mutex

	access   sync.Mutex
	runners  map[string]*runner
	removing string
	closed   bool

	watchAccess sync.Mutex
	watchers    map[*serviceWatcher]struct{}

	locationAccess  sync.Mutex
	locationState   sync.Mutex
	location        *simulatedLocation
	locationChanged chan struct{}

	timeline timeline

	databases databaseStore

	appVersions sync.Map
}

type Swiplet struct {
	ID        string
	Directory string
	Manifest  Manifest
}

func New(options Options) *Host {
	logFactory := options.LogFactory
	if logFactory == nil {
		logFactory = log.NewFactory(log.Options{})
	}
	if options.Caches == "" {
		options.Caches = filepath.Join(os.TempDir(), "Swiplets")
	}
	h := &Host{
		options: options, logFactory: logFactory, logger: logFactory.Logger(),
		runners: make(map[string]*runner), watchers: make(map[*serviceWatcher]struct{}),
		locationChanged: make(chan struct{}),
		timeline:        timeline{watches: make(map[*TimelineWatch]struct{})}, databases: newDatabaseStore(),
	}
	h.ctx, h.cancel = context.WithCancelCause(context.Background())
	h.lease = &foregroundLease{host: h}
	if phone := options.Device; phone != nil {
		phone.Trace(func(trace device.Trace) {
			if !h.timeline.active() {
				return
			}
			h.timeline.record(TimelineEvent{
				Time: trace.At.UnixMilli(), Type: "operation", Name: "trace." + trace.Name, Message: trace.Detail,
				DurationMs: float64(trace.Duration.Microseconds()) / 1000,
			})
		})
	}
	return h
}

func (h *Host) Start() {
	h.restoreLocation()
	swiplets, err := h.Swiplets()
	if err != nil {
		h.logger.Error("start the services: ", err)
	}
	for _, swiplet := range swiplets {
		if !swiplet.Manifest.Service {
			continue
		}
		if _, err := os.Stat(filepath.Join(swiplet.Directory, serviceFile)); err != nil {
			continue
		}
		r, err := h.runner(swiplet.ID)
		if err != nil {
			h.logger.Error("start the service of ", swiplet.ID, ": ", err)
			continue
		}
		r.access.Lock()
		r.startLocked()
		r.access.Unlock()
	}
}

func (h *Host) Close() {
	h.access.Lock()
	h.closed = true
	runners := h.runners
	h.runners = make(map[string]*runner)
	h.access.Unlock()
	h.cancel(errClosed)
	for _, r := range runners {
		r.end()
	}
}

func (h *Host) Swiplets() ([]Swiplet, error) {
	entries, err := os.ReadDir(h.options.Directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, E.Cause(err, "read the swiplets")
	}
	var swiplets []Swiplet
	for _, entry := range entries {
		if !entry.IsDir() || CheckID(entry.Name()) != nil {
			continue
		}
		directory := filepath.Join(h.options.Directory, entry.Name())
		manifest, err := LoadManifest(directory)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err == nil && manifest.ID != entry.Name() {
			err = E.New("its manifest names ", strconv.Quote(manifest.ID))
		}
		if err != nil {
			h.logger.Error("load the swiplet ", entry.Name(), ": ", err)
			continue
		}
		swiplets = append(swiplets, Swiplet{ID: manifest.ID, Directory: directory, Manifest: manifest})
	}
	return swiplets, nil
}

func (h *Host) Swiplet(id string) (Swiplet, error) {
	if err := CheckID(id); err != nil {
		return Swiplet{}, err
	}
	directory := filepath.Join(h.options.Directory, id)
	manifest, err := LoadManifest(directory)
	if errors.Is(err, fs.ErrNotExist) || err == nil && manifest.ID != id {
		return Swiplet{}, E.Cause1(ErrNotFound, E.New("there is no swiplet ", strconv.Quote(id)))
	}
	if err != nil {
		return Swiplet{}, err
	}
	return Swiplet{ID: id, Directory: directory, Manifest: manifest}, nil
}

func (h *Host) runner(id string) (*runner, error) {
	if err := CheckID(id); err != nil {
		return nil, err
	}
	h.access.Lock()
	defer h.access.Unlock()
	if r, found := h.runners[id]; found {
		return r, nil
	}
	if h.closed {
		return nil, errClosed
	}
	directory := filepath.Join(h.options.Directory, id)
	if _, err := os.Stat(filepath.Join(directory, ManifestFile)); err != nil || h.removing == id {
		return nil, E.Cause1(ErrNotFound, E.New("there is no swiplet ", strconv.Quote(id)))
	}
	_, err := os.Stat(filepath.Join(directory, serviceFile))
	r := &runner{
		host: h, id: id, logger: h.logFactory.NewLogger("swiplet/" + id), retry: make(chan struct{}, 1),
		state: ServiceState{Enabled: err == nil, Status: ServiceStopped}, changed: make(chan struct{}), logWatchers: make(map[*logWatcher]struct{}),
	}
	r.ctx, r.cancel = context.WithCancelCause(h.ctx)
	h.runners[id] = r
	return r, nil
}

func (h *Host) Install(archive io.ReaderAt, size int64) (Swiplet, error) {
	reader, err := zip.NewReader(archive, size)
	if err != nil {
		return Swiplet{}, E.Cause(err, "open the archive")
	}
	contents, err := ReadArchive(reader)
	if err != nil {
		return Swiplet{}, err
	}
	id := contents.Manifest.ID
	err = h.install(contents)
	if err != nil {
		return Swiplet{}, err
	}
	if h.options.OnSwipletsChanged != nil {
		h.options.OnSwipletsChanged()
	}
	return h.Swiplet(id)
}

func (h *Host) install(contents Archive) error {
	h.installAccess.Lock()
	defer h.installAccess.Unlock()
	if err := os.MkdirAll(h.options.Directory, 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(h.options.Directory, ".install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err = contents.extract(staging); err != nil {
		return err
	}
	directory := filepath.Join(h.options.Directory, contents.Manifest.ID)
	_, err = os.Stat(directory)
	if errors.Is(err, fs.ErrNotExist) {
		if err = os.Mkdir(filepath.Join(staging, DataFolder), 0o755); err != nil {
			return err
		}
		return os.Rename(staging, directory)
	}
	if err != nil {
		return err
	}
	r, err := h.runner(contents.Manifest.ID)
	if err != nil {
		return err
	}
	r.access.Lock()
	r.stopLocked()
	previous := r.done
	r.access.Unlock()
	if previous != nil {
		<-previous
	}
	err = swap(directory, staging)
	marker := filepath.Join(directory, serviceFile)
	_, markerErr := os.Stat(marker)
	r.access.Lock()
	defer r.access.Unlock()
	switch {
	case err != nil:
	case !contents.Manifest.Service:
		if markerErr == nil {
			err = os.Remove(marker)
		}
		if r.state.Enabled {
			r.state.Enabled = false
			r.publishLocked()
		}
	case markerErr == nil:
		r.startLocked()
	}
	return err
}

func swap(directory, staging string) error {
	replaced := filepath.Join(staging, ".replaced")
	if err := os.Mkdir(replaced, 0o755); err != nil {
		return err
	}
	var moved []string
	restore := func(err error) error {
		for _, name := range moved {
			target := filepath.Join(directory, name)
			err = E.Append(err, E.Errors(os.RemoveAll(target), os.Rename(filepath.Join(replaced, name), target)), func(err error) error {
				return E.Cause(err, "restore ", name)
			})
		}
		return err
	}
	for _, name := range packageFiles {
		err := os.Rename(filepath.Join(directory, name), filepath.Join(replaced, name))
		if err == nil {
			moved = append(moved, name)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return restore(err)
		}
	}
	for _, name := range packageFiles {
		err := os.Rename(filepath.Join(staging, name), filepath.Join(directory, name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return restore(err)
		}
	}
	return nil
}

func (h *Host) cacheDirectory(owner string) string {
	return filepath.Join(h.options.Caches, owner)
}

func (h *Host) clearCaches(id string) error {
	directory := h.cacheDirectory(id)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		errs = append(errs, os.RemoveAll(filepath.Join(directory, entry.Name())))
	}
	return E.Errors(errs...)
}

func (h *Host) ClearData(id string) error {
	h.installAccess.Lock()
	defer h.installAccess.Unlock()
	swiplet, err := h.Swiplet(id)
	if err != nil {
		return err
	}
	r, err := h.runner(id)
	if err != nil {
		return err
	}
	r.access.Lock()
	r.stopLocked()
	previous := r.done
	r.access.Unlock()
	if previous != nil {
		<-previous
	}
	h.CloseDatabases(nil, id)
	data := filepath.Join(swiplet.Directory, DataFolder)
	err = E.Errors(os.RemoveAll(data), os.Mkdir(data, 0o755), h.clearCaches(id))
	_, markerErr := os.Stat(filepath.Join(swiplet.Directory, serviceFile))
	r.access.Lock()
	defer r.access.Unlock()
	if swiplet.Manifest.Service && markerErr == nil {
		r.startLocked()
	}
	return err
}

func (h *Host) Remove(id string) error {
	err := h.remove(id)
	if err != nil {
		return err
	}
	if h.options.OnSwipletsChanged != nil {
		h.options.OnSwipletsChanged()
	}
	return nil
}

func (h *Host) remove(id string) error {
	h.installAccess.Lock()
	defer h.installAccess.Unlock()
	swiplet, err := h.Swiplet(id)
	if err != nil {
		return err
	}
	h.access.Lock()
	h.removing = id
	r := h.runners[id]
	delete(h.runners, id)
	h.access.Unlock()
	defer func() {
		h.access.Lock()
		h.removing = ""
		h.access.Unlock()
	}()
	if r != nil {
		r.cancel(errRemoved)
		r.end()
	}
	h.releaseLocation(id)
	h.CloseDatabases(nil, id)
	if err = E.Errors(os.RemoveAll(h.cacheDirectory(id)), os.RemoveAll(swiplet.Directory)); err != nil {
		return err
	}
	if swiplet.Manifest.Service {
		h.publish(id, ServiceState{Status: ServiceStopped, Since: time.Now()})
	}
	return nil
}
