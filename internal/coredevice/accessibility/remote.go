package accessibility

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	F "github.com/sagernet/sing/common/format"
	"github.com/swipt-automation/swipt/internal/coredevice"
	"github.com/swipt-automation/swipt/internal/coredevice/dtx"
)

// axauditd serves requests and its own reads of the tree on its main thread,
// waiting up to 10000 s for each read of an application (24B5110a
// AccessibilityAudit 0x253f9c670; 24B5110a axauditd 0x1000115b8, 0x10000cbc0;
// iPhone18,2, iOS 27.2 24B115).
var ErrStalled = E.New("accessibility: the device stopped answering")

type StallError struct {
	PID    uint64
	Reason string
}

func (e *StallError) Error() string {
	return ErrStalled.Error() + ": " + e.Reason
}

func (e *StallError) Unwrap() error {
	return ErrStalled
}

// axauditd accepts connections on its main queue (24B5110a axauditd
// 0x10000cc78..0x10000cc90), which also serves the requests of every
// connection and its own reads of the tree (24B5110a AccessibilityAudit
// 0x253f9c670..0x253f9c67c; 24B5110a AccessibilityPlatformTranslation
// 0x253fe12b8..0x253fe12c4): a snapshot held it for up to 0.9 s with Control
// Center open (iPad17,1, iPadOS 27.2 24B115) and a round of snapshots of
// SpringBoard for up to 4.57 s while Control Center opened, while a connection
// to a daemon blocked on a process that did not answer completed its handshake
// after 182 s (iPhone18,2, iOS 27.2 24B115).
const handshakeWait = 7 * time.Second

// AXPTranslatorRequest request types, which the response carries back as
// associatedRequestType (24A446 AccessibilityPlatformTranslation
// -[AXPTranslator processTranslatorRequest:] 0x24fe57a4c, 0x24fe57bec;
// iPhone18,2, iOS 27.2 24B115).
const (
	requestApplication = 1
	requestAttribute   = 2
	requestFrontmost   = 4
	requestAttributes  = 5
	requestHitTest     = 6
	requestAction      = 7
	requestTree        = 11
)

// AXPAttribute numbers (24A446 AccessibilityPlatformTranslation
// __AXPAttributeToString 0x24fe585dc; iPhone18,2, iOS 27.2 24B115). In a
// requestAttribute request, attributeNative reads the native UIAccessibility
// attribute given as "parameter" (24A446 AccessibilityPlatformTranslation
// -[AXPTranslator_iOS processAttributeRequest:] 0x24fe4e4c8).
const (
	attributeBundleIdentifier = 6
	attributeCustomActions    = 13
	attributeFocusedElement   = 19
	attributeFrame            = 21
	attributeIdentifier       = 25
	attributeIsEnabled        = 27
	attributeIsValid          = 31
	attributeIsVisible        = 32
	attributeLabel            = 33
	attributeRawElementData   = 60
	attributeNative           = 63
	attributeTraits           = 77
)

// UIAccessibility native attributes (24A446 UIAccessibility
// -[NSObject(AXPrivCategory) _iosAccessibilityAttributeValue:] 0x1bb739f00;
// iPhone18,2, iOS 27.2 24B115).
const (
	nativeActiveKeyboard   = 2076
	nativeIsFirstResponder = 2198
	nativeFirstResponder   = 3002
	nativeClassName        = 5003
)

// AX trait bits (24A446 AXRuntime _kAXSelectedTrait 0x18f8773f0,
// _kAXNotEnabledTrait 0x18f877418, _kAXTextEntryTrait 0x18f877460,
// _kAXSecureTextFieldTrait 0x18f877498). Spotlight's search field had
// traitTextEntry, and the lock-screen passcode field both (iPhone18,2, iOS
// 27.2 24B115).
const (
	traitSelected        = 0x8
	traitNotEnabled      = 0x100
	traitTextEntry       = 0x40000
	traitSecureTextField = 0x01000000
)

