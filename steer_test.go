package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testKey stands in for Claude Code's control key.
const testKey = "0123456789abcdef0123456789abcdef"

// controlFake answers the supervisor's control protocol as Claude Code 2.1.310 does, for the ops bosun sends.
type controlFake struct {
	mu   sync.Mutex
	reqs []map[string]any
	// keys holds by short id what each attach typed into the session's terminal.
	keys  map[string]string
	proto int
	key   string
	// onKeys runs once an attach to short closes, having typed keys.
	onKeys func(short, keys string)
}

// fakeControl serves a fake supervisor for d, with the control key in Claude Code's config directory.
func fakeControl(t *testing.T, d *daemon) *controlFake {
	t.Helper()
	dir := filepath.Join(claudeDir(), "daemon")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "control.key"), []byte(testKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &controlFake{proto: 1, key: testKey, keys: map[string]string{}}
	sock := fmt.Sprintf("@bosun-control-%d-%s", os.Getpid(), t.Name())
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(conn)
		}
	}()
	useFakeSocket(t, sock)
	settle := keysSettle
	keysSettle = 10 * time.Millisecond
	t.Cleanup(func() { keysSettle = settle })
	return s
}

func (s *controlFake) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return
	}
	var req map[string]any
	if json.Unmarshal(line, &req) != nil {
		return
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	proto, key := s.proto, s.key
	s.mu.Unlock()
	answer := func(v map[string]any) { _ = json.NewEncoder(conn).Encode(v) }
	if req["op"] == "ping" {
		answer(map[string]any{"ok": true, "op": "ping", "version": "2.1.310", "proto": proto})
		return
	}
	if p, _ := req["proto"].(float64); int(p) != proto {
		answer(map[string]any{"ok": false, "error": "proto mismatch", "code": "EPROTO"})
		return
	}
	if req["auth"] != key {
		answer(map[string]any{"ok": false, "error": fmt.Sprintf("%s rejected: the presented daemon control key %v doesn't match", req["op"], req["auth"]), "code": "EAUTH"})
		return
	}
	short, _ := req["short"].(string)
	switch req["op"] {
	case "reply":
		answer(map[string]any{"ok": true, "op": "reply"})
	case "attach":
		answer(map[string]any{"ok": true, "op": "attach", "tempo": "active"})
		_, _ = conn.Write([]byte("\x1b[?25l terminal output"))
		typed, _ := io.ReadAll(r)
		s.mu.Lock()
		s.keys[short] += string(typed)
		on := s.onKeys
		s.mu.Unlock()
		if on != nil {
			on(short, string(typed))
		}
	default:
		answer(map[string]any{"ok": false, "error": "unknown op", "code": "EUNKNOWN"})
	}
}

// ops returns the op of each request the supervisor got.
func (s *controlFake) ops() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ops []string
	for _, r := range s.reqs {
		ops = append(ops, fmt.Sprint(r["op"]))
	}
	return ops
}

func (s *controlFake) last() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs[len(s.reqs)-1]
}

func (s *controlFake) typed(short string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys[short]
}

// openPromptNotice opens a permission of worker a's to run tool, then leaves it to a's own prompt, as bosun does once it can hold it no longer, and returns the notice's id.
func openPromptNotice(d *daemon, tool string, input json.RawMessage) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	dec := &Decision{Kind: "permission", Node: "a", Holder: "master", Tool: tool, Input: input}
	d.openLocked(dec)
	d.promptLocked(dec, "waits in its own prompt", "test")
	for id, dec := range d.decisions {
		if dec.Prompt {
			return id
		}
	}
	return ""
}

// setNeeds writes what the dialog of session id asks, as Claude Code's job state holds it.
func setNeeds(t *testing.T, id, needs string) {
	t.Helper()
	dir := filepath.Join(claudeDir(), "jobs", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"tempo": "blocked", "needs": needs})
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// steerDaemon is testDaemon with worker a listed in its turn, under a fake supervisor.
func steerDaemon(t *testing.T, status string) (*daemon, *fakeClaude, *controlFake) {
	d, fc := testDaemon(t)
	fc.add(agent{ID: "aaaaaaaa", SessionID: "s-a", Name: "a", Status: status})
	d.mu.Lock()
	d.nodes["a"].Status, d.nodes["a"].Turn = status, status != "idle"
	d.mu.Unlock()
	return d, fc, fakeControl(t, d)
}

