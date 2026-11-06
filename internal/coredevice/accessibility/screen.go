package accessibility

import (
	"context"
	"errors"
	"math"
	"slices"
	"time"
)

type Screen struct {
	BundleID string
	App      Element
	Root     *TreeNode
	System   *TreeNode
	Focused  *ElementRef
	Rounds   []time.Duration
	// Animating is set when the application ran UIView animations as it was
	// read, or did not say, so that frames may be between two places.
	Animating bool
}

const (
	remoteProbeDepth     = 3
	webSearchResultLimit = 10000
)

// WebKit's search with AXVisibleOnly answered the objects around the
// viewport in document order: 324 objects of a Wikipedia page in 112 ms,
// where the snapshot of the page answered all 4878 in 4.5 s (iPad17,1, iPadOS
// 27.2 24B115).
var webSearch = map[string]any{
	"AXDirection": "AXDirectionNext", "AXResultsLimit": int64(webSearchResultLimit), "AXSearchKey": "AXAnyTypeSearchKey",
	"AXVisibleOnly": true, "AXImmediateDescendantsOnly": false,
}

// After its process was told to end, an application stayed the frontmost one
// for about half a second, answering its bundle ID but no snapshot, until
// SpringBoard was in front (iPhone18,2, iOS 27.2 24B115).
const frontmostTurnover = time.Second

func untilAnswered(read func() (*Screen, error)) (*Screen, error) {
	deadline := time.Now().Add(frontmostTurnover)
	for {
		screen, err := read()
		if !errors.Is(err, ErrNoAnswer) || time.Now().After(deadline) {
			return screen, err
		}
	}
}

func (c *RemoteClient) ReadScreen(ctx context.Context, springboard uint64, overlay func(front Element) bool) (*Screen, error) {
	return untilAnswered(func() (*Screen, error) {
		var rounds []time.Duration
		run := func(batch *Batch) error {
			started := time.Now()
			err := c.Run(ctx, batch)
			rounds = append(rounds, time.Since(started))
			return err
		}
		first := &Batch{}
		front := first.Frontmost()
		system := first.Application(springboard)
		if err := run(first); err != nil {
			return nil, err
		}
		if err := front.Err("read the frontmost application"); err != nil {
			return nil, err
		}
		if system.Code != 0 {
			return nil, ErrNoApplication
		}
		app := front.Value
		second := &Batch{}
		bundle := second.BundleID(app)
		full := second.Snapshot(app, SnapshotOptions{})
		modal := second.Snapshot(app, SnapshotOptions{Attributes: []int64{nativeClassName}, HonorModal: true})
		focused := second.Focused(app)
		animating := second.AnimationsInProgress(app)
		var systemSnapshot *Result[*Snapshot]
		if system.Value.Ref.PID != app.Ref.PID && (overlay == nil || overlay(app)) {
			systemSnapshot = second.Snapshot(system.Value, SnapshotOptions{})
		}
		if err := run(second); err != nil {
			return nil, err
		}
		if err := bundle.Err("read the frontmost application's bundle ID"); err != nil {
			return nil, c.silent(ctx, app, false, err)
		}
		if err := full.Err("snapshot the frontmost application"); err != nil {
			return nil, c.silent(ctx, app, false, err)
		}
		if err := modal.Err("snapshot the frontmost application's modal view"); err != nil {
			return nil, err
		}
		screen := &Screen{BundleID: bundle.Value, App: app, Root: full.Value.Root, Animating: animating.Code != 0 || animating.Value}
		if focused.Code == 0 {
			ref := focused.Value.Ref
			screen.Focused = &ref
		}
		markInert(full.Value.Root, modal.Value.Listed, false)
		snapshots := []*Snapshot{full.Value}
		if systemSnapshot != nil {
			if err := systemSnapshot.Err("snapshot SpringBoard"); err != nil {
				return nil, err
			}
			screen.System = systemSnapshot.Value.Root
			snapshots = append(snapshots, systemSnapshot.Value)
		}
		err := c.expand(run, snapshots, springboard)
		screen.Rounds = rounds
		return screen, err
	})
}

