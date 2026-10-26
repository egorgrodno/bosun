package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// trackedListener keeps the connections it accepts, so that a test can drop them all, as a daemon's exit does.
type trackedListener struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (l *trackedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.conns = append(l.conns, c)
		l.mu.Unlock()
	}
	return c, err
}

func (l *trackedListener) drop() {
	l.Listener.Close()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.conns {
		c.Close()
	}
}

var (
	servedMu sync.Mutex
	served   = map[*daemon]*trackedListener{}
)

// serve has d listen on its socket until the test ends.
func serve(t *testing.T, d *daemon) {
	t.Helper()
	ln, err := listen(d.socket)
	if err != nil {
		t.Fatal(err)
	}
	tl := &trackedListener{Listener: ln}
	servedMu.Lock()
	served[d] = tl
	servedMu.Unlock()
	t.Cleanup(tl.drop)
	go d.serve(tl)
}

// restart replaces d with a daemon that loads what d last saved and serves its socket, as after a crash, a power loss or an upgrade.
func restart(t *testing.T, d *daemon) *daemon {
	t.Helper()
	d.mu.Lock()
	d.stopping = true
	d.mu.Unlock()
	servedMu.Lock()
	served[d].drop()
	servedMu.Unlock()
	next := newDaemon(d.exe, d.socket, d.stateDir, d.masterDir, d.claude)
	if err := next.load(); err != nil {
		t.Fatal(err)
	}
	serve(t, next)
	return next
}

func save(d *daemon) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.saveLocked()
}

