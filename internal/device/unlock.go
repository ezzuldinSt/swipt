package device

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	"github.com/swipt-automation/swipt/internal/coredevice"
	"github.com/swipt-automation/swipt/internal/coredevice/accessibility"
	"github.com/swipt-automation/swipt/internal/coredevice/hid"
)

type PasscodeType int

const (
	PasscodePIN PasscodeType = iota
	PasscodeNumeric
	PasscodeAlphanumeric
)

const maxPasscodeLength = 64

// Without a pause between reads, the phone's own server read the lock state
// about 2,000 times a second while the display woke, and after about 820 reads
// every read took 27 ms instead of under 1 ms (iPad17,1 and iPhone18,2). From
// a Mac, reads on one connection went from 16 to 31 ms when its replies passed
// 256 KiB, where sing-tun's Go stack starts holding back acknowledgements of
// small segments for up to 25 ms (sing-tun 3f8acd9, stack_go_tcp_input.go:884,
// stack_go_tcp_input.go:900): each reply's 4-byte length arrived a round
// trip before its body, and 22 ms before it from then on, while reading
// lockdown over usbmuxd directly stayed at 8 ms for 1,200 reads (iPhone18,2).
// With this pause, iPad17,1 unlocked from a dark display in 0.82-0.84 s
// instead of 1.03 s. All on iOS and iPadOS 27.2 24B115.
const unlockPoll = 10 * time.Millisecond

// From the always-on display, a Mac over Wi-Fi unlocked iPhone18,2 in up to
// 4.2 s (iOS 27.2 24B115).
const unlockLimit = 4500 * time.Millisecond

func validateAutoUnlock(settings Settings) error {
	passcode := settings.AutoUnlockPasscode
	if passcode == "" {
		return nil
	}
	if len(passcode) > maxPasscodeLength {
		return E.New("the device passcode must contain at most 64 characters")
	}
	switch settings.AutoUnlockPasscodeType {
	case PasscodePIN:
		if len(passcode) != 4 && len(passcode) != 6 {
			return E.New("a fixed-length device PIN must contain 4 or 6 digits")
		}
	case PasscodeNumeric, PasscodeAlphanumeric:
	default:
		return E.New("unknown device passcode type")
	}
	for index := range len(passcode) {
		character := passcode[index]
		if settings.AutoUnlockPasscodeType != PasscodeAlphanumeric && (character < '0' || character > '9') {
			return E.New("a numeric device passcode can contain only digits")
		}
		if _, ok := asciiKeystrokes[character]; !ok {
			return E.New("automatic unlocking supports only printable ASCII characters")
		}
	}
	return nil
}

type deviceUnlockTarget struct {
	connection *Connection
	settings   *Settings
	conn       *conn
	input      *hid.Client
	keys       *keyboard
	logger     logger.ContextLogger
}

func (t *deviceUnlockTarget) unlock(parent context.Context) (consumed bool, err error) {
	ctx, cancel := context.WithTimeout(parent, unlockLimit)
	defer cancel()
	defer func() {
		if err != nil && ctx.Err() != nil && parent.Err() == nil {
			err = E.Cause(err, "the device was not unlocked with an active display within ", unlockLimit)
		}
	}()
	locked, err := t.conn.lockState.locked(ctx, t.conn.device)
	if err != nil {
		return false, err
	}
	if !locked {
		if consumed, locked, err = t.wake(ctx); err != nil || !locked {
			return consumed, err
		}
	}
	state, err := t.prepare(ctx)
	if err != nil {
		return true, err
	}
	for {
		count, known := 0, false
		if t.settings.AutoUnlockPasscodeType == PasscodePIN {
			count, known = enteredDigits(state.Value, len(t.settings.AutoUnlockPasscode))
		}
		if known && count == 0 {
			break
		}
		if !known {
			count = maxPasscodeLength
			if t.settings.AutoUnlockPasscodeType == PasscodePIN {
				count = 6
			}
		}
		if locked, err = t.clear(ctx, count); err != nil || !locked {
			return true, err
		}
		if state, err = t.ready(ctx); err != nil {
			return true, err
		}
		if !state.Ready {
			return true, E.New("the lock-screen passcode field lost keyboard focus")
		}
		if !known {
			break
		}
	}
	strokes := common.Map([]byte(t.settings.AutoUnlockPasscode), func(character byte) asciiKeystroke {
		return asciiKeystrokes[character]
	})
	if t.settings.AutoUnlockPasscodeType != PasscodePIN {
		strokes = append(strokes, asciiKeystroke{usage: usageReturn})
	}
	for _, stroke := range strokes {
		if locked, err = t.conn.lockState.locked(ctx, t.conn.device); err != nil || !locked {
			return true, err
		}
		if err = t.key(ctx, stroke); err != nil {
			return true, err
		}
	}
	var polled time.Time
	for {
		if err = sleep(ctx, unlockPoll-time.Since(polled)); err != nil {
			return true, err
		}
		polled = time.Now()
		if locked, err = t.conn.lockState.locked(ctx, t.conn.device); err != nil || !locked {
			return true, err
		}
	}
}