// assertKeyKept fails if the control key reached the daemon's state, its log or text.
func assertKeyKept(t *testing.T, d *daemon, texts ...string) {
	t.Helper()
	for _, name := range []string{"state.json", "log.jsonl"} {
		data, _ := os.ReadFile(filepath.Join(d.stateDir, name))
		if strings.Contains(string(data), testKey) {
			t.Fatalf("%s holds the control key", name)
		}
	}
	for _, s := range texts {
		if strings.Contains(s, testKey) {
			t.Fatalf("%q holds the control key", s)
		}
	}
}

// Only the user, from a frontend, steers a session: neither a session's request nor its MCP server reaches these ops.
func TestSteeringRefusesSessions(t *testing.T) {
	d, _, sup := steerDaemon(t, "busy")
	notice := openPromptNotice(d, "Bash", json.RawMessage(`{"command":"npm test"}`))
	setNeeds(t, "aaaaaaaa", "approve Bash: npm test")
	for _, node := range []string{masterNode, deskNode, "a"} {
		for _, r := range []Request{
			{Op: "say", Node: node, Name: "a", Text: "hi"},
			{Op: "interrupt", Node: node, Name: "a"},
			{Op: "approve", Node: node, ID: notice},
			{Op: "deny", Node: node, ID: notice},
		} {
			if _, err := call(context.Background(), r); err == nil {
				t.Fatalf("%s from %s went through", r.Op, node)
			}
		}
		for _, tool := range tools(node) {
			if tool.Name == "say" || tool.Name == "interrupt" {
				t.Fatalf("%s has the tool %s", node, tool.Name)
			}
		}
		for _, name := range []string{"say", "interrupt"} {
			args := json.RawMessage(`{"name":"a","text":"hi"}`)
			if _, err := newMCPServer(node, io.Discard).run(context.Background(), name, args); err == nil {
				t.Fatalf("%s's MCP server ran %s", node, name)
			}
		}
	}
	if ops := sup.ops(); len(ops) != 0 {
		t.Fatalf("the supervisor got %v", ops)
	}
	if n := len(logOf(d, "say")) + len(logOf(d, "interrupt")) + len(logOf(d, "prompt")); n != 0 {
		t.Fatalf("%d uses logged", n)
	}
}

// bosun steers only its own sessions, not one Claude Code lists that bosun did not start.
func TestSteeringRefusesOtherSessions(t *testing.T) {
	_, fc, sup := steerDaemon(t, "busy")
	fc.add(agent{ID: "eeeeeeee", SessionID: "s-e", Name: "stranger", Status: "busy"})
	for _, r := range []Request{{Op: "say", Name: "stranger", Text: "hi"}, {Op: "interrupt", Name: "stranger"}, {Op: "say", Name: "eeeeeeee", Text: "hi"}} {
		if _, err := call(context.Background(), r); err == nil {
			t.Fatalf("%s to %s went through", r.Op, r.Name)
		}
	}
	if ops := sup.ops(); len(ops) != 0 {
		t.Fatalf("the supervisor got %v", ops)
	}
}

