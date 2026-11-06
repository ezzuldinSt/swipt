package device

import (
	"context"
	"errors"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/coredevice/accessibility"
	"github.com/swipt-automation/swipt/internal/coredevice/apps"
	"github.com/swipt-automation/swipt/internal/coredevice/hid"
)

// A test app came to the front 122 to 138 ms after a warm launch request and
// 662 to 1,042 ms after a cold one, and Clock 1.71 s after a cold one
// (iPhone18,2, iOS 27.2 24B115).
const launchLimit = 3500 * time.Millisecond

// A text field takes the focus as the tap's touch-up is handled, and the
// device reports the change within 150 ms (iPad17,1, iPadOS 27.2 24B115).
const focusLimit = 500 * time.Millisecond

func (p *Phone) readApplication(ctx context.Context, c *conn, client *accessibility.RemoteClient, geometry phoneGeometry, options accessibility.ApplicationOptions, base *PhoneNode) (*PhoneModel, error) {
	started := time.Now()
	screen, err := client.ReadApplication(ctx, options)
	if err != nil {
		if options.PID != 0 && errors.Is(err, accessibility.ErrNoAnswer) {
			c.springboard.CompareAndSwap(options.PID, 0)
		}
		return nil, err
	}
	read := SnapshotOptions{Depth: options.Depth}
	if base != nil {
		read.Within = refText(base.AX.Ref)
	}
	return p.model(c, geometry, screen, started, base, read, options.PID == 0 && base == nil), nil
}

type axSession struct {
	phone    *Phone
	conn     *conn
	client   *accessibility.RemoteClient
	geometry phoneGeometry
	grant    *hidGrant
	acted    bool
	previous *accessibility.LayoutFrame

	target    Aim
	settled   time.Time
	animating bool
	moved     bool
	stale     bool
	reread    SnapshotOptions
	fixed     bool
	blocker   *blocker
	retry     *Point
}

func (s *axSession) tap(ctx context.Context, at Point, count int, hold time.Duration) error {
	s.acted = true
	return s.grant.send(func(input *hid.Client) error {
		return tap(ctx, s.geometry, input, at, count, hold)
	})
}

func pointText(at Point) string {
	return "[" + strconv.FormatFloat(math.Round(at.X*10)/10, 'f', -1, 64) + ", " + strconv.FormatFloat(math.Round(at.Y*10)/10, 'f', -1, 64) + "]"
}

func describeElement(element accessibility.Element) string {
	switch {
	case element.Label != "":
		return "the element " + strconv.Quote(PlainText(element.Label))
	case element.Identifier != "":
		return "the element with the identifier " + strconv.Quote(element.Identifier)
	case element.Value != "":
		return "the element valued " + strconv.Quote(PlainText(element.Value))
	case element.Class != "":
		return "an unlabeled " + element.Class
	}
	return "an unlabeled element"
}

var phoneActions = map[string]uint64{
	"press": accessibility.ActionPress, "increment": accessibility.ActionIncrement, "decrement": accessibility.ActionDecrement,
	"escape": accessibility.ActionEscape, "menu": accessibility.ActionShowContextMenu,
	"scrollUp": accessibility.ActionScrollUpByPage, "scrollDown": accessibility.ActionScrollDownByPage,
	"scrollLeft": accessibility.ActionScrollLeftByPage, "scrollRight": accessibility.ActionScrollRightByPage,
	"scrollToVisible": accessibility.ActionScrollToVisible,
}

func (s *axSession) perform(ctx context.Context, target *PhoneNode, name string) error {
	number, standard := phoneActions[name]
	actions := target.AX.Actions
	if !standard && actions == nil {
		read := &accessibility.Batch{}
		attributes := read.Attributes(target.AX)
		if err := s.client.Run(ctx, read); err != nil {
			return err
		}
		if err := attributes.Err("read the element's actions"); err != nil {
			return err
		}
		actions = attributes.Value.Actions
	}
	custom := common.Find(actions, func(candidate accessibility.CustomAction) bool { return candidate.Name == name })
	if !standard && custom.Identifier == "" {
		names := append(slices.Sorted(maps.Keys(phoneActions)), common.Map(actions, func(it accessibility.CustomAction) string { return it.Name })...)
		return E.Extend(ErrInvalid, describeElement(target.AX), " has no action ", strconv.Quote(name), "; its actions are ", strings.Join(names, ", "))
	}
	batch := &accessibility.Batch{}
	var perform *accessibility.Result[bool]
	if standard {
		perform = batch.Perform(target.AX, number)
	} else {
		perform = batch.PerformCustom(target.AX, custom)
	}
	s.acted = true
	if err := s.client.Run(ctx, batch); err != nil {
		return err
	}
	if err := perform.Err("perform " + strconv.Quote(name)); err != nil {
		return err
	}
	if !perform.Value {
		return E.New(describeElement(target.AX), " refused to perform ", strconv.Quote(name))
	}
	return nil
}

