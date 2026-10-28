package main

import (
	"strings"
	"testing"
	"time"
)

// stalls returns the stalled events waiting for the master, taking them.
func stalls(d *daemon) []Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Event
	for _, ev := range d.pendingLocked() {
		if ev.Kind == "stalled" {
			out = append(out, ev)
		}
	}
	d.events = nil
	return out
}

func TestStalledWorker(t *testing.T) {
	d, _ := testDaemon(t)
	receive(t, runHookAsync("a", hookEvent("PreToolUse", "t1", "Bash", map[string]any{"command": "make slow"})))
	at := time.Now()

	d.checkStalls(at.Add(9 * time.Minute))
	if evs := stalls(d); len(evs) != 0 {
		t.Fatalf("below the threshold: %+v", evs)
	}
	d.checkStalls(at.Add(11 * time.Minute))
	evs := stalls(d)
	if len(evs) != 1 || evs[0].Node != "a" || evs[0].ID != "" || evs[0].Decision != nil {
		t.Fatalf("a quiet worker: %+v", evs)
	}
	for _, want := range []string{"11m", "Bash make slow", "Open call: Bash"} {
		if !strings.Contains(evs[0].Text, want) {
			t.Errorf("text lacks %q:\n%s", want, evs[0].Text)
		}
	}
	d.checkStalls(at.Add(20 * time.Minute))
	if evs := stalls(d); len(evs) != 0 {
		t.Fatalf("the same stall again: %+v", evs)
	}

	// A hook ends the stall, and a second quiet spell reports again.
	receive(t, runHookAsync("a", hookEvent("PostToolUse", "t1", "Bash", map[string]any{"command": "make slow"})))
	at = time.Now()
	d.checkStalls(at.Add(5 * time.Minute))
	if evs := stalls(d); len(evs) != 0 {
		t.Fatalf("after new activity: %+v", evs)
	}
	d.checkStalls(at.Add(11 * time.Minute))
	if evs := stalls(d); len(evs) != 1 || strings.Contains(evs[0].Text, "Open call") {
		t.Fatalf("a second quiet spell: %+v", evs)
	}
}

func TestStalledWorkerSkips(t *testing.T) {
	d, _ := testDaemon(t)
	late := time.Now().Add(time.Hour)
	d.mu.Lock()
	a := d.nodes["a"]
	a.Turn = true
	d.mu.Unlock()
	// A worker that waits on an answer is not stuck: its decision is the news.
	d.mu.Lock()
	d.openLocked(&Decision{Kind: "question", Node: "a", Holder: "master", Question: "x or y?"})
	d.mu.Unlock()
	d.checkStalls(late)
	if evs := stalls(d); len(evs) != 0 {
		t.Fatalf("a worker with a decision open: %+v", evs)
	}
	for name, change := range map[string]func(*Session){
		"between turns": func(n *Session) { n.Status = "idle" },
		"killed":        func(n *Session) { n.Killed = true },
		"not in a turn": func(n *Session) { n.Turn = false },
	} {
		d.mu.Lock()
		clear(d.decisions)
		a.Status, a.Killed, a.Turn = "busy", false, true
		change(a)
		d.mu.Unlock()
		d.checkStalls(late)
		if evs := stalls(d); len(evs) != 0 {
			t.Errorf("%s: %+v", name, evs)
		}
	}
	d.mu.Lock()
	d.stallAfter = 0
	a.Status, a.Killed, a.Turn = "busy", false, true
	d.mu.Unlock()
	d.checkStalls(late)
	if evs := stalls(d); len(evs) != 0 {
		t.Errorf("reports off: %+v", evs)
	}
}

// A grandchild's stall goes to its parent, which hears it as it hears a turn_end.
func TestStalledChildGoesToParent(t *testing.T) {
	d, _ := testDaemon(t)
	d.mu.Lock()
	d.nodes["b"] = &Session{Node: "b", Parent: "a", Repo: "/repo", Dir: worktree + "b", Depth: 2, ID: "bbbbbbbb", Status: "busy", Turn: true}
	d.mu.Unlock()
	d.checkStalls(d.began.Add(11 * time.Minute))
	if evs := stalls(d); len(evs) != 0 {
		t.Fatalf("the master heard of a grandchild: %+v", evs)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	a := d.nodes["a"]
	if len(a.Events) != 1 || a.Events[0].Kind != "stalled" || a.Events[0].Node != "b" {
		t.Fatalf("a's events = %+v", a.Events)
	}
	if report := childReport(a.Events); !strings.Contains(report, "b may be stuck") {
		t.Errorf("report = %s", report)
	}
}

func TestStalledDecision(t *testing.T) {
	d, _ := testDaemon(t)
	open := func(dec *Decision) string {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.openLocked(dec)
		return dec.ID
	}
	userOnly := open(&Decision{Kind: "permission", Node: "a", Holder: "user", UserOnly: true, Reserved: "a write to a credential store", Tool: "Write", Input: []byte(`{"file_path":"/home/x/.ssh/config"}`)})
	notice := open(&Decision{Kind: "notice", Node: "a", Holder: "user", Note: "The desk could not start."})
	prompt := open(&Decision{Kind: "notice", Node: "a", Holder: "user", Prompt: true, Note: "a waits in its own prompt: Bash"})
	masters := open(&Decision{Kind: "permission", Node: masterNode, Holder: "user", Tool: "Bash"})
	open(&Decision{Kind: "permission", Node: "a", Holder: "master", Tool: "Bash"})
	now := time.Now()

	d.checkStalls(now.Add(9 * time.Minute))
	if evs := stalls(d); len(evs) != 0 {
		t.Fatalf("below the threshold: %+v", evs)
	}
	d.checkStalls(now.Add(11 * time.Minute))
	evs := stalls(d)
	if len(evs) != 2 || evs[0].ID != userOnly || evs[1].ID != prompt || evs[0].Node != "a" {
		t.Fatalf("stalled decisions %s, %s, %s, %s: %+v", userOnly, notice, prompt, masters, evs)
	}
	for _, want := range []string{"decision " + userOnly, "11m", "Write", "credential store", "notify"} {
		if !strings.Contains(evs[0].Text, want) {
			t.Errorf("text lacks %q:\n%s", want, evs[0].Text)
		}
	}
	d.checkStalls(now.Add(30 * time.Minute))
	if evs := stalls(d); len(evs) != 0 {
		t.Fatalf("reported again: %+v", evs)
	}
	if n := len(mustCall(t, Request{Op: "log", Node: masterNode, Log: &LogQuery{Kind: "stalled"}}).Log); n != 2 {
		t.Errorf("%d stalled entries in the log", n)
	}
}