func TestSayToRunningSession(t *testing.T) {
	d, fc, sup := steerDaemon(t, "busy")
	resp := mustCall(t, Request{Op: "say", Name: "a", Text: "also run the linter"})
	if ops := sup.ops(); len(ops) != 2 || ops[0] != "ping" || ops[1] != "reply" {
		t.Fatalf("the supervisor got %v", ops)
	}
	want := map[string]any{"proto": float64(1), "op": "reply", "short": "aaaaaaaa", "text": "also run the linter", "auth": testKey}
	if got := sup.last(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("reply = %v, want %v", got, want)
	}
	if !strings.Contains(resp.Text, "once its running tool call returns") {
		t.Fatalf("say answered %q", resp.Text)
	}
	if len(fc.stops) != 0 || len(fc.starts) != 1 {
		t.Fatalf("stops %v, starts %v", fc.stops, fc.starts)
	}
	logged := logOf(d, "say")
	if len(logged) != 1 || logged[0].By != "user" || logged[0].Node != "a" || logged[0].Via != "supervisor" || logged[0].Text != "also run the linter" || logged[0].Error != "" {
		t.Fatalf("logged %+v", logged)
	}
	assertKeyKept(t, d, resp.Text)
}

func TestSayAnswersQuestion(t *testing.T) {
	_, fc, sup := steerDaemon(t, "waiting")
	fc.setStatus("aaaaaaaa", "waiting")
	setNeeds(t, "aaaaaaaa", "answer: red or blue? (Red · Blue)")
	resp := mustCall(t, Request{Op: "say", Name: "a", Text: "Blue"})
	if sup.last()["op"] != "reply" || !strings.Contains(resp.Text, "red or blue?") {
		t.Fatalf("say answered %q after %v", resp.Text, sup.ops())
	}
}

// A text sent while a permission prompt waits would wait behind it, and a stop would drop it.
func TestSayRefusesPermissionPrompt(t *testing.T) {
	_, _, sup := steerDaemon(t, "waiting")
	setNeeds(t, "aaaaaaaa", "approve Bash: npm test")
	if _, err := call(context.Background(), Request{Op: "say", Name: "a", Text: "hi"}); err == nil || !strings.Contains(err.Error(), "permission") {
		t.Fatalf("say during a permission prompt: %v", err)
	}
	if ops := sup.ops(); len(ops) != 0 {
		t.Fatalf("the supervisor got %v", ops)
	}
}

func TestSayRefuses(t *testing.T) {
	d, _, sup := steerDaemon(t, "busy")
	for _, text := range []string{"", "  ", "/model opus", "/clear", " /clear now"} {
		if _, err := call(context.Background(), Request{Op: "say", Name: "a", Text: text}); err == nil {
			t.Fatalf("say %q went through", text)
		}
	}
	d.mu.Lock()
	d.nodes["a"].Killed = true
	d.mu.Unlock()
	for _, r := range []Request{{Op: "say", Name: "a", Text: "hi"}, {Op: "interrupt", Name: "a"}} {
		if _, err := call(context.Background(), r); err == nil || !strings.Contains(err.Error(), "stopped") {
			t.Fatalf("%s to a killed worker: %v", r.Op, err)
		}
	}
	if ops := sup.ops(); len(ops) != 0 {
		t.Fatalf("the supervisor got %v", ops)
	}
}

// A stopped session takes the text in a resume, without the key, a slash command such as /compact included.
func TestSayToStoppedSession(t *testing.T) {
	d, fc, sup := steerDaemon(t, "idle")
	fc.setStatus("aaaaaaaa", "")
	resp := mustCall(t, Request{Op: "say", Name: "a", Text: "/compact"})
	if got := fc.lastStart(); strings.Join(got, " ") != "--bg --resume s-a -- /compact" {
		t.Fatalf("started %v", got)
	}
	if ops := sup.ops(); len(ops) != 0 {
		t.Fatalf("the supervisor got %v", ops)
	}
	if logged := logOf(d, "say"); len(logged) != 1 || logged[0].Via != "resume" || logged[0].Text != "/compact" {
		t.Fatalf("logged %+v", logged)
	}
	if !strings.HasPrefix(resp.Text, "Resumed a") {
		t.Fatalf("say answered %q", resp.Text)
	}
}

