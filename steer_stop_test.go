package main

import (
	"context"
	"strings"
	"testing"
)

// A turn that ended on a backgrounded call leaves the listing showing the session busy for a moment, but its Stop says it is idle.
func TestSayFollowsStopOverListing(t *testing.T) {
	d, _, _ := steerDaemon(t, "busy")
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
	if got := status(); got != "busy" {
		t.Fatalf("status in its turn is %q", got)
	}
	receive(t, runHookAsync("a", hookEvent("Stop", "", "", nil)))
	if got := status(); got != "idle" {
		t.Fatalf("status after Stop is %q, though the listing shows busy", got)
	}
	resp := mustCall(t, Request{Op: "say", Name: "a", Text: "pong"})
	if !strings.Contains(resp.Text, "takes it as a new turn") {
		t.Fatalf("say answered %q", resp.Text)
	}
	if _, err := call(context.Background(), Request{Op: "interrupt", Name: "a"}); err == nil || !strings.Contains(err.Error(), "not in its turn") {
		t.Fatalf("interrupt of the settled session: %v", err)
	}
	// Its next turn begins with a hook, and the listing is right again.
	receive(t, runHookAsync("a", hookEvent("UserPromptSubmit", "", "", nil)))
	if got := status(); got != "busy" {
		t.Fatalf("status in its next turn is %q", got)
	}
	resp = mustCall(t, Request{Op: "say", Name: "a", Text: "again"})
	if !strings.Contains(resp.Text, "once its running tool call returns") {
		t.Fatalf("say answered %q", resp.Text)
	}
}