type RemoteClient struct {
	conn                    *dtx.Conn
	ctx                     context.Context
	owner                   context.Context
	cancel                  context.CancelCauseFunc
	stopContext             func() bool
	closeOnce               sync.Once
	closeErr                error
	enabled                 bool
	deviceClass             string
	mu                      sync.Mutex
	stateMu                 sync.Mutex
	pending                 []*pendingRead
	drained                 chan struct{}
	batchRawElementDisabled bool
	guessMissed             atomic.Bool
	screenViewing           []translationObject
	screenViewingPending    chan struct{}
	pushed                  func(Push)
}

func DialRemote(ctx, owner context.Context, device *coredevice.Device, pushed func(Push)) (_ *RemoteClient, err error) {
	// axauditd starts DTX on a remoteAXService connection without reading an
	// RSDCheckin (24B5110a axauditd 0x10000d214..0x10000d310): with none sent,
	// it invoked _notifyOfPublishedCapabilities: and hostAPIVersion and
	// answered deviceAPIVersion (iPhone18,2, iOS 27.2 24B115).
	conn, err := device.DialService(ctx, "com.apple.accessibility.axAuditDaemon.remoteAXService")
	if err != nil {
		return nil, E.Cause(err, "accessibility: connect remoteAXService")
	}
	dtxConn, err := dtx.NewConn(owner, conn, func(event dtx.Event) (any, bool) {
		// Accessibility Inspector answers 7 (Xcode 27.1 27A9275
		// AccessibilityAuditDeviceManager -[XDMDevice hostApiVersion] 0x2954).
		if event.Selector == "hostAPIVersion" {
			return int64(7), true
		}
		if event.Selector == "_notifyOfPublishedCapabilities:" {
			return nil, true
		}
		return nil, false
	})
	if err != nil {
		return nil, E.Cause(err, "accessibility: initialize DTX")
	}
	operation, cancel := context.WithCancelCause(ctx)
	c := &RemoteClient{
		conn: dtxConn, ctx: operation, owner: owner, cancel: cancel,
		deviceClass:          device.Info().DeviceClass,
		screenViewingPending: make(chan struct{}, 1),
		pushed:               pushed,
	}
	c.mu.Lock()
	go c.readLoop()
	c.stopContext = context.AfterFunc(operation, func() { c.Close() })
	defer func() {
		c.mu.Unlock()
		if err != nil {
			err = E.Append(err, c.Close(), func(err error) error {
				return E.Cause(err, "accessibility: stop remote AX")
			})
		}
	}()
	handshake, cancelHandshake := context.WithTimeout(operation, handshakeWait)
	_, err = dtxConn.Invoke(handshake, 0, "deviceAPIVersion")
	cancelHandshake()
	if errors.Is(err, context.DeadlineExceeded) && operation.Err() == nil {
		return nil, &StallError{Reason: "no answer to deviceAPIVersion within " + handshakeWait.String()}
	}
	if cause := context.Cause(operation); cause != nil {
		return nil, cause
	}
	if err != nil {
		return nil, E.Cause(err, "accessibility: invoke deviceAPIVersion")
	}
	// AXAuditDevicesAppRemoteServer drops processDataFromHost: data until
	// clientNeedsAccessibility: true, which also starts pushing the cached tree
	// (24A446 AccessibilityAudit 0x24fe0b45c, 0x24fe0b5e4..0x24fe0b61c).
	c.enabled = true
	if err = dtxConn.Notify(owner, 0, "clientNeedsAccessibility:", true); err != nil {
		return nil, E.Cause(err, "accessibility: notify clientNeedsAccessibility:")
	}
	if err = context.Cause(operation); err != nil {
		return nil, err
	}
	return c, nil
}

// After clientNeedsAccessibility: false, the device closed the connection
// within 0.1 s (iPhone18,2, iOS 27.2 24B115); one that stopped answering
// requests did not close it at all, and one stopped reading, so that the
// write blocked (iPad17,1, iPadOS 27.2 24B115).
const closeWait = 200 * time.Millisecond

