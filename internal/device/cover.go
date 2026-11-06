package device

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/swipt-automation/swipt/internal/coredevice/accessibility"
)

// An application's own bars float over its content: a tap on a Settings row
// whose accessibility frame lay under the search bar at the bottom did
// nothing, though the accessibility hit test, which walks accessibility
// elements rather than views, answered the row (iPhone18,2, iOS 27.0.1). A
// tap keeps its element's activation point when that lies coverClearance from
// every cover, and otherwise aims coverMargin clear of them; it is refused
// when less than coverShown of the element shows. A view as large as
// coverLargest of the screen is a backdrop, or a container that passes
// touches through, rather than a bar.
const (
	coverMargin    = 8.0
	coverClearance = 6.0
	coverShown     = 0.5
	coverLargest   = 0.5
)

type cover struct {
	node *PhoneNode
	rect Rect
}

func (r Rect) area() float64 {
	if r.Empty() {
		return 0
	}
	return r.Width * r.Height
}

func (r Rect) inset(by float64) Rect {
	return Rect{r.X + by, r.Y + by, r.Width - 2*by, r.Height - 2*by}
}

// overlaps holds for rects that share at least a point square, so neighbours
// that touch or overlap by rounding do not cover each other.
func (r Rect) overlaps(other Rect) bool {
	shared := r.Intersect(other)
	return shared.Width >= 1 && shared.Height >= 1
}

// subtract cuts the rects out of r, leaving disjoint pieces.
func (r Rect) subtract(cuts []Rect) []Rect {
	pieces := []Rect{r}
	for _, cut := range cuts {
		var next []Rect
		for _, piece := range pieces {
			shared := piece.Intersect(cut)
			if shared.Empty() {
				next = append(next, piece)
				continue
			}
			next = append(next,
				Rect{piece.X, piece.Y, piece.Width, shared.Y - piece.Y},
				Rect{piece.X, shared.Y + shared.Height, piece.Width, piece.Y + piece.Height - shared.Y - shared.Height},
				Rect{piece.X, shared.Y, shared.X - piece.X, shared.Height},
				Rect{shared.X + shared.Width, shared.Y, piece.X + piece.Width - shared.X - shared.Width, shared.Height},
			)
		}
		pieces = slices.DeleteFunc(next, Rect.Empty)
	}
	return pieces
}

// distance is how far at lies from r, zero inside it.
func (r Rect) distance(at Point) float64 {
	dx := max(r.X-at.X, 0, at.X-r.X-r.Width)
	dy := max(r.Y-at.Y, 0, at.Y-r.Y-r.Height)
	return math.Hypot(dx, dy)
}

// depth is how far at lies inside r.
func (r Rect) depth(at Point) float64 {
	return min(at.X-r.X, r.X+r.Width-at.X, at.Y-r.Y, r.Y+r.Height-at.Y)
}

func clearance(at Point, covers []cover) float64 {
	nearest := math.Inf(1)
	for _, it := range covers {
		nearest = min(nearest, it.rect.distance(at))
	}
	return nearest
}

func (n *PhoneNode) holds(other *PhoneNode) bool {
	return n.order <= other.order && other.order < n.end
}

func scrolls(ax accessibility.Element) bool {
	return slices.Contains(scrollingRoles, roleName(ax))
}

func roleName(ax accessibility.Element) string {
	if kind := elementType(ax.Type); kind < uint64(len(roles)) {
		return roles[kind]
	}
	return "other"
}

// uncover marks what covers each shown element of the application: the
// keyboard and SpringBoard's elements given in over, and the application's
// own elements in the same window that do not scroll with it. An element in a
// scroll view is covered by such elements outside it that come after it, as
// a floating search bar or tab bar does, or that belong to a bar, as the
// navigation bar does, which is listed first; views listed before the scroll
// view and in no bar lie behind it, as the pins of a map behind a sheet. An
// element in no scroll view is covered by unscrolled elements that come after
// it, which are drawn above it. It then moves each element's point clear of
// its covers.
func (m *PhoneModel) uncover(over []cover, input *PhoneNode) {
	var nodes []*PhoneNode
	var barred []bool
	var index func(node, scroll, window *PhoneNode, bar bool)
	index = func(node, scroll, window *PhoneNode, bar bool) {
		if node.AX.Type == windowAutomationType {
			window = node
		}
		bar = bar || slices.Contains(barRoles, roleName(node.AX))
		node.order, node.scroll, node.win = len(nodes), scroll, window
		nodes, barred = append(nodes, node), append(barred, bar)
		if scrolls(node.AX) {
			scroll = node
		}
		for _, child := range node.Children {
			index(child, scroll, window, bar)
		}
		node.end = len(nodes)
		if bar {
			for parent := node.Parent; parent != nil && !barred[parent.order]; parent = parent.Parent {
				barred[parent.order] = true
			}
		}
	}
	index(m.Root, nil, nil, false)
	inKeyboard := func(node *PhoneNode) bool { return input != nil && input.holds(node) }
	geometry := m.Root.space.geometry
	largest := geometry.Width * geometry.Height * coverLargest
	var candidates, unscrolled []*PhoneNode
	for _, node := range nodes {
		if node.Parent != nil && node.AX.Type != windowAutomationType && node.Shown() && !inKeyboard(node) &&
			node.Visible.Width >= 1 && node.Visible.Height >= 1 && node.Visible.area() < largest {
			candidates = append(candidates, node)
			if node.scroll == nil {
				unscrolled = append(unscrolled, node)
			}
		}
	}
	fixed := make(map[*PhoneNode][]*PhoneNode)
	for _, node := range nodes {
		if !node.Shown() || inKeyboard(node) {
			continue
		}
		var covers []cover
		for _, it := range over {
			if it.rect.overlaps(node.Visible) {
				covers = append(covers, it)
			}
		}
		add := func(it *PhoneNode) {
			if it.Visible.overlaps(node.Visible) && (!it.Inert || node.Inert) {
				covers = append(covers, cover{node: it, rect: it.Visible})
			}
		}
		if scroll := node.scroll; scroll != nil {
			list, known := fixed[scroll]
			if !known {
				list = scroll.fixedOver(candidates, barred)
				fixed[scroll] = list
			}
			for _, it := range list {
				add(it)
			}
		} else {
			later, _ := slices.BinarySearchFunc(unscrolled, node.end, func(it *PhoneNode, order int) int { return it.order - order })
			for _, it := range unscrolled[later:] {
				if it.win == node.win {
					add(it)
				}
			}
		}
		node.place(covers)
	}
}