// A backspace from the keyboard changed Claude's chat input 177 to 518 ms after
// it was sent (iPhone18,2, iOS 27.2 24B115).
const keyLimit = 2 * time.Second

func (s *axSession) setText(ctx context.Context, target *PhoneNode, at Point, text string) error {
	if !target.AX.TextInput() {
		return E.New(describeElement(target.AX), " is not a text entry")
	}
	entry, err := s.focus(ctx, target, at)
	if err != nil {
		return err
	}
	// UIKit sets a text view's value with setText: and keyboardInputChanged:
	// alone, which Claude's chat input did not take as an edit, while a text
	// field's setter sends fieldEditorDidChange: and a focused web entry
	// inserts the text through its editor (24B5110a UIKit.axbundle
	// 0x18a052c34, 0x18a047f50; 24B5110a UIKitCore 0x185cffcc4; WebKit
	// f98a24d3, Source/WebCore/accessibility/AccessibilityRenderObject.cpp:1886;
	// iPhone18,2, iOS 27.2 24B115). Deleting a trailing space with the keyboard
	// made it one.
	nudge := entry.AX.TextView() && !entry.Web
	value := text
	if nudge {
		value += " "
	}
	batch := &accessibility.Batch{}
	set := batch.SetValue(entry.AX, value)
	s.acted = true
	if err = s.client.Run(ctx, batch); err != nil {
		return err
	}
	if err = set.Err("set the text"); err != nil {
		return err
	}
	if !nudge {
		return s.checkText(ctx, entry, text)
	}
	err = s.grant.send(func(input *hid.Client) error {
		return pressKeys(ctx, input, []uint8{usageBackspace})
	})
	if err != nil {
		return err
	}
	var last error
	matched, err := s.conn.wait(ctx, time.Now().Add(keyLimit), func(ctx context.Context) (bool, error) {
		last = s.checkText(ctx, entry, text)
		return last == nil, nil
	})
	if err == nil && !matched {
		err = last
	}
	return err
}

// Tapping a text field can bring another entry in its place, as Safari's
// address bar gives way to its editing field (iPhone18,2, iOS 27.2 24B115).
func (s *axSession) focus(ctx context.Context, target *PhoneNode, at Point) (*PhoneNode, error) {
	app := target.model.app
	before, err := s.focused(ctx, app)
	if err != nil {
		return nil, err
	}
	if before != nil && *before == target.AX.Ref {
		return target, nil
	}
	if err = s.tap(ctx, at, 1, 0); err != nil {
		return nil, err
	}
	var focused *accessibility.ElementRef
	changed, err := s.conn.wait(ctx, time.Now().Add(focusLimit), func(ctx context.Context) (bool, error) {
		var err error
		focused, err = s.focused(ctx, app)
		return focused != nil && (before == nil || *focused != *before), err
	})
	if err != nil {
		return nil, err
	}
	if !changed {
		if before != nil {
			held := "another element"
			if node := target.model.Node(*before); node != nil {
				held = describeElement(node.AX)
			}
			return nil, E.New("tapping ", describeElement(target.AX), " left the keyboard focus on ", held)
		}
		return nil, E.New("tapping ", describeElement(target.AX), " did not focus a text entry")
	}
	if *focused == target.AX.Ref {
		return target, nil
	}
	options := accessibility.ApplicationOptions{
		PID: app.Ref.PID, Depth: target.model.read.Depth, Attributes: accessibility.SnapshotAttributes([]string{"label", "identifier", "value"}),
	}
	var base *PhoneNode
	if within := target.model.read.Within; within != "" {
		if base, err = s.phone.element(s.conn, within); err != nil {
			return nil, err
		}
		options.Root, options.Clip = &base.AX, &base.axClip
	}
	model, err := s.phone.readApplication(ctx, s.conn, s.client, s.geometry, options, base)
	if err != nil {
		return nil, err
	}
	node := model.Node(*focused)
	if node == nil {
		return nil, E.New("tapping ", describeElement(target.AX), " focused an element the screen does not list")
	}
	return node, nil
}

