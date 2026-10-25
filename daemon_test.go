package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	mu     sync.Mutex
	agents []agent
	starts [][]string
	dirs   []string
}

func (f *fakeClaude) Start(dir string, args []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("%08x", len(f.starts)+1)
	f.starts = append(f.starts, args)
	f.dirs = append(f.dirs, dir)
	f.agents = append(f.agents, agent{ID: id, Name: args[slices.Index(args, "-n")+1], Status: "busy"})
	return id, nil
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
	sock := filepath.Join(dir, "sock")
	t.Setenv("BOSUN_SOCKET", sock)
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

func TestSpawn(t *testing.T) {
	d, fc := testDaemon(t)
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
	if dir != repo || !slices.Contains(args, "-w") || args[len(args)-1] != "fix the bug" {
		t.Fatalf("claude ran in %s with %q", dir, args)
	}

	data, err := os.ReadFile(args[slices.Index(args, "--settings")+1])
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Worktree    map[string]string      `json:"worktree"`
		Permissions map[string][]string    `json:"permissions"`
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
	if !settings.Sandbox.Enabled || !slices.Contains(settings.Sandbox.Filesystem["denyRead"], d.socket) {
		t.Fatalf("sandbox = %+v, want it on with the socket hidden", settings.Sandbox)
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
	settingsPath, _, err := d.writeConfig("a")
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