// When the supervisor speaks another protocol or refuses the key, say stops the session and resumes it with the text, and its parent hears that the user cut the turn short.
func TestSayFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name  string
		proto int
		key   string
		ops   string
	}{
		{"proto", 2, testKey, "ping"},
		{"key", 1, "another key", "ping reply"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, fc, sup := steerDaemon(t, "busy")
			sup.mu.Lock()
			sup.proto, sup.key = tc.proto, tc.key
			sup.mu.Unlock()
			resp := mustCall(t, Request{Op: "say", Name: "a", Text: "stop and do X"})
			if got := strings.Join(sup.ops(), " "); got != tc.ops {
				t.Fatalf("the supervisor got %s, want %s", got, tc.ops)
			}
			if len(fc.stops) != 1 || strings.Join(fc.lastStart(), " ") != "--bg --resume s-a -- stop and do X" {
				t.Fatalf("stops %v, last start %v", fc.stops, fc.lastStart())
			}
			evs := masterEvents(d)
			if len(evs) != 1 || evs[0].Kind != "turn_end" || evs[0].Node != "a" || !strings.Contains(evs[0].Text, "user cut its turn short") {
				t.Fatalf("the master got %+v", evs)
			}
			logged := logOf(d, "say")
			if last := logged[len(logged)-1]; last.Via != "resume" || last.Error != "" {
				t.Fatalf("logged %+v", logged)
			}
			var texts []string
			for _, e := range logged {
				texts = append(texts, e.Error)
			}
			assertKeyKept(t, d, append(texts, resp.Text)...)
		})
	}
}

func TestInterrupt(t *testing.T) {
	d, fc, sup := steerDaemon(t, "busy")
	sup.onKeys = func(short, keys string) {
		if keys == "\x1b" {
			fc.setStatus(short, "idle")
		}
	}
	resp := mustCall(t, Request{Op: "interrupt", Name: "a"})
	if got := sup.typed("aaaaaaaa"); got != "\x1b" {
		t.Fatalf("typed %q", got)
	}
	attach := sup.last()
	if attach["op"] != "attach" || attach["short"] != "aaaaaaaa" || attach["auth"] != testKey || attach["cols"] == nil || attach["rows"] == nil {
		t.Fatalf("attach = %v", attach)
	}
	if len(fc.stops) != 0 {
		t.Fatalf("stopped %v", fc.stops)
	}
	evs := masterEvents(d)
	if len(evs) != 1 || evs[0].Kind != "turn_end" || evs[0].Text != "The user interrupted its turn." {
		t.Fatalf("the master got %+v", evs)
	}
	if logged := logOf(d, "interrupt"); len(logged) != 1 || logged[0].Via != "supervisor" || logged[0].Node != "a" || logged[0].By != "user" {
		t.Fatalf("logged %+v", logged)
	}
	// The poll finds the worker idle, and reports no second turn end.
	d.poll()
	if evs := masterEvents(d); len(evs) != 1 {
		t.Fatalf("the master got %+v", evs)
	}
	if _, err := call(context.Background(), Request{Op: "interrupt", Name: "a"}); err == nil {
		t.Fatal("interrupted an idle worker")
	}
	assertKeyKept(t, d, resp.Text)
}

// Without a supervisor to take the Esc, or when the Esc ends no turn, an interrupt stops the session and resumes it idle.
func TestInterruptFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		ops  string
	}{
		{"no supervisor", ""},
		{"no turn ended", "ping attach"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, fc, sup := steerDaemon(t, "busy")
			wait := interruptWait
			interruptWait = 100 * time.Millisecond
			t.Cleanup(func() { interruptWait = wait })
			if tc.ops == "" {
				useFakeSocket(t, "")
			}
			mustCall(t, Request{Op: "interrupt", Name: "a"})
			if got := strings.Join(sup.ops(), " "); got != tc.ops {
				t.Fatalf("the supervisor got %q", got)
			}
			if len(fc.stops) != 1 || strings.Join(fc.lastStart(), " ") != "--bg --resume s-a" {
				t.Fatalf("stops %v, last start %v", fc.stops, fc.lastStart())
			}
			if evs := masterEvents(d); len(evs) != 1 || evs[0].Kind != "turn_end" {
				t.Fatalf("the master got %+v", evs)
			}
			logged := logOf(d, "interrupt")
			if last := logged[len(logged)-1]; last.Via != "resume" || last.Error != "" {
				t.Fatalf("logged %+v", logged)
			}
			if tc.ops != "" && (len(logged) != 2 || logged[0].Via != "supervisor" || logged[0].Error == "") {
				t.Fatalf("logged %+v", logged)
			}
		})
	}
}

