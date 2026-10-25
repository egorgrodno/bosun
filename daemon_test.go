package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClaude struct {
	mu      sync.Mutex
	agents  []agent
	starts  [][]string
	dirs    []string
	removed []string
	fail    func(args []string) error
}

func (f *fakeClaude) Start(dir string, args []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, args)
	f.dirs = append(f.dirs, dir)
	if f.fail != nil {
		if err := f.fail(args); err != nil {
			return "", err
		}
	}
	id := fmt.Sprintf("%08x", len(f.starts))
	name := ""
	if i := slices.Index(args, "-n"); i >= 0 {
		name = args[i+1]
	}
	f.agents = append(f.agents, agent{ID: id, Name: name, Status: "busy"})
	return id, nil
}

func (f *fakeClaude) lastStart() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts[len(f.starts)-1]
}

func (f *fakeClaude) Remove(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	f.agents = slices.DeleteFunc(f.agents, func(a agent) bool { return a.ID == id })
	return nil
}

// argAfter returns the value after name in args, or "" when args lack it.
func argAfter(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func (f *fakeClaude) Agents() ([]agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.agents), nil
}

func (f *fakeClaude) Stop(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agents = slices.DeleteFunc(f.agents, func(a agent) bool { return a.ID == id })
	return nil
}

const worktree = "/repo/.claude/worktrees/a"

// testDaemon serves a daemon with a master and a worker "a" on a socket the hook and call reach through BOSUN_SOCKET.
func testDaemon(t *testing.T) (*daemon, *fakeClaude) {
	dir := t.TempDir()
	// t.TempDir() runs deep inside a worker's sandbox, so the daemon listens on an abstract socket, whose name no folder lengthens.
	// No fake session creates the sandbox's sockets, so a node's TMPDIR may run past their room.
	sock := fmt.Sprintf("@bosun-%d-%s", os.Getpid(), t.Name())
	room := tempDirRoom
	tempDirRoom = math.MaxInt
	t.Cleanup(func() { tempDirRoom = room })
	t.Setenv("BOSUN_SOCKET", sock)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	fc := &fakeClaude{}
	d := newDaemon("/bin/bosun", sock, filepath.Join(dir, "state"), filepath.Join(dir, "master"), fc)
	ln, err := listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go d.serve(ln)
	if err := d.ensureMaster(); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.nodes["a"] = &Session{Node: "a", Parent: masterNode, Repo: "/repo", Dir: worktree, Depth: 1, ID: "aaaaaaaa", Status: "busy"}
	d.mu.Unlock()
	return d, fc
}

func hookEvent(event, toolUseID, tool string, input map[string]any) []byte {
	raw, _ := json.Marshal(map[string]any{
		"hook_event_name": event,
		"session_id":      "s-a",
		"tool_use_id":     toolUseID,
		"tool_name":       tool,
		"tool_input":      input,
	})
	return raw
}

func runHookAsync(node string, raw []byte) chan string {
	ch := make(chan string, 1)
	go func() {
		out := hookOutput(node, bytes.NewReader(raw))
		data, _ := json.Marshal(out)
		ch <- string(data)
	}()
	return ch
}

func receive(t *testing.T, ch chan string) string {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("hook did not return")
		return ""
	}
}

func mustCall(t *testing.T, req Request) Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := call(ctx, req)
	if err != nil {
		t.Fatalf("%s: %v", req.Op, err)
	}
	return resp
}