func (s *axSession) focused(ctx context.Context, app accessibility.Element) (*accessibility.ElementRef, error) {
	batch := &accessibility.Batch{}
	focused := batch.Focused(app)
	if err := s.client.Run(ctx, batch); err != nil {
		return nil, err
	}
	if focused.Code != 0 {
		return nil, nil
	}
	ref := focused.Value.Ref
	return &ref, nil
}

func (s *axSession) checkText(ctx context.Context, target *PhoneNode, text string) error {
	batch := &accessibility.Batch{}
	attributes := batch.Attributes(target.AX)
	if err := s.client.Run(ctx, batch); err != nil {
		return err
	}
	if err := attributes.Err("read the element"); err != nil {
		return err
	}
	element := attributes.Value
	if err := element.TextErr(); err != nil {
		return err
	}
	value, placeholder, expected := PlainText(element.Value), PlainText(element.Placeholder), PlainText(text)
	switch {
	// UIKit and WebKit answer a secure entry's value as one • per UTF-16 code
	// unit of its text (24B5110a UIKit.axbundle -[UIView
	// _accessibilitySecureTextForString:] 0x18a046274; WebKit f98a24d3,
	// Source/WebCore/accessibility/ios/WebAccessibilityObjectWrapperIOS.mm:1690,
	// Source/WebCore/accessibility/ios/AccessibilityObjectIOS.mm:107).
	case target.AX.Secure():
		units := len(utf16.Encode([]rune(text)))
		if placeholder != "" && value == placeholder {
			if expected == "" {
				return nil
			}
			return E.New(describeElement(target.AX), " shows its placeholder instead of ", units, " UTF-16 code units")
		}
		if masked := len(utf16.Encode([]rune(value))); masked != units {
			return E.New(describeElement(target.AX), " holds ", masked, " UTF-16 code units after setting ", units)
		}
		return nil
	case placeholder == "" || value != placeholder:
		if value == expected {
			return nil
		}
		return E.New(describeElement(target.AX), " holds ", strconv.Quote(value), " instead of ", strconv.Quote(text))
	case expected != "" && expected != placeholder:
		return E.New(describeElement(target.AX), " shows its placeholder instead of ", strconv.Quote(text))
	}
	empty, err := s.empty(ctx, target)
	if err != nil {
		return err
	}
	if empty != (expected == "") {
		return E.New(describeElement(target.AX), " holds ", strconv.Quote(value), " instead of ", strconv.Quote(text))
	}
	return nil
}

const nativeHandwriting = 2601

// UIKit answers native attribute 2601 with handwriting attributes only for an
// element _accessibilitySupportsHandwriting accepts, and their
// AXVoiceOverHandwritingAttributeCanDeleteCharacter is hasText for a
// UIKeyInput and YES for anything else (24B5110a UIAccessibility 0x1bbde7d10,
// 0x1bbdf8e08, 0x1bbdf8e90, 0x1bbdf9248); WebCore reports its placeholder as
// the value of an empty input (24A446 WebCore.axbundle 0x242ab8a54).
func (s *axSession) empty(ctx context.Context, target *PhoneNode) (bool, error) {
	if target.Web {
		return false, E.New(describeElement(target.AX), " shows its placeholder, and there is no way to tell an empty web input from one holding that text")
	}
	batch := &accessibility.Batch{}
	native := batch.Native(target.AX, nativeHandwriting)
	if err := s.client.Run(ctx, batch); err != nil {
		return false, err
	}
	if err := native.Err("read the handwriting attributes"); err != nil {
		return false, err
	}
	fields, _ := native.Value.(map[string]any)
	deletable, known := fields["AXVoiceOverHandwritingAttributeCanDeleteCharacter"].(bool)
	if !known {
		return false, E.New(describeElement(target.AX), " shows its placeholder, and answers no handwriting attributes that would tell whether it is empty or holds that text")
	}
	return !deletable, nil
}