func (c *RemoteClient) Close() error {
	c.closeOnce.Do(func() {
		c.cancel(net.ErrClosed)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.stopContext()
		// clientNeedsAccessibility: false clears the server's cache manager and
		// cancels the connection (24B5110a AccessibilityAudit
		// 0x253f9c578..0x253f9c5a8, 0x253f9be80..0x253f9be90), while a lost
		// socket only clears the connection (24B5110a AccessibilityAudit
		// 0x253f9bc3c..0x253f9bce4) and leaves the server, its cache manager and
		// the manager's _transportChannel retaining each other (24B5110a
		// AccessibilityPlatformTranslation 0x253fe4994..0x253fe49a8).
		if c.enabled {
			ctx, cancel := context.WithTimeout(c.owner, closeWait)
			if err := c.conn.Notify(ctx, 0, "clientNeedsAccessibility:", false); err != nil {
				c.closeErr = E.Cause(err, "accessibility: notify clientNeedsAccessibility: false")
			} else {
				select {
				case <-c.conn.Done():
				case <-ctx.Done():
					c.closeErr = E.New("accessibility: the device kept the connection open after clientNeedsAccessibility: false")
				}
			}
			cancel()
		}
		c.conn.Close()
	})
	return c.closeErr
}

func (c *RemoteClient) CloseCause(cause error) error {
	c.cancel(cause)
	return c.Close()
}

type PasscodeState struct {
	Ready       bool                `json:"ready"`
	Reason      string              `json:"reason"`
	Value       string              `json:"value,omitempty"`
	Diagnostics PasscodeDiagnostics `json:"diagnostics"`
}

type PasscodeDiagnostics struct {
	Stage            string  `json:"stage"`
	RequestType      uint64  `json:"request_type"`
	Attribute        uint64  `json:"attribute,omitempty"`
	NativeAttribute  uint64  `json:"native_attribute,omitempty"`
	ResponseCode     *uint64 `json:"response_code"`
	SpringBoard      *bool   `json:"springboard,omitempty"`
	Candidate        string  `json:"candidate,omitempty"`
	Enabled          *uint64 `json:"enabled,omitempty"`
	Valid            *bool   `json:"valid,omitempty"`
	Visible          *bool   `json:"visible,omitempty"`
	Traits           *uint64 `json:"traits,omitempty"`
	Secure           *bool   `json:"secure,omitempty"`
	FirstResponder   *bool   `json:"first_responder,omitempty"`
	ResponderMatches *bool   `json:"responder_matches,omitempty"`
	FrontmostMatches *bool   `json:"frontmost_matches,omitempty"`
}

func (c *RemoteClient) ReadPasscodeState(ctx context.Context) (PasscodeState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var state PasscodeState
	app, values, _, err := c.readPasscodeApplication(ctx, &state, false)
	if err != nil || values == nil {
		return state, err
	}
	return c.readPasscodeField(ctx, state, app, values)
}

func (c *RemoteClient) ReadPasscodePreparation(ctx context.Context) (PasscodeState, *PasscodeLayout, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var state PasscodeState
	app, values, raw, err := c.readPasscodeApplication(ctx, &state, true)
	if err != nil || values == nil {
		return state, nil, err
	}
	state, err = c.readPasscodeField(ctx, state, app, values)
	if err != nil || state.Ready || state.Diagnostics.Stage == "frontmost_recheck" {
		return state, nil, err
	}
	layout, err := c.readPasscodeLayoutForApp(ctx, app, values, raw)
	return state, &layout, err
}

