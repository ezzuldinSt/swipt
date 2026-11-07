package accessibility

import "strconv"

// __AXPNotificationToString's table (24B5110a AccessibilityPlatformTranslation
// 0x2693d7de0).
var notificationNames = map[uint64]string{
	1: "Announcement", 2: "ElementDestroyed", 3: "FocusChanged", 4: "LayoutChanged", 5: "ScreenChanged",
	6: "SelectionChanged", 7: "UserTestingNotification", 8: "ValueChanged", 9: "SelectionChanged",
	10: "WebPageLoaded", 11: "Alert", 12: "ElementScrolledByPage", 13: "TextInputMarkingSessionBegan",
	14: "TextInputMarkingSessionEnded", 15: "UpdateElementVisuals",
}

func NotificationName(notification uint64) string {
	if name, found := notificationNames[notification]; found {
		return name
	}
	return strconv.FormatUint(notification, 10)
}

type Push struct {
	Notification uint64
	PID          uint64
	Tree         []PushedNode
	Frontmost    *Element
}

type PushedNode struct {
	PID        uint64
	Label      string
	Identifier string
	Value      string
}

func pushedNotification(notification uint64, response map[string]any) Push {
	push := Push{Notification: notification}
	if obj, ok := parseTranslation(response["associatedNotificationObject"]); ok {
		push.PID = obj.pid
	} else if obj, ok := parseTranslation(response["resultData"]); ok {
		push.PID = obj.pid
	}
	return push
}

// A tree push holds every accessibility element of the application in front
// with its attributes, not what changed: axauditd pushed an
// AXPTreeDumpTypeInitialDump, then 0.2-0.5 s later an
// AXPTreeDumpTypeAdditionalData of the same elements with more attributes and
// their actions, each time an application in use changed: of 71 pushes, one
// repeated the one before of its kind, the others moved frames or added and
// removed elements. Over five minutes of that it sent no layout, screen, value
// or focus notification, only announcements, so no push names the elements to
// read again (iPhone18,2, iOS 27.0.1 24A446).
func pushedTree(value any) Push {
	data, _ := value.(map[string]any)
	tree, _ := data["treeDump"].([]any)
	var nodes []PushedNode
	var frontmost *Element
	index := make(map[[2]uint64]int)
	for _, item := range tree {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := unsigned(entry["associatedRequestType"]); kind == requestFrontmost {
			if app, ok := parseTranslation(entry["resultData"]); ok && app.application {
				front := app.element()
				frontmost = &front
			}
			continue
		}
		obj, _ := parseTranslation(entry["associatedTranslationObject"])
		values, _ := entry["resultData"].(map[any]any)
		if attribute, _ := unsigned(entry["attribute"]); attribute != 0 {
			values = map[any]any{attribute: entry["resultData"]}
		}
		key := [2]uint64{obj.pid, obj.objectID}
		at, found := index[key]
		if !found {
			at = len(nodes)
			index[key] = at
			nodes = append(nodes, PushedNode{PID: obj.pid})
		}
		node := &nodes[at]
		for attribute, field := range map[uint64]*string{attributeLabel: &node.Label, attributeIdentifier: &node.Identifier, attributeValue: &node.Value} {
			if text, ok := values[attribute].(string); ok {
				*field = text
			}
		}
	}
	return Push{Tree: nodes, Frontmost: frontmost}
}