// The fixed-digit PIN field's value was "共6位数，已输入0位" when empty and
// "共6位数，已输入5位" after five digits (iPhone18,2 and iPad17,1, iOS and
// iPadOS 27.2 24B115, Simplified Chinese).
func enteredDigits(value string, length int) (int, bool) {
	var numbers []int
	for _, field := range strings.FieldsFunc(value, func(r rune) bool { return r < '0' || r > '9' }) {
		number, err := strconv.Atoi(field)
		if err != nil {
			return 0, false
		}
		numbers = append(numbers, number)
	}
	if len(numbers) != 2 {
		return 0, false
	}
	switch length {
	case numbers[0]:
		return numbers[1], numbers[1] < length
	case numbers[1]:
		return numbers[0], numbers[0] < length
	}
	return 0, false
}

// SpringBoard posts com.apple.springboard.lockstate when the device locks or
// unlocks (iPhone18,2, iOS 27.2 24B115), so a lock state read while the
// notifications are relayed stays current until one arrives. Without them, it
// is kept for a second.
const unrelayedLockStateLife = time.Second

type lockStateReader struct {
	access sync.Mutex
	conn   *coredevice.LockStateConn

	owner    *conn
	value    bool
	readAt   time.Time
	readRun  uint64
	watching bool
	relaying func() (uint64, bool)
	unwatch  func()
}

func (r *lockStateReader) recent(ctx context.Context) (bool, error) {
	r.access.Lock()
	if !r.watching && r.owner != nil {
		changed, relaying, cancel := r.owner.darwin.watch()
		r.watching, r.relaying, r.unwatch = true, relaying, cancel
		go func() {
			for {
				select {
				case <-changed:
					r.forget()
					_, _ = r.recent(r.owner.ctx)
				case <-r.owner.ctx.Done():
					return
				}
			}
		}()
	}
	if r.currentLocked(time.Now()) {
		value := r.value
		r.access.Unlock()
		return value, nil
	}
	r.access.Unlock()
	return r.locked(ctx, r.owner.device)
}

func (r *lockStateReader) currentLocked(now time.Time) bool {
	if r.readAt.IsZero() {
		return false
	}
	if r.relaying != nil {
		if run, relaying := r.relaying(); relaying && run == r.readRun {
			return true
		}
	}
	return now.Sub(r.readAt) < unrelayedLockStateLife
}

func (r *lockStateReader) run() uint64 {
	if r.relaying == nil {
		return 0
	}
	run, _ := r.relaying()
	return run
}

func (r *lockStateReader) forget() {
	r.access.Lock()
	r.readAt = time.Time{}
	r.access.Unlock()
}

