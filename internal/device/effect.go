package device

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/coredevice/accessibility"
)

// Words of a label whose action cannot be taken back or reaches outside the
// device. A tap on such an element is never repeated on its own: a retry
// after a tap that showed nothing could send, pay or delete twice.
var riskyWords = []string{
	"send", "pay", "buy", "purchase", "order", "checkout", "subscribe", "unsubscribe", "subscription", "donate", "transfer",
	"delete", "remove", "erase", "reset", "wipe", "discard", "trash", "clear", "forget", "restore", "install", "uninstall",
	"confirm", "allow", "approve", "accept", "agree", "submit", "publish", "post", "share", "reply", "upload",
	"call", "dial", "facetime", "block", "report", "sign out", "log out", "sign off", "shut down", "restart", "power off",
}

// riskyWord answers the risky word or phrase of the element's label or
// identifier, or "".
func riskyWord(element accessibility.Element) string {
	for _, text := range []string{element.Label, element.Identifier} {
		words := strings.FieldsFunc(strings.ToLower(PlainText(splitCamel(text))), func(r rune) bool {
			return !unicode.IsLetter(r)
		})
		if len(words) == 0 {
			continue
		}
		joined := " " + strings.Join(words, " ") + " "
		for _, risky := range riskyWords {
			if strings.Contains(joined, " "+risky+" ") {
				return risky
			}
		}
	}
	return ""
}

// splitCamel spaces the words of an identifier such as deleteButton.
func splitCamel(text string) string {
	var out strings.Builder
	var previous rune
	for _, r := range text {
		if unicode.IsUpper(r) && unicode.IsLower(previous) {
			out.WriteByte(' ')
		}
		out.WriteRune(r)
		previous = r
	}
	return out.String()
}

// Mark is what the device had reported when an input went: the screen changes
// it had pushed, the application in front, and the element aimed at with its
// state then.
type Mark struct {
	conn       *conn
	screens    uint64
	front      uint64
	generation uint64
	element    *accessibility.Element
}

// Reaction is what the device reports changed after an input.
type Reaction struct {
	// Where is "target" when the element itself changed, "elsewhere" when the
	// rest of the screen did, and "" when the device reports no change.
	Where  string
	Detail string
}

// MarkScreen marks the screen changes the device has reported, for the
// Reaction to an input that aimed at no element.
func (p *Phone) MarkScreen(ctx context.Context) (Mark, error) {
	c, _, done, err := p.begin(ctx)
	if err != nil {
		return Mark{}, err
	}
	defer done()
	return Mark{conn: c, screens: c.events.screen.Load()}, nil
}

// Reaction waits until the device reports a change of the screen after the
// mark, or until until, and then reads the element marked: whether another
// application came to the front, the element went or moved, or its value or
// selection changed. The device pushes a screen change 0.75 s after the change
// at the soonest, so an input that changed nothing it can see costs the whole
// wait.
func (p *Phone) Reaction(ctx context.Context, mark Mark, until time.Time) (Reaction, error) {
	c, ctx, done, err := p.begin(ctx)
	if err != nil {
		return Reaction{}, err
	}
	defer done()
	if mark.conn != c {
		return Reaction{}, nil
	}
	changed, stop := c.events.watch(true)
	defer stop()
	for c.events.screen.Load() == mark.screens {
		wait := time.Until(until)
		if wait <= 0 {
			break
		}
		timer := time.NewTimer(wait)
		select {
		case <-changed:
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return Reaction{}, ctx.Err()
		}
		timer.Stop()
	}
	pushed := Reaction{}
	if c.events.screen.Load() != mark.screens {
		pushed = Reaction{Where: "elsewhere", Detail: "the device reported a change of the screen"}
	}
	before := mark.element
	if before == nil {
		return pushed, nil
	}
	var front, attributes *accessibility.Result[accessibility.Element]
	var mapped *accessibility.Result[bool]
	err = c.ax.do(ctx, false, mark.generation, func(ctx context.Context, client *accessibility.RemoteClient) error {
		batch := &accessibility.Batch{}
		front, mapped, attributes = batch.Frontmost(), batch.Mapped(*before), batch.Attributes(*before)
		return client.Run(ctx, batch)
	})
	if err != nil {
		return pushed, err
	}
	name := describeElement(*before)
	after := attributes.Value
	switch {
	case front.Code == 0 && mark.front != 0 && front.Value.Ref.PID != mark.front:
		return Reaction{Where: "elsewhere", Detail: "another application came to the front"}, nil
	case mapped.Code == 0 && !mapped.Value:
		return Reaction{Where: "elsewhere", Detail: name + " is gone"}, nil
	case attributes.Code != 0:
	case after.Value != before.Value:
		return Reaction{Where: "target", Detail: "the value of " + name + " changed from " + strconv.Quote(PlainText(before.Value)) +
			" to " + strconv.Quote(PlainText(after.Value))}, nil
	case after.Selected != before.Selected:
		if after.Selected {
			return Reaction{Where: "target", Detail: name + " became selected"}, nil
		}
		return Reaction{Where: "target", Detail: name + " is no longer selected"}, nil
	case !nearFrame(after.Frame, before.Frame):
		return Reaction{Where: "elsewhere", Detail: name + " moved"}, nil
	}
	return pushed, nil
}

// HitElement is the element a hit test reached.
type HitElement struct {
	Label      string `json:"label,omitempty"`
	Value      string `json:"value,omitempty"`
	Identifier string `json:"identifier,omitempty"`
	Role       string `json:"role"`
}

// HitTest answers the element a tap at each point would reach, nil where the
// device names none.
func (p *Phone) HitTest(ctx context.Context, points []Point) ([]*HitElement, error) {
	c, ctx, done, err := p.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	geometry, err := c.readGeometry(ctx)
	if err != nil {
		return nil, err
	}
	for _, at := range points {
		if err = geometry.check(at); err != nil {
			return nil, E.Cause1(ErrInvalid, err)
		}
	}
	out := make([]*HitElement, len(points))
	err = c.ax.do(ctx, false, 0, func(ctx context.Context, client *accessibility.RemoteClient) error {
		batch := &accessibility.Batch{}
		hits := make([]*accessibility.Result[accessibility.Element], len(points))
		for index, at := range points {
			hits[index] = batch.HitTest(geometry.digitizer(at))
		}
		if err := client.Run(ctx, batch); err != nil {
			return err
		}
		read := &accessibility.Batch{}
		attributes := make([]*accessibility.Result[accessibility.Element], len(points))
		asked := false
		for index, hit := range hits {
			if hit.Code == 0 {
				attributes[index], asked = read.Attributes(hit.Value), true
			}
		}
		if !asked {
			return nil
		}
		if err := client.Run(ctx, read); err != nil {
			return err
		}
		for index, answer := range attributes {
			if answer != nil && answer.Code == 0 {
				ax := answer.Value
				out[index] = &HitElement{Label: PlainText(ax.Label), Value: PlainText(ax.Value), Identifier: ax.Identifier, Role: roleOf(ax)}
			}
		}
		return nil
	})
	return out, err
}

func roleOf(ax accessibility.Element) string {
	var names []string
	for bit, name := range traits {
		if name != "" && ax.Traits&(1<<bit) != 0 {
			names = append(names, name)
		}
	}
	return role(ax, false, names)
}