func (c *RemoteClient) readPasscodeQuery(ctx context.Context, state *PasscodeState, stage string, kind uint64, obj *translationObject, attribute, native uint64, params any) (remoteResponse, error) {
	state.Diagnostics.Stage = stage
	state.Diagnostics.RequestType = kind
	state.Diagnostics.Attribute = attribute
	state.Diagnostics.NativeAttribute = native
	state.Diagnostics.ResponseCode = nil
	state.Reason = stage + "_transport_error"
	response, err := c.query(ctx, kind, obj, attribute, 0, params)
	if err == nil {
		state.Diagnostics.ResponseCode = &response.code
		state.Reason = stage + "_response_error"
	}
	return response, err
}

func (c *RemoteClient) readPasscodeApplication(ctx context.Context, state *PasscodeState, includeFrame bool) (translationObject, map[any]any, layoutRawElement, error) {
	var raw layoutRawElement
	appResponse, err := c.readPasscodeQuery(ctx, state, "frontmost", requestFrontmost, nil, 0, 0, map[string]any{"displayId": int64(1)})
	if err != nil {
		return translationObject{}, nil, raw, err
	}
	if appResponse.code != 0 {
		return translationObject{}, nil, raw, nil
	}
	app, ok := parseTranslation(appResponse.value)
	if !ok || !app.application {
		state.Reason = "frontmost_invalid"
		return translationObject{}, nil, raw, nil
	}
	attributes := []any{int64(attributeBundleIdentifier), int64(attributeFocusedElement)}
	batchRaw := includeFrame && !c.batchRawElementDisabled
	if includeFrame {
		attributes = append(attributes, int64(attributeFrame))
		raw.status = "disabled"
		if batchRaw {
			attributes = append(attributes, int64(attributeRawElementData))
		}
	}
	// attributeFocusedElement reads nativeFirstResponder and, only when that is
	// nil, native 3027, the UIKit focus element (24A446
	// AccessibilityPlatformTranslation 0x24fe4c9a4..0x24fe4ca18). It returned
	// the first responder in Spotlight's search field, and error 3 on the home
	// screen, where both were nil (iPhone18,2, iOS 27.2 24B115).
	candidate, err := c.readPasscodeQuery(ctx, state, "candidate", requestAttributes, &app, 0, 0, map[string]any{"attributes": attributes})
	if err != nil {
		return translationObject{}, nil, raw, err
	}
	if batchRaw {
		code := candidate.code
		raw.code = &code
	}
	if batchRaw && candidate.code != 0 {
		// The attributes of a batch share one error: an attribute that fails
		// can set it for the whole batch, or only drop out of it (24A446
		// AccessibilityPlatformTranslation -[AXPTranslator_iOS
		// processMultipleAttributeRequest:removeEmptyValue:] 0x24fe47af4,
		// 0x24fe47b34, 0x24fe47b58): attributes 5 and 7 set errors 7 and 3,
		// while 4, 13, 18 and 19 dropped out (iPhone18,2, iOS 27.2 24B115).
		c.batchRawElementDisabled = true
		raw.status = "response_error"
		candidate, err = c.readPasscodeQuery(ctx, state, "candidate", requestAttributes, &app, 0, 0, map[string]any{"attributes": attributes[:len(attributes)-1]})
		if err != nil {
			return translationObject{}, nil, raw, err
		}
	}
	values, ok := candidate.value.(map[any]any)
	if candidate.code != 0 {
		return translationObject{}, nil, raw, nil
	}
	if !ok {
		state.Reason = "app_attributes_invalid"
		return translationObject{}, nil, raw, nil
	}
	springboard := values[uint64(attributeBundleIdentifier)] == "com.apple.springboard"
	state.Diagnostics.SpringBoard = &springboard
	if !springboard {
		state.Reason = "not_springboard"
		return translationObject{}, nil, raw, nil
	}
	if batchRaw && raw.status != "response_error" {
		value, present := values[uint64(attributeRawElementData)]
		raw.data = decodeLayoutRawElement(value)
		switch {
		case len(raw.data) != 0:
			raw.status = "valid"
		case !present || value == nil:
			raw.status = "missing"
		default:
			raw.status = "invalid"
		}
		if raw.status != "valid" {
			c.batchRawElementDisabled = true
		}
	}
	return app, values, raw, nil
}

