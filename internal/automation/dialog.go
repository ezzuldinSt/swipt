package automation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sync"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
)

const (
	springBoard   = "com.apple.springboard"
	returnTimeout = 30 * time.Second
	returnGrace   = 500 * time.Millisecond
)

var ErrDismissed = E.New("the person dismissed the dialog")

type Dialog struct {
	App      string
	InApp    bool
	Ask      func(ctx context.Context, request string) (string, error)
	Backdrop func(image []byte)
	Active   func() bool
}

func presentDialog(ctx context.Context, call *operation) (any, error) {
	var request json.RawMessage
	var inApp bool
	if err := call.decode(2, &request, &inApp); err != nil {
		return nil, err
	}
	dialog := call.engine.dialog
	if dialog.Ask == nil {
		return nil, E.New("this server runs on a computer, so it can't show dialogs on the device")
	}
	var kind struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(request, &kind)
	if dialog.App != "" && (dialog.InApp || inApp) {
		if err := call.engine.host.lease.enter(ctx, call, dialog); err != nil {
			return nil, err
		}
		defer call.engine.host.lease.leave()
	}
	answer, err := dialog.Ask(ctx, string(request))
	if err != nil {
		return nil, err
	}
	if kind.Type != "file" {
		return json.RawMessage(answer), nil
	}
	var file struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Data string `json:"data"`
	}
	if err = json.Unmarshal([]byte(answer), &file); err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(file.Data)
	if err != nil {
		return nil, E.Cause(err, "decode the file")
	}
	return map[string]string{"name": file.Name, "type": file.Type, "data": call.engine.putBlob(data)}, nil
}

type foregroundLease struct {
	host     *Host
	access   sync.Mutex
	open     int
	app      string
	returnTo string
	pending  *time.Timer
	idle     chan struct{}
}

func (l *foregroundLease) enter(ctx context.Context, call *operation, dialog Dialog) error {
	l.access.Lock()
	defer l.access.Unlock()
	if l.pending != nil {
		l.pending.Stop()
		l.pending = nil
	}
	if l.idle == nil {
		l.idle = make(chan struct{})
	}
	l.open++
	if l.open > 1 || l.returnTo != "" || dialog.Active != nil && dialog.Active() {
		return nil
	}
	err := l.bringForward(ctx, call, dialog)
	if err != nil {
		l.release()
	}
	return err
}

func (l *foregroundLease) bringForward(ctx context.Context, call *operation, dialog Dialog) error {
	phone, err := call.phone()
	if err != nil {
		return err
	}
	front, err := phone.Frontmost(ctx)
	if err != nil {
		return E.Cause(err, "find the app in front")
	}
	if front == dialog.App {
		return nil
	}
	if dialog.Backdrop != nil {
		image, _, err := phone.Capture(ctx)
		if err != nil {
			return E.Cause(err, "capture the screen")
		}
		dialog.Backdrop(image)
		// The app shows the snapshot it was suspended with for a frame when it
		// comes back unless the new one, of the backdrop, is in by then
		// (iOS 27.2 24B115).
		select {
		case <-time.After(400 * time.Millisecond):
		case <-ctx.Done():
			dialog.Backdrop(nil)
			return ctx.Err()
		}
	}
	if err = phone.Launch(ctx, dialog.App, "", false, false); err != nil {
		if dialog.Backdrop != nil {
			dialog.Backdrop(nil)
		}
		return E.Cause(err, "bring the app to the front")
	}
	l.app, l.returnTo = dialog.App, front
	return nil
}

func (l *foregroundLease) leave() {
	l.access.Lock()
	defer l.access.Unlock()
	l.release()
}

func (l *foregroundLease) release() {
	l.open--
	if l.open > 0 {
		return
	}
	if l.returnTo == "" {
		l.finish()
		return
	}
	l.pending = time.AfterFunc(returnGrace, l.giveWay)
}

func (l *foregroundLease) finish() {
	if l.idle != nil {
		close(l.idle)
		l.idle = nil
	}
}

func (l *foregroundLease) giveWay() {
	l.access.Lock()
	defer l.access.Unlock()
	if l.open > 0 || l.returnTo == "" {
		return
	}
	app, returnTo := l.app, l.returnTo
	l.pending, l.returnTo = nil, ""
	defer l.finish()
	phone := l.host.options.Device
	if phone == nil {
		return
	}
	ctx, cancel := context.WithTimeout(l.host.ctx, returnTimeout)
	defer cancel()
	front, err := phone.Frontmost(ctx)
	if err != nil || front != app {
		return
	}
	if returnTo == springBoard {
		err = phone.Button(ctx, "home")
	} else {
		err = phone.Launch(ctx, returnTo, "", false, false)
	}
	if err != nil {
		l.host.logger.Warn(E.Cause(err, "return to ", returnTo))
	}
}

func (l *foregroundLease) wait(ctx context.Context) error {
	l.access.Lock()
	idle := l.idle
	l.access.Unlock()
	if idle == nil {
		return nil
	}
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return E.Cause(ctx.Err(), "wait for the dialog on the screen")
	}
}
