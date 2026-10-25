package main

import (
	"bufio"
	"bytes"
	"cmp"
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

// fakeClaude lists a stopped session without a PID or Status, as `claude agents --json --all` does, until Remove.
type fakeClaude struct {
	mu      sync.Mutex
	agents  []agent
	starts  [][]string
	dirs    []string
	stops   []string
	removed []string
	copies  []string
	// discard holds the --discard-unpushed value that claude rm asks for, for a session whose branch has commits on no remote.
	discard    map[string]string
	fail       func(args []string) error
	supervisor int
	// linger keeps a stopped session's process alive for that many more looks at it, as Claude Code lists the session stopped before its process exits.
	linger int
	// procs holds each session's latest process, and live each live process with the looks left before it exits, or -1 while it runs.
	procs   map[string]int
	live    map[int]int
	nextPID int
}

func (f *fakeClaude) spawnLocked(id string) int {
	if f.procs == nil {
		f.procs, f.live = map[string]int{}, map[int]int{}
	}
	f.nextPID++
	pid := 1000 + f.nextPID
	f.procs[id], f.live[pid] = pid, -1
	return pid
}

// tickLocked counts down the processes of stopped sessions.
func (f *fakeClaude) tickLocked() {
	for pid, n := range f.live {
		switch {
		case n > 0:
			f.live[pid] = n - 1
		case n == 0:
			delete(f.live, pid)
		}
	}
}

func (f *fakeClaude) alive(pid int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tickLocked()
	_, ok := f.live[pid]
	return ok
}

func (f *fakeClaude) Supervisor() (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.supervisor, nil
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
	// A resume without flags continues a stopped session under its own id, once its process exits, and makes a copy otherwise.
	if sid := argAfter(args, "--resume"); sid != "" {
		i := slices.IndexFunc(f.agents, func(a agent) bool { return a.SessionID == sid })
		flagless := len(args) == 3 || args[3] == "--"
		if i >= 0 {
			if _, running := f.live[f.procs[f.agents[i].ID]]; running || f.agents[i].Status != "" {
				f.copies = append(f.copies, id)
			} else if flagless {
				a := &f.agents[i]
				a.PID, a.Status = f.spawnLocked(a.ID), "busy"
				return a.ID, nil
			}
		}
	}
	name := ""
	if i := slices.Index(args, "-n"); i >= 0 {
		name = args[i+1]
	}
	f.agents = append(f.agents, agent{ID: id, Name: name, PID: f.spawnLocked(id), Status: "busy"})
	return id, nil
}

func (f *fakeClaude) lastStart() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts[len(f.starts)-1]
}

func (f *fakeClaude) Remove(id, discard string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if want := f.discard[id]; want != "" && discard != want {
		return fmt.Errorf("claude rm %s: exit status 1: kept %s\n  discard the worktree and its commits: claude rm %s --discard-unpushed %s", id, id, id, want)
	}
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
	f.tickLocked()
	return slices.Clone(f.agents), nil
}

func (f *fakeClaude) Stop(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, id)
	if i := slices.IndexFunc(f.agents, func(a agent) bool { return a.ID == id }); i >= 0 {
		f.agents[i].PID, f.agents[i].Status = 0, ""
	}
	if pid, ok := f.procs[id]; ok && f.live[pid] == -1 {
		f.live[pid] = f.linger
	}
	return nil
}

// add lists a, running in a process of its own.
func (f *fakeClaude) add(a agent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a.PID = f.spawnLocked(a.ID)
	f.agents = append(f.agents, a)
}

// setStatus sets the status Claude Code lists for session id. A session with none has no process, as one that stopped long ago.
func (f *fakeClaude) setStatus(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agents[slices.IndexFunc(f.agents, func(a agent) bool { return a.ID == id })].Status = status
	if status == "" {
		delete(f.live, f.procs[id])
	}
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
	alive := processAlive
	processAlive = fc.alive
	t.Cleanup(func() { processAlive = alive })
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
	fc.setStatus("aaaaaaaa", "")
	d.poll()
	events := mustCall(t, Request{Op: "next_event", Node: masterNode}).Events
	if len(events) != 1 || events[0].Kind != "exit" || events[0].Node != "a" || events[0].Text != "" {
		t.Fatalf("events = %+v", events)
	}
}