func (r *lockStateReader) locked(ctx context.Context, device *coredevice.Device) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.access.Lock()
	defer r.access.Unlock()
	run := r.run()
	var state coredevice.LockState
	err := r.useLocked(ctx, device, func(conn *coredevice.LockStateConn) (err error) {
		state, err = conn.LockState(ctx)
		return
	})
	if err != nil {
		return false, err
	}
	r.value, r.readAt, r.readRun = state.PasscodeRequired, time.Now(), run
	return state.PasscodeRequired, nil
}

func (r *lockStateReader) language(ctx context.Context, device *coredevice.Device) (language string, err error) {
	if err = ctx.Err(); err != nil {
		return "", err
	}
	r.access.Lock()
	defer r.access.Unlock()
	err = r.useLocked(ctx, device, func(conn *coredevice.LockStateConn) (err error) {
		language, err = conn.Language(ctx)
		return
	})
	return language, err
}

// useLocked runs read on the kept lockdown connection, connecting again when
// one kept from an earlier read fails.
func (r *lockStateReader) useLocked(ctx context.Context, device *coredevice.Device, read func(conn *coredevice.LockStateConn) error) error {
	for {
		reused := r.conn != nil
		if !reused {
			conn, err := device.DialLockState(ctx)
			if err != nil {
				return err
			}
			r.conn = conn
		}
		err := read(r.conn)
		if err == nil {
			return nil
		}
		r.conn.Close()
		r.conn = nil
		if !reused || ctx.Err() != nil {
			return err
		}
	}
}

func (r *lockStateReader) close() error {
	r.access.Lock()
	defer r.access.Unlock()
	if r.unwatch != nil {
		r.unwatch()
		r.unwatch, r.watching, r.relaying = nil, false, nil
	}
	if r.conn == nil {
		return nil
	}
	err := r.conn.Close()
	r.conn = nil
	return err
}