// writeTranscript writes the transcript Claude Code keeps of session sid, with one assistant message for each of texts.
func writeTranscript(t *testing.T, sid string, texts ...string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", "-repo--claude-worktrees-a")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString(`{"type":"user","message":{"role":"user","content":"go"}}` + "\n")
	for _, text := range texts {
		tool, _ := json.Marshal(map[string]any{"type": "assistant", "timestamp": time.Now(), "message": map[string]any{"stop_reason": "tool_use", "content": []map[string]any{{"type": "tool_use", "id": "t", "name": "Task", "input": map[string]any{}}}}})
		said, _ := json.Marshal(map[string]any{"type": "assistant", "timestamp": time.Now(), "message": map[string]any{"stop_reason": "end_turn", "content": []map[string]string{{"type": "text", "text": text}}}})
		b.Write(tool)
		b.WriteString("\n" + `{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":"a subagent's"}]}}` + "\n")
		b.Write(said)
		b.WriteString("\n")
	}
	b.WriteString(`{"type":"system","subtype":"turn_duration"}`)
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A worker blocked in ask or in a permission hook sends its request again once the daemon is back, and its decision survives with its id, holder and note.
func TestRestartKeepsDecisions(t *testing.T) {
	d, _ := testDaemon(t)
	answer := make(chan string, 1)
	go func() {
		text, err := newMCPServer("a", nil).run(context.Background(), "ask", json.RawMessage(`{"question":"X or Y?"}`))
		if err != nil {
			text = err.Error()
		}
		answer <- text
	}()
	question := openDecision(t, 1)
	mustCall(t, Request{Op: "escalate", Node: masterNode, ID: question.ID, Text: "the user's call"})
	receive(t, runHookAsync("a", hookEvent("PreToolUse", "t1", "Bash", bashTest)))
	hook := runHookAsync("a", hookEvent("PermissionRequest", "", "Bash", bashTest))
	permission := openDecision(t, 2)
	mustCall(t, Request{Op: "notify", Node: masterNode, Text: "b failed"})
	mustCall(t, Request{Op: "tell", Text: "x"})

	d = restart(t, d)
	got := openDecision(t, 3)
	if got.Kind != "notice" || got.Note != "b failed" {
		t.Fatalf("decision = %+v", got)
	}
	decs := mustCall(t, Request{Op: "status"}).Decisions
	if decs[0].ID != question.ID || decs[0].Holder != "user" || decs[0].Note != "the user's call" || decs[1].ID != permission.ID {
		t.Fatalf("decisions = %+v", decs)
	}
	events := mustCall(t, Request{Op: "next_event", Node: masterNode}).Events
	if len(events) != 2 || events[0].Kind != "tell" || events[1].Decision == nil || events[1].Decision.ID != permission.ID {
		t.Fatalf("events = %+v", events)
	}

	mustCall(t, Request{Op: "reply", ID: question.ID, Text: "X"})
	if got := receive(t, answer); got != "X" {
		t.Fatalf("ask got %q", got)
	}
	mustCall(t, Request{Op: "approve", Node: masterNode, ID: permission.ID})
	if out := receive(t, hook); !strings.Contains(out, `"behavior":"allow"`) {
		t.Fatalf("hook printed %s", out)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.orphans) != 0 {
		t.Fatalf("orphans %v", d.orphans)
	}
}

// A decision whose waiter did not come back after a restart waits on bosun no more, and a permission still waiting in its session's prompt becomes a notice.
func TestRestartExpiresOrphans(t *testing.T) {
	d, fc := testDaemon(t)
	fc.add(agent{ID: "aaaaaaaa", SessionID: "s-a", Name: "a", Status: "busy"})
	go func() { _, _ = call(context.Background(), Request{Op: "ask", Node: "a", Text: "X or Y?"}) }()
	openDecision(t, 1)
	go func() {
		_, _ = call(context.Background(), Request{Op: "hook", Node: "a", Key: "k", Hook: hookEvent("PermissionRequest", "", "Bash", bashTest)})
	}()
	openDecision(t, 2)

	d = restart(t, d)
	fc.setStatus("aaaaaaaa", "waiting")
	d.poll()
	d.expireOrphans(time.Now())
	if n := len(mustCall(t, Request{Op: "status"}).Decisions); n != 2 {
		t.Fatalf("%d decisions expired before their waiters' time ran out", n)
	}
	d.expireOrphans(time.Now().Add(time.Hour))
	dec := openDecision(t, 1)
	if dec.Kind != "notice" || dec.Node != "a" || !strings.Contains(dec.Note, "own prompt") {
		t.Fatalf("decision = %+v", dec)
	}
	// The session moving on settles it, as answering its prompt does.
	receive(t, runHookAsync("a", hookEvent("PreToolUse", "t2", "Bash", bashTest)))
	if n := len(mustCall(t, Request{Op: "status"}).Decisions); n != 0 {
		t.Fatalf("%d decisions open", n)
	}
}

// Workers whose turns ended or stopped while no daemon ran, a reboot or a crash, are news at the next start.
func TestRestartReportsMissedTurns(t *testing.T) {
	d, fc := testDaemon(t)
	d.mu.Lock()
	for _, name := range []string{"b", "c", "e"} {
		d.nodes[name] = &Session{Node: name, Parent: masterNode, Repo: "/repo", Dir: "/repo/.claude/worktrees/" + name, Depth: 1, ID: strings.Repeat(name, 8), Status: "busy"}
	}
	for _, n := range d.nodes {
		n.Heard = time.Now().Add(-time.Hour)
	}
	d.mu.Unlock()
	fc.add(agent{ID: "aaaaaaaa", SessionID: "s-a", Name: "a", Status: "busy"})
	fc.add(agent{ID: "bbbbbbbb", SessionID: "s-b", Name: "b", Status: "idle"})
	fc.add(agent{ID: "cccccccc", SessionID: "s-c", Name: "c", Status: "busy"})
	fc.add(agent{ID: "eeeeeeee", SessionID: "s-e", Name: "e", Status: "idle"})
	d.poll()
	// b's turn began a moment before the power went, too soon for a poll to see it.
	raw, _ := json.Marshal(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "s-b", "prompt": "go on"})
	receive(t, runHookAsync("b", raw))

	// a's turn ended while the daemon was away, and e ran a whole turn then.
	writeTranscript(t, "s-a", "Working on it.", "Done: committed abc123.")
	writeTranscript(t, "s-e", "Answered d's message.")
	fc.setStatus("aaaaaaaa", "idle")
	fc.setStatus("bbbbbbbb", "")
	fc.setStatus("cccccccc", "")
	d = restart(t, d)
	d.poll()
	events := mustCall(t, Request{Op: "next_event", Node: masterNode}).Events
	got := map[string]Event{}
	for _, ev := range events {
		got[ev.Node] = ev
	}
	if len(events) != 4 || got["a"].Kind != "turn_end" || got["a"].Text != "Done: committed abc123." || got["b"].Kind != "exit" || got["c"].Kind != "exit" || got["e"].Text != "Answered d's message." {
		t.Fatalf("events = %+v", events)
	}
	d.poll()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.events) != 0 {
		t.Fatalf("a second poll told the master again: %+v", d.events)
	}
}