func (c *RemoteClient) readPasscodeField(ctx context.Context, state PasscodeState, app translationObject, values map[any]any) (PasscodeState, error) {
	field, ok := parseTranslation(values[uint64(attributeFocusedElement)])
	switch {
	case values[uint64(attributeFocusedElement)] == nil:
		state.Diagnostics.Candidate = "missing"
	case !ok:
		state.Diagnostics.Candidate = "invalid"
	case field.application:
		state.Diagnostics.Candidate = "application"
	case field.pid != app.pid:
		state.Diagnostics.Candidate = "foreign_process"
	default:
		state.Diagnostics.Candidate = "element"
	}
	if state.Diagnostics.Candidate != "element" {
		state.Reason = "candidate_" + state.Diagnostics.Candidate
		return state, nil
	}
	attributes, err := c.readPasscodeQuery(ctx, &state, "field_attributes", requestAttributes, &field, 0, 0, map[string]any{"attributes": []any{
		int64(attributeIsEnabled), int64(attributeIsValid), int64(attributeIsVisible), int64(attributeTraits), int64(attributeValue),
	}})
	if err != nil {
		return state, err
	}
	if attributes.code != 0 {
		return state, nil
	}
	if values, ok := attributes.value.(map[any]any); ok {
		state.Value = textValue(values[uint64(attributeValue)])
	}
	if reason := inspectSecureFieldAttributes(attributes.value, &state.Diagnostics); reason != "" {
		state.Reason = reason
		return state, nil
	}
	// nativeIsFirstResponder asks the nearest self-or-ancestor UIResponder
	// (24A446 UIAccessibility 0x1bb73be28, 0x1bb73c5ac). It was true on
	// Spotlight's focused search field and false on the keyboard's keys and the
	// other elements (iPhone18,2, iOS 27.2 24B115).
	responder, err := c.readPasscodeQuery(ctx, &state, "first_responder", requestAttribute, &field, attributeNative, nativeIsFirstResponder, map[string]any{"parameter": int64(nativeIsFirstResponder)})
	if err != nil {
		return state, err
	}
	if responder.code != 0 {
		return state, nil
	}
	firstResponder, ok := responder.value.(bool)
	if !ok {
		state.Reason = "first_responder_unavailable"
		return state, nil
	}
	state.Diagnostics.FirstResponder = &firstResponder
	if !firstResponder {
		state.Reason = "not_first_responder"
		return state, nil
	}
	// A request carries no UI snapshot or version to tie it to another (24A446
	// AccessibilityPlatformTranslation -[AXPTranslatorRequest encodeWithCoder:]
	// 0x24fe59010), so the field read above may already be replaced:
	// consecutive reads of the passcode field saw the digits typed between them
	// (iPhone18,2, iOS 27.2 24B115).
	recheck, err := c.readPasscodeQuery(ctx, &state, "responder_recheck", requestAttribute, &app, attributeNative, nativeFirstResponder, map[string]any{"parameter": int64(nativeFirstResponder)})
	if err != nil {
		return state, err
	}
	current, ok := parseTranslation(recheck.value)
	if recheck.code != 0 {
		return state, nil
	}
	if !ok {
		state.Reason = "responder_recheck_unavailable"
		return state, nil
	}
	responderMatches := field.same(current)
	state.Diagnostics.ResponderMatches = &responderMatches
	if !responderMatches {
		state.Reason = "responder_changed"
		return state, nil
	}
	frontmost, err := c.readPasscodeQuery(ctx, &state, "frontmost_recheck", requestFrontmost, nil, 0, 0, map[string]any{"displayId": int64(1)})
	if err != nil {
		return state, err
	}
	currentApp, ok := parseTranslation(frontmost.value)
	if frontmost.code != 0 {
		return state, nil
	}
	if !ok {
		state.Reason = "frontmost_recheck_unavailable"
		return state, nil
	}
	frontmostMatches := app.same(currentApp)
	state.Diagnostics.FrontmostMatches = &frontmostMatches
	state.Ready = frontmostMatches
	state.Reason = "frontmost_changed"
	if state.Ready {
		state.Reason = "ready"
	}
	return state, nil
}