// A copy lacks the session's options, bosun's hooks and sandbox among them, so it must not stand in for the worker.
func TestResumeIntoCopy(t *testing.T) {
	d, fc := testDaemon(t)
	fc.add(agent{ID: "aaaaaaaa", SessionID: "s-a", Name: "a", Status: "idle"})
	fc.mu.Lock()
	// The daemon cannot wait for a process the listing does not name.
	fc.agents[len(fc.agents)-1].PID = 0
	fc.linger = 1000
	fc.mu.Unlock()
	d.poll()
	if _, err := call(context.Background(), Request{Op: "resume", Node: masterNode, Name: "a", Text: "x"}); err == nil {
		t.Fatal("a resume into a copy succeeded")
	}
	fc.mu.Lock()
	copies, stops := slices.Clone(fc.copies), slices.Clone(fc.stops)
	fc.mu.Unlock()
	if len(copies) != 1 || !slices.Contains(stops, copies[0]) {
		t.Fatalf("copies %v, stops %v: the copy runs on", copies, stops)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if a := d.nodes["a"]; a.ID != "aaaaaaaa" {
		t.Fatalf("a = %+v, want the session it was", a)
	}
}

// Claude Code stops a session idle for an hour, and lists it as it lists one `claude stop` stopped.
func TestRetiredWorkerResumes(t *testing.T) {
	d, fc := testDaemon(t)
	fc.mu.Lock()
	fc.agents = append(fc.agents, agent{ID: "aaaaaaaa", SessionID: "s-a", Name: "a", Status: "idle"})
	fc.mu.Unlock()
	d.poll()
	fc.setStatus("aaaaaaaa", "")
	d.poll()
	d.mu.Lock()
	status, events := d.nodes["a"].Status, len(d.events)
	d.mu.Unlock()
	if status != "stopped" || events != 0 {
		t.Fatalf("the retired worker is %q, and the master got %d events", status, events)
	}

	mustCall(t, Request{Op: "resume", Node: masterNode, Name: "a", Text: "fix the review"})
	fc.mu.Lock()
	stops := len(fc.stops)
	fc.mu.Unlock()
	if args := fc.lastStart(); !slices.Equal(args, []string{"--bg", "--resume", "s-a", "--", "fix the review"}) || stops != 0 {
		t.Fatalf("claude ran with %q after %d stops", args, stops)
	}
	d.poll()
	d.mu.Lock()
	defer d.mu.Unlock()
	if a := d.nodes["a"]; a.ID != "aaaaaaaa" || a.Status != "busy" {
		t.Fatalf("a = %+v", a)
	}
}

func TestKill(t *testing.T) {
	d, fc := testDaemon(t)
	fc.mu.Lock()
	fc.agents = append(fc.agents, agent{ID: "aaaaaaaa", SessionID: "s-a", Name: "a", Status: "busy"})
	fc.mu.Unlock()
	d.poll()
	mustCall(t, Request{Op: "kill", Name: "a"})
	d.poll()
	events := mustCall(t, Request{Op: "next_event", Node: masterNode}).Events
	if len(events) != 1 || events[0].Kind != "exit" || events[0].Node != "a" || events[0].Text == "" {
		t.Fatalf("events = %+v", events)
	}
	if _, err := call(context.Background(), Request{Op: "resume", Node: masterNode, Name: "a", Text: "x"}); err == nil {
		t.Fatal("the master resumed a worker the user stopped")
	}
}

func TestKillMaster(t *testing.T) {
	d, fc := testDaemon(t)
	fc.mu.Lock()
	fc.agents[0].SessionID = "s-m"
	fc.mu.Unlock()
	d.poll()
	// The poll found a, which Claude Code does not list, gone.
	mustCall(t, Request{Op: "next_event", Node: masterNode})
	mustCall(t, Request{Op: "kill", Name: masterNode})
	d.poll()
	mustCall(t, Request{Op: "tell", Text: "x"})
	awaitWoken(t, d, fc, 2)

	// Events that wait when the master stops would otherwise wait for one more.
	d.poll()
	mustCall(t, Request{Op: "next_event", Node: masterNode})
	mustCall(t, Request{Op: "tell", Text: "y"})
	mustCall(t, Request{Op: "kill", Name: masterNode})
	d.poll()
	awaitWoken(t, d, fc, 3)
}

func TestKillDuringResume(t *testing.T) {
	d, fc := testDaemon(t)
	fc.mu.Lock()
	fc.agents = append(fc.agents, agent{ID: "aaaaaaaa", SessionID: "s-a", Name: "a", Status: "busy"})
	fc.mu.Unlock()
	d.poll()
	done := make(chan error, 1)
	go func() {
		_, err := call(context.Background(), Request{Op: "resume", Node: masterNode, Name: "a", Text: "x"})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	mustCall(t, Request{Op: "kill", Name: "a"})
	if err := <-done; err == nil {
		t.Fatal("a resume that waited for the worker to go idle revived it after the user stopped it")
	}
}

func TestResumeWhileStarting(t *testing.T) {
	d, fc := testDaemon(t)
	fc.mu.Lock()
	fc.agents = append(fc.agents, agent{ID: "aaaaaaaa", SessionID: "s-a", Name: "a"})
	fc.mu.Unlock()
	d.poll()
	mustCall(t, Request{Op: "resume", Node: masterNode, Name: "a", Text: "one"})
	// The listing may still show the session stopped.
	fc.setStatus("aaaaaaaa", "")
	if _, err := call(context.Background(), Request{Op: "resume", Node: masterNode, Name: "a", Text: "two"}); err == nil {
		t.Fatal("resumed a worker still starting from the last resume")
	}
}

// A switch and a wake would each resume the master, and the old master would run beside the new one.
func TestWakeAndSwitchExclude(t *testing.T) {
	d, fc := testDaemon(t)
	fc.mu.Lock()
	fc.agents[0].SessionID = "s-m"
	fc.mu.Unlock()
	d.mu.Lock()
	d.nodes[masterNode].SessionID = "s-m"
	d.mu.Unlock()
	d.poll()
	mustCall(t, Request{Op: "next_event", Node: masterNode})
	mustCall(t, Request{Op: "kill", Name: masterNode})
	d.poll()

	d.masterLock.Lock()
	if err := d.switchModel("opus"); err == nil {
		t.Fatal("switched the master while it was being woken")
	}
	mustCall(t, Request{Op: "tell", Text: "x"})
	time.Sleep(50 * time.Millisecond)
	fc.mu.Lock()
	starts := len(fc.starts)
	fc.mu.Unlock()
	if starts != 1 {
		t.Fatalf("woke the master while it was being switched: %d starts", starts)
	}
	d.masterLock.Unlock()
	awaitWoken(t, d, fc, 2)
}

func TestLoadRewritesSettings(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	old := newDaemon("/old/bosun", "sock", dir, "/master", &fakeClaude{})
	old.nodes[masterNode] = &Session{Node: masterNode, Dir: "/master"}
	if _, _, err := old.writeConfig(old.nodes[masterNode]); err != nil {
		t.Fatal(err)
	}
	old.saveLocked()

	link, err := linkExe(dir, "/new/bosun")
	if err != nil {
		t.Fatal(err)
	}
	d := newDaemon(link, "sock", dir, "/master", &fakeClaude{})
	if err := d.load(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"settings.json", "mcp.json"} {
		data, err := os.ReadFile(filepath.Join(dir, "nodes", masterNode, name))
		if err != nil || !strings.Contains(string(data), link) || strings.Contains(string(data), "/old/bosun") {
			t.Fatalf("%s: %v\n%s", name, err, data)
		}
	}
	if _, err := linkExe(dir, "/newer/bosun"); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(link); target != "/newer/bosun" {
		t.Fatalf("the link points at %q: %v", target, err)
	}
}

func TestForeignSupervisor(t *testing.T) {
	d, fc := testDaemon(t)
	cgroups := map[int]string{os.Getpid(): "0::/bosun.service", 7: "0::/bosun.service", 8: "0::/kitty-1.scope"}
	orig := cgroupOf
	cgroupOf = func(pid int) (string, error) { return cgroups[pid], nil }
	t.Cleanup(func() { cgroupOf = orig })
	master := func(pid, supervisor int) {
		fc.mu.Lock()
		fc.agents[0].PID, fc.supervisor = pid, supervisor
		fc.mu.Unlock()
		d.poll()
	}

	master(100, 7)
	if n := len(mustCall(t, Request{Op: "status"}).Decisions); n != 0 {
		t.Fatalf("%d decisions open for a master under the daemon's supervisor", n)
	}
	master(101, 8)
	if dec := openDecision(t, 1); dec.Kind != "notice" || !strings.Contains(dec.Note, "kitty-1.scope") {
		t.Fatalf("decision = %+v", dec)
	}
	// A master resumed under the same supervisor tells the user nothing new.
	master(102, 8)
	if n := len(mustCall(t, Request{Op: "status"}).Decisions); n != 1 {
		t.Fatalf("%d decisions open", n)
	}
}

func TestPollDue(t *testing.T) {
	d, fc := testDaemon(t)
	fc.mu.Lock()
	fc.agents = append(fc.agents, agent{ID: "aaaaaaaa", Name: "a", Status: "busy"})
	fc.mu.Unlock()
	d.poll()
	if !d.pollDue(time.Now()) {
		t.Fatal("no poll while sessions are in their turns")
	}
	fc.mu.Lock()
	for i := range fc.agents {
		fc.agents[i].Status = "idle"
	}
	fc.mu.Unlock()
	d.poll()
	now := time.Now()
	if d.pollDue(now) {
		t.Fatal("a poll while every session is idle")
	}
	if !d.pollDue(now.Add(quietPollEvery)) {
		t.Fatal("no poll after a quiet minute")
	}
	receive(t, runHookAsync("a", hookEvent("PostToolUse", "t1", "Bash", bashTest)))
	if !d.pollDue(now) {
		t.Fatal("no poll after a hook")
	}
}

func TestResume(t *testing.T) {
	d, fc := testDaemon(t)
	idleWait = 0
	t.Cleanup(func() { idleWait = 30 * time.Second })
	fc.add(agent{ID: "aaaaaaaa", SessionID: "s-a", Name: "a", Status: "busy"})
	d.poll()
	resume := Request{Op: "resume", Node: masterNode, Name: "a", Text: "fix the review"}
	if _, err := call(context.Background(), resume); err == nil {
		t.Fatal("resumed a worker in the middle of its turn")
	}
	fc.mu.Lock()
	fc.agents[len(fc.agents)-1].Status = "idle"
	fc.linger = 2
	fc.mu.Unlock()
	if _, err := call(context.Background(), Request{Op: "resume", Node: "a", Name: "a", Text: "x"}); err == nil {
		t.Fatal("a worker resumed itself")
	}

	mustCall(t, resume)
	fc.mu.Lock()
	dir := fc.dirs[len(fc.dirs)-1]
	stops, copies := slices.Clone(fc.stops), slices.Clone(fc.copies)
	fc.mu.Unlock()
	if args := fc.lastStart(); dir != worktree || !slices.Equal(args, []string{"--bg", "--resume", "s-a", "--", "fix the review"}) {
		t.Fatalf("claude ran in %s with %q", dir, args)
	}
	if !slices.Equal(stops, []string{"aaaaaaaa"}) || len(copies) != 0 {
		t.Fatalf("the worker was resumed while its process ran, which starts a copy: stops %v, copies %v", stops, copies)
	}
	d.poll()
	d.mu.Lock()
	defer d.mu.Unlock()
	if a := d.nodes["a"]; a.ID != "aaaaaaaa" || a.Status != "busy" {
		t.Fatalf("a = %+v", a)
	}
	if len(d.events) != 0 {
		t.Fatalf("the master got %+v", d.events)
	}
}

// A daemon that stopped left the master's calls failing, so the master may have ended its turn, and Claude Code may have stopped it since.
func TestRestartLeavesIdleMasterAsleep(t *testing.T) {
	for _, status := range []string{"idle", ""} {
		t.Run(cmp.Or(status, "stopped"), func(t *testing.T) {
			d, fc := testDaemon(t)
			idleWait = 0
			t.Cleanup(func() { idleWait = 30 * time.Second })
			fc.mu.Lock()
			fc.agents[0].SessionID = "s-m"
			fc.mu.Unlock()
			fc.setStatus("00000001", status)
			if err := d.ensureMaster(); err != nil || len(fc.starts) != 1 {
				t.Fatalf("adopting the master: %v, %d starts", err, len(fc.starts))
			}

			mustCall(t, Request{Op: "tell", Text: "x"})
			awaitWoken(t, d, fc, 2)
			fc.mu.Lock()
			stops := slices.Clone(fc.stops)
			fc.mu.Unlock()
			want := []string{"00000001"}
			if status == "" {
				want = nil
			}
			if args := fc.lastStart(); !slices.Equal(args, []string{"--bg", "--resume", "s-m", "--", eventsWaiting}) || !slices.Equal(stops, want) {
				t.Fatalf("claude ran with %q after stopping %v", args, stops)
			}
			if events := mustCall(t, Request{Op: "next_event", Node: masterNode}).Events; len(events) != 1 || events[0].Kind != "tell" {
				t.Fatalf("events = %+v", events)
			}
		})
	}
}

// awaitWoken waits until the daemon has resumed the master with claude's start number starts.
func awaitWoken(t *testing.T, d *daemon, fc *fakeClaude, starts int) {
	t.Helper()
	for range 500 {
		// The master's id is empty from before the resume's start until the resume ends.
		fc.mu.Lock()
		n := len(fc.starts)
		fc.mu.Unlock()
		d.mu.Lock()
		id := d.nodes[masterNode].ID
		d.mu.Unlock()
		if n == starts && id != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the master was not resumed")
}

func TestMasterStop(t *testing.T) {
	d, _ := testDaemon(t)
	stop := func() string { return receive(t, runHookAsync(masterNode, hookEvent("Stop", "", "", nil))) }
	if out := stop(); out != "null" {
		t.Fatalf("the master's Stop with no events waiting printed %s", out)
	}
	d.mu.Lock()
	asleep := d.asleep
	d.asleep = false
	d.mu.Unlock()
	if !asleep {
		t.Fatal("the master ended its turn without sleeping")
	}

	mustCall(t, Request{Op: "tell", Text: "x"})
	if out := stop(); !strings.HasPrefix(out, `{"decision":"block","reason":`) {
		t.Fatalf("the master's Stop with an event waiting printed %s", out)
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
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.asleep {
		t.Fatal("the master that ignored the block sleeps through the next event")
	}
}

func TestWakeMasterFails(t *testing.T) {
	d, fc := testDaemon(t)
	idleWait = 0
	t.Cleanup(func() { idleWait = 30 * time.Second })
	d.poll()
	// A master in a turn, as one the user typed into is, finds the events at its Stop.
	d.wakeMaster()
	if n := len(mustCall(t, Request{Op: "status"}).Decisions); n != 0 || len(fc.starts) != 1 {
		t.Fatalf("waking a master in its turn: %d decisions, %d starts", n, len(fc.starts))
	}

	fc.mu.Lock()
	fc.agents = nil
	fc.mu.Unlock()
	d.poll()
	d.wakeMaster()
	if dec := openDecision(t, 1); dec.Kind != "notice" {
		t.Fatalf("decision = %+v", dec)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.asleep {
		t.Fatal("a master that could not be woken is not tried again")
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

// A stopped worker still resumes, so it holds its place under the cap until the user stops it.
func TestCapCountsStoppedWorkers(t *testing.T) {
	d, fc := testDaemon(t)
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	fc.mu.Lock()
	fc.agents = append(fc.agents, agent{ID: "aaaaaaaa", SessionID: "s-a", Name: "a"})
	fc.mu.Unlock()
	d.poll()
	for _, name := range []string{"b", "c", "d"} {
		mustCall(t, Request{Op: "spawn", Node: masterNode, Name: name, Repo: repo, Text: "x"})
	}
	if _, err := call(context.Background(), Request{Op: "spawn", Node: masterNode, Name: "e", Repo: repo, Text: "x"}); err == nil {
		t.Fatal("spawned a fifth child beside a stopped one")
	}
	mustCall(t, Request{Op: "kill", Name: "a"})
	mustCall(t, Request{Op: "spawn", Node: masterNode, Name: "e", Repo: repo, Text: "x"})
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
	if args := fc.lastStart(); len(args) != 5 || !slices.Equal(args[:4], []string{"--bg", "--resume", "s-master", "--"}) {
		t.Fatalf("after the failure claude ran with %q, want the old master resumed", args)
	}
	d.mu.Lock()
	m := *d.nodes[masterNode]
	d.mu.Unlock()
	if m.Model != "" || m.Status != "starting" {
		t.Fatalf("master = %+v", m)
	}
}

// gitEnv is the environment without the GIT_CONFIG_* settings of a bosun session the tests may run in, whose excludesFile hides the files they look for.
func gitEnv() []string {
	return slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "GIT_CONFIG_") })
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

// mergeable gives the master a child b, idle between its turns, in a repository whose checked-out branch main b's one commit fast-forwards.
func mergeable(t *testing.T, d *daemon, fc *fakeClaude) (b *Session, repo, tip string) {
	repo = t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(repo, ".claude", "worktrees", "b")
	git(t, repo, "worktree", "add", "-q", "-b", "worktree-b", wt)
	tip = commit(t, wt, "work")
	b = &Session{Node: "b", Parent: masterNode, Repo: repo, Dir: wt, Base: repo, BaseBranch: "main", Depth: 1, ID: "bbbbbbbb", Status: "idle"}
	if _, _, err := d.writeConfig(b); err != nil {
		t.Fatal(err)
	}
	if err := standInGitmodules(wt); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.nodes["b"] = b
	d.mu.Unlock()
	fc.add(agent{ID: "bbbbbbbb", Name: "b", Status: "idle"})
	fc.mu.Lock()
	fc.discard = map[string]string{"bbbbbbbb": tip + "@0123abcd"}
	fc.mu.Unlock()
	return b, repo, tip
}

// commit commits what dir holds, files are path and content pairs to write first, and returns the new HEAD.
func commit(t *testing.T, dir, message string, files ...string) string {
	t.Helper()
	for i := 0; i+1 < len(files); i += 2 {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, files[i])), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, files[i]), []byte(files[i+1]), 0o755); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "-c", "core.hooksPath=/dev/null", "add", "-f", files[i])
	}
	git(t, dir, "-c", "core.hooksPath=/dev/null", "commit", "-q", "--allow-empty", "-m", message)
	return strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
}