// An action whose input touches the screen gets its geometry; a key or button
// press goes without.
type action struct {
	name     string
	unlocked bool
	touches  bool
	input    func(ctx context.Context, c *conn, geometry phoneGeometry) error
	check    func(ctx context.Context, c *conn) error
}

func (p *Phone) act(ctx context.Context, do action) error {
	began := time.Now()
	c, ctx, done, err := p.begin(ctx)
	if err != nil {
		return err
	}
	defer done()
	defer c.tree.act()()
	connected := time.Now()
	geometry, err := p.prepare(ctx, c, do.unlocked, do.touches)
	if err != nil {
		return err
	}
	prepared := time.Now()
	err = do.input(ctx, c, geometry)
	p.connection.trace(prepared, do.name, "prepared in "+prepared.Sub(began).String()+": connection "+connected.Sub(began).String()+
		", geometry, lock and backlight "+prepared.Sub(connected).String(), time.Since(prepared))
	c.events.publish(Event{Kind: EventInput})
	c.front.acted()
	if err != nil {
		return err
	}
	if do.check != nil {
		if err = do.check(ctx, c); err != nil {
			return E.Cause(err, "check the result")
		}
	}
	return nil
}

func (p *Phone) gesture(ctx context.Context, c *conn, send func(input *hid.Client) error) error {
	asked := time.Now()
	grant, err := p.connection.input.acquire(ctx, c)
	if err != nil {
		return err
	}
	defer grant.release()
	granted := time.Now()
	err = grant.send(send)
	p.connection.trace(granted, "gesture", "input queue "+granted.Sub(asked).String(), time.Since(granted))
	return err
}

// prepare reads what input needs first: the geometry for input that touches
// the screen, and the lock and backlight state for input that needs it
// unlocked. Each comes from its cache while that is current. The lock state is
// read alongside the others, and a geometry read brings the backlight state
// with it.
func (p *Phone) prepare(ctx context.Context, c *conn, unlocked, touches bool) (phoneGeometry, error) {
	var locked bool
	var lockErr error
	var lockRead sync.WaitGroup
	if unlocked {
		lockRead.Go(func() { locked, lockErr = c.lockState.recent(ctx) })
	}
	var geometry phoneGeometry
	var err error
	if touches {
		geometry, err = c.readGeometry(ctx)
	}
	var display string
	var displayErr error
	if unlocked {
		display, displayErr = c.recentBacklight(ctx)
	}
	lockRead.Wait()
	switch {
	case lockErr != nil:
		return phoneGeometry{}, E.Cause(lockErr, "read the lock state")
	case locked:
		return phoneGeometry{}, E.Extend(ErrLocked, "unlock it first")
	case displayErr != nil:
		return phoneGeometry{}, displayErr
	case display == "off":
		return phoneGeometry{}, E.Extend(ErrLocked, "the screen is off; press Home to wake it first")
	}
	return geometry, err
}

func (p *Phone) TapPoint(ctx context.Context, at Point, count int) error {
	return p.act(ctx, action{name: "tap " + pointText(at), unlocked: true, touches: true, input: func(ctx context.Context, c *conn, geometry phoneGeometry) error {
		if err := geometry.check(at); err != nil {
			return err
		}
		return p.gesture(ctx, c, func(input *hid.Client) error {
			return tap(ctx, geometry, input, at, count, 0)
		})
	}})
}

func (p *Phone) Touch(ctx context.Context, fingers [][]TouchSample) error {
	return p.act(ctx, action{name: "touch", unlocked: true, touches: true, input: func(ctx context.Context, c *conn, geometry phoneGeometry) error {
		if err := checkFingers(geometry, fingers); err != nil {
			return err
		}
		return p.gesture(ctx, c, func(input *hid.Client) error {
			return play(ctx, geometry, input, fingers)
		})
	}})
}

func (p *Phone) Pinch(ctx context.Context, center Point, scale float64, duration time.Duration) error {
	return p.act(ctx, action{name: "pinch", unlocked: true, touches: true, input: func(ctx context.Context, c *conn, geometry phoneGeometry) error {
		fingers, err := pinchPath(geometry, center, scale, duration)
		if err != nil {
			return err
		}
		return p.gesture(ctx, c, func(input *hid.Client) error {
			return play(ctx, geometry, input, fingers)
		})
	}})
}