type translationObject struct {
	pid         uint64
	objectID    uint64
	application bool
	fields      map[string]any
}

func parseTranslation(value any) (translationObject, bool) {
	fields, ok := value.(map[string]any)
	if !ok || fields["$class"] != "AXPTranslationObject" {
		return translationObject{}, false
	}
	pid, pidOK := unsigned(fields["pid"])
	id, idOK := unsigned(fields["objectID"])
	application, appOK := fields["isApplicationElement"].(bool)
	return translationObject{pid: pid, objectID: id, application: application, fields: fields}, pidOK && idOK && appOK && pid != 0 && id != 0
}

func (t translationObject) same(other translationObject) bool {
	return t.pid == other.pid && t.objectID == other.objectID && t.application == other.application
}

// AXPTranslationObject spells the key didPopuldateAppInfo (24A446
// AccessibilityPlatformTranslation -[AXPTranslationObject encodeWithCoder:]
// 0x24fe58b20; iPhone18,2, iOS 27.2 24B115).
func (t translationObject) archiveObject() dtx.Object {
	fields := make(map[string]any)
	for _, key := range []string{"pid", "objectID", "isApplicationElement", "didPopuldateAppInfo", "bridgeDelegateToken", "rawElementData"} {
		if value, ok := t.fields[key]; ok {
			fields[key] = value
		}
	}
	return dtx.Object{ClassName: "AXPTranslationObject", Fields: fields, PrimitiveFields: []string{"pid", "isApplicationElement", "didPopuldateAppInfo"}}
}

type remoteResponse struct {
	value any
	code  uint64
}

type remoteRequest struct {
	kind      uint64
	object    *translationObject
	attribute uint64
	action    uint64
	params    any
}

func (c *RemoteClient) query(ctx context.Context, kind uint64, obj *translationObject, attribute, action uint64, params any) (remoteResponse, error) {
	responses, err := c.exchange(ctx, []remoteRequest{{kind: kind, object: obj, attribute: attribute, action: action, params: params}})
	if err != nil {
		return remoteResponse{}, err
	}
	return responses[0], nil
}

