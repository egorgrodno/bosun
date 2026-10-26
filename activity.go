package main

import (
	"encoding/json"
	"strings"
	"time"
)

// Activity is what a session did last: the tool it called with that call's main argument, or with no call yet in the turn, the start of its last assistant message in Text.
// A frontend picks its glyph from Tool, so the daemon sends the two apart.
type Activity struct {
	Tool string    `json:"tool,omitempty"`
	Arg  string    `json:"arg,omitempty"`
	Text string    `json:"text,omitempty"`
	At   time.Time `json:"at"`
}

// activityWindow is how long a session's activity changes are held back before the frontends hear of the latest, so a burst of tool calls sends one event per window.
var activityWindow = 500 * time.Millisecond

// activityWidth caps Arg and Text in runes.
const activityWidth = 200

// activityKeys are the fields of a tool's input that hold its main argument, in the order tried.
var activityKeys = []string{"command", "file_path", "notebook_path", "path", "url", "pattern", "query", "description", "name", "prompt"}

// activityOf reads the activity of a PreToolUse, which is empty when the call has no tool name.
func activityOf(tool string, input json.RawMessage) Activity {
	fields := map[string]any{}
	_ = json.Unmarshal(input, &fields)
	a := Activity{Tool: tool}
	for _, k := range activityKeys {
		if s, ok := fields[k].(string); ok && strings.TrimSpace(s) != "" {
			a.Arg = oneLine(s, activityWidth)
			break
		}
	}
	return a
}

// oneLine collapses s to one line of at most width runes, cut with an ellipsis.
func oneLine(s string, width int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > width {
		return string(r[:width-1]) + "…"
	}
	return s
}

// setActivityLocked records a as the latest activity of n, and tells the frontends at once if none heard of one within the window, or else when the window ends.
func (d *daemon) setActivityLocked(n *Session, a Activity) {
	if prev := n.Activity; prev != nil && prev.Tool == a.Tool && prev.Arg == a.Arg && prev.Text == a.Text {
		return
	}
	a.At = time.Now()
	n.Activity = &a
	if n.actTimer != nil {
		return
	}
	if wait := activityWindow - time.Since(n.actSent); wait > 0 {
		n.actTimer = time.AfterFunc(wait, func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			n.actTimer = nil
			if d.nodes[n.Node] == n {
				d.sendActivityLocked(n)
			}
		})
		return
	}
	d.sendActivityLocked(n)
}

// activityLine is how a frontend without glyphs shows activity a: the tool and its argument, or the start of the last message.
func activityLine(a *Activity) string {
	if a == nil {
		return ""
	}
	if a.Tool != "" {
		return strings.TrimSpace(a.Tool + " " + a.Arg)
	}
	return a.Text
}

func (d *daemon) sendActivityLocked(n *Session) {
	n.actSent = time.Now()
	d.broadcastLocked(Event{Kind: "activity", Node: n.Node, Activity: n.Activity})
}