// openDecision waits until the daemon holds n open decisions and returns the newest.
func openDecision(t *testing.T, n int) Decision {
	t.Helper()
	for range 500 {
		resp := mustCall(t, Request{Op: "status"})
		if len(resp.Decisions) == n {
			return resp.Decisions[n-1]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %d open decisions", n)
	return Decision{}
}

var bashTest = map[string]any{"command": "npm test"}

func TestMasterAllowsPermission(t *testing.T) {
	testDaemon(t)
	if out := receive(t, runHookAsync("a", hookEvent("PreToolUse", "t1", "Bash", bashTest))); out != "null" {
		t.Fatalf("PreToolUse of npm test printed %s", out)
	}
	hook := runHookAsync("a", hookEvent("PermissionRequest", "", "Bash", bashTest))

	events := mustCall(t, Request{Op: "next_event", Node: masterNode}).Events
	if len(events) != 1 || events[0].Decision == nil || events[0].Decision.Kind != "permission" || events[0].Decision.Holder != "master" {
		t.Fatalf("events = %+v", events)
	}
	mustCall(t, Request{Op: "approve", Node: masterNode, ID: events[0].Decision.ID})

	want := `{"hookSpecificOutput":{"decision":{"behavior":"allow"},"hookEventName":"PermissionRequest"}}`
	if out := receive(t, hook); out != want {
		t.Fatalf("hook printed %s, want %s", out, want)
	}
}

func TestEscalatedPermissionWaitsOnUser(t *testing.T) {
	testDaemon(t)
	hook := runHookAsync("a", hookEvent("PermissionRequest", "", "Bash", bashTest))
	dec := mustCall(t, Request{Op: "next_event", Node: masterNode}).Events[0].Decision
	mustCall(t, Request{Op: "escalate", Node: masterNode, ID: dec.ID, Text: "touches CI"})

	if _, err := call(context.Background(), Request{Op: "approve", Node: masterNode, ID: dec.ID}); err == nil {
		t.Fatal("the master approved a decision escalated to the user")
	}
	if got := openDecision(t, 1); got.Holder != "user" || got.Note != "touches CI" {
		t.Fatalf("decision = %+v", got)
	}
	mustCall(t, Request{Op: "deny", ID: dec.ID, Text: "run the unit tests only"})

	want := `{"hookSpecificOutput":{"decision":{"behavior":"deny","message":"run the unit tests only"},"hookEventName":"PermissionRequest"}}`
	if out := receive(t, hook); out != want {
		t.Fatalf("hook printed %s, want %s", out, want)
	}
}

func TestMasterNotifiesUser(t *testing.T) {
	d, _ := testDaemon(t)
	if _, err := call(context.Background(), Request{Op: "notify", Node: "a", Text: "x"}); err == nil {
		t.Fatal("a worker notified the user")
	}
	mustCall(t, Request{Op: "notify", Node: masterNode, Text: "/src/app is not trusted, so b did not start"})
	dec := openDecision(t, 1)
	if dec.Kind != "notice" || dec.Holder != "user" || dec.Note != "/src/app is not trusted, so b did not start" {
		t.Fatalf("decision = %+v", dec)
	}
	d.mu.Lock()
	events := len(d.events)
	d.mu.Unlock()
	if events != 0 {
		t.Fatalf("the master got %d events", events)
	}
	for _, r := range []Request{{Op: "dismiss", Node: masterNode, ID: dec.ID}, {Op: "approve", ID: dec.ID}} {
		if _, err := call(context.Background(), r); err == nil {
			t.Fatalf("%s by %q settled a notice", r.Op, r.Node)
		}
	}
	mustCall(t, Request{Op: "dismiss", ID: dec.ID})
	if n := len(mustCall(t, Request{Op: "status"}).Decisions); n != 0 {
		t.Fatalf("%d decisions open after the dismiss", n)
	}
}

func TestMCPNotify(t *testing.T) {
	testDaemon(t)
	args := json.RawMessage(`{"message":"b failed"}`)
	if _, err := newMCPServer("a", io.Discard).run(context.Background(), "notify", args); err == nil {
		t.Fatal("a worker has notify")
	}
	if _, err := newMCPServer(masterNode, io.Discard).run(context.Background(), "notify", args); err != nil {
		t.Fatal(err)
	}
	if dec := openDecision(t, 1); dec.Kind != "notice" || dec.Note != "b failed" {
		t.Fatalf("decision = %+v", dec)
	}
}

func TestMasterAnswersQuestion(t *testing.T) {
	testDaemon(t)
	answer := make(chan string, 1)
	go func() {
		resp, err := call(context.Background(), Request{Op: "ask", Node: "a", Text: "X or Y?"})
		if err != nil {
			answer <- err.Error()
		}
		answer <- resp.Text
	}()
	dec := mustCall(t, Request{Op: "next_event", Node: masterNode}).Events[0].Decision
	if dec.Kind != "question" || dec.Question != "X or Y?" {
		t.Fatalf("decision = %+v", dec)
	}
	mustCall(t, Request{Op: "reply", Node: masterNode, ID: dec.ID, Text: "X"})
	if got := receive(t, answer); got != "X" {
		t.Fatalf("ask got %q", got)
	}
}

func TestReservedSkipsMaster(t *testing.T) {
	d, _ := testDaemon(t)
	hook := runHookAsync("a", hookEvent("PreToolUse", "t1", "Bash", map[string]any{"command": "git push origin HEAD"}))
	dec := openDecision(t, 1)
	if dec.Holder != "user" || dec.Reserved != "git push" {
		t.Fatalf("decision = %+v", dec)
	}
	d.mu.Lock()
	queued := len(d.events)
	d.mu.Unlock()
	if queued != 0 {
		t.Fatalf("the master was sent %d events", queued)
	}
	if _, err := call(context.Background(), Request{Op: "approve", Node: masterNode, ID: dec.ID}); err == nil {
		t.Fatal("the master approved a reserved action")
	}
	mustCall(t, Request{Op: "approve", ID: dec.ID})

	want := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow"}}`
	if out := receive(t, hook); out != want {
		t.Fatalf("hook printed %s, want %s", out, want)
	}
}

func TestReservedSparesClaudeCodeDirs(t *testing.T) {
	d, _ := testDaemon(t)
	claude := os.Getenv("CLAUDE_CONFIG_DIR")
	for _, p := range []string{
		filepath.Join(claude, "projects", "-repo", "memory", "feedback.md"),
		filepath.Join(claude, "jobs", "aaaaaaaa", "tmp", "query.sql"),
		filepath.Join(d.scratchDir("a"), "probe.sh"),
	} {
		write := map[string]any{"file_path": p, "content": "x"}
		if out := receive(t, runHookAsync("a", hookEvent("PreToolUse", "t1", "Write", write))); out != "null" {
			t.Fatalf("PreToolUse of a write to %s printed %s", p, out)
		}
	}
	state := map[string]any{"file_path": filepath.Join(claude, "jobs", "aaaaaaaa", "state.json"), "content": "x"}
	hook := runHookAsync("a", hookEvent("PreToolUse", "t2", "Write", state))
	dec := openDecision(t, 1)
	if dec.Reserved != "a write outside the worktree" {
		t.Fatalf("decision = %+v", dec)
	}
	mustCall(t, Request{Op: "deny", ID: dec.ID})
	receive(t, hook)
}

func TestMemoryDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/c")
	worker := Session{Repo: "/home/u/Projects/app", Dir: "/home/u/Projects/app/.claude/worktrees/a"}
	if got, want := worker.memoryDir(), "/c/projects/-home-u-Projects-app/memory"; got != want {
		t.Errorf("worker: %s, want %s", got, want)
	}
	master := Session{Dir: "/home/u/.local/state/bosun/master"}
	if got, want := master.memoryDir(), "/c/projects/-home-u--local-state-bosun-master/memory"; got != want {
		t.Errorf("master: %s, want %s", got, want)
	}
}

func TestReservedDeniedWithoutDaemon(t *testing.T) {
	t.Setenv("BOSUN_SOCKET", filepath.Join(t.TempDir(), "missing"))
	out := receive(t, runHookAsync("a", hookEvent("PreToolUse", "t1", "Bash", bashTest)))
	if !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Fatalf("hook printed %s", out)
	}
	if out := receive(t, runHookAsync("a", hookEvent("PermissionRequest", "", "Bash", bashTest))); out != "null" {
		t.Fatalf("PermissionRequest printed %s, want nothing", out)
	}
}