// AXP responses carry the request type but no identifier (24A446
// AccessibilityPlatformTranslation -[AXPTranslatorResponse encodeWithCoder:]
// 0x24fe59764), and axauditd serves requests one after another on its main
// queue (24B5110a AccessibilityAudit 0x253f9c670..0x253f9c67c): only responses
// to single-attribute requests carried an associatedTranslationObject, and
// five pipelined requests of four types were answered in order (iPhone18,2,
// iOS 27.2 24B115).
func (c *RemoteClient) exchange(ctx context.Context, requests []remoteRequest) ([]remoteResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := context.Cause(c.ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := context.AfterFunc(c.ctx, func() { cancel(context.Cause(c.ctx)) })
	defer stop()
	wait := exchangeWait(requests)
	if err := c.drain(ctx, wait); err != nil {
		return nil, err
	}
	reads := make([]*pendingRead, 0, len(requests))
	for _, request := range requests {
		var translation any
		if request.object != nil {
			translation = request.object.archiveObject()
		} else if request.kind == requestFrontmost || request.kind == requestHitTest {
			translation = dtx.Object{ClassName: "AXPTranslationObject", Fields: map[string]any{
				"pid": int64(0), "objectID": int64(0), "isApplicationElement": false, "didPopuldateAppInfo": false,
			}, PrimitiveFields: []string{"pid", "isApplicationElement", "didPopuldateAppInfo"}}
		}
		data, err := dtx.MarshalArchive(dtx.Object{ClassName: "AXPTranslatorRequest", Fields: map[string]any{
			"requestType": request.kind, "attributeType": request.attribute, "actionType": request.action, "clientType": int64(2),
			"translation": translation, "parameters": request.params,
		}, PrimitiveFields: []string{"requestType", "attributeType", "actionType", "clientType"}})
		if err != nil {
			return nil, err
		}
		read := &pendingRead{kind: request.kind, attribute: request.attribute, sent: time.Now(), response: make(chan remoteResponse, 1)}
		if request.object != nil {
			read.pid = request.object.pid
		}
		if params, isMap := request.params.(map[string]any); isMap && request.attribute == attributeNative {
			read.native, _ = unsigned(params["parameter"])
		}
		reads = append(reads, read)
		c.stateMu.Lock()
		c.pending = append(c.pending, read)
		c.stateMu.Unlock()
		if err = c.conn.Notify(ctx, 0, "processDataFromHost:", data); err != nil {
			if ctx.Err() != nil {
				err = context.Cause(ctx)
			}
			c.cancel(err)
			return nil, err
		}
	}
	stalled := time.NewTimer(wait)
	defer stalled.Stop()
	responses := make([]remoteResponse, len(reads))
	for index, read := range reads {
		select {
		case responses[index] = <-read.response:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		case <-stalled.C:
			c.stateMu.Lock()
			queued := len(c.pending)
			c.stateMu.Unlock()
			return nil, &StallError{PID: read.pid, Reason: F.ToString("no answer to ", len(reads)-index, " of ", len(reads), " requests within ", wait,
				"; the first unanswered is ", read.describe(), "; ", queued, " requests of this connection are unanswered")}
		}
	}
	return responses, nil
}

func (c *RemoteClient) drain(ctx context.Context, wait time.Duration) error {
	c.stateMu.Lock()
	if len(c.pending) == 0 {
		c.stateMu.Unlock()
		return nil
	}
	head := c.pending[0]
	queued := len(c.pending)
	if c.drained == nil {
		c.drained = make(chan struct{})
	}
	drained := c.drained
	c.stateMu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return &StallError{PID: head.pid, Reason: F.ToString("the ", queued, " requests an earlier read left unanswered got no answer within ", wait,
			" more; the first of them is ", head.describe())}
	}
}

// A round of requests took 20 to 190 ms plus about 3 ms for each request, 909
// ms for 326 snapshots of web objects, and a snapshot of SpringBoard with
// Control Center open took up to 867 ms (iPad17,1, iPadOS 27.2 24B115); in 464
// rounds during 20 cold launches of system apps, one request took up to 1.26 s
// and a round with snapshots up to 1.67 s, and while Control Center opened, one
// request took up to 1.85 s and a round with snapshots of SpringBoard up to
// 4.57 s (iPhone18,2, iOS 27.2 24B115).
func exchangeWait(requests []remoteRequest) time.Duration {
	wait := 3*time.Second + time.Duration(len(requests))*6*time.Millisecond
	if slices.ContainsFunc(requests, remoteRequest.heavy) {
		wait += 4 * time.Second
	}
	return wait
}

func (r remoteRequest) heavy() bool {
	switch r.kind {
	case requestAction, requestSet:
		return true
	case requestAttribute:
		return r.attribute == attributeUserTestingSnapshot || r.attribute == attributeSearch
	}
	return false
}

type pendingRead struct {
	kind      uint64
	attribute uint64
	native    uint64
	pid       uint64
	sent      time.Time
	response  chan remoteResponse
}

func (r *pendingRead) describe() string {
	text := F.ToString("request type ", r.kind)
	if r.kind == requestAttribute {
		text += F.ToString(" for attribute ", r.attribute)
	}
	if r.native != 0 {
		text += F.ToString(" of native attribute ", r.native)
	}
	if r.pid != 0 {
		text += F.ToString(" to process ", r.pid)
	}
	return text + ", sent " + time.Since(r.sent).Round(time.Millisecond).String() + " ago"
}