func (p *Phone) Key(ctx context.Context, combo string) error {
	usages, err := parseKeyCombo(combo)
	if err != nil {
		return E.Cause1(ErrInvalid, err)
	}
	return p.act(ctx, action{name: "key " + combo, unlocked: true, input: func(ctx context.Context, c *conn, _ phoneGeometry) error {
		return p.gesture(ctx, c, func(input *hid.Client) error {
			return pressKeys(ctx, input, usages)
		})
	}})
}

var phoneButtons = map[string][]hid.Button{
	"home": {hid.ButtonHome}, "appSwitcher": {hid.ButtonHome, hid.ButtonHome},
	"power": {{UsagePage: hid.ButtonLock.UsagePage, Usage: hid.ButtonLock.Usage}}, "volumeUp": {hid.ButtonVolumeUp}, "volumeDown": {hid.ButtonVolumeDown},
}

const springboardBundleID = "com.apple.springboard"

func (p *Phone) Button(ctx context.Context, name string) error {
	buttons, known := phoneButtons[name]
	if !known {
		return E.Extend(ErrInvalid, "unknown button ", strconv.Quote(name))
	}
	var check func(ctx context.Context, c *conn) error
	if name == "home" {
		check = func(ctx context.Context, c *conn) error {
			_, err := c.front.waitFor(ctx, springboardBundleID, 0, time.Now().Add(launchLimit))
			return err
		}
	}
	return p.act(ctx, action{
		name: "button " + name,
		input: func(ctx context.Context, c *conn, _ phoneGeometry) error {
			defer c.lockState.forget()
			defer c.backlightCache.invalidate()
			return p.gesture(ctx, c, func(input *hid.Client) error {
				for index, button := range buttons {
					if index > 0 {
						if err := sleep(ctx, homeGap); err != nil {
							return err
						}
					}
					if err := input.PressButton(ctx, button); err != nil {
						return err
					}
				}
				return nil
			})
		},
		check: check,
	})
}

func (p *Phone) Launch(ctx context.Context, bundleID, url string, restart, animated bool) error {
	var started time.Time
	var pid uint64
	return p.act(ctx, action{
		name:     "launch " + bundleID,
		unlocked: true,
		input: func(ctx context.Context, c *conn, _ phoneGeometry) error {
			started = time.Now()
			launchContext, cancel := context.WithDeadline(ctx, started.Add(launchLimit))
			defer cancel()
			var err error
			pid, err = apps.Launch(launchContext, c.device, bundleID, url, restart, animated)
			if err == nil {
				return nil
			}
			list, listErr := apps.List(ctx, c.device)
			if listErr == nil && !slices.ContainsFunc(list, func(app apps.App) bool { return app.BundleID == bundleID }) {
				return E.Cause1(ErrNotFound, E.Cause(err, "no app with the bundle ID ", strconv.Quote(bundleID), " is listed"))
			}
			return E.Append(err, listErr, func(err error) error { return E.Cause(err, "list the apps") })
		},
		check: func(ctx context.Context, c *conn) error {
			front, err := c.front.waitFor(ctx, bundleID, pid, started.Add(launchLimit))
			if err != nil {
				return err
			}
			if front != bundleID {
				return E.New(bundleID, " launched, but ", front, " is still in front after ", launchLimit)
			}
			return nil
		},
	})
}

func (p *Phone) Unlock(ctx context.Context) error {
	settings := p.connection.settings.Load()
	if settings.AutoUnlockPasscode == "" {
		return E.New("no unlock passcode is configured")
	}
	return p.act(ctx, action{name: "unlock", input: func(ctx context.Context, c *conn, _ phoneGeometry) error {
		locked, err := c.lockState.locked(ctx, c.device)
		if err != nil {
			return E.Cause(err, "read the lock state")
		}
		display, err := c.backlight(ctx)
		if err != nil {
			return err
		}
		if !locked && (display == "activeOn" || display == "dimmed") {
			return nil
		}
		grant, err := p.connection.input.acquire(ctx, c)
		if err != nil {
			return err
		}
		defer grant.release()
		if err = grant.closeSoftware(); err != nil {
			return err
		}
		target := &deviceUnlockTarget{connection: p.connection, settings: settings, conn: c, input: grant.hardware(), keys: newKeyboard(), logger: p.logger}
		_, err = target.unlock(ctx)
		grant.settleHardware(err)
		if err != nil {
			return E.Cause(err, "unlock")
		}
		return nil
	}})
}