// rmClaude removes a worktree and its branch on claude rm, and runs onStop on claude stop.
type rmClaude struct {
	*fakeClaude
	repo, wt string
	onStop   func()
}

func (r *rmClaude) Stop(id string) error {
	if r.onStop != nil {
		r.onStop()
	}
	return r.fakeClaude.Stop(id)
}

func (r *rmClaude) Remove(id, discard string) error {
	if err := r.fakeClaude.Remove(id, discard); err != nil {
		return err
	}
	exec.Command("git", "-C", r.repo, "worktree", "remove", "--force", r.wt).Run()
	exec.Command("git", "-C", r.repo, "branch", "-D", "worktree-b").Run()
	return nil
}

func TestMerge(t *testing.T) {
	d, fc := testDaemon(t)
	b, repo, tip := mergeable(t, d, fc)
	if _, err := call(context.Background(), Request{Op: "merge", Node: "a", Name: "b"}); err == nil {
		t.Fatal("a worker merged the master's child")
	}

	text := mustCall(t, Request{Op: "merge", Node: masterNode, Name: "b"}).Text
	if head := strings.TrimSpace(git(t, repo, "rev-parse", "main")); head != tip {
		t.Fatalf("main is at %s, want b's %s: %s", head, tip, text)
	}
	fc.mu.Lock()
	stops, removed := slices.Clone(fc.stops), slices.Clone(fc.removed)
	fc.mu.Unlock()
	if !slices.Equal(stops, []string{"bbbbbbbb"}) || !slices.Equal(removed, []string{"bbbbbbbb"}) {
		t.Fatalf("stopped %v and removed %v, want b", stops, removed)
	}
	if _, err := os.Stat(filepath.Join(b.Dir, ".gitmodules")); err == nil {
		t.Fatal("the stand-in is left for claude rm to refuse")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.nodes["b"] != nil {
		t.Fatal("the daemon still holds b")
	}
}

// The worker stops before its worktree is judged, so a write its last moments make is refused, not discarded with the worktree.
func TestMergeStopsFirst(t *testing.T) {
	d, fc := testDaemon(t)
	b, repo, _ := mergeable(t, d, fc)
	late := filepath.Join(b.Dir, "late")
	d.claude = &rmClaude{fakeClaude: fc, repo: repo, wt: b.Dir, onStop: func() { os.WriteFile(late, []byte("work"), 0o644) }}
	if _, err := call(context.Background(), Request{Op: "merge", Node: masterNode, Name: "b"}); err == nil {
		t.Fatal("merged a worktree with uncommitted work")
	}
	if _, err := os.Stat(late); err != nil {
		t.Fatalf("the late write is gone: %v", err)
	}
}

// The master may start a worker from a linked worktree, whose branch the worker began from.
func TestMergeIntoBaseBranch(t *testing.T) {
	d, fc := testDaemon(t)
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	linked := filepath.Join(t.TempDir(), "feat")
	git(t, repo, "worktree", "add", "-q", "-b", "feature", linked)
	commit(t, linked, "feature work")
	wt := filepath.Join(repo, ".claude", "worktrees", "b")
	git(t, linked, "worktree", "add", "-q", "-b", "worktree-b", wt, "HEAD")
	tip := commit(t, wt, "b's work")
	b := &Session{Node: "b", Parent: masterNode, Repo: repo, Dir: wt, Base: linked, BaseBranch: "feature", Depth: 1, ID: "bbbbbbbb", Status: "idle"}
	d.mu.Lock()
	d.nodes["b"] = b
	d.mu.Unlock()
	fc.add(agent{ID: "bbbbbbbb", Name: "b", Status: "idle"})
	mustCall(t, Request{Op: "merge", Node: masterNode, Name: "b"})
	if head := strings.TrimSpace(git(t, repo, "rev-parse", "feature")); head != tip {
		t.Fatalf("feature is at %s, want b's %s", head, tip)
	}
	if strings.Contains(git(t, repo, "log", "--oneline", "main"), "work") {
		t.Fatal("main moved")
	}
}

// A worker's commits must not reach the daemon's process, as a hook or through a repository its worktree names.
func TestMergeRunsNoWorkerCode(t *testing.T) {
	t.Run("hook", func(t *testing.T) {
		d, fc := testDaemon(t)
		b, repo, _ := mergeable(t, d, fc)
		git(t, repo, "config", "core.hooksPath", ".githooks")
		marker := filepath.Join(t.TempDir(), "ran")
		tip := commit(t, b.Dir, "hook", ".githooks/post-merge", "#!/bin/sh\ntouch "+marker+"\n")
		fc.mu.Lock()
		fc.discard["bbbbbbbb"] = tip + "@0123abcd"
		fc.mu.Unlock()
		mustCall(t, Request{Op: "merge", Node: masterNode, Name: "b"})
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("the daemon ran the worker's post-merge hook")
		}
	})
	t.Run("fsmonitor", func(t *testing.T) {
		d, fc := testDaemon(t)
		b, _, _ := mergeable(t, d, fc)
		marker := filepath.Join(t.TempDir(), "ran")
		planted := filepath.Join(b.Dir, "x")
		git(t, b.Dir, "init", "-q", planted)
		script := filepath.Join(b.Dir, "mon.sh")
		os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o755)
		git(t, planted, "config", "core.fsmonitor", script)
		os.WriteFile(filepath.Join(b.Dir, ".git"), []byte("gitdir: "+filepath.Join(planted, ".git")+"\n"), 0o644)
		if _, err := call(context.Background(), Request{Op: "merge", Node: masterNode, Name: "b"}); err == nil {
			t.Fatal("merged a worktree of another repository")
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("the daemon ran the worker's fsmonitor")
		}
	})
}