var placementAttributes = []int64{nativeAutomationType, nativeTraits, nativeFrame, nativeVisibleFrame, nativeActivation, nativeContextID}

var fieldAttributes = map[string][]int64{
	"class":       {nativeClassName},
	"label":       {nativeLabel},
	"value":       {nativeValue, nativeNumberValue, nativePlaceholder},
	"identifier":  {nativeIdentifier},
	"placeholder": {nativePlaceholder},
	"hint":        {nativeHelp, nativeHint},
	"url":         {nativeURL},
	"expanded":    {nativeExpanded},
	"actions":     {nativeCustomActions},
}

func SnapshotAttributes(fields []string) []int64 {
	attributes := slices.Clone(placementAttributes)
	for _, field := range fields {
		for _, attribute := range fieldAttributes[field] {
			if !slices.Contains(attributes, attribute) {
				attributes = append(attributes, attribute)
			}
		}
	}
	return attributes
}

type ApplicationOptions struct {
	PID        uint64
	Attributes []int64
	Modal      bool
	Focused    bool
	Expand     bool
	Wanted     func(bundleID string) bool
	// Animations reads whether the application runs UIView animations.
	Animations bool
	// Front, with PID 0, is the application last read as the frontmost one:
	// it is read with the frontmost application in one round, kept only when
	// that is still it.
	Front *Front
}

// Front is an application read as the frontmost one, and its bundle ID.
type Front struct {
	App      Element
	BundleID string
}

// An app that read the pasteboard while launching answered no snapshot until
// its paste prompt was answered, while the frontmost application was already
// SpringBoard (iPhone18,2, iOS 27.2 24B115).
func (c *RemoteClient) ReadApplication(ctx context.Context, options ApplicationOptions) (*Screen, error) {
	read := func() (*Screen, error) {
		var app *Element
		if guess := options.Front; options.PID == 0 && guess != nil && !c.guessMissed.Load() {
			screen, front, err := c.readGuessed(ctx, options, guess)
			if err != nil || screen != nil {
				return screen, err
			}
			app = front
		}
		if app == nil {
			first := &Batch{}
			var found *Result[Element]
			if options.PID == 0 {
				found = first.Frontmost()
			} else {
				found = first.Application(options.PID)
			}
			if err := c.Run(ctx, first); err != nil {
				return nil, err
			}
			if err := found.Err("read the application"); err != nil {
				return nil, err
			}
			app = &found.Value
		}
		var bundle *Result[string]
		if options.Wanted != nil {
			check := &Batch{}
			bundle = check.BundleID(*app)
			if err := c.Run(ctx, check); err != nil {
				return nil, err
			}
			if err := bundle.Err("read the application's bundle ID"); err != nil {
				return nil, c.silent(ctx, *app, options.PID != 0, err)
			}
			if !options.Wanted(bundle.Value) {
				return &Screen{BundleID: bundle.Value, App: *app}, nil
			}
		}
		second := &Batch{}
		reads := addApplicationReads(second, *app, options, bundle)
		if err := c.Run(ctx, second); err != nil {
			return nil, err
		}
		return c.applicationScreen(ctx, *app, options, reads)
	}
	if options.PID != 0 {
		return read()
	}
	return untilAnswered(read)
}

