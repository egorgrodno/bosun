package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// logOf returns the daemon's log entries of kind.
func logOf(d *daemon, kind string) []Entry {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Entry
	for _, e := range d.logged {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestLogSettledByMasterAndUser(t *testing.T) {
	d, _ := testDaemon(t)
	hook := runHookAsync("a", hookEvent("PermissionRequest", "", "Bash", bashTest))
	dec := mustCall(t, Request{Op: "next_event", Node: masterNode}).Events[0].Decision
	mustCall(t, Request{Op: "approve", Node: masterNode, ID: dec.ID})
	receive(t, hook)

	hook = runHookAsync("a", hookEvent("PermissionRequest", "", "Bash", bashTest))
	dec = mustCall(t, Request{Op: "next_event", Node: masterNode}).Events[0].Decision
	mustCall(t, Request{Op: "escalate", Node: masterNode, ID: dec.ID, Text: "touches CI"})
	mustCall(t, Request{Op: "deny", ID: dec.ID, Text: "unit tests only"})
	receive(t, hook)

	settled := logOf(d, "settled")
	if len(settled) != 2 {
		t.Fatalf("settled entries %+v", settled)
	}
	if e := settled[0]; e.By != "master" || e.From != masterNode || e.Behavior != "allow" || e.Node != "a" || e.Decision == nil || e.Withdrawn {
		t.Fatalf("the master's allow logged as %+v", e)
	}
	if e := settled[1]; e.By != "user" || e.From != "" || e.Behavior != "deny" || e.Text != "unit tests only" || e.Decision.ID != dec.ID {
		t.Fatalf("the user's deny logged as %+v", e)
	}
	esc := logOf(d, "escalated")
	if len(esc) != 1 || esc[0].By != "master" || esc[0].Text != "touches CI" || esc[0].Decision.ID != dec.ID || esc[0].Decision.Holder != "user" {
		t.Fatalf("escalated entries %+v", esc)
	}
	if !(esc[0].Seq < settled[1].Seq) || settled[1].At.IsZero() {
		t.Fatalf("entries out of order: %+v then %+v", esc[0], settled[1])
	}
	if settled[0].Route != routeMaster || esc[0].Route != routeEscalated || settled[1].Route != routeEscalated || settled[1].Rule != "" {
		t.Fatalf("routes %q, %q and %q, rule %q", settled[0].Route, esc[0].Route, settled[1].Route, settled[1].Rule)
	}
}

// Each decision that reaches the user logs the rule that sent it there, what the rule matched, its route and its wait, and a restart keeps the rule.
func TestLogRoutes(t *testing.T) {
	d, _ := testDesk(t)
	hook := runHookAsync("a", hookEvent("PreToolUse", "t1", "Bash", map[string]any{"command": "git push --force origin HEAD"}))
	dec := openDecision(t, 1)
	opened := logOf(d, "decision")
	if len(opened) != 1 || opened[0].Rule != rulePushForce || opened[0].Match != "--force" || opened[0].Route != routeUserOnly {
		t.Fatalf("decision entries %+v", opened)
	}
	d.mu.Lock()
	d.saveLocked()
	d.mu.Unlock()
	restarted := newDaemon("/bin/bosun", "", d.stateDir, d.masterDir, &fakeClaude{})
	if err := restarted.load(); err != nil {
		t.Fatal(err)
	}
	if r := restarted.decisions[dec.ID]; r == nil || r.rule != rulePushForce || r.match != "--force" {
		t.Fatalf("the restart restored %+v", r)
	}
	mustCall(t, Request{Op: "deny", ID: dec.ID})
	receive(t, hook)
	settled := logOf(d, "settled")
	if len(settled) != 1 || settled[0].Rule != rulePushForce || settled[0].Route != routeUserOnly || settled[0].By != "user" || settled[0].Waited < 0 {
		t.Fatalf("settled entries %+v", settled)
	}

	hook = runHookAsync(deskNode, hookEvent("PreToolUse", "t2", "Write", map[string]any{"file_path": "/tmp/x", "content": "x"}))
	dec = openDecision(t, 1)
	mustCall(t, Request{Op: "deny", ID: dec.ID})
	receive(t, hook)
	if e := logOf(d, "settled")[1]; e.Rule != ruleWriteOut || e.Match != "/tmp/x" || e.Route != routeRequester {
		t.Fatalf("the desk's own write settled as %+v", e)
	}
}

// The session's own prompt settles a permission: the tool running allows it, and the session moving on denies it.
func TestLogSettledInSessionPrompt(t *testing.T) {
	for next, want := range map[string]string{"PostToolUse": "allow", "PreToolUse": "deny", "Stop": "deny"} {
		t.Run(next, func(t *testing.T) {
			d, _ := testDaemon(t)
			receive(t, runHookAsync("a", hookEvent("PreToolUse", "t1", "Bash", bashTest)))
			hook := runHookAsync("a", hookEvent("PermissionRequest", "", "Bash", bashTest))
			openDecision(t, 1)
			id := "t1"
			if next == "PreToolUse" {
				id = "t2"
			}
			receive(t, runHookAsync("a", hookEvent(next, id, "Bash", bashTest)))
			receive(t, hook)
			settled := logOf(d, "settled")
			if len(settled) != 1 || settled[0].By != "session" || settled[0].From != "a" || settled[0].Behavior != want || settled[0].Withdrawn {
				t.Fatalf("settled entries %+v", settled)
			}
		})
	}
}

// A pause stops a worker whose hook waits on a decision, which withdraws it.
func TestLogWithdrawnByPause(t *testing.T) {
	d, _ := testDaemon(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = d.hook(ctx, "a", "", "k", hookEvent("PermissionRequest", "", "Bash", bashTest))
		close(done)
	}()
	dec := openDecision(t, 1)
	d.mu.Lock()
	d.paused = true
	d.nodes["a"].Paused = true
	d.mu.Unlock()
	cancel()
	<-done
	settled := logOf(d, "settled")
	if len(settled) != 1 || !settled[0].Withdrawn || settled[0].Cause != "the pause stopped a" || settled[0].By != "bosun" || settled[0].Decision.ID != dec.ID || settled[0].Behavior != "" {
		t.Fatalf("settled entries %+v", settled)
	}
}

func TestLogFleetEvents(t *testing.T) {
	d, fc := testDaemon(t)
	snap, next := watchKinds(t)
	if len(snap.Log) != 0 {
		t.Fatalf("a fresh daemon's snapshot holds %+v", snap.Log)
	}
	receive(t, runHookAsync("a", peerPrompt("b", "rebase on mine")))
	awaitEvent(t, next, "log")
	mustCall(t, Request{Op: "tell", Text: "fix the flake"})
	mustCall(t, Request{Op: "notify", Node: masterNode, Text: "b did not start"})
	stopHook(t, "a", "Done.")

	fc.add(agent{ID: "aaaaaaaa", Name: "a", Status: "idle"})
	if err := d.kill("a"); err != nil {
		t.Fatal(err)
	}

	if e := logOf(d, "message"); len(e) != 1 || e[0].By != "worker" || e[0].From != "b" || e[0].Node != "a" || e[0].Text != "rebase on mine" {
		t.Fatalf("message entries %+v", e)
	}
	// testDaemon starts no desk, so the user's tell goes to the master.
	if e := logOf(d, "tell"); len(e) != 1 || e[0].By != "user" || e[0].Node != masterNode || e[0].Text != "fix the flake" {
		t.Fatalf("tell entries %+v", e)
	}
	if e := logOf(d, "decision"); len(e) != 1 || e[0].By != "master" || e[0].Decision.Kind != "notice" || e[0].Decision.Note != "b did not start" {
		t.Fatalf("decision entries %+v", e)
	}
	if e := logOf(d, "turn_end"); len(e) != 1 || e[0].By != "worker" || e[0].Node != "a" || e[0].Text != "Done." {
		t.Fatalf("turn_end entries %+v", e)
	}
	if e := logOf(d, "killed"); len(e) != 1 || e[0].By != "user" || e[0].Node != "a" {
		t.Fatalf("killed entries %+v", e)
	}
	if e := logOf(d, "exit"); len(e) != 0 {
		t.Fatalf("a kill logged an exit too: %+v", e)
	}
	if snap := mustCall(t, Request{Op: "status"}); len(snap.Log) != 5 || len(snap.Messages) != 1 {
		t.Fatalf("the snapshot holds %d entries and %d messages", len(snap.Log), len(snap.Messages))
	}
}

func TestLogPauseAndUnpause(t *testing.T) {
	d, _ := testDaemon(t)
	mustCall(t, Request{Op: "pause"})
	mustCall(t, Request{Op: "unpause"})
	if e := logOf(d, "paused"); len(e) != 1 || e[0].By != "user" {
		t.Fatalf("paused entries %+v", e)
	}
	if e := logOf(d, "unpaused"); len(e) != 1 || e[0].By != "user" || !strings.HasPrefix(e[0].Text, "Unpaused bosun") {
		t.Fatalf("unpaused entries %+v", e)
	}
}

// The log outlasts a restart, moves to log.1.jsonl once it holds logRotate entries, and keeps Seq rising across both.
func TestLogOutlastsRestart(t *testing.T) {
	defer func(n int) { logRotate = n }(logRotate)
	logRotate = 2 * keptLog
	dir := t.TempDir()
	d := newDaemon("/bin/bosun", "", dir, dir, &fakeClaude{})
	d.mu.Lock()
	for i := range 2*keptLog - 2 {
		d.logLocked(Entry{Kind: "message", By: "worker", From: "b", Node: "a", Text: strings.Repeat("x", i%3)})
	}
	d.mu.Unlock()
	// A crash cuts the last line short.
	f, _ := os.OpenFile(d.logPath(), os.O_WRONLY|os.O_APPEND, 0)
	_, _ = f.WriteString(`{"seq":`)
	f.Close()

	load := func() *daemon {
		t.Helper()
		d := newDaemon("/bin/bosun", "", dir, dir, &fakeClaude{})
		if err := d.load(); err != nil {
			t.Fatal(err)
		}
		return d
	}
	lines := func(path string) int {
		data, _ := os.ReadFile(path)
		return bytes.Count(data, []byte("\n"))
	}
	d = load()
	if len(d.logged) != keptLog || d.logSeq != 2*keptLog-2 || d.logged[keptLog-1].Seq != 2*keptLog-2 {
		t.Fatalf("loaded %d entries up to %d", len(d.logged), d.logSeq)
	}
	// The cut line ends, and stays until the file moves.
	if n := lines(d.logPath()); n != 2*keptLog-1 {
		t.Fatalf("log.jsonl holds %d lines", n)
	}

	d.mu.Lock()
	d.logLocked(Entry{Kind: "paused", By: "user"})
	d.mu.Unlock()
	if n, old := lines(d.logPath()), lines(oldLogPath(dir)); n != keptLog || old != 2*keptLog {
		t.Fatalf("after the rotation log.jsonl holds %d lines and log.1.jsonl %d", n, old)
	}

	d = load()
	if d.logged[keptLog-1].Kind != "paused" || d.logSeq != 2*keptLog-1 {
		t.Fatalf("after the rotation loaded up to %d, the last %+v", d.logSeq, d.logged[keptLog-1])
	}
	d.mu.Lock()
	d.logLocked(Entry{Kind: "unpaused", By: "user"})
	d.mu.Unlock()
	var seqs []int
	if err := readLogs(dir, func(e Entry) { seqs = append(seqs, e.Seq) }); err != nil {
		t.Fatal(err)
	}
	if len(seqs) != 2*keptLog || seqs[0] != 1 || !slices.IsSorted(seqs) || seqs[len(seqs)-1] != 2*keptLog {
		t.Fatalf("readLogs read %d entries, %v to %v", len(seqs), seqs[0], seqs[len(seqs)-1])
	}

	// A crash between a rotation's two steps leaves log.1.jsonl alone, and Seq still rises.
	os.Remove(d.logPath())
	if d = load(); d.logSeq != 2*keptLog-1 {
		t.Fatalf("with log.1.jsonl alone loaded up to %d", d.logSeq)
	}
}

func TestLogCutsLongInput(t *testing.T) {
	long := strings.Repeat("y", 3*logInputCut)
	input, _ := json.Marshal(map[string]any{"file_path": "/x", "content": long})
	var got map[string]string
	if err := json.Unmarshal(cutInput(input), &got); err != nil {
		t.Fatal(err)
	}
	if got["file_path"] != "/x" || len([]rune(got["content"])) != logInputCut+1 {
		t.Fatalf("cut input to %d runes", len([]rune(got["content"])))
	}
	if short := json.RawMessage(`{"command":"npm test"}`); string(cutInput(short)) != string(short) {
		t.Fatal("cut a short input")
	}
}

func TestLogLine(t *testing.T) {
	at := time.Date(2026, 10, 26, 17, 44, 0, 0, time.Local)
	dec := &Decision{ID: "18", Kind: "permission", Node: "a", Tool: "Edit", Input: json.RawMessage(`{"file_path":"retry.go"}`)}
	for _, c := range []struct {
		e    Entry
		want string
	}{
		{Entry{Kind: "settled", By: "master", From: masterNode, Node: "a", Decision: dec, Behavior: "allow"}, "17:44 master       allowed a's 18 Edit: retry.go"},
		{Entry{Kind: "settled", By: "session", From: "a", Node: "a", Decision: dec, Behavior: "deny"}, "17:44 a            denied a's 18 Edit: retry.go, in its own prompt"},
		{Entry{Kind: "settled", By: "bosun", Node: "a", Decision: dec, Withdrawn: true, Cause: "the pause stopped a"}, "17:44 bosun        decision 18 withdrawn when the pause stopped a"},
		{Entry{Kind: "settled", By: "desk", From: deskNode, Node: "a", Decision: dec, Behavior: "allow", Words: "yes"}, `17:44 desk         allowed a's 18 Edit: retry.go, on your words "yes"`},
		{Entry{Kind: "spawned", By: "master", From: masterNode, Node: "b", Repo: "/app", Model: "sonnet", Text: "Fix it.\nAll of it."}, "17:44 master       spawned b in /app on sonnet: Fix it."},
		{Entry{Kind: "killed", By: "user", Node: "b"}, "17:44 you          stopped b"},
		{Entry{Kind: "model", By: "user", Node: masterNode, Model: "opus", Effort: "high"}, "17:44 you          switched master to opus high"},
		{Entry{Kind: "someday", By: "bosun", Text: "new"}, "17:44 bosun        someday: new"},
	} {
		c.e.At = at
		if got := logLine(c.e); got != c.want {
			t.Errorf("logLine(%s) = %q, want %q", c.e.Kind, got, c.want)
		}
	}
}

func TestStatusShowsLog(t *testing.T) {
	var log []Entry
	for i := range shownLog + 2 {
		log = append(log, Entry{Seq: i + 1, Kind: "paused", By: "user"})
	}
	var out bytes.Buffer
	printStatus(&out, Response{Log: log})
	_, after, ok := strings.Cut(out.String(), "Log:\n")
	if !ok || strings.Count(after, "paused the fleet") != shownLog {
		t.Fatalf("status printed %q", out.String())
	}
}