// Workers that stop before the daemon dies leave their exits queued, which a power loss must not lose.
func TestPowerLossKeepsEvents(t *testing.T) {
	d, fc := testDaemon(t)
	fc.add(agent{ID: "aaaaaaaa", SessionID: "s-a", Name: "a", Status: "busy"})
	d.poll()
	fc.setStatus("aaaaaaaa", "")
	d.poll()
	d = restart(t, d)
	d.poll()
	events := mustCall(t, Request{Op: "next_event", Node: masterNode}).Events
	if len(events) != 1 || events[0].Kind != "exit" || events[0].Node != "a" {
		t.Fatalf("events = %+v", events)
	}
}

// A turn whose Stop never reached the daemon, as when the user interrupts it, ends in a turn_end all the same, once the listing has settled.
func TestMissedStop(t *testing.T) {
	d, fc := testDaemon(t)
	fc.add(agent{ID: "aaaaaaaa", SessionID: "s-a", Name: "a", Status: "busy"})
	d.poll()
	receive(t, runHookAsync("a", hookEvent("PostToolUse", "t1", "Bash", bashTest)))
	fc.setStatus("aaaaaaaa", "idle")
	d.poll()
	d.mu.Lock()
	queued := len(d.events)
	d.nodes["a"].turnAt = time.Now().Add(-turnSettle - time.Second)
	d.mu.Unlock()
	if queued != 0 {
		t.Fatal("a listing taken right after a hook of the turn ended it")
	}
	if !d.pollDue(time.Now()) {
		t.Fatal("no poll while a turn's end is unknown")
	}
	writeTranscript(t, "s-a", "Interrupted.")
	d.poll()
	events := mustCall(t, Request{Op: "next_event", Node: masterNode}).Events
	if len(events) != 1 || events[0].Kind != "turn_end" || events[0].Text != "Interrupted." {
		t.Fatalf("events = %+v", events)
	}

	// A Stop that reached the daemon is the turn's only end.
	receive(t, runHookAsync("a", hookEvent("PreToolUse", "t2", "Bash", bashTest)))
	receive(t, runHookAsync("a", hookEvent("Stop", "", "", nil)))
	d.mu.Lock()
	d.nodes["a"].turnAt = time.Time{}
	d.mu.Unlock()
	d.poll()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.events) != 1 {
		t.Fatalf("events = %+v", d.events)
	}
}

// A missed turn end of a worker's child goes to the worker, and a worker cut short in its turn gets back the reports of its children that the turn took.
func TestMissedTurnsReachParents(t *testing.T) {
	d, fc := testDaemon(t)
	addChild(d)
	fc.add(agent{ID: "aaaaaaaa", SessionID: "s-a", Name: "a", Status: "busy"})
	fc.add(agent{ID: "cccccccc", SessionID: "s-c", Name: "c", Status: "busy"})
	d.poll()
	receive(t, runHookAsync("c", hookEvent("PreToolUse", "t1", "Bash", bashTest)))
	writeTranscript(t, "s-c", "Fixed the parser.")
	fc.setStatus("cccccccc", "idle")
	d.mu.Lock()
	d.nodes["c"].turnAt = time.Time{}
	d.mu.Unlock()
	d.poll()
	awaitQueued(t, d, "a", 1, 0)
	if evs := masterEvents(d); len(evs) != 0 {
		t.Fatalf("the master got %+v", evs)
	}

	// a took c's report in its turn, which then stops.
	d.mu.Lock()
	a := d.nodes["a"]
	a.Taken, a.Events = a.Events, nil
	d.mu.Unlock()
	receive(t, runHookAsync("a", hookEvent("PreToolUse", "t2", "Bash", bashTest)))
	fc.setStatus("aaaaaaaa", "")
	d.mu.Lock()
	a.turnAt = time.Time{}
	d.mu.Unlock()
	d.poll()
	d.mu.Lock()
	events, taken := slices.Clone(a.Events), len(a.Taken)
	d.mu.Unlock()
	if len(events) != 1 || events[0].Node != "c" || events[0].Text != "Fixed the parser." || taken != 0 {
		t.Fatalf("a holds %+v and %d taken", events, taken)
	}
	if evs := masterEvents(d); len(evs) != 1 || evs[0].Kind != "exit" || evs[0].Node != "a" {
		t.Fatalf("the master got %+v", evs)
	}
}

