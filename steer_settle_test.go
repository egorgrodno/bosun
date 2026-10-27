package main

import (
	"strings"
	"testing"
	"time"
)

// A turn that ended on a backgrounded command reads idle until a hook of a new turn or the listing between turns, however long the command runs.
func TestSettledHoldsPastTurnSettle(t *testing.T) {
	d, fc, _ := steerDaemon(t, "busy")
	status := func() string {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, s := range d.snapshotLocked().Sessions {
			if s.Node == "a" {
				return s.Status
			}
		}
		return ""
	}
	receive(t, runHookAsync("a", hookEvent("PreToolUse", "t1", "Bash", map[string]any{"command": "sleep 40", "run_in_background": true})))
	receive(t, runHookAsync("a", hookEvent("Stop", "", "", nil)))
	d.mu.Lock()
	d.nodes["a"].turnAt = time.Now().Add(-6 * turnSettle)
	d.nodes["a"].stopAt = time.Now().Add(-5 * turnSettle)
	d.mu.Unlock()
	d.poll()
	if got := status(); got != "idle" {
		t.Fatalf("status long after Stop is %q", got)
	}
	if resp := mustCall(t, Request{Op: "say", Name: "a", Text: "pong"}); !strings.Contains(resp.Text, "takes it as a new turn") {
		t.Fatalf("say answered %q", resp.Text)
	}
	// The listing between turns ends the override, so a busy listing after it is a turn.
	fc.setStatus("aaaaaaaa", "idle")
	d.poll()
	fc.setStatus("aaaaaaaa", "busy")
	d.poll()
	if got := status(); got != "busy" {
		t.Fatalf("status after the listing went idle and busy is %q", got)
	}
	// A hook of a new turn ends it too.
	receive(t, runHookAsync("a", hookEvent("Stop", "", "", nil)))
	if got := status(); got != "idle" {
		t.Fatalf("status after a second Stop is %q", got)
	}
	receive(t, runHookAsync("a", hookEvent("UserPromptSubmit", "", "", nil)))
	if got := status(); got != "busy" {
		t.Fatalf("status in the notification's turn is %q", got)
	}
}