// readGuessed reads the frontmost application and, in the same round, the
// application guessed to be it. axauditd answers a round's requests in order
// (see exchange), so the guessed one's tree is kept only when the frontmost
// application, read first, is the guessed one; otherwise it answers that
// application to read instead. SpringBoard's tree with labels came in 192 to
// 224 ms that way, and in 257 to 524 ms in two rounds; its application
// element kept its object ID across reads (iPhone18,2, iOS 27.0.1 24A446 over
// Wi-Fi).
func (c *RemoteClient) readGuessed(ctx context.Context, options ApplicationOptions, guess *Front) (*Screen, *Element, error) {
	batch := &Batch{}
	front := batch.Frontmost()
	wanted := options.Wanted == nil || options.Wanted(guess.BundleID)
	var reads *applicationReads
	if wanted {
		reads = addApplicationReads(batch, guess.App, options, nil)
	}
	if err := c.Run(ctx, batch); err != nil {
		return nil, nil, err
	}
	if err := front.Err("read the application"); err != nil {
		return nil, nil, err
	}
	if !front.Value.object.same(guess.App.object) {
		if front.Value.Ref.PID == guess.App.Ref.PID {
			// The same application under another object: guesses would keep
			// missing on this connection.
			c.guessMissed.Store(true)
		}
		return nil, &front.Value, nil
	}
	if !wanted {
		return &Screen{BundleID: guess.BundleID, App: front.Value}, nil, nil
	}
	screen, err := c.applicationScreen(ctx, front.Value, options, reads)
	return screen, nil, err
}

type applicationReads struct {
	bundle    *Result[string]
	full      *Result[*Snapshot]
	modal     *Result[*Snapshot]
	focused   *Result[Element]
	animating *Result[bool]
}

func addApplicationReads(batch *Batch, app Element, options ApplicationOptions, bundle *Result[string]) *applicationReads {
	attributes := options.Attributes
	if options.Expand && len(attributes) > 0 && !slices.Contains(attributes, nativeClassName) {
		attributes = append(slices.Clone(attributes), nativeClassName)
	}
	reads := &applicationReads{bundle: bundle}
	if reads.bundle == nil {
		reads.bundle = batch.BundleID(app)
	}
	reads.full = batch.Snapshot(app, SnapshotOptions{Attributes: attributes})
	if options.Modal {
		reads.modal = batch.Snapshot(app, SnapshotOptions{Attributes: []int64{nativeClassName}, HonorModal: true})
	}
	if options.Focused {
		reads.focused = batch.Focused(app)
	}
	if options.Animations {
		reads.animating = batch.AnimationsInProgress(app)
	}
	return reads
}

func (c *RemoteClient) applicationScreen(ctx context.Context, app Element, options ApplicationOptions, reads *applicationReads) (*Screen, error) {
	if err := reads.bundle.Err("read the application's bundle ID"); err != nil {
		return nil, c.silent(ctx, app, options.PID != 0, err)
	}
	if err := reads.full.Err("snapshot the application"); err != nil {
		return nil, c.silent(ctx, app, options.PID != 0, err)
	}
	screen := &Screen{BundleID: reads.bundle.Value, App: app, Root: reads.full.Value.Root}
	if reads.modal != nil {
		if err := reads.modal.Err("snapshot the application's modal view"); err != nil {
			return nil, err
		}
		markInert(reads.full.Value.Root, reads.modal.Value.Listed, false)
	}
	if reads.focused != nil && reads.focused.Code == 0 {
		ref := reads.focused.Value.Ref
		screen.Focused = &ref
	}
	if reads.animating != nil {
		screen.Animating = reads.animating.Code != 0 || reads.animating.Value
	}
	if options.Expand {
		run := func(batch *Batch) error { return c.Run(ctx, batch) }
		if err := c.expand(run, []*Snapshot{reads.full.Value}, 0); err != nil {
			return nil, err
		}
	}
	return screen, nil
}

type SilentFrontmostError struct {
	PID uint64
	err error
}

func (e *SilentFrontmostError) Error() string {
	return e.err.Error()
}

func (e *SilentFrontmostError) Unwrap() error {
	return e.err
}

func (c *RemoteClient) silent(ctx context.Context, app Element, check bool, err error) error {
	if !errors.Is(err, ErrNoAnswer) {
		return err
	}
	if check {
		batch := &Batch{}
		front := batch.Frontmost()
		if c.Run(ctx, batch) != nil || front.Code != 0 || front.Value.Ref.PID != app.Ref.PID {
			return err
		}
	}
	return &SilentFrontmostError{PID: app.Ref.PID, err: err}
}