// approve and deny on a notice of a permission waiting in the session's own prompt press the dialog's keys.
func TestAnswerOwnPrompt(t *testing.T) {
	d, _, sup := steerDaemon(t, "waiting")
	open := func() string { return openPromptNotice(d, "Bash", json.RawMessage(`{"command":"npm test"}`)) }
	id := open()
	if dec := openDecision(t, 1); !dec.Prompt || dec.Tool != "Bash" {
		t.Fatalf("notice = %+v", dec)
	}
	setNeeds(t, "aaaaaaaa", "approve Read: /etc/hosts")
	if _, err := call(context.Background(), Request{Op: "approve", ID: id}); err == nil {
		t.Fatal("approved a prompt that asks about another call")
	}
	setNeeds(t, "aaaaaaaa", "approve Bash: npm test")
	mustCall(t, Request{Op: "approve", ID: id})
	if got := sup.typed("aaaaaaaa"); got != "\r" {
		t.Fatalf("typed %q", got)
	}
	if n := len(mustCall(t, Request{Op: "status"}).Decisions); n != 0 {
		t.Fatalf("%d decisions open", n)
	}

	id = open()
	mustCall(t, Request{Op: "deny", ID: id, Text: "run the unit tests only"})
	if got := sup.typed("aaaaaaaa"); got != "\r\x1b" {
		t.Fatalf("typed %q", got)
	}
	if last := sup.last(); last["op"] != "reply" || last["text"] != "run the unit tests only" {
		t.Fatalf("last request %v", last)
	}
	logged := logOf(d, "prompt")
	if len(logged) != 2 || logged[0].Behavior != "allow" || logged[1].Behavior != "deny" || logged[1].Via != "supervisor" || logged[1].Node != "a" {
		t.Fatalf("logged %+v", logged)
	}
	settled := logOf(d, "settled")
	if last := settled[len(settled)-1]; last.By != "user" || last.Behavior != "deny" {
		t.Fatalf("settled %+v", settled)
	}
	assertKeyKept(t, d)
}

// Without a supervisor bosun cannot press the dialog's keys, so the notice stays open.
func TestAnswerOwnPromptWithoutSupervisor(t *testing.T) {
	d, _, _ := steerDaemon(t, "waiting")
	useFakeSocket(t, "")
	openPromptNotice(d, "Bash", nil)
	setNeeds(t, "aaaaaaaa", "approve Bash: npm test")
	dec := openDecision(t, 1)
	if _, err := call(context.Background(), Request{Op: "approve", ID: dec.ID}); err == nil || !strings.Contains(err.Error(), "bosun attach aaaaaaaa") {
		t.Fatalf("approve without a supervisor: %v", err)
	}
	openDecision(t, 1)
}

// The master's queue holds what the user said until next_event returns, so a say ends the wait.
func TestSayEndsMasterWait(t *testing.T) {
	d, _, sup := steerDaemon(t, "busy")
	d.mu.Lock()
	d.nodes[masterNode].Status = "busy"
	d.mu.Unlock()
	done := make(chan []Event, 1)
	go func() {
		resp, _ := call(context.Background(), Request{Op: "next_event", Node: masterNode})
		done <- resp.Events
	}()
	time.Sleep(50 * time.Millisecond)
	mustCall(t, Request{Op: "say", Name: masterNode, Text: "what is a doing?"})
	select {
	case evs := <-done:
		if len(evs) != 0 {
			t.Fatalf("next_event returned %+v", evs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("next_event still waits after a say to the master")
	}
	if sup.last()["op"] != "reply" {
		t.Fatalf("the supervisor got %v", sup.ops())
	}
}