func TestMergeKeepsIgnoredFiles(t *testing.T) {
	d, fc := testDaemon(t)
	b, repo, _ := mergeable(t, d, fc)
	os.WriteFile(filepath.Join(repo, ".git", "info", "exclude"), []byte(".envrc\n"), 0o644)
	os.WriteFile(filepath.Join(repo, ".envrc"), []byte("mine"), 0o644)
	commit(t, b.Dir, "envrc", ".envrc", "theirs")
	if _, err := call(context.Background(), Request{Op: "merge", Node: masterNode, Name: "b"}); err == nil {
		t.Fatal("merged over the user's ignored file")
	}
	if data, _ := os.ReadFile(filepath.Join(repo, ".envrc")); string(data) != "mine" {
		t.Fatalf("the user's .envrc holds %q", data)
	}
}

// A worker whose work is on its base branch already, which has moved on since, needs no rebase.
func TestMergeAlreadyMerged(t *testing.T) {
	d, fc := testDaemon(t)
	_, repo, tip := mergeable(t, d, fc)
	git(t, repo, "merge", "-q", "--ff-only", "worktree-b")
	later := commit(t, repo, "later")
	mustCall(t, Request{Op: "merge", Node: masterNode, Name: "b"})
	if head := strings.TrimSpace(git(t, repo, "rev-parse", "main")); head != later {
		t.Fatalf("main is at %s, want %s, past b's %s", head, later, tip)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.nodes["b"] != nil {
		t.Fatal("the daemon still holds b")
	}
}

// The Stop hook reports a turn's end before Claude Code lists the session idle.
func TestMergeRightAfterTurnEnd(t *testing.T) {
	d, fc := testDaemon(t)
	b, _, _ := mergeable(t, d, fc)
	d.mu.Lock()
	b.Status = "busy"
	d.mu.Unlock()
	mustCall(t, Request{Op: "merge", Node: masterNode, Name: "b"})
}

func TestMergeGrandchildren(t *testing.T) {
	d, fc := testDaemon(t)
	b, _, _ := mergeable(t, d, fc)
	d.mu.Lock()
	d.nodes["c"] = &Session{Node: "c", Parent: "b", Base: b.Dir, BaseBranch: "worktree-b", Status: "stopped", ID: "cccccccc", Killed: true}
	d.mu.Unlock()
	_, err := call(context.Background(), Request{Op: "merge", Node: masterNode, Name: "b"})
	if err == nil || !strings.Contains(err.Error(), "claude rm cccccccc") {
		t.Fatalf("merging b over a child the user stopped: %v", err)
	}
	// Once the user removes c, b merges, and c goes with it.
	d.mu.Lock()
	d.nodes["c"].Status = "exited"
	d.mu.Unlock()
	mustCall(t, Request{Op: "merge", Node: masterNode, Name: "b"})
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.nodes["c"] != nil {
		t.Fatal("the daemon still holds c")
	}
}

// A worker whose session is gone, as one the user removed is, holds up nothing once its work is safe.
func TestMergeGone(t *testing.T) {
	d, fc := testDaemon(t)
	b, repo, _ := mergeable(t, d, fc)
	d.mu.Lock()
	b.Status = "exited"
	d.mu.Unlock()
	if _, err := call(context.Background(), Request{Op: "merge", Node: masterNode, Name: "b"}); err == nil {
		t.Fatal("forgot a worker whose branch holds unmerged commits")
	}
	git(t, repo, "merge", "-q", "--ff-only", "worktree-b")
	mustCall(t, Request{Op: "merge", Node: masterNode, Name: "b"})
	fc.mu.Lock()
	removed := len(fc.removed)
	fc.mu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.nodes["b"] != nil || removed != 0 {
		t.Fatalf("b = %+v, %d claude rm runs", d.nodes["b"], removed)
	}
}

func TestMergeRefuses(t *testing.T) {
	for name, spoil := range map[string]func(t *testing.T, d *daemon, b *Session, repo string){
		"behind": func(t *testing.T, d *daemon, b *Session, repo string) {
			commit(t, repo, "elsewhere")
		},
		"dirty": func(t *testing.T, d *daemon, b *Session, repo string) {
			if err := os.WriteFile(filepath.Join(b.Dir, "notes"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"switched": func(t *testing.T, d *daemon, b *Session, repo string) {
			git(t, repo, "switch", "-q", "-c", "other")
		},
		"in its turn": func(t *testing.T, d *daemon, b *Session, repo string) {
			idleWait = 0
			t.Cleanup(func() { idleWait = 30 * time.Second })
			d.claude.(*fakeClaude).setStatus("bbbbbbbb", "busy")
		},
		"with a child": func(t *testing.T, d *daemon, b *Session, repo string) {
			d.mu.Lock()
			d.nodes["c"] = &Session{Node: "c", Parent: "b", Status: "idle", ID: "cccccccc"}
			d.mu.Unlock()
		},
	} {
		t.Run(name, func(t *testing.T) {
			d, fc := testDaemon(t)
			b, repo, tip := mergeable(t, d, fc)
			spoil(t, d, b, repo)
			if _, err := call(context.Background(), Request{Op: "merge", Node: masterNode, Name: "b"}); err == nil {
				t.Fatal("merged")
			}
			if head := strings.TrimSpace(git(t, repo, "rev-parse", "main")); head == tip {
				t.Fatal("main moved to b's branch")
			}
			fc.mu.Lock()
			removed := len(fc.removed)
			fc.mu.Unlock()
			d.mu.Lock()
			defer d.mu.Unlock()
			if removed != 0 || d.nodes["b"] == nil || d.nodes["b"].held {
				t.Fatalf("removed %d, b = %+v", removed, d.nodes["b"])
			}
		})
	}
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
	cmd.Env = gitEnv()
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
	if !slices.Equal(names, []string{"spawn", "ask", "merge"}) {
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