// A permission that waits longer than its hook can hold it waits in the session's own prompt, and the user hears so.
func TestPermissionOutlivesHook(t *testing.T) {
	testDaemon(t)
	promptAfter = 100 * time.Millisecond
	t.Cleanup(func() { promptAfter = holdTimeout - 2*holdMargin })
	receive(t, runHookAsync("a", hookEvent("PreToolUse", "t1", "Bash", bashTest)))
	hook := runHookAsync("a", hookEvent("PermissionRequest", "", "Bash", bashTest))
	if out := receive(t, hook); out != "null" {
		t.Fatalf("hook printed %s, want nothing, which leaves the call to the session's prompt", out)
	}
	dec := openDecision(t, 1)
	if dec.Kind != "notice" || dec.Holder != "user" || !strings.Contains(dec.Note, "npm test") || !strings.Contains(dec.Note, "bosun attach aaaaaaaa") {
		t.Fatalf("decision = %+v", dec)
	}
	receive(t, runHookAsync("a", hookEvent("PostToolUse", "t1", "Bash", bashTest)))
	if n := len(mustCall(t, Request{Op: "status"}).Decisions); n != 0 {
		t.Fatalf("%d decisions open once the session's prompt answered", n)
	}
}

// next_event sent again under its key, after the daemon took events for it, gets them again, across a restart too.
func TestNextEventAgain(t *testing.T) {
	d, _ := testDaemon(t)
	mustCall(t, Request{Op: "tell", Text: "x"})
	first := mustCall(t, Request{Op: "next_event", Node: masterNode, Key: "k1"}).Events
	d = restart(t, d)
	again := mustCall(t, Request{Op: "next_event", Node: masterNode, Key: "k1"}).Events
	if len(first) != 1 || len(again) != 1 || again[0].Text != "x" {
		t.Fatalf("first %+v, again %+v", first, again)
	}
	mustCall(t, Request{Op: "tell", Text: "y"})
	next := mustCall(t, Request{Op: "next_event", Node: masterNode, Key: "k2"}).Events
	if len(next) != 1 || next[0].Text != "y" {
		t.Fatalf("next %+v", next)
	}
}

// The events a master took in a turn cut short wait for it again, marked again, and it wakes for them.
func TestMasterCutOff(t *testing.T) {
	for _, cut := range []string{"stopped", "failed", "restarted"} {
		t.Run(cut, func(t *testing.T) {
			d, fc := testDaemon(t)
			idleWait = 0
			t.Cleanup(func() { idleWait = 30 * time.Second })
			fc.mu.Lock()
			fc.agents[0].SessionID = "s-m"
			fc.mu.Unlock()
			fc.add(agent{ID: "aaaaaaaa", SessionID: "s-a", Name: "a", Status: "idle"})
			d.poll()
			mustCall(t, Request{Op: "tell", Text: "x"})
			receive(t, runHookAsync(masterNode, hookEvent("PreToolUse", "t1", "mcp__bosun__next_event", nil)))
			mustCall(t, Request{Op: "next_event", Node: masterNode, Key: "k"})
			d.mu.Lock()
			d.nodes[masterNode].turnAt = time.Time{}
			d.mu.Unlock()
			switch cut {
			case "stopped":
				fc.setStatus("00000001", "")
				d.poll()
			case "failed":
				stopFailure(t, masterNode, "server_error", "API Error: 500")
				fc.setStatus("00000001", "idle")
				// An event retries a master whose turn failed.
				mustCall(t, Request{Op: "tell", Text: "y"})
			case "restarted":
				fc.setStatus("00000001", "idle")
				d = restart(t, d)
				if err := d.ensureMaster(); err != nil {
					t.Fatal(err)
				}
			}
			awaitWoken(t, d, fc, 2)
			events := mustCall(t, Request{Op: "next_event", Node: masterNode, Key: "k2"}).Events
			if len(events) == 0 || events[0].Text != "x" || !events[0].Again {
				t.Fatalf("events = %+v", events)
			}
		})
	}
}

// The listing shows the session, which the master had taken an event of, as idle: its turn ended without its Stop reaching the daemon.
func TestReportsSurviveListingLag(t *testing.T) {
	d, fc := testDaemon(t)
	fc.add(agent{ID: "aaaaaaaa", SessionID: "s-a", Name: "a", Status: "busy"})
	d.poll()
	receive(t, runHookAsync("a", hookEvent("PreToolUse", "t1", "Bash", bashTest)))
	receive(t, runHookAsync("a", hookEvent("Stop", "", "", nil)))
	// Claude Code lists the session idle only a moment after its Stop.
	d.poll()
	fc.setStatus("aaaaaaaa", "idle")
	d.poll()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.events) != 1 || d.events[0].Kind != "turn_end" {
		t.Fatalf("events = %+v", d.events)
	}
}