// An answer in the session's own prompt settles the call: a tool that ran sends PostToolUse, and a denial runs none, so the next call's PreToolUse comes instead.
func TestSessionAnswerWithdraws(t *testing.T) {
	for _, next := range []string{"PostToolUse", "PreToolUse", "Stop"} {
		t.Run(next, func(t *testing.T) {
			testDaemon(t)
			receive(t, runHookAsync("a", hookEvent("PreToolUse", "t1", "Bash", bashTest)))
			hook := runHookAsync("a", hookEvent("PermissionRequest", "", "Bash", bashTest))
			openDecision(t, 1)

			id := "t1"
			if next == "PreToolUse" {
				id = "t2"
			}
			receive(t, runHookAsync("a", hookEvent(next, id, "Bash", bashTest)))
			if out := receive(t, hook); out != "null" {
				t.Fatalf("withdrawn hook printed %s", out)
			}
			if n := len(mustCall(t, Request{Op: "status"}).Decisions); n != 0 {
				t.Fatalf("%d decisions still open", n)
			}
		})
	}
}

func TestWorkerExitTellsMaster(t *testing.T) {
	d, fc := testDaemon(t)
	fc.mu.Lock()
	fc.agents = append(fc.agents, agent{ID: "aaaaaaaa", Name: "a", Status: "busy"})
	fc.mu.Unlock()
	d.poll()
	mustCall(t, Request{Op: "kill", Name: "a"})
	d.poll()
	events := mustCall(t, Request{Op: "next_event", Node: masterNode}).Events
	if len(events) != 1 || events[0].Kind != "exit" || events[0].Node != "a" {
		t.Fatalf("events = %+v", events)
	}
}