// Behind a UIKit alert, the snapshot that honors modal views kept the alert's
// 43 of 134 nodes (iPad17,1, iPadOS 27.2 24B115).
func markInert(node *TreeNode, listed map[ElementRef]bool, inert bool) {
	node.Inert = inert || !listed[node.Ref]
	for _, child := range node.Children {
		markInert(child, listed, node.Inert)
	}
}

type remoteProbe struct {
	node   *TreeNode
	result *Result[*Snapshot]
}

// SpringBoard's remote elements are not read: with Control Center open,
// reading its 5 remote elements took 1.4 s and added nothing to the controls
// its own snapshot held (iPad17,1, iPadOS 27.2 24B115), and while another
// application is in front, one hosts that application's scene.
func (c *RemoteClient) expand(run func(*Batch) error, snapshots []*Snapshot, springboard uint64) error {
	probes := &Batch{}
	var remotes []remoteProbe
	for _, snapshot := range snapshots {
		if snapshot.Root.Ref.PID == springboard {
			continue
		}
		for _, node := range snapshot.Remote {
			if !node.Inert {
				remotes = append(remotes, remoteProbe{node, probes.Snapshot(node.Element, SnapshotOptions{Attributes: []int64{nativeClassName}, MaxDepth: remoteProbeDepth})})
			}
		}
	}
	if err := run(probes); err != nil {
		return err
	}
	contents := &Batch{}
	type attachment struct {
		parent *TreeNode
		result *Result[*Snapshot]
	}
	var attachments []attachment
	var webs []*webRead
	for _, probe := range remotes {
		if probe.result.Code != 0 {
			continue
		}
		if area := webArea(probe.result.Value.Root); area != nil {
			webs = append(webs, &webRead{host: probe.node, area: area.Element, search: contents.Search(area.Element, webSearch)})
			continue
		}
		attachments = append(attachments, attachment{probe.node, contents.Snapshot(probe.node.Element, SnapshotOptions{})})
	}
	if err := run(contents); err != nil {
		return err
	}
	for _, item := range attachments {
		if item.result.Code == 0 {
			item.parent.Children = item.result.Value.Root.Children
		}
	}
	return c.readWeb(run, webs)
}

// The snapshot of Safari's remote element answers a
// WKAccessibilityWebPageObject under it, whose child, a
// WebAccessibilityObjectWrapper, holds the page (iPhone18,2, iOS 27.2 24B115).
func webArea(node *TreeNode) *TreeNode {
	if node.Class == webPageClass {
		for _, child := range node.Children {
			if child.Class == webObjectClass {
				return child
			}
		}
		return nil
	}
	for _, child := range node.Children {
		if area := webArea(child); area != nil {
			return area
		}
	}
	return nil
}

type webRead struct {
	host    *TreeNode
	area    Element
	search  *Result[[]Element]
	order   []ElementRef
	nodes   map[ElementRef]*TreeNode
	parent  map[ElementRef]ElementRef
	missing map[ElementRef]Element
}

func (c *RemoteClient) readWeb(run func(*Batch) error, webs []*webRead) error {
	batch := &Batch{}
	var reads []*Result[loneNode]
	var owners []*webRead
	for _, web := range webs {
		if web.search.Code != 0 {
			continue
		}
		web.nodes, web.parent, web.missing = make(map[ElementRef]*TreeNode), make(map[ElementRef]ElementRef), make(map[ElementRef]Element)
		for _, element := range append([]Element{web.area}, web.search.Value...) {
			if _, seen := web.nodes[element.Ref]; seen {
				continue
			}
			web.nodes[element.Ref] = nil
			web.order = append(web.order, element.Ref)
			reads = append(reads, batch.lone(element))
			owners = append(owners, web)
		}
	}
	for len(batch.requests) > 0 {
		if err := run(batch); err != nil {
			return err
		}
		for index, read := range reads {
			owners[index].add(read)
		}
		batch, reads, owners = &Batch{}, nil, nil
		for _, web := range webs {
			for ref, element := range web.missing {
				delete(web.missing, ref)
				web.nodes[ref] = nil
				reads = append(reads, batch.lone(element))
				owners = append(owners, web)
			}
		}
	}
	for _, web := range webs {
		if web.nodes != nil {
			web.assemble()
		}
	}
	return nil
}