func (t *deviceUnlockTarget) prepare(ctx context.Context) (accessibility.PasscodeState, error) {
	started := time.Now()
	if err := t.input.ReleaseAll(ctx); err != nil {
		return accessibility.PasscodeState{}, err
	}
	if err := t.keys.capsLockKey(ctx, t.input, false); err != nil {
		return accessibility.PasscodeState{}, err
	}
	deviceClass := t.conn.device.Info().DeviceClass
	// From a dark display, the phone's own server unlocked by typing in
	// 0.89-0.98 s on iPhone18,2 and 1.03 s on iPad17,1, and by pressing Home
	// twice and tapping the keypad in 1.43-1.60 s and 1.38-1.42 s; from the
	// always-on display, a Mac over Wi-Fi took 2.0-2.35 s and 3.7-4.2 s on
	// iPhone18,2. After Home woke the display, a second Home sent 0.5 s after
	// the first brought the keypad 0.6-0.76 s later, while after a backspace it
	// was there within 0.1 s. Opening the page with a backspace and tapping the
	// digits took 0.90-0.96 s and 1.08-1.09 s. All on iOS and iPadOS 27.2
	// 24B115.
	transportContext, cancelTransports := context.WithCancel(ctx)
	touchesPrepared, keyboardPrepared, accessibilityDialed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var touchesErr, keyboardErr, accessibilityErr error
	go func() {
		defer close(touchesPrepared)
		touchesErr = t.input.PrepareTouches(transportContext)
	}()
	go func() {
		defer close(keyboardPrepared)
		keyboardErr = t.input.PrepareKeyboard(transportContext)
	}()
	go func() {
		defer close(accessibilityDialed)
		accessibilityErr = t.conn.ax.do(ctx, false, 0, func(context.Context, *accessibility.RemoteClient) error { return nil })
	}()
	defer func() {
		cancelTransports()
		<-touchesPrepared
		<-keyboardPrepared
		<-accessibilityDialed
	}()
	join := func(done <-chan struct{}, err *error) error {
		select {
		case <-done:
			return *err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	wakeSent, openSent, shortcutTried := false, false, false
	var polled time.Time
	for first := true; ; first = false {
		if err := sleep(ctx, unlockPoll-time.Since(polled)); err != nil {
			return accessibility.PasscodeState{}, err
		}
		polled = time.Now()
		if !first {
			if locked, err := t.conn.lockState.locked(ctx, t.conn.device); err != nil || !locked {
				return accessibility.PasscodeState{}, err
			}
		}
		active, err := t.activeDisplay(ctx)
		if err != nil {
			return accessibility.PasscodeState{}, err
		}
		if !active {
			openSent, shortcutTried = false, false
			if wakeSent {
				continue
			}
			// Creating the keyboard took 0.38 s, so Home wakes the display
			// meanwhile: it was active 0.48 s after Home, while creating the
			// keyboard first and pressing Home then took 0.38 + 0.28 s (iPad17,1,
			// iPadOS 27.2 24B115, from the iPad's own server).
			var wake func() error
			select {
			case <-keyboardPrepared:
				if keyboardErr != nil {
					return accessibility.PasscodeState{}, keyboardErr
				}
				wake = func() error { return t.key(ctx, asciiKeystroke{usage: usageBackspace}) }
			default:
				if err = join(touchesPrepared, &touchesErr); err != nil {
					return accessibility.PasscodeState{}, err
				}
				home := hid.ButtonHome
				home.Hold = 0
				wake = func() error { return t.input.PressButton(ctx, home) }
			}
			if locked, err := t.conn.lockState.locked(ctx, t.conn.device); err != nil || !locked {
				return accessibility.PasscodeState{}, err
			}
			if err = wake(); err != nil {
				return accessibility.PasscodeState{}, err
			}
			wakeSent = true
			t.logger.Debug("unlock wake after ", time.Since(started))
			continue
		}
		wakeSent = false
		if !openSent {
			if err = join(keyboardPrepared, &keyboardErr); err != nil {
				return accessibility.PasscodeState{}, err
			}
			if locked, err := t.conn.lockState.locked(ctx, t.conn.device); err != nil || !locked {
				return accessibility.PasscodeState{}, err
			}
			if err = t.key(ctx, asciiKeystroke{usage: usageBackspace}); err != nil {
				return accessibility.PasscodeState{}, err
			}
			openSent = true
			t.logger.Debug("unlock first backspace after ", time.Since(started))
			continue
		}
		if err = join(accessibilityDialed, &accessibilityErr); err != nil {
			return accessibility.PasscodeState{}, E.Cause(err, "read the lock-screen keyboard focus")
		}
		state, layout, err := t.preparation(ctx, !shortcutTried)
		if err != nil {
			return accessibility.PasscodeState{}, err
		}
		if state.Ready {
			t.logger.Debug("lock-screen keyboard ready after ", time.Since(started))
			return state, nil
		}
		if layout == nil {
			continue
		}
		data, _ := json.Marshal(layout)
		t.logger.Debug("unlock layout probe after ", time.Since(started), ": ", string(data))
		point, ok := accessibility.NumericPasscodeShortcutPoint(*layout, deviceClass)
		if !ok {
			continue
		}
		if err = join(touchesPrepared, &touchesErr); err != nil {
			return accessibility.PasscodeState{}, err
		}
		if locked, err := t.conn.lockState.locked(ctx, t.conn.device); err != nil || !locked {
			return accessibility.PasscodeState{}, err
		}
		err = t.input.Tap(ctx, hid.Touch{X: point.X, Y: point.Y})
		shortcutTried = true
		t.logger.Debug("unlock shortcut tap after ", time.Since(started), ", error: ", err)
		if err != nil {
			return accessibility.PasscodeState{}, err
		}
	}
}

func (t *deviceUnlockTarget) clear(ctx context.Context, length int) (bool, error) {
	for range length {
		locked, err := t.conn.lockState.locked(ctx, t.conn.device)
		if err != nil || !locked {
			return false, err
		}
		if err = t.key(ctx, asciiKeystroke{usage: usageBackspace}); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (t *deviceUnlockTarget) ready(ctx context.Context) (accessibility.PasscodeState, error) {
	active, err := t.activeDisplay(ctx)
	if err != nil || !active {
		return accessibility.PasscodeState{}, err
	}
	state, _, err := t.preparation(ctx, false)
	return state, err
}

func (t *deviceUnlockTarget) preparation(ctx context.Context, readLayout bool) (accessibility.PasscodeState, *accessibility.PasscodeLayout, error) {
	var state accessibility.PasscodeState
	var layout *accessibility.PasscodeLayout
	err := t.conn.ax.do(ctx, false, 0, func(ctx context.Context, client *accessibility.RemoteClient) (err error) {
		if readLayout {
			state, layout, err = client.ReadPasscodePreparation(ctx)
		} else {
			state, err = client.ReadPasscodeState(ctx)
		}
		return
	})
	data, _ := json.Marshal(state)
	t.logger.Debug("unlock readiness probe: ", string(data), ", error: ", err)
	return state, layout, err
}

func (t *deviceUnlockTarget) wake(ctx context.Context) (consumed, locked bool, err error) {
	state, err := t.conn.backlight(ctx)
	if err != nil || state == "activeOn" || state == "dimmed" {
		return false, false, err
	}
	home := hid.ButtonHome
	home.Hold = 0
	if err = t.input.PressButton(ctx, home); err != nil {
		return true, false, err
	}
	var polled time.Time
	for {
		if err = sleep(ctx, unlockPoll-time.Since(polled)); err != nil {
			return true, false, err
		}
		polled = time.Now()
		if locked, err = t.conn.lockState.locked(ctx, t.conn.device); err != nil || locked {
			return true, locked, err
		}
		active, err := t.activeDisplay(ctx)
		if err != nil || active {
			return true, false, err
		}
	}
}

func (t *deviceUnlockTarget) activeDisplay(ctx context.Context) (bool, error) {
	state, err := t.conn.backlight(ctx)
	return state == "activeOn", err
}

func (t *deviceUnlockTarget) key(ctx context.Context, stroke asciiKeystroke) error {
	input := t.input
	if stroke.shift {
		if err := input.KeyDown(ctx, usageLeftShift); err != nil {
			return err
		}
	}
	if err := input.KeyDown(ctx, stroke.usage); err != nil {
		return err
	}
	// An up right after each down, after a lock-state read, entered every
	// digit: the field read five entered before the sixth key (iPhone18,2 and
	// iPad17,1, iOS and iPadOS 27.2 24B115).
	if err := input.KeyUp(ctx, stroke.usage); err != nil {
		return err
	}
	if stroke.shift {
		if err := input.KeyUp(ctx, usageLeftShift); err != nil {
			return err
		}
	}
	return nil
}

func (q *hidQueue) checkUnlock(c *conn) bool {
	settings := q.connection.settings.Load()
	if len(q.connection.sessions()) == 0 || settings.AutoUnlockPasscode == "" {
		q.unlockPending = false
		return false
	}
	started := time.Now()
	target := &deviceUnlockTarget{connection: q.connection, settings: settings, conn: c, input: q.client, keys: q.keys, logger: q.logger}
	consumed, err := target.unlock(c.ctx)
	if consumed || err != nil {
		released := c.ctx.Err() == nil
		if released {
			if releaseErr := q.client.ReleaseAll(c.ctx); releaseErr != nil {
				q.logger.Warn(E.Cause(releaseErr, "release input after automatic unlocking"))
				released = false
			}
		}
		if err != nil || !released {
			q.closeClient()
		} else {
			q.resetState()
		}
		q.access.Lock()
		q.queue = slices.DeleteFunc(q.queue, func(item hidItem) bool { return item.event != nil })
		q.access.Unlock()
	}
	if consumed {
		q.logger.Info("automatic unlock finished in ", time.Since(started), ", error: ", err)
	}
	if err != nil && c.ctx.Err() != nil {
		return true
	}
	q.unlockPending = false
	if err != nil {
		q.logger.Warn(E.Cause(err, "automatic unlocking"))
		return true
	}
	return consumed
}