func TestResume(t *testing.T) {
	d, fc := testDaemon(t)
	idleWait = 0
	t.Cleanup(func() { idleWait = 30 * time.Second })
	fc.mu.Lock()
	fc.agents = append(fc.agents, agent{ID: "aaaaaaaa", SessionID: "s-a", Name: "a", Status: "busy"})
	fc.mu.Unlock()
	d.poll()
	resume := Request{Op: "resume", Node: masterNode, Name: "a", Text: "fix the review"}
	if _, err := call(context.Background(), resume); err == nil {
		t.Fatal("resumed a worker in the middle of its turn")
	}
	fc.mu.Lock()
	fc.agents[len(fc.agents)-1].Status = "idle"
	fc.mu.Unlock()
	if _, err := call(context.Background(), Request{Op: "resume", Node: "a", Name: "a", Text: "x"}); err == nil {
		t.Fatal("a worker resumed itself")
	}

	mustCall(t, resume)
	fc.mu.Lock()
	dir := fc.dirs[len(fc.dirs)-1]
	stopped := !slices.ContainsFunc(fc.agents, func(a agent) bool { return a.ID == "aaaaaaaa" })
	fc.mu.Unlock()
	if args := fc.lastStart(); dir != worktree || !slices.Equal(args, []string{"--bg", "--resume", "s-a", "--", "fix the review"}) {
		t.Fatalf("claude ran in %s with %q", dir, args)
	}
	if !stopped {
		t.Fatal("the worker was resumed without being stopped, which starts a copy")
	}
	d.poll()
	d.mu.Lock()
	defer d.mu.Unlock()
	if a := d.nodes["a"]; a.ID == "aaaaaaaa" || a.ID == "" {
		t.Fatalf("a = %+v, want the id claude printed", a)
	}
	if len(d.events) != 0 {
		t.Fatalf("the master got %+v", d.events)
	}
}

