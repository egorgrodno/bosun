package main

import (
	"testing"
	"time"
)

// A turn's start and end show at once, and a listing taken before the hook does not revert them.
func TestHookOutranksStaleListing(t *testing.T) {
	d, _, _ := steerDaemon(t, "idle")
	shown := func() string {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, s := range d.snapshotLocked().Sessions {
			if s.Node == "a" {
				return sessionState(s)
			}
		}
		return ""
	}
	if got := shown(); got != "idle" {
		t.Fatalf("before the send: %q", got)
	}
	polled := time.Now()
	receive(t, runHookAsync("a", hookEvent("UserPromptSubmit", "", "", nil)))
	if got := shown(); got != "busy" {
		t.Fatalf("after UserPromptSubmit: %q", got)
	}
	// This listing began before the hook.
	d.refresh(map[string]agent{"aaaaaaaa": {ID: "aaaaaaaa", SessionID: "s-a", Name: "a", Status: "idle"}}, polled)
	if got := shown(); got != "busy" {
		t.Fatalf("after a stale poll: %q", got)
	}
	d.refresh(map[string]agent{"aaaaaaaa": {ID: "aaaaaaaa", SessionID: "s-a", Name: "a", Status: "busy"}}, time.Now())
	if got := shown(); got != "busy" {
		t.Fatalf("after the poll confirmed: %q", got)
	}
	receive(t, runHookAsync("a", hookEvent("Stop", "", "", nil)))
	if got := shown(); got != "idle" {
		t.Fatalf("after Stop: %q", got)
	}
}
