package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

// watchKinds reads the watch stream of the daemon on the test's socket, and returns the snapshot and a function that returns the next event's kind.
func watchKinds(t *testing.T) (Response, func() string) {
	t.Helper()
	conn, err := net.Dial("unix", socketPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := json.NewEncoder(conn).Encode(Request{Op: "watch"}); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(conn)
	var snap Response
	if err := dec.Decode(&snap); err != nil {
		t.Fatal(err)
	}
	return snap, func() string {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var r Response
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		return r.Event.Kind
	}
}

// awaitEvent reads events until one of kind.
func awaitEvent(t *testing.T, next func() string, kind string) {
	t.Helper()
	for next() != kind {
	}
}

func TestPause(t *testing.T) {
	d, fc := testDaemon(t)
	idleWait = 0
	t.Cleanup(func() { idleWait = 30 * time.Second })
	fc.mu.Lock()
	fc.agents[0].SessionID = "s-m"
	fc.mu.Unlock()
	d.mu.Lock()
	for _, name := range []string{"b", "c"} {
		d.nodes[name] = &Session{Node: name, Parent: masterNode, Repo: "/repo", Dir: "/repo/.claude/worktrees/" + name, Depth: 1, ID: strings.Repeat(name, 8), Status: "idle"}
	}
	d.mu.Unlock()
	fc.add(agent{ID: "aaaaaaaa", SessionID: "s-a", Name: "a", Status: "busy"})
	fc.add(agent{ID: "bbbbbbbb", SessionID: "s-b", Name: "b", Status: "idle"})
	fc.add(agent{ID: "cccccccc", SessionID: "s-c", Name: "c"})
	fc.setStatus("cccccccc", "")
	d.poll()
	// The master took an event and is in its turn, a is in its turn, b sits idle and c stopped long ago.
	mustCall(t, Request{Op: "tell", Text: "x"})
	receive(t, runHookAsync(masterNode, hookEvent("PreToolUse", "t1", "mcp__bosun__next_event", nil)))
	mustCall(t, Request{Op: "next_event", Node: masterNode, Key: "k1"})
	_, next := watchKinds(t)

	if text := mustCall(t, Request{Op: "pause"}).Text; !strings.Contains(text, "a, master") {
		t.Fatalf("pause said %q", text)
	}
	awaitEvent(t, next, "paused")
	fc.mu.Lock()
	stops := slices.Clone(fc.stops)
	fc.mu.Unlock()
	slices.Sort(stops)
	if !slices.Equal(stops, []string{"00000001", "aaaaaaaa", "bbbbbbbb"}) {
		t.Fatalf("stopped %v", stops)
	}
	d.poll()
	snap := mustCall(t, Request{Op: "status"})
	paused := map[string]bool{}
	for _, s := range snap.Sessions {
		paused[s.Node] = s.Paused
	}
	if !snap.Paused || !paused[masterNode] || !paused["a"] || paused["b"] || paused["c"] {
		t.Fatalf("paused %v, sessions %v", snap.Paused, paused)
	}

	// Nothing resumes a session while paused: an event for the master, the master's own resume, a spawn, a switch of model, a usage limit's reset.
	mustCall(t, Request{Op: "tell", Text: "y"})
	for _, r := range []Request{
		{Op: "resume", Node: masterNode, Name: "b", Text: "x"},
		{Op: "spawn", Name: "d", Repo: "/repo", Text: "x"},
		{Op: "model", Model: "opus"},
	} {
		if _, err := call(context.Background(), r); err == nil || !strings.Contains(err.Error(), "paused") {
			t.Fatalf("%s while paused: %v", r.Op, err)
		}
	}
	d.mu.Lock()
	past := time.Now().Add(-time.Minute)
	d.nodes["b"].LimitedUntil = &past
	d.mu.Unlock()
	d.recoverLimited(time.Now())
	time.Sleep(50 * time.Millisecond)
	fc.mu.Lock()
	starts := len(fc.starts)
	fc.mu.Unlock()
	d.mu.Lock()
	events := slices.Clone(d.events)
	d.mu.Unlock()
	if starts != 1 {
		t.Fatalf("claude started %d times while paused", starts)
	}
	if len(events) != 2 || events[0].Text != "x" || !events[0].Again || events[1].Text != "y" {
		t.Fatalf("events %+v: the master's batch is not back in front, or a stopped worker was news", events)
	}

	// The pause outlives the daemon.
	d = restart(t, d)
	if !mustCall(t, Request{Op: "status"}).Paused {
		t.Fatal("a restart lifted the pause")
	}
	_, next = watchKinds(t)
	text := mustCall(t, Request{Op: "unpause"}).Text
	awaitEvent(t, next, "unpaused")
	fc.mu.Lock()
	resumed := slices.Clone(fc.starts[1:])
	fc.mu.Unlock()
	want := [][]string{{"--bg", "--resume", "s-m", "--", pauseResetMaster}, {"--bg", "--resume", "s-a", "--", pauseReset}}
	if !slices.EqualFunc(resumed, want, slices.Equal) || !strings.Contains(text, "master, a") {
		t.Fatalf("unpause said %q and ran claude with %q", text, resumed)
	}
	if _, err := call(context.Background(), Request{Op: "unpause"}); err == nil {
		t.Fatal("unpaused twice")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.nodes[masterNode].Paused || d.nodes["a"].Paused {
		t.Fatal("the sessions unpause resumed are still marked")
	}
}

// A report that reaches a parent between turns during a pause waits for unpause, which hands it over.
func TestPauseHoldsChildReports(t *testing.T) {
	d, fc := testDaemon(t)
	idleWait = 0
	t.Cleanup(func() { idleWait = 30 * time.Second })
	addChild(d)
	fc.mu.Lock()
	fc.agents[0].SessionID = "s-m"
	fc.mu.Unlock()
	fc.add(agent{ID: "aaaaaaaa", SessionID: "s-a", Name: "a", Status: "idle"})
	fc.add(agent{ID: "cccccccc", SessionID: "s-c", Name: "c", Status: "idle"})
	d.poll()
	mustCall(t, Request{Op: "pause"})
	// The user attached to c, whose turn then ended.
	stopHook(t, "c", "Done.")
	d.poll()
	time.Sleep(50 * time.Millisecond)
	awaitQueued(t, d, "a", 1, 0)
	fc.mu.Lock()
	starts := len(fc.starts)
	fc.mu.Unlock()
	if starts != 1 {
		t.Fatalf("claude started %d times while paused", starts)
	}
	mustCall(t, Request{Op: "unpause"})
	awaitQueued(t, d, "a", 0, 1)
	if args := fc.lastStart(); argAfter(args, "--resume") != "s-a" || !strings.Contains(args[len(args)-1], "c ended its turn:\nDone.") {
		t.Fatalf("claude ran with %q", args)
	}
}

// A resume already under way when the pause comes must not start its session after the pause stopped it.
func TestPauseRefusesResumeUnderWay(t *testing.T) {
	d, fc := testDaemon(t)
	fc.add(agent{ID: "aaaaaaaa", SessionID: "s-a", Name: "a", Status: "idle"})
	d.poll()
	fc.mu.Lock()
	fc.linger = 10
	fc.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- d.carryOn(context.Background(), "a", "x") }()
	for {
		fc.mu.Lock()
		stopped := len(fc.stops) > 0
		fc.mu.Unlock()
		if stopped {
			break
		}
		time.Sleep(time.Millisecond)
	}
	d.mu.Lock()
	d.paused = true
	d.mu.Unlock()
	if err := <-done; !errors.Is(err, errPaused) {
		t.Fatalf("a resume that the pause overtook: %v", err)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.starts) != 1 {
		t.Fatalf("claude started %d times", len(fc.starts))
	}
}