func (w *webRead) add(read *Result[loneNode]) {
	if read.Code != 0 || read.Value.node.Class == webPageClass {
		return
	}
	node, parent := read.Value.node, read.Value.parent
	if w.nodes[node.Ref] == nil {
		w.nodes[node.Ref] = node
	}
	if node.Ref == w.area.Ref || parent == nil || parent.Class == webPageClass {
		return
	}
	w.parent[node.Ref] = parent.Ref
	if _, known := w.nodes[parent.Ref]; !known {
		w.nodes[parent.Ref] = parent
		w.order = append(w.order, parent.Ref)
	}
	if parent.Ref == w.area.Ref || read.Value.grandparent == nil {
		return
	}
	grandparent := *read.Value.grandparent
	w.parent[parent.Ref] = grandparent.Ref
	if _, known := w.nodes[grandparent.Ref]; !known {
		w.missing[grandparent.Ref] = grandparent
	}
}

func (w *webRead) assemble() {
	root := w.nodes[w.area.Ref]
	if root == nil {
		return
	}
	root.Web = true
	if hasArea(root.VisibleFrame) {
		root.Clip = *root.VisibleFrame
	}
	attached := map[ElementRef]bool{w.area.Ref: true}
	var attach func(ref ElementRef) *TreeNode
	attach = func(ref ElementRef) *TreeNode {
		node := w.nodes[ref]
		if node == nil || attached[ref] {
			return node
		}
		attached[ref] = true
		parentRef, found := w.parent[ref]
		parent := root
		if found && parentRef != w.area.Ref {
			if candidate := attach(parentRef); candidate != nil {
				parent = candidate
			}
		}
		node.Web = true
		node.Clip = parent.Clip
		if hasArea(node.VisibleFrame) {
			node.Clip = *node.VisibleFrame
		}
		parent.Children = append(parent.Children, node)
		return node
	}
	for _, ref := range w.order {
		attach(ref)
	}
	w.host.Children = append(w.host.Children, root)
}

type loneNode struct {
	node        *TreeNode
	parent      *TreeNode
	grandparent *Element
}

var loneAttributes = slices.Concat(snapshotAttributes, []any{int64(nativeParent)})

// In a snapshot, an attribute whose value is an element, such as the parent
// (native 5002), comes as that element with its attributes; an element among
// those attributes comes only as a reference (24B5110a UIAccessibility
// __fetchAttributesIfNecessary 0x1bbe141e8, 0x1bbe14260; iPhone18,2, iOS 27.2
// 24B115).
func (b *Batch) lone(element Element) *Result[loneNode] {
	parameter := map[string]any{
		"attributes": loneAttributes, "maxDepth": int64(0), "maxChildren": int64(math.MaxInt32), "maxArrayCount": int64(math.MaxInt32),
		"traverseFromParentsToChildren": true, "snapshotKeyHonorModalViews": false,
	}
	result := &Result[loneNode]{}
	b.add(remoteRequest{kind: requestAttribute, object: &element.object, attribute: attributeUserTestingSnapshot, params: map[string]any{"parameter": parameter}},
		func(response remoteResponse, _ *Batch) error {
			result.Code = response.code
			if result.Code != 0 {
				return nil
			}
			// While an app launched, the snapshot of a web element its search had
			// just found answered nothing (iPhone18,2, iOS 27.2 24B115).
			if response.value == nil {
				result.Code = errorNoElement
				return nil
			}
			node, _, values, err := snapshotNode(response.value)
			if err != nil {
				return err
			}
			result.Value.node = node
			if values[uint64(nativeParent)] == nil {
				return nil
			}
			parent, _, parentValues, err := snapshotNode(values[uint64(nativeParent)])
			if err != nil {
				return err
			}
			result.Value.parent = parent
			if grandparent, ok := parseTranslation(parentValues[uint64(nativeParent)]); ok {
				element := grandparent.element()
				result.Value.grandparent = &element
			}
			return nil
		})
	return result
}