func (c *RemoteClient) readLoop() {
	for {
		event, err := c.conn.Receive(context.Background())
		if err == nil {
			err = c.receive(event)
		}
		if err != nil {
			c.cancel(err)
			return
		}
	}
}

func (c *RemoteClient) receive(event dtx.Event) error {
	if event.Selector != "processDataFromRemoteDevice:" {
		return nil
	}
	for _, argument := range event.Arguments {
		data, ok := argument.([]byte)
		if !ok {
			return E.New("accessibility: remote AX response is not archived data")
		}
		decoded, err := dtx.UnmarshalArchive(data)
		if err != nil {
			return err
		}
		response, ok := decoded.(map[string]any)
		if !ok || response["$class"] != "AXPTranslatorResponse" {
			return E.New("accessibility: invalid remote AX response")
		}
		kind, kindOK := unsigned(response["associatedRequestType"])
		attribute, attrOK := unsigned(response["attribute"])
		code, codeOK := unsigned(response["error"])
		notification, notificationOK := unsigned(response["notification"])
		if !kindOK || !attrOK || !codeOK || !notificationOK {
			return E.New("accessibility: invalid remote AX response metadata")
		}
		switch {
		case notification != 0:
			if c.pushed != nil {
				c.pushed(pushedNotification(notification, response))
			}
		case kind == requestTree:
			c.receiveScreenViewingNotifications(response["resultData"])
			if c.pushed != nil {
				c.pushed(pushedTree(response["resultData"]))
			}
		default:
			c.stateMu.Lock()
			if len(c.pending) == 0 {
				c.stateMu.Unlock()
				return E.New("accessibility: a response of request type ", kind, " came with no request pending")
			}
			head := c.pending[0]
			if head.kind != kind || kind == requestAttribute && head.attribute != attribute {
				c.stateMu.Unlock()
				return E.New("accessibility: a response of request type ", kind, " and attribute ", attribute,
					" came for request type ", head.kind, " and attribute ", head.attribute)
			}
			c.pending = c.pending[1:]
			if len(c.pending) == 0 && c.drained != nil {
				close(c.drained)
				c.drained = nil
			}
			c.stateMu.Unlock()
			head.response <- remoteResponse{value: response["resultData"], code: code}
		}
	}
	return nil
}

func inspectSecureFieldAttributes(value any, diagnostics *PasscodeDiagnostics) string {
	values, ok := value.(map[any]any)
	if !ok {
		return "field_attributes_invalid"
	}
	attributes := make(map[uint64]any)
	for key, value := range values {
		if number, ok := unsigned(key); ok {
			attributes[number] = value
		}
	}
	enabled, enabledOK := unsigned(attributes[attributeIsEnabled])
	traits, traitsOK := unsigned(attributes[attributeTraits])
	valid, validOK := attributes[attributeIsValid].(bool)
	visible, visibleOK := attributes[attributeIsVisible].(bool)
	if enabledOK {
		diagnostics.Enabled = &enabled
	}
	if validOK {
		diagnostics.Valid = &valid
	}
	if visibleOK {
		diagnostics.Visible = &visible
	}
	secure := traits&traitSecureTextField != 0
	if traitsOK {
		diagnostics.Traits = &traits
		diagnostics.Secure = &secure
	}
	switch {
	case !enabledOK:
		return "enabled_unavailable"
	case enabled != 1:
		return "not_enabled"
	case !validOK:
		return "validity_unavailable"
	case !valid:
		return "invalid_field"
	case !visibleOK:
		return "visibility_unavailable"
	case !visible:
		return "invisible_field"
	case !traitsOK:
		return "traits_unavailable"
	case !secure:
		return "not_secure"
	default:
		return ""
	}
}

func unsigned(value any) (uint64, bool) {
	switch number := value.(type) {
	case uint64:
		return number, true
	case int64:
		return uint64(number), number >= 0
	case uint32:
		return uint64(number), true
	case int:
		return uint64(number), number >= 0
	default:
		return 0, false
	}
}
