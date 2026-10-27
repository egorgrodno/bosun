package main

import (
	"encoding/json"
	"testing"
)

var forcePush = map[string]any{"command": "git push --force origin HEAD"}

const (
	preAllowed  = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow"}}`
	permAllowed = `{"hookSpecificOutput":{"decision":{"behavior":"allow"},"hookEventName":"PermissionRequest"}}`
)

// allowReserved runs the PreToolUse of a reserved call of worker a, which the user allows, and returns the decision.
func allowReserved(t *testing.T, toolUseID, tool string, input map[string]any) Decision {
	t.Helper()
	hook := runHookAsync("a", hookEvent("PreToolUse", toolUseID, tool, input))
	dec := openDecision(t, 1)
	if dec.Reserved == "" {
		t.Fatalf("decision = %+v", dec)
	}
	mustCall(t, Request{Op: "approve", ID: dec.ID})
	if out := receive(t, hook); out != preAllowed {
		t.Fatalf("PreToolUse printed %s, want %s", out, preAllowed)
	}
	return dec
}

// stillAsks checks that a PermissionRequest of worker a opens a decision for the master, which the master denies.
func stillAsks(t *testing.T, tool string, input map[string]any) {
	t.Helper()
	hook := runHookAsync("a", hookEvent("PermissionRequest", "", tool, input))
	var dec *Decision
	for dec == nil {
		events := mustCall(t, Request{Op: "next_event", Node: masterNode}).Events
		for _, e := range events {
			if e.Decision != nil {
				dec = e.Decision
			}
		}
		if dec == nil && len(events) == 0 {
			t.Fatal("no decision reached the master")
		}
	}
	if dec.Reserved != "" || dec.Tool != tool {
		t.Fatalf("decision = %+v", dec)
	}
	mustCall(t, Request{Op: "deny", Node: masterNode, ID: dec.ID})
	if out := receive(t, hook); out == permAllowed {
		t.Fatalf("PermissionRequest printed %s", out)
	}
}

// Claude Code asks again for a call outside the sandbox that a PreToolUse allowed, and the reserved decision's allow answers it.
func TestReservedAllowAnswersPermissionRequest(t *testing.T) {
	d, _ := testDaemon(t)
	dec := allowReserved(t, "t1", "Bash", forcePush)
	if out := receive(t, runHookAsync("a", hookEvent("PermissionRequest", "", "Bash", forcePush))); out != permAllowed {
		t.Fatalf("PermissionRequest printed %s, want %s", out, permAllowed)
	}
	d.mu.Lock()
	open, queued := len(d.decisions), len(d.events)
	d.mu.Unlock()
	if open != 0 || queued != 0 {
		t.Fatalf("%d decisions open, %d events for the master", open, queued)
	}
	if got := logOf(d, "allowed"); len(got) != 1 || got[0].Node != "a" || got[0].By != "bosun" || got[0].Decision == nil || got[0].Decision.ID != dec.ID {
		t.Fatalf("allowed entries = %+v", got)
	}
	if got := logOf(d, "decision"); len(got) != 1 {
		t.Fatalf("decision entries = %+v", got)
	}

	// The allow answers one PermissionRequest.
	stillAsks(t, "Bash", forcePush)
}

// A worker's reserved action that the master allows answers its PermissionRequest too.
func TestMasterReservedAllowAnswersPermissionRequest(t *testing.T) {
	d, _ := testDaemon(t)
	write := map[string]any{"file_path": "/tmp/cfgprobe.sh", "content": "x"}
	hook := runHookAsync("a", hookEvent("PreToolUse", "t1", "Write", write))
	events := mustCall(t, Request{Op: "next_event", Node: masterNode}).Events
	if len(events) != 1 || events[0].Decision == nil || events[0].Decision.Reserved == "" {
		t.Fatalf("events = %+v", events)
	}
	mustCall(t, Request{Op: "approve", Node: masterNode, ID: events[0].Decision.ID})
	receive(t, hook)
	if out := receive(t, runHookAsync("a", hookEvent("PermissionRequest", "", "Write", write))); out != permAllowed {
		t.Fatalf("PermissionRequest printed %s, want %s", out, permAllowed)
	}
	if got := logOf(d, "allowed"); len(got) != 1 || got[0].Decision.ID != events[0].Decision.ID {
		t.Fatalf("allowed entries = %+v", got)
	}
}

// Only the very call that was allowed counts: another input, or another call of the same input, still asks.
func TestReservedAllowNeedsTheSameCall(t *testing.T) {
	d, _ := testDaemon(t)
	allowReserved(t, "t1", "Bash", forcePush)
	stillAsks(t, "Bash", map[string]any{"command": "git push --force origin main"})
	stillAsks(t, "Write", forcePush)

	// t2 runs the same command, so a PermissionRequest for it could be either call's.
	hook := runHookAsync("a", hookEvent("PreToolUse", "t2", "Bash", forcePush))
	dec := openDecision(t, 1)
	stillAsks(t, "Bash", forcePush)
	mustCall(t, Request{Op: "deny", ID: dec.ID})
	receive(t, hook)

	d.mu.Lock()
	by := d.nodes["a"].pre["t1"].allowedBy
	d.mu.Unlock()
	if by == "" {
		t.Fatal("the calls that asked took t1's allow")
	}
}

// A reserved action that the user denies never runs, and leaves nothing to answer a PermissionRequest.
func TestDeniedReservedAnswersNothing(t *testing.T) {
	testDaemon(t)
	hook := runHookAsync("a", hookEvent("PreToolUse", "t1", "Bash", forcePush))
	dec := openDecision(t, 1)
	mustCall(t, Request{Op: "deny", ID: dec.ID})
	receive(t, hook)
	stillAsks(t, "Bash", forcePush)
}

// The allow goes once the call has run, failed, or its turn has ended.
func TestReservedAllowEndsWithTheCall(t *testing.T) {
	for _, end := range []string{"PostToolUse", "PostToolUseFailure", "Stop", "StopFailure"} {
		t.Run(end, func(t *testing.T) {
			d, _ := testDaemon(t)
			allowReserved(t, "t1", "Bash", forcePush)
			raw, _ := json.Marshal(map[string]any{"hook_event_name": end, "session_id": "s-a", "tool_use_id": "t1", "tool_name": "Bash", "tool_input": forcePush})
			receive(t, runHookAsync("a", raw))
			stillAsks(t, "Bash", forcePush)
			if got := logOf(d, "allowed"); len(got) != 0 {
				t.Fatalf("allowed entries = %+v", got)
			}
		})
	}
}