// A daemon that stopped left the master's calls failing, so the master may have ended its turn.
func TestRestartResumesIdleMaster(t *testing.T) {
	d, fc := testDaemon(t)
	idleWait = 0
	t.Cleanup(func() { idleWait = 30 * time.Second })
	if err := d.ensureMaster(); err != nil || len(fc.starts) != 1 {
		t.Fatalf("adopting a busy master: %v, %d starts", err, len(fc.starts))
	}
	fc.mu.Lock()
	fc.agents[0].SessionID, fc.agents[0].Status = "s-m", "idle"
	fc.mu.Unlock()
	if err := d.ensureMaster(); err != nil {
		t.Fatal(err)
	}
	fc.mu.Lock()
	stopped := !slices.ContainsFunc(fc.agents, func(a agent) bool { return a.ID == "00000001" })
	fc.mu.Unlock()
	if args := fc.lastStart(); len(args) != 5 || !slices.Equal(args[:4], []string{"--bg", "--resume", "s-m", "--"}) || !stopped {
		t.Fatalf("claude ran with %q, the old master stopped: %v", args, stopped)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if id := d.nodes[masterNode].ID; id != "00000002" {
		t.Fatalf("the master's id is %q, want the resumed session's", id)
	}
}

func TestMasterStopBlocks(t *testing.T) {
	testDaemon(t)
	stop := func() string { return receive(t, runHookAsync(masterNode, hookEvent("Stop", "", "", nil))) }
	block := `{"decision":"block","reason":`
	if out := stop(); !strings.HasPrefix(out, block) {
		t.Fatalf("the master's Stop printed %s", out)
	}
	mustCall(t, Request{Op: "tell", Text: "x"})
	mustCall(t, Request{Op: "next_event", Node: masterNode})
	if out := stop(); !strings.HasPrefix(out, block) {
		t.Fatalf("a Stop after next_event printed %s", out)
	}
	if n := len(mustCall(t, Request{Op: "status"}).Decisions); n != 0 {
		t.Fatalf("%d decisions open", n)
	}
	// A Stop that the block did not send back to next_event lets the turn end rather than loop, and the user hears of it.
	if out := stop(); out != "null" {
		t.Fatalf("the master's second Stop in a row printed %s", out)
	}
	if dec := openDecision(t, 1); dec.Kind != "notice" {
		t.Fatalf("decision = %+v", dec)
	}
}

func TestSpawn(t *testing.T) {
	d, fc := testDaemon(t)
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1234")
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	s := mustCall(t, Request{Op: "spawn", Node: masterNode, Name: "b", Repo: repo, Text: "fix the bug"}).Session
	if want := filepath.Join(repo, ".claude", "worktrees", "b"); s.Dir != want || s.Parent != masterNode || s.Depth != 1 {
		t.Fatalf("session = %+v", s)
	}
	fc.mu.Lock()
	args, dir := fc.starts[len(fc.starts)-1], fc.dirs[len(fc.dirs)-1]
	fc.mu.Unlock()
	if dir != repo || !slices.Contains(args, "-w") || args[len(args)-1] != "fix the bug" || slices.Contains(args, "--model") {
		t.Fatalf("claude ran in %s with %q", dir, args)
	}

	data, err := os.ReadFile(args[slices.Index(args, "--settings")+1])
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Worktree    map[string]string      `json:"worktree"`
		Memory      string                 `json:"autoMemoryDirectory"`
		Permissions map[string][]string    `json:"permissions"`
		Env         map[string]string      `json:"env"`
		Hooks       map[string][]hookGroup `json:"hooks"`
		Sandbox     struct {
			Enabled    bool                `json:"enabled"`
			Filesystem map[string][]string `json:"filesystem"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	if settings.Worktree["baseRef"] != "head" || !slices.Contains(settings.Permissions["deny"], "AskUserQuestion") {
		t.Fatalf("settings = %s", data)
	}
	if want := (&Session{Repo: repo}).memoryDir(); settings.Memory != want {
		t.Fatalf("autoMemoryDirectory = %q, want %q", settings.Memory, want)
	}
	if deny := settings.Sandbox.Filesystem["denyRead"]; !settings.Sandbox.Enabled || !slices.Contains(deny, d.socket) || !slices.Contains(deny, "/run/user/1234") {
		t.Fatalf("sandbox = %+v, want it on with the socket and the runtime folder hidden", settings.Sandbox)
	}
	if tmp := settings.Env["TMPDIR"]; tmp != d.tempDir("b") {
		t.Fatalf("TMPDIR = %q, want %q", tmp, d.tempDir("b"))
	} else if info, err := os.Stat(tmp); err != nil || !info.IsDir() {
		t.Fatalf("TMPDIR: %v", err)
	}
	if system := argAfter(args, "--append-system-prompt"); !strings.Contains(system, d.scratchDir("b")) {
		t.Fatalf("the system prompt names no scratch folder: %s", system)
	}
	if cmd := settings.Hooks["PreToolUse"][0].Hooks[0].Command; !strings.Contains(cmd, "BOSUN_NODE=b ") || !strings.HasSuffix(cmd, "'/bin/bosun' hook") {
		t.Fatalf("hook command = %s", cmd)
	}

	if _, err := call(context.Background(), Request{Op: "spawn", Node: masterNode, Name: "b", Repo: repo, Text: "again"}); err == nil {
		t.Fatal("spawned a second b")
	}
	// With the fixture's a, these fill the master's four children.
	for _, name := range []string{"c", "d"} {
		mustCall(t, Request{Op: "spawn", Node: masterNode, Name: name, Repo: repo, Text: "x"})
	}
	if _, err := call(context.Background(), Request{Op: "spawn", Node: masterNode, Name: "e", Repo: repo, Text: "x"}); err == nil {
		t.Fatal("the master ran more children than the cap")
	}
	d.mu.Lock()
	child := d.nodes["b"].Depth
	d.mu.Unlock()
	if child != 1 {
		t.Fatalf("b at depth %d", child)
	}
}

func TestSpawnLeavesRoomForSandboxSockets(t *testing.T) {
	// A 29-byte state path gives a node a TMPDIR of 40 bytes plus its name.
	d := newDaemon("/bin/bosun", "sock", "/home/egor/.local/state/bosun", "/master", &fakeClaude{})
	d.nodes[masterNode] = &Session{Node: masterNode}
	for _, tc := range []struct {
		length  int
		refused bool
	}{{33, false}, {34, true}} {
		// No repo, so a name the check lets through fails on that instead, before anything is written.
		_, err := d.spawn("", strings.Repeat("n", tc.length), "", "", "x")
		if refused := err != nil && strings.Contains(err.Error(), "sockets"); refused != tc.refused {
			t.Errorf("spawn of a %d-character name: %v", tc.length, err)
		}
	}
}

func TestModels(t *testing.T) {
	d, fc := testDaemon(t)
	d.masterModel, d.workerModel = "fable", "sonnet"
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	for _, tc := range []struct{ name, model, want string }{
		{"b", "", "sonnet"},
		{"c", "haiku", "haiku"},
	} {
		s := mustCall(t, Request{Op: "spawn", Name: tc.name, Repo: repo, Model: tc.model, Text: "x"}).Session
		if got := argAfter(fc.lastStart(), "--model"); s.Model != tc.want || got != tc.want {
			t.Errorf("spawn %s with model %q: session model %q, --model %q, want %q", tc.name, tc.model, s.Model, got, tc.want)
		}
	}

	fc.mu.Lock()
	fc.agents = nil
	fc.mu.Unlock()
	if err := d.ensureMaster(); err != nil {
		t.Fatal(err)
	}
	if got := argAfter(fc.lastStart(), "--model"); got != "fable" {
		t.Errorf("a new master runs --model %q, want fable", got)
	}
}

// The workers block on the daemon, not on the master, so a switch leaves them running.
func TestSwitchModel(t *testing.T) {
	d, fc := testDaemon(t)
	d.mu.Lock()
	old := d.nodes[masterNode].ID
	d.nodes[masterNode].SessionID = "s-master"
	d.mu.Unlock()
	hook := runHookAsync("a", hookEvent("PermissionRequest", "", "Bash", bashTest))
	// The old master takes the decision and stops before it answers.
	if events := mustCall(t, Request{Op: "next_event", Node: masterNode}).Events; len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}

	mustCall(t, Request{Op: "model", Model: "opus"})
	args := fc.lastStart()
	if argAfter(args, "--resume") != "s-master" || argAfter(args, "--model") != "opus" || argAfter(args, "--settings") == "" {
		t.Fatalf("claude ran with %q", args)
	}
	d.mu.Lock()
	m := *d.nodes[masterNode]
	d.mu.Unlock()
	if m.ID == old || m.SessionID != "" || m.Model != "opus" {
		t.Fatalf("master = %+v", m)
	}
	fc.mu.Lock()
	removed := slices.Clone(fc.removed)
	fc.mu.Unlock()
	if !slices.Equal(removed, []string{old}) {
		t.Fatalf("removed %v, want the old master %s", removed, old)
	}

	events := mustCall(t, Request{Op: "next_event", Node: masterNode}).Events
	if len(events) != 1 || events[0].Decision == nil {
		t.Fatalf("the new master got %+v, want the open decision", events)
	}
	mustCall(t, Request{Op: "approve", Node: masterNode, ID: events[0].Decision.ID})
	if out := receive(t, hook); !strings.Contains(out, `"behavior":"allow"`) {
		t.Fatalf("hook printed %s", out)
	}
}

func TestSwitchModelFailureResumesMaster(t *testing.T) {
	d, fc := testDaemon(t)
	d.mu.Lock()
	d.nodes[masterNode].SessionID = "s-master"
	d.mu.Unlock()
	fc.mu.Lock()
	fc.fail = func(args []string) error {
		if slices.Contains(args, "--model") {
			return fmt.Errorf("unknown model")
		}
		return nil
	}
	fc.mu.Unlock()

	if _, err := call(context.Background(), Request{Op: "model", Model: "nope"}); err == nil {
		t.Fatal("the switch to a failing model succeeded")
	}
	if args := fc.lastStart(); !slices.Equal(args, []string{"--bg", "--resume", "s-master"}) {
		t.Fatalf("after the failure claude ran with %q, want the old master resumed", args)
	}
	d.mu.Lock()
	model := d.nodes[masterNode].Model
	d.mu.Unlock()
	if model != "" {
		t.Fatalf("master model = %q", model)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func TestSessionStartStandsInGitmodules(t *testing.T) {
	d, _ := testDaemon(t)
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(repo, ".claude", "worktrees", "a")
	git(t, repo, "worktree", "add", "-q", "-b", "worktree-a", wt)
	d.mu.Lock()
	d.nodes["a"].Repo, d.nodes["a"].Dir = repo, wt
	d.mu.Unlock()

	receive(t, runHookAsync("a", hookEvent("SessionStart", "", "", nil)))
	if info, err := os.Stat(filepath.Join(wt, ".gitmodules")); err != nil || info.Size() != 0 {
		t.Fatalf(".gitmodules: %v, %v", info, err)
	}

	// Regular files stand in for the sandbox's /dev/null mounts, which git ignores by name alike.
	for _, p := range []string{".bashrc", ".claude/settings.json", ".claude/.cc-writes/x"} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(wt, p)), 0o755)
		_ = os.WriteFile(filepath.Join(wt, p), nil, 0o644)
	}
	d.mu.Lock()
	a := d.nodes["a"]
	d.mu.Unlock()
	settingsPath, _, err := d.writeConfig(a)
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	data, _ := os.ReadFile(settingsPath)
	_ = json.Unmarshal(data, &settings)
	cmd := exec.Command("git", "-C", wt, "status", "--porcelain")
	cmd.Env = os.Environ()
	for k, v := range settings.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if out, err := cmd.Output(); err != nil || len(out) > 0 {
		t.Fatalf("git status in the session's environment: %q, %v", out, err)
	}
	if status := git(t, wt, "status", "--porcelain"); !strings.Contains(status, ".gitmodules") {
		t.Fatalf("git status outside the session's environment: %q", status)
	}
}

func TestMCPAskSendsProgress(t *testing.T) {
	testDaemon(t)
	progressEvery = 20 * time.Millisecond
	t.Cleanup(func() { progressEvery = 10 * time.Second })

	in, stdin := io.Pipe()
	stdout, out := io.Pipe()
	go func() { _ = newMCPServer("a", out).serve(in) }()
	t.Cleanup(func() { stdin.Close() })
	lines := bufio.NewScanner(stdout)
	send := func(s string) { _, _ = io.WriteString(stdin, s+"\n") }
	read := func() map[string]any {
		t.Helper()
		if !lines.Scan() {
			t.Fatal("the MCP server closed")
		}
		var m map[string]any
		_ = json.Unmarshal(lines.Bytes(), &m)
		return m
	}

	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if v := read()["result"].(map[string]any)["protocolVersion"]; v != "2025-06-18" {
		t.Fatalf("protocolVersion = %v", v)
	}
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var names []string
	for _, tl := range read()["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if !slices.Equal(names, []string{"spawn", "ask"}) {
		t.Fatalf("a worker lists %v", names)
	}

	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ask","arguments":{"question":"X or Y?"},"_meta":{"progressToken":"p"}}}`)
	if m := read(); m["method"] != "notifications/progress" {
		t.Fatalf("got %v, want progress", m)
	}
	dec := openDecision(t, 1)
	mustCall(t, Request{Op: "reply", ID: dec.ID, Text: "Y"})
	for {
		m := read()
		if m["method"] == "notifications/progress" {
			continue
		}
		text := m["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
		if m["id"] != float64(3) || text != "Y" {
			t.Fatalf("got %v", m)
		}
		return
	}
}
