package device

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/swipt-automation/swipt/internal/coredevice/accessibility"
)

// The AXP notification an alert's appearance posts (24B5110a
// AccessibilityPlatformTranslation __AXPNotificationToString 0x2693d7de0).
const notificationAlert = 11

const (
	interruptionButtons  = 8
	interruptionMessages = 2
)

var modalRoles = []string{"alert", "sheet", "dialog", "popover"}

// modal finds the alert, sheet, dialog or popover holding the element.
func (n *PhoneNode) modal() *PhoneNode {
	for node := n; node != nil; node = node.Parent {
		if slices.Contains(modalRoles, roleName(node.AX)) {
			return node
		}
	}
	return nil
}

func isButton(node *PhoneNode) bool {
	return elementType(node.AX.Type) == typeButton || node.AX.Traits&1 != 0
}

// interruption describes an alert or another modal view by its title, its
// message and the labels of its buttons, so that a script can tell what is in
// the way and answer it itself.
func interruption(container *PhoneNode) string {
	var texts, buttons []string
	walkNodes(container, func(node *PhoneNode) {
		label := clipText(PlainText(node.AX.Label))
		if node == container || label == "" || !node.AX.AccessibilityElement || node.HasFrame() && !node.Shown() {
			return
		}
		if isButton(node) {
			if !slices.Contains(buttons, label) {
				buttons = append(buttons, label)
			}
		} else if !slices.Contains(texts, label) {
			texts = append(texts, label)
		}
	})
	// The application element, which stands for a modal view the snapshot
	// alone holds, is labeled with the application's name.
	var title string
	if container.Parent != nil {
		title = clipText(PlainText(container.AX.Label))
	}
	if title == "" && len(texts) > 0 {
		title, texts = texts[0], texts[1:]
	}
	texts = slices.DeleteFunc(texts, func(text string) bool { return text == title })
	var out strings.Builder
	switch role := roleName(container.AX); role {
	case "alert":
		out.WriteString("an alert")
	case "sheet", "dialog", "popover":
		out.WriteString("a " + role)
	default:
		out.WriteString("a modal view")
	}
	if title != "" {
		out.WriteString(" " + strconv.Quote(title))
	}
	if len(texts) > 0 {
		out.WriteString(" saying " + quoted(texts[:min(len(texts), interruptionMessages)]))
	}
	switch {
	case len(buttons) == 1:
		out.WriteString(" with the button " + strconv.Quote(buttons[0]))
	case len(buttons) > 0:
		shown := buttons[:min(len(buttons), interruptionButtons)]
		out.WriteString(" with the buttons " + quoted(shown))
		if len(buttons) > len(shown) {
			out.WriteString(" and " + strconv.Itoa(len(buttons)-len(shown)) + " more")
		}
	}
	return out.String()
}

func quoted(texts []string) string {
	parts := make([]string, len(texts))
	for index, text := range texts {
		parts[index] = strconv.Quote(text)
	}
	return strings.Join(parts, ", ")
}

// blocker is what kept an action from its element: an element of another view
// in front of it, or the modal view of a process.
type blocker struct {
	pid   uint64
	ref   *accessibility.ElementRef
	modal bool
}

// interrupted reads the screen again to describe the alert, sheet or other
// modal view that kept an action from its element, with the labels its read
// may have left out, and answers "" when nothing of the kind is in the way.
// It never presses any of its buttons.
func (p *Phone) interrupted(ctx context.Context, c *conn, geometry phoneGeometry, found *blocker, alerts []uint64) string {
	var asked []blocker
	if found != nil {
		asked = append(asked, *found)
	}
	for _, pid := range alerts {
		if !slices.ContainsFunc(asked, func(it blocker) bool { return it.pid == pid }) {
			asked = append(asked, blocker{pid: pid})
		}
	}
	if len(asked) == 0 || ctx.Err() != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, interruptionLimit)
	defer cancel()
	var out string
	err := c.ax.do(ctx, false, 0, func(ctx context.Context, client *accessibility.RemoteClient) error {
		for _, it := range asked {
			text, err := describeBlocker(ctx, client, geometry, it)
			if err != nil || text != "" {
				out = text
				return err
			}
		}
		return nil
	})
	if err != nil {
		p.logger.Debug("describe what is in the way: ", err)
		return ""
	}
	return out
}

// The read that explains a refusal gives up after this, so that the refusal
// still comes promptly when the process in the way does not answer.
const interruptionLimit = 2 * time.Second

func describeBlocker(ctx context.Context, client *accessibility.RemoteClient, geometry phoneGeometry, it blocker) (string, error) {
	first := &accessibility.Batch{}
	app := first.Application(it.pid)
	if err := client.Run(ctx, first); err != nil {
		return "", err
	}
	if app.Code != 0 {
		return "", nil
	}
	second := &accessibility.Batch{}
	snapshot := second.Snapshot(app.Value, accessibility.SnapshotOptions{Depth: accessibility.DefaultDepth, HonorModal: it.modal})
	if err := client.Run(ctx, second); err != nil {
		return "", err
	}
	if snapshot.Code != 0 {
		return "", nil
	}
	model := modelOf(geometry, &accessibility.Screen{App: app.Value, Root: snapshot.Value.Root}, nil)
	var container *PhoneNode
	switch {
	case it.ref != nil:
		if node := model.Node(*it.ref); node != nil {
			container = node.modal()
		}
	case it.modal:
		// The snapshot that honors modal views holds only the modal view.
		container = model.Root
		if role := model.firstModal(); role != nil {
			container = role
		}
	default:
		container = model.firstModal()
	}
	if container == nil {
		return "", nil
	}
	return interruption(container), nil
}

func (m *PhoneModel) firstModal() *PhoneNode {
	var out *PhoneNode
	walkNodes(m.Root, func(node *PhoneNode) {
		if out == nil && node.Shown() && slices.Contains(modalRoles, roleName(node.AX)) {
			out = node
		}
	})
	return out
}