var barRoles = []string{"navigationBar", "tabBar", "toolbar", "searchField"}

// fixedOver lists the candidates in the scroll view's window that overlap it,
// stay put while it scrolls, and are drawn above it.
func (n *PhoneNode) fixedOver(candidates []*PhoneNode, barred []bool) []*PhoneNode {
	var out []*PhoneNode
	for _, it := range candidates {
		if it.win == n.win && !n.holds(it) && !it.holds(n) && (it.scroll == nil || it.scroll.holds(n)) &&
			(it.order >= n.end || barred[it.order]) && it.Visible.overlaps(n.Visible) {
			out = append(out, it)
		}
	}
	return out
}

// place keeps the element's point when it is clear of every cover, and
// otherwise moves it to the middle of the uncovered piece of the element that
// lies farthest from the covers and the element's edges, marking the element
// covered instead when too little of it shows.
func (n *PhoneNode) place(covers []cover) {
	if len(covers) == 0 || clearance(n.Point, covers) >= coverClearance {
		return
	}
	n.overlapped = true
	cuts := func(margin float64) []Rect {
		out := make([]Rect, len(covers))
		for index, it := range covers {
			out[index] = it.rect.inset(-margin)
		}
		return out
	}
	open := n.Visible.subtract(cuts(0))
	shown := 0.0
	for _, piece := range open {
		shown += piece.area()
	}
	n.shown = shown / n.Visible.area()
	pieces := n.Visible.subtract(cuts(coverMargin))
	if len(pieces) == 0 {
		pieces = open
	}
	if n.shown < coverShown || len(pieces) == 0 {
		n.Covered = true
		n.cover = largestCover(covers, n.Visible)
		return
	}
	best, score := n.Point, math.Inf(-1)
	for _, piece := range pieces {
		at := piece.Center()
		if value := min(clearance(at, covers), n.Visible.depth(at)); value > score {
			best, score = at, value
		}
	}
	n.Point = best
}

// largestCover names the cover over most of the element, preferring one with
// a label to the unlabeled container holding it.
func largestCover(covers []cover, of Rect) *PhoneNode {
	var out *PhoneNode
	area, labeled := -1.0, false
	for _, it := range covers {
		named := it.node.AX.Label != "" || it.node.AX.Identifier != ""
		if shared := it.rect.Intersect(of).area(); named && !labeled || named == labeled && shared > area {
			out, area, labeled = it.node, shared, named
		}
	}
	return out
}

// coveredInApp tells whether the element is covered by the application's own
// elements, which stay where they are, rather than by the keyboard or by
// SpringBoard.
func (n *PhoneNode) coveredInApp() bool {
	return n.Covered && n.cover != nil && n.model.Root.Contains(n.cover) && elementType(n.cover.AX.Type) != typeKeyboard
}

func (n *PhoneNode) coverText() string {
	cover := n.cover
	switch {
	case cover == nil:
		return "another element"
	case elementType(cover.AX.Type) == typeKeyboard:
		return "the keyboard"
	}
	if modal := cover.modal(); modal != nil {
		return interruption(modal)
	}
	return describeNode(cover)
}

// describeNode names an element by its role and label, such as the search
// field "Search", or by its class when it has no label.
func describeNode(node *PhoneNode) string {
	role := roleWords(roleName(node.AX))
	ax := node.AX
	switch {
	case ax.Label != "":
		return "the " + role + " " + strconv.Quote(clipText(PlainText(ax.Label)))
	case ax.Identifier != "":
		return "the " + role + " with the identifier " + strconv.Quote(ax.Identifier)
	case ax.Class != "":
		return "an unlabeled " + role + " (" + ax.Class + ")"
	}
	return "an unlabeled " + role
}

func roleWords(role string) string {
	switch role {
	case "other", "element", "group":
		return "view"
	}
	var out strings.Builder
	for index, r := range role {
		if unicode.IsUpper(r) {
			if index > 0 {
				out.WriteByte(' ')
			}
			r = unicode.ToLower(r)
		}
		out.WriteRune(r)
	}
	return out.String()
}

const interruptionText = 120

func clipText(text string) string {
	runes := []rune(strings.Join(strings.Fields(text), " "))
	if len(runes) <= interruptionText {
		return string(runes)
	}
	return string(runes[:interruptionText-1]) + "…"
}
