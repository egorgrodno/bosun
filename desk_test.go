package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// testDesk serves the testDaemon with a desk beside the master, listed idle as session 00000002 with the Claude Code session id s-d, and lists worker a busy.
func testDesk(t *testing.T) (*daemon, *fakeClaude) {
	t.Helper()
	d, fc := testDaemon(t)
	idleWait = 0
	t.Cleanup(func() { idleWait = 30 * time.Second })
	if err := d.ensureDesk(); err != nil {
		t.Fatal(err)
	}
	fc.mu.Lock()
	fc.agents[1].SessionID = "s-d"
	fc.mu.Unlock()
	fc.setStatus("00000002", "idle")
	fc.add(agent{ID: "aaaaaaaa", SessionID: "s-a", Name: "a", Status: "busy"})
	d.poll()
	return d, fc
}

// awaitStarts waits until claude has run n times, and returns the last run's arguments.
func awaitStarts(t *testing.T, fc *fakeClaude, n int) []string {
	t.Helper()
	for range 500 {
		fc.mu.Lock()
		got := len(fc.starts)
		fc.mu.Unlock()
		if got == n {
			return fc.lastStart()
		}
		time.Sleep(10 * time.Millisecond)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	t.Fatalf("claude ran %d times, want %d: %q", len(fc.starts), n, fc.starts)
	return nil
}

func TestDeskStarts(t *testing.T) {
	d, fc := testDaemon(t)
	d.deskModel = "sonnet"
	if err := d.ensureDesk(); err != nil {
		t.Fatal(err)
	}
	fc.mu.Lock()
	args, dir := fc.starts[1], fc.dirs[1]
	fc.mu.Unlock()
	if dir != filepath.Join(d.masterDir, deskNode) || argAfter(args, "-n") != deskNode || slices.Contains(args, "-w") || argAfter(args, "--model") != "sonnet" || args[len(args)-1] != deskStart {
		t.Fatalf("claude ran in %s with %q", dir, args)
	}
	if system := argAfter(args, "--append-system-prompt"); !strings.HasPrefix(system, deskPrompt) {
		t.Fatalf("the desk's system prompt is %q", system)
	}
	data, err := os.ReadFile(argAfter(args, "--settings"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Memory      string              `json:"autoMemoryDirectory"`
		Inbound     string              `json:"crossSessionInbound"`
		Permissions map[string][]string `json:"permissions"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if want := (&Session{Dir: d.masterDir}).memoryDir(); settings.Memory != want || settings.Inbound != "refuse" || len(settings.Permissions["deny"]) != 0 {
		t.Fatalf("settings = %s, want the master's memory directory %s, no inbound messages and AskUserQuestion allowed", data, want)
	}
	d.mu.Lock()
	desk := d.nodes[deskNode]
	writable := d.writable(desk)
	d.mu.Unlock()
	if desk.ID != "00000002" || desk.Parent != "" || !slices.Contains(writable, settings.Memory) {
		t.Fatalf("desk = %+v, writing to %q", desk, writable)
	}

	// The desk's transcripts land apart from the master's, which the master's clear reads.
	d.mu.Lock()
	master := d.nodes[masterNode].projectDir()
	d.mu.Unlock()
	if desk.projectDir() == master {
		t.Fatalf("the desk and the master share the project folder %s", master)
	}

	// Another start adopts the desk Claude Code still lists.
	if err := d.ensureDesk(); err != nil || awaitStarts(t, fc, 2) == nil {
		t.Fatalf("adopting the desk: %v", err)
	}
}

// The master may not write the desk's directory, whose settings and hooks would run outside the master's sandbox once the desk starts, and the desk may not write the master's.
func TestDeskFenced(t *testing.T) {
	d, _ := testDesk(t)
	d.mu.Lock()
	masterDir, deskDir := d.nodes[masterNode].Dir, d.nodes[deskNode].Dir
	d.mu.Unlock()
	for node, want := range map[string][]string{masterNode: {deskDir}, deskNode: nil} {
		data, err := os.ReadFile(filepath.Join(d.stateDir, "nodes", node, "settings.json"))
		if err != nil {
			t.Fatal(err)
		}
		var s struct {
			Sandbox struct {
				Filesystem map[string][]string `json:"filesystem"`
			} `json:"sandbox"`
		}
		if err := json.Unmarshal(data, &s); err != nil || !slices.Equal(s.Sandbox.Filesystem["denyWrite"], want) {
			t.Fatalf("%s's sandbox denies writes to %q, want %q: %v", node, s.Sandbox.Filesystem["denyWrite"], want, err)
		}
	}

	for _, c := range []struct {
		node, path string
		refused    bool
	}{
		{masterNode, filepath.Join(deskDir, ".claude", "settings.json"), true},
		{masterNode, filepath.Join(deskDir, ".mcp.json"), true},
		{masterNode, "desk/.claude/hooks/start.sh", true},
		{masterNode, filepath.Join(masterDir, "notes.md"), false},
		{masterNode, filepath.Join(masterDir, "desk-notes.md"), false},
		{deskNode, filepath.Join(masterDir, ".claude", "settings.json"), true},
		{deskNode, filepath.Join(deskDir, "notes.md"), false},
	} {
		input, _ := json.Marshal(map[string]string{"file_path": c.path, "content": "x"})
		if r := d.gate(c.node, "").reserved("Write", input); (r.reason != "") != c.refused {
			t.Errorf("%s writing %s: reserved %+v", c.node, c.path, r)
		}
	}

	hook := runHookAsync(masterNode, hookEvent("PreToolUse", "t1", "Write", map[string]any{"file_path": filepath.Join(deskDir, ".claude", "settings.json"), "content": "{}"}))
	if dec := openDecision(t, 1); dec.Holder != "user" || dec.Reserved != "a write into the desk's directory" {
		t.Fatalf("decision = %+v", dec)
	}
	mustCall(t, Request{Op: "deny", ID: "1"})
	if out := receive(t, hook); !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Fatalf("the master's write into the desk's directory printed %s", out)
	}
}

// The desk reads the fleet and tells the master, and changes nothing in the fleet itself.
func TestDeskTools(t *testing.T) {
	testDesk(t)
	var names []string
	for _, tool := range tools(deskNode) {
		names = append(names, tool.Name)
	}
	if !slices.Equal(names, []string{"tell_master", "status", "transcript"}) {
		t.Fatalf("the desk's tools are %q", names)
	}
	desk := newMCPServer(deskNode, nil)
	if _, err := desk.run(context.Background(), "spawn", json.RawMessage(`{"name":"b","prompt":"x"}`)); err == nil {
		t.Fatal("the desk has spawn")
	}
	for _, req := range []Request{
		{Op: "spawn", Node: deskNode, Name: "b", Text: "x"},
		{Op: "ask", Node: deskNode, Text: "x"},
		{Op: "merge", Node: deskNode, Name: "a"},
		{Op: "resume", Node: deskNode, Name: "a", Text: "x"},
		{Op: "approve", Node: deskNode, ID: "1"},
		{Op: "reply", Node: deskNode, ID: "1", Text: "x"},
		{Op: "escalate", Node: deskNode, ID: "1"},
		{Op: "notify", Node: deskNode, Text: "x"},
		{Op: "next_event", Node: deskNode},
		{Op: "spawn", Node: masterNode, Name: deskNode, Repo: "/repo", Text: "x"},
	} {
		if _, err := call(context.Background(), req); err == nil {
			t.Errorf("%+v went through", req)
		}
	}

	receive(t, runHookAsync("a", hookEvent("PreToolUse", "t1", "Bash", bashTest)))
	hook := runHookAsync("a", hookEvent("PermissionRequest", "", "Write", map[string]any{"file_path": "/repo/x", "content": "a long file"}))
	openDecision(t, 1)
	receive(t, runHookAsync("a", peerPrompt("b", "b merges first.\nThen rebase.")))
	text, err := desk.run(context.Background(), "status", nil)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(text, "\n")
	i := slices.Index(lines, "Sessions:")
	if i < 0 || !strings.HasPrefix(strings.TrimSpace(lines[i+1]), deskNode+" ") || !strings.HasPrefix(strings.TrimSpace(lines[i+2]), masterNode+" ") {
		t.Fatalf("status shows no desk above the master:\n%s", text)
	}
	if !strings.Contains(text, `"content": "a long file"`) || !strings.Contains(text, "b → a:\nb merges first.\nThen rebase.") {
		t.Fatalf("status leaves out the decision or the message in full:\n%s", text)
	}
	mustCall(t, Request{Op: "approve", ID: "1"})
	receive(t, hook)
}

func TestDeskMessages(t *testing.T) {
	testDesk(t)
	send := func(from, to string) string {
		return receive(t, runHookAsync(from, hookEvent("PreToolUse", "t1", "SendMessage", map[string]any{"to": to, "message": "hi"})))
	}
	for _, c := range []struct{ from, to, hint string }{
		{"a", deskNode, "ask"},
		{"a", "desk [c9e04b]", "ask"},
		{masterNode, deskNode, "tell_desk"},
		{deskNode, "a", "tell_master"},
		{deskNode, masterNode, "tell_master"},
	} {
		if out := send(c.from, c.to); !strings.Contains(out, `"permissionDecision":"deny"`) || !strings.Contains(out, c.hint) {
			t.Errorf("a message from %s to %q printed %s", c.from, c.to, out)
		}
	}
	if out := send(deskNode, "main"); out != "null" {
		t.Errorf("a message from the desk to its own session printed %s", out)
	}

	// The desk's own permissions wait on the user, as the master's do.
	receive(t, runHookAsync(deskNode, hookEvent("PreToolUse", "t2", "Bash", bashTest)))
	hook := runHookAsync(deskNode, hookEvent("PermissionRequest", "", "Bash", bashTest))
	if dec := openDecision(t, 1); dec.Holder != "user" || dec.Node != deskNode {
		t.Fatalf("decision = %+v", dec)
	}
	mustCall(t, Request{Op: "deny", ID: "1"})
	receive(t, hook)
}

// A tell from a frontend goes to the desk, which takes it in a new turn between turns, or at the end of the turn it is in.
func TestTellReachesDesk(t *testing.T) {
	d, fc := testDesk(t)
	mustCall(t, Request{Op: "tell", Text: "what is a doing?"})
	if args := awaitStarts(t, fc, 3); !slices.Equal(args, []string{"--bg", "--resume", "s-d", "--", deskReport([]Event{{Kind: "tell", Text: "what is a doing?"}})}) {
		t.Fatalf("claude ran with %q", args)
	}
	if evs := masterEvents(d); len(evs) != 0 {
		t.Fatalf("the master got %+v", evs)
	}

	d.poll()
	mustCall(t, Request{Op: "tell", Text: "and b?"})
	out := stopHook(t, deskNode, "a is fixing the parser.")
	if !strings.HasPrefix(out, `{"decision":"block","reason":"bosun: the user sent this through bosun tell`) || !strings.Contains(out, "and b?") {
		t.Fatalf("the desk's Stop with a tell waiting printed %s", out)
	}
	if out := stopHook(t, deskNode, "b is not running."); out != "null" {
		t.Fatalf("the desk's next Stop printed %s", out)
	}
	if evs := masterEvents(d); len(evs) != 0 || awaitStarts(t, fc, 3) == nil {
		t.Fatalf("the master got %+v", evs)
	}
}

// The desk tells the master, which answers the desk through bosun.
func TestDeskAndMasterTell(t *testing.T) {
	_, fc := testDesk(t)
	if _, err := newMCPServer(deskNode, nil).run(context.Background(), "tell_master", json.RawMessage(`{"text":"Start a worker to fix the flaky test in /repo."}`)); err != nil {
		t.Fatal(err)
	}
	evs := mustCall(t, Request{Op: "next_event", Node: masterNode}).Events
	if len(evs) != 1 || evs[0].Kind != "tell" || evs[0].From != deskNode || evs[0].Text != "Start a worker to fix the flaky test in /repo." {
		t.Fatalf("events = %+v", evs)
	}
	if _, err := newMCPServer(masterNode, nil).run(context.Background(), "tell_desk", json.RawMessage(`{"text":"a took the flaky test."}`)); err != nil {
		t.Fatal(err)
	}
	if args := awaitStarts(t, fc, 3); args[len(args)-1] != "bosun: the master answers:\na took the flaky test." {
		t.Fatalf("claude ran with %q", args)
	}
	if _, err := call(context.Background(), Request{Op: "tell", Node: "a", Text: "x"}); err == nil {
		t.Fatal("a worker told")
	}
	if n := len(mustCall(t, Request{Op: "status"}).Decisions); n != 0 {
		t.Fatalf("%d decisions open", n)
	}
}

// A desk turn cut short gives back the tells it took, once: cut short again, they go on to the master.
func TestDeskCutGivesBackTells(t *testing.T) {
	d, fc := testDesk(t)
	mustCall(t, Request{Op: "tell", Text: "fix it"})
	awaitStarts(t, fc, 3)
	d.poll()
	// The turn the tell began takes one more at its Stop, and goes on until the desk's process dies.
	mustCall(t, Request{Op: "tell", Text: "and the lexer"})
	stopHook(t, deskNode, "On it.")
	fc.setStatus("00000002", "")
	d.poll()
	d.poll()
	want := deskReport([]Event{{Kind: "tell", Text: "fix it", Again: true}, {Kind: "tell", Text: "and the lexer", Again: true}})
	if args := awaitStarts(t, fc, 4); args[len(args)-1] != want {
		t.Fatalf("claude ran with %q, want the tells again", args)
	}
	if evs := masterEvents(d); len(evs) != 0 {
		t.Fatalf("the master got %+v", evs)
	}

	d.poll()
	fc.setStatus("00000002", "")
	d.poll()
	if evs := masterEvents(d); len(evs) != 2 || evs[0].Text != "fix it" || evs[1].Text != "and the lexer" {
		t.Fatalf("events = %+v", evs)
	}
	if dec := openDecision(t, 1); dec.Kind != "notice" || !strings.Contains(dec.Note, "cut short twice") {
		t.Fatalf("decision = %+v", dec)
	}

	// A desk the user stopped stays stopped.
	d.poll()
	mustCall(t, Request{Op: "tell", Text: "x"})
	awaitStarts(t, fc, 5)
	d.poll()
	mustCall(t, Request{Op: "kill", Name: deskNode})
	d.poll()
	d.poll()
	time.Sleep(50 * time.Millisecond)
	awaitStarts(t, fc, 5)
}

// Without a desk, a tell goes to the master, and the master's answer to the user.
func TestTellWithoutDesk(t *testing.T) {
	d, fc := testDesk(t)
	fc.mu.Lock()
	fc.agents = slices.DeleteFunc(fc.agents, func(a agent) bool { return a.ID == "00000002" })
	fc.mu.Unlock()
	d.poll()
	mustCall(t, Request{Op: "tell", Text: "fix it"})
	if evs := masterEvents(d); len(evs) != 1 || evs[0].Kind != "tell" || evs[0].From != "" {
		t.Fatalf("events = %+v", evs)
	}
	mustCall(t, Request{Op: "tell", Node: masterNode, Text: "a fixed it."})
	if dec := openDecision(t, 1); dec.Kind != "notice" || !strings.Contains(dec.Note, "a fixed it.") {
		t.Fatalf("decision = %+v", dec)
	}
}

// A desk that a usage limit stopped hands what waits for it on, and takes nothing until the limit resets, as the master may run on a model the limit spares.
func TestLimitedDesk(t *testing.T) {
	d, fc := testDesk(t)
	fc.setStatus("00000002", "busy")
	d.poll()
	mustCall(t, Request{Op: "tell", Text: "fix it"})
	stopFailure(t, deskNode, "rate_limit", "You've hit your limit · resets 3pm (Europe/Berlin)")
	mustCall(t, Request{Op: "tell", Text: "and the lexer"})
	evs := masterEvents(d)
	if len(evs) != 2 || evs[0].Text != "fix it" || evs[1].Text != "and the lexer" {
		t.Fatalf("events = %+v", evs)
	}
	mustCall(t, Request{Op: "tell", Node: masterNode, Text: "a took both."})
	notes := ""
	for _, dec := range mustCall(t, Request{Op: "status"}).Decisions {
		notes += dec.Note + "\n"
	}
	if !strings.Contains(notes, "usage limit") || !strings.Contains(notes, "The master answers: a took both.") {
		t.Fatalf("notices:\n%s", notes)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if n := len(d.nodes[deskNode].Events); n != 0 {
		t.Fatalf("the limited desk holds %d tells", n)
	}
}

// A desk that cannot be resumed hands the user's tells to the master, and the master's answers to the user.
func TestDeskStranded(t *testing.T) {
	d, fc := testDesk(t)
	fc.mu.Lock()
	fc.fail = func(args []string) error {
		if argAfter(args, "--resume") == "s-d" {
			return errors.New("claude --bg: exit status 1")
		}
		return nil
	}
	fc.mu.Unlock()
	fc.setStatus("00000002", "busy")
	d.poll()
	mustCall(t, Request{Op: "tell", Text: "fix it"})
	mustCall(t, Request{Op: "tell", Node: masterNode, Text: "a fixed the last one."})
	fc.setStatus("00000002", "")
	d.poll()
	dec := openDecision(t, 1)
	if dec.Kind != "notice" || !strings.Contains(dec.Note, "a fixed the last one.") || !strings.Contains(dec.Note, "exit status 1") {
		t.Fatalf("decision = %+v", dec)
	}
	if evs := masterEvents(d); len(evs) != 1 || evs[0].Kind != "tell" || evs[0].Text != "fix it" {
		t.Fatalf("events = %+v", evs)
	}
}

func TestDeskTranscript(t *testing.T) {
	testDesk(t)
	writeTurns(t, "s-a")
	text, err := newMCPServer(deskNode, nil).run(context.Background(), "transcript", json.RawMessage(`{"name":"a","turns":1}`))
	if want := "a is busy. Its last turns, oldest first:\n\n" + lastTurn; err != nil || text != want {
		t.Fatalf("transcript =\n%s\nwant\n%s(%v)", text, want, err)
	}
	if text, _ := newMCPServer(deskNode, nil).run(context.Background(), "transcript", json.RawMessage(`{"name":"a"}`)); !strings.Contains(text, "> the first task\n") {
		t.Fatalf("transcript of three turns =\n%s", text)
	}
	if _, err := newMCPServer(deskNode, nil).run(context.Background(), "transcript", json.RawMessage(`{"name":"z"}`)); err == nil {
		t.Fatal("a transcript of no session")
	}
}

// staleDesk is the testDesk with the desk's last transcript written two hours ago. Each clear writes its new conversation's transcript and reports it, as Claude Code 2.1.310 does.
func staleDesk(t *testing.T) (*daemon, *fakeClaude) {
	t.Helper()
	d, fc := testDesk(t)
	d.mu.Lock()
	dir := d.nodes[deskNode].projectDir()
	d.mu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "s-d.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	written := time.Now().Add(-2 * cacheLife)
	if err := os.Chtimes(path, written, written); err != nil {
		t.Fatal(err)
	}
	fc.mu.Lock()
	fc.cleared = func(name, sid string) {
		if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), nil, 0o600); err != nil {
			t.Error(err)
		}
		raw, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": sid, "source": "clear"})
		hookOutput(name, "", bytes.NewReader(raw))
	}
	fc.mu.Unlock()
	return d, fc
}

// A desk idle for an hour wakes in a new conversation, whatever runs in the fleet, and the master's clear ignores it.
func TestStaleDeskClears(t *testing.T) {
	d, fc := staleDesk(t)
	if d.stale(masterNode, time.Now()) {
		t.Fatal("the master is stale with worker a left")
	}
	mustCall(t, Request{Op: "tell", Text: "x"})
	awaitStarts(t, fc, 4)
	if got, want := resumes(fc)[1:], []string{"--bg --resume s-d -- /clear", "--bg --resume s-d -- " + deskReport([]Event{{Kind: "tell", Text: "x"}})}; !slices.Equal(got, want) {
		t.Fatalf("claude ran with %q, want %q", got, want)
	}
	d.mu.Lock()
	conversation := d.nodes[deskNode].Conversation
	delete(d.nodes, "a")
	d.mu.Unlock()
	if conversation != "s-d-3" {
		t.Fatalf("the desk's conversation is %q, want the one the clear began", conversation)
	}
	if d.stale(masterNode, time.Now()) {
		t.Fatal("the desk's old transcript makes the master stale")
	}
}

func TestAttachClearsStaleDesk(t *testing.T) {
	_, fc := staleDesk(t)
	fc.setStatus("00000002", "")
	mustCall(t, Request{Op: "attach", ID: "00000002"})
	if got, want := resumes(fc)[1:], []string{"--bg --resume s-d -- /clear"}; !slices.Equal(got, want) {
		t.Fatalf("claude ran with %q, want %q", got, want)
	}
	mustCall(t, Request{Op: "attach", ID: "aaaaaaaa"})
	if got := resumes(fc); len(got) != 2 {
		t.Fatalf("attaching to a running worker ran claude with %q", got)
	}
	// The user said nothing to the cleared desk, which a tell then finds as it was left.
	mustCall(t, Request{Op: "tell", Text: "x"})
	awaitStarts(t, fc, 4)
	if got, want := resumes(fc)[1:], []string{"--bg --resume s-d -- /clear", "--bg --resume s-d -- " + deskReport([]Event{{Kind: "tell", Text: "x"}})}; !slices.Equal(got, want) {
		t.Fatalf("claude ran with %q, want %q", got, want)
	}
}

// A stale desk holding a reply the user has not opened it to read stays uncleared until they have.
func TestStaleDeskKeepsUnseenReply(t *testing.T) {
	d, fc := staleDesk(t)
	mustCall(t, Request{Op: "tell", Text: "x"})
	awaitStarts(t, fc, 4)
	d.mu.Lock()
	dir, unseen := d.nodes[deskNode].projectDir(), d.nodes[deskNode].Unseen
	d.mu.Unlock()
	if !unseen {
		t.Fatal("the desk's reply to a tell counts as seen")
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	written := time.Now().Add(-2 * cacheLife)
	for _, p := range paths {
		if err := os.Chtimes(p, written, written); err != nil {
			t.Fatal(err)
		}
	}
	fc.setStatus("00000002", "")
	mustCall(t, Request{Op: "attach", ID: "00000002"})
	if args := fc.lastStart(); !slices.Equal(args, []string{"--bg", "--resume", "s-d"}) {
		t.Fatalf("attaching to a desk with a reply unseen ran claude with %q", args)
	}
	fc.setStatus("00000002", "")
	mustCall(t, Request{Op: "attach", ID: "00000002"})
	if args := fc.lastStart(); !slices.Equal(args, []string{"--bg", "--resume", "s-d", "--", "/clear"}) {
		t.Fatalf("attaching again to the stale desk ran claude with %q", args)
	}
}

// The desk outlasts a restart of the daemon with the tells that wait for it.
func TestRestartAdoptsDesk(t *testing.T) {
	d, fc := testDesk(t)
	fc.setStatus("00000002", "busy")
	d.poll()
	mustCall(t, Request{Op: "tell", Text: "x"})
	fc.setStatus("00000002", "idle")
	d = restart(t, d)
	if err := d.ensureDesk(); err != nil {
		t.Fatal(err)
	}
	if args := awaitStarts(t, fc, 3); !slices.Equal(args, []string{"--bg", "--resume", "s-d", "--", deskReport([]Event{{Kind: "tell", Text: "x"}})}) {
		t.Fatalf("claude ran with %q", args)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if desk := d.nodes[deskNode]; desk.ID != "00000002" || len(desk.Events) != 0 {
		t.Fatalf("desk = %+v", desk)
	}
}
