package automation

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/device"
)

func (e *engine) watch(ctx context.Context, kind string, args []json.RawMessage, publish func(any, error)) error {
	call := &operation{engine: e, name: kind, args: args}
	ended := func(err error) {
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, context.Canceled) {
			err = call.droppedError(err, "while watching "+kind)
		}
		publish(nil, err)
	}
	switch kind {
	case "device.state":
		var name string
		if err := call.decode(1, &name); err != nil {
			return err
		}
		phone, err := call.phone()
		if err != nil {
			return err
		}
		switch name {
		case "foreground":
			go func() {
				ended(phone.WatchForeground(ctx, func(app string) { publish(app, nil) }))
			}()
		case "locked", "display":
			go func() {
				ended(phone.WatchState(ctx, name, func(value any) { publish(value, nil) }))
			}()
		case "simulatedLocation":
			go e.host.watchLocation(ctx, func(location any) { publish(location, nil) })
		default:
			return E.Cause1(ErrInvalid, E.New("unknown state ", strconv.Quote(name)))
		}
		return nil
	case "screen.watch":
		return e.watchGraphics(ctx, call, publish, ended)
	case "sqlite.changes":
		if err := call.decode(0); err != nil {
			return err
		}
		go e.host.watchDatabaseChanges(ctx, e.dataOwner(), publish)
		return nil
	case "device.applicationEvents":
		if err := call.decode(0); err != nil {
			return err
		}
		phone, err := call.phone()
		if err != nil {
			return err
		}
		go func() {
			ended(phone.WatchApps(ctx, func(event device.AppEvent) {
				publish(map[string]any{"type": event.Kind, "bundleId": event.BundleID, "pid": event.PID, "time": event.Time.UnixMilli()}, nil)
			}))
		}()
		return nil
	}
	return E.Cause1(ErrInvalid, E.New("unknown subscription ", strconv.Quote(kind)))
}

const (
	locationFile       = ".simulated-location"
	locationClearLimit = 10 * time.Second
)

type simulatedLocation struct {
	Owner     string  `json:"owner"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

func (h *Host) restoreLocation() {
	phone := h.options.Device
	if phone == nil {
		return
	}
	data, err := os.ReadFile(filepath.Join(h.options.Directory, locationFile))
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	var location simulatedLocation
	if err == nil {
		err = json.Unmarshal(data, &location)
	}
	if err != nil {
		h.logger.Error(E.Cause(err, "read the simulated location"))
		return
	}
	h.publishLocation(&location)
	phone.RestoreLocation(location.Latitude, location.Longitude)
}

func (h *Host) storeLocation(location *simulatedLocation) error {
	path := filepath.Join(h.options.Directory, locationFile)
	if location == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	} else {
		data, err := json.Marshal(location)
		if err != nil {
			return err
		}
		if err = os.MkdirAll(h.options.Directory, 0o755); err != nil {
			return err
		}
		if err = writeAtomically(path, data); err != nil {
			return err
		}
	}
	h.publishLocation(location)
	return nil
}

func (h *Host) publishLocation(location *simulatedLocation) {
	h.locationState.Lock()
	defer h.locationState.Unlock()
	h.location = location
	close(h.locationChanged)
	h.locationChanged = make(chan struct{})
}

func (h *Host) currentLocation() (any, <-chan struct{}) {
	h.locationState.Lock()
	defer h.locationState.Unlock()
	if h.location == nil {
		return nil, h.locationChanged
	}
	return map[string]any{"latitude": h.location.Latitude, "longitude": h.location.Longitude}, h.locationChanged
}

func (h *Host) watchLocation(ctx context.Context, publish func(any)) {
	for {
		location, changed := h.currentLocation()
		publish(location)
		select {
		case <-changed:
		case <-ctx.Done():
			return
		}
	}
}

func (h *Host) setLocation(ctx context.Context, phone *device.Phone, owner string, latitude, longitude float64) error {
	h.locationAccess.Lock()
	defer h.locationAccess.Unlock()
	if err := phone.SetLocation(ctx, latitude, longitude); err != nil {
		return err
	}
	return h.storeLocation(&simulatedLocation{Owner: owner, Latitude: latitude, Longitude: longitude})
}

func (h *Host) clearLocation(ctx context.Context, phone *device.Phone) error {
	h.locationAccess.Lock()
	defer h.locationAccess.Unlock()
	if err := h.storeLocation(nil); err != nil {
		return err
	}
	return phone.ClearLocation(ctx)
}

func (h *Host) releaseLocation(id string) {
	h.locationAccess.Lock()
	defer h.locationAccess.Unlock()
	h.locationState.Lock()
	owned := h.location != nil && h.location.Owner == id
	h.locationState.Unlock()
	if !owned {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), locationClearLimit)
	defer cancel()
	err := h.storeLocation(nil)
	if err == nil {
		err = h.options.Device.ClearLocation(ctx)
	}
	if err != nil {
		h.logger.Error(E.Cause(err, "clear the simulated location"))
	}
}
