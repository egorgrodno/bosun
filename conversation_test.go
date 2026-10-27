package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeProbe writes testdata/probe.jsonl, a transcript in the 2.1.310 format, as the transcript of session sid, cut to its first lines when lines is above 0.
func writeProbe(t *testing.T, sid string, lines int) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/probe.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if lines > 0 {
		all := bytes.SplitAfter(data, []byte("\n"))
		data = bytes.Join(all[:lines], nil)
	}
	writeProbeFile(t, sid, data)
	return data
}

// writeProbeFile replaces the transcript of session sid with data at once, as a rename does.
func writeProbeFile(t *testing.T, sid string, data []byte) {
	t.Helper()
	dir := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", "-repo--claude-worktrees-a")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, sid+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, sid+".jsonl")); err != nil {
		t.Fatal(err)
	}
}

// appendProbeFile adds data to the end of the transcript of session sid, as Claude Code writes it.
func appendProbeFile(t *testing.T, sid string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", "-repo--claude-worktrees-a", sid+".jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
}

// line shows an entry as its ID, its kind and the fields its kind says most by.
func line(e Said) string {
	s := e.ID + " " + e.Kind
	switch e.Kind {
	case "prompt":
		s += fmt.Sprintf(" %s %q", e.From, e.Text)
		if e.MidTurn {
			s += " midTurn"
		}
	case "queued":
		s += fmt.Sprintf(" %q", e.Text)
		if e.Taken {
			s += " taken"
		}
	case "text", "thinking":
		s += fmt.Sprintf(" %q", e.Text)
	case "tool":
		s += " " + e.Name
		if r := e.Result; r != nil {
			s += fmt.Sprintf(" → %q", clip(r.Text, 20))
			if r.Error {
				s += " error"
			}
			if r.Interrupted {
				s += " interrupted"
			}
			if r.Diff != nil {
				s += " diff " + string(r.Diff)
			}
		}
	case "compact":
		s += fmt.Sprintf(" %q", clip(e.Summary, 30))
	case "command":
		s += fmt.Sprintf(" %s %q", e.Name, e.Output)
	case "meta", "unsupported":
		s += " " + e.RecordType
	}
	return s
}

// probeLines is how the entries of testdata/probe.jsonl show.
var probeLines = []string{
	`L1 meta permission-mode`,
	`L2 queued "Fix the parser and run the tests." taken`,
	`u-task prompt user "Fix the parser and run the tests."`,
	`at-tools meta attachment/deferred_tools_record`,
	`L6 meta api-request`,
	`a-think thinking "The parser first."`,
	`a-text text "I'll run the tests first."`,
	`a-test tool Bash → "FAIL\tbosun\t0.2s" error`,
	`a-edit tool Edit → "The file /repo/.cla…" diff [{"oldStart":1,"oldLines":2,"newStart":1,"newLines":2,"lines":[" alpha","-beta","+gamma"]}]`,
	`L13 queued "also the lexer" taken`,
	`at-queued prompt user "also the lexer" midTurn`,
	`a-write tool Write → "File created succes…" diff [{"lines":["+package main","+","+func lex() {}"],"newLines":3,"newStart":1,"oldLines":0,"oldStart":0}]`,
	`u-peer prompt worker-b "I'm touching lexer.go too."`,
	`a-fancy unsupported assistant/fancy_block`,
	`L20 unsupported future-record`,
	`at-new unsupported attachment/brand_new_kind`,
	`a-sleep tool Bash → "[Tool call interrup…" error interrupted`,
	`a-done text "Done: the parser passes."`,
	`s-duration meta system/turn_duration`,
	`L26 meta cost-state`,
	`u-compact prompt user "/compact"`,
	`s-boundary compact "This session is being continu…"`,
	`u-caveat meta user`,
	`u-command command /compact "Compacted (ctrl+o to see full summary)"`,
	`at-ref meta attachment/compact_file_reference`,
}

func lines(es []Said) []string {
	var out []string
	for _, e := range es {
		out = append(out, line(e))
	}
	return out
}

func TestConversationEntries(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	writeProbe(t, "s-probe", 0)
	c, err := readConversation("s-probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := lines(c.entries); !slices.Equal(got, probeLines) {
		t.Fatalf("the probe's entries are\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(probeLines, "\n"))
	}
	// A record no build of bosun knows goes out whole, as do the bookkeeping records.
	for id, holds := range map[string]string{"L20": `"payload":`, "at-new": `"brand_new_kind"`, "a-fancy": `"data":`, "L26": `"totalCostUSD":`} {
		e := c.entries[c.at[id]]
		if !json.Valid(e.Raw) || !strings.Contains(string(e.Raw), holds) {
			t.Fatalf("%s carries raw %s", id, e.Raw)
		}
	}
	e := c.entries[c.at["a-test"]]
	if e.Call != "toolu_test" || !strings.Contains(string(e.Input), `"command":"go test ./..."`) || !e.At.Equal(time.Date(2026, 10, 26, 21, 36, 41, 841e6, time.UTC)) || !e.Result.At.Equal(time.Date(2026, 10, 26, 21, 36, 43, 100e6, time.UTC)) {
		t.Fatalf("the test call is %+v", e)
	}
	if c.lastText != "Done: the parser passes." || !c.ended {
		t.Fatalf("the tail is %q, ended %v", c.lastText, c.ended)
	}
}

// The routine bookkeeping records of Claude Code go out as meta, not unsupported.
func TestConversationMapsRoutineRecords(t *testing.T) {
	c := newConversation(nil)
	for _, l := range []string{
		`{"type":"tools-used","tools":["Bash"],"sessionId":"s"}`,
		`{"type":"bridge-session","bridgeSessionId":"b","sessionId":"s"}`,
		`{"type":"agent-setting","agentSetting":"x","sessionId":"s"}`,
		`{"type":"continued-in","continuedInSessionId":"t","sessionId":"s"}`,
		`{"type":"attachment","uuid":"a1","attachment":{"type":"hook_system_message"}}`,
		`{"type":"attachment","uuid":"a2","attachment":{"type":"remote_session_change"}}`,
		`{"type":"system","subtype":"bridge_status","uuid":"s1"}`,
	} {
		c.add([]byte(l))
	}
	want := []string{
		`L1 meta tools-used`, `L2 meta bridge-session`, `L3 meta agent-setting`, `L4 meta continued-in`,
		`a1 meta attachment/hook_system_message`, `a2 meta attachment/remote_session_change`, `s1 meta system/bridge_status`,
	}
	if got := lines(c.entries); !slices.Equal(got, want) {
		t.Fatalf("entries are\n%q\nwant\n%q", got, want)
	}
}

// A long string of a record goes out cut, as the log cuts a decision's input.
func TestConversationCutsRaw(t *testing.T) {
	c := newConversation(nil)
	c.add([]byte(`{"type":"future-record","payload":"` + strings.Repeat("x", 3000) + `"}`))
	var raw struct{ Payload string }
	if err := json.Unmarshal(c.entries[0].Raw, &raw); err != nil || len([]rune(raw.Payload)) != logInputCut+1 {
		t.Fatalf("raw is %d runes long (%v)", len([]rune(raw.Payload)), err)
	}
	c.add([]byte(`{not json`))
	if e := c.entries[1]; e.Kind != "unsupported" || e.RecordType != "invalid JSON" || string(e.Raw) != `"{not json"` {
		t.Fatalf("a line that is no JSON is %+v", e)
	}
}

// A record that maps to nothing still goes out, and bosun's own prompts name bosun.
func TestConversationEdges(t *testing.T) {
	c := newConversation(nil)
	for _, l := range []string{
		`{"type":"assistant","uuid":"a-empty","message":{"content":[]}}`,
		`{"type":"queue-operation","operation":"dequeue"}`,
		`{"type":"queue-operation","operation":"enqueue","content":"one"}`,
		`{"type":"queue-operation","operation":"remove","content":"two"}`,
		`{"type":"queue-operation","operation":"shuffle"}`,
		`{"type":"user","uuid":"u-wake","message":{"content":"` + eventsWaiting + `"}}`,
		`{"type":"user","uuid":"u-img","message":{"content":[{"type":"text","text":"see this"},{"type":"image","source":{}}]}}`,
	} {
		c.add([]byte(l))
	}
	want := []string{
		`a-empty unsupported assistant`,
		`L2 meta queue-operation/dequeue`,
		`L3 queued "one"`,
		`L4 meta queue-operation/remove`,
		`L5 unsupported queue-operation/shuffle`,
		`u-wake prompt bosun "Events are waiting: call next_event."`,
		`u-img prompt user "see this"`,
		`u-img#1 unsupported user/image`,
	}
	if got := lines(c.entries); !slices.Equal(got, want) {
		t.Fatalf("the entries are\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// follow leaves out a change to an entry it never sent, and starts over on a transcript cut short.
func TestFollowEdges(t *testing.T) {
	d, _ := testDaemon(t)
	followFast(t)
	fakeSupervisor(t, func(op, short string) []string { return nil })
	call := `{"type":"assistant","uuid":"a-call","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"true"}}]}}` + "\n"
	text := `{"type":"assistant","uuid":"a-text","message":{"content":[{"type":"text","text":"Waiting."}]}}` + "\n"
	result := `{"type":"user","uuid":"r-call","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}}` + "\n"
	more := `{"type":"assistant","uuid":"a-more","message":{"content":[{"type":"text","text":"Done."}]}}` + "\n"
	writeProbeFile(t, "s-edge", []byte(call+text))
	d.mu.Lock()
	d.nodes["a"].SessionID = "s-edge"
	d.mu.Unlock()

	next := convDial(t, Request{Op: "follow", Name: "a", Limit: 1})
	next()
	if got := next(); !got.More || !slices.Equal(lines(got.Entries), []string{`a-text text "Waiting."`}) {
		t.Fatalf("the page is %v", lines(got.Entries))
	}
	appendProbeFile(t, "s-edge", []byte(result+more))
	if got := next(); !slices.Equal(lines(got.Entries), []string{`a-more text "Done."`}) {
		t.Fatalf("follow sent %v, and the call before the page has no entry at the frontend", lines(got.Entries))
	}
	writeProbeFile(t, "s-edge", []byte(call))
	if got := next(); got.Type != "reset" || got.Session != "s-edge" {
		t.Fatalf("the cut transcript came as %+v", got)
	}
	if got := next(); !slices.Equal(lines(got.Entries), []string{`a-call tool Bash`}) {
		t.Fatalf("the cut transcript holds %v", lines(got.Entries))
	}
}

// A peer's message may reach a turn between tool calls, so it begins none in bosun transcript.
func TestTranscriptQuotesPeer(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	writeProbe(t, "s-probe", 24)
	tr, err := readTranscript("s-probe", 1)
	if err != nil || len(tr.turns) != 1 || !strings.Contains(tr.render(), "> [from worker-b] I'm touching lexer.go too.\n") {
		t.Fatalf("the last turn renders as\n%s(%v)", tr.render(), err)
	}
}

func TestConversationPage(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	writeProbe(t, "s-probe", 0)
	c, _ := readConversation("s-probe", nil)
	ids := func(es []Said) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.ID)
		}
		return out
	}
	// Meta entries count against no limit, so the page reaches back past those before its oldest entry.
	got, more, err := page(c.entries, "", 3)
	if want := []string{"s-duration", "L26", "u-compact", "s-boundary", "u-caveat", "u-command", "at-ref"}; err != nil || !more || !slices.Equal(ids(got), want) {
		t.Fatalf("the last page is %v, more %v (%v), want %v", ids(got), more, err, want)
	}
	got, more, err = page(c.entries, "s-duration", 100)
	if err != nil || more || len(got) != len(probeLines)-7 || got[0].ID != "L1" {
		t.Fatalf("the page before s-duration is %v, more %v (%v)", ids(got), more, err)
	}
	got, more, _ = page(c.entries, "a-text", 2)
	if !slices.Equal(ids(got), []string{"u-task", "at-tools", "L6", "a-think"}) || !more {
		t.Fatalf("the page before a-text is %v, more %v", ids(got), more)
	}
	if _, _, err := page(c.entries, "nope", 2); err == nil {
		t.Fatal("a page before an unknown entry succeeded")
	}
}

// convDial sends req on a connection of its own and returns a reader of the lines that come back.
func convDial(t *testing.T, req Request) func() ConvLine {
	t.Helper()
	conn, err := net.Dial("unix", socketPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(conn)
	return func() ConvLine {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var l ConvLine
		if err := dec.Decode(&l); err != nil {
			t.Fatal(err)
		}
		return l
	}
}

func TestConversationRequest(t *testing.T) {
	d, _ := testDaemon(t)
	writeProbe(t, "s-probe", 0)
	d.mu.Lock()
	d.nodes["a"].SessionID = "s-probe"
	d.nodes["a"].handedLocked(masterNode, "Fix the parser and run the tests.")
	d.mu.Unlock()

	got := convDial(t, Request{Op: "conversation", Name: "a", Limit: 3})()
	if got.Type != "result" || got.Session != "s-probe" || !got.More || len(got.Entries) != 7 || got.Error != "" {
		t.Fatalf("the conversation's last entries are %+v", got)
	}
	got = convDial(t, Request{Op: "conversation", Name: "a", Before: "a-think", Limit: 1})()
	if want := []string{`u-task prompt master "Fix the parser and run the tests."`, `at-tools meta attachment/deferred_tools_record`, `L6 meta api-request`}; !slices.Equal(lines(got.Entries), want) || got.More != true {
		t.Fatalf("the entries before a-think are %v, more %v; the prompt the master handed a names it", lines(got.Entries), got.More)
	}
	if got := convDial(t, Request{Op: "conversation", Name: "nobody"})(); got.Error == "" {
		t.Fatalf("an unknown session's conversation is %+v", got)
	}

	// bosun transcript reads through the same entries.
	if text, err := d.transcript("a", 1); err != nil || !strings.Contains(text, "> /compact\n") {
		t.Fatalf("the transcript is %q (%v)", text, err)
	}
}

// fakeSocket is the socket of the fake supervisor that runs, which controlSocket names once a test has installed it.
var fakeSocket struct {
	once sync.Once
	mu   sync.Mutex
	name string
}

// useFakeSocket points controlSocket at name until the test ends.
// A follow's goroutines outlive its test, so controlSocket changes once, and the socket it names changes under a lock.
func useFakeSocket(t *testing.T, name string) {
	fakeSocket.once.Do(func() {
		controlSocket = func() (string, error) {
			fakeSocket.mu.Lock()
			defer fakeSocket.mu.Unlock()
			if fakeSocket.name == "" {
				return "", errors.New("no fake supervisor runs")
			}
			return fakeSocket.name, nil
		}
	})
	fakeSocket.mu.Lock()
	fakeSocket.name = name
	fakeSocket.mu.Unlock()
	t.Cleanup(func() {
		fakeSocket.mu.Lock()
		fakeSocket.name = ""
		fakeSocket.mu.Unlock()
	})
}

// fakeSupervisor answers on an abstract socket as Claude Code's supervisor does: serve gets each request's op and short, and returns the lines to answer with.
func fakeSupervisor(t *testing.T, serve func(op, short string) []string) {
	t.Helper()
	name := fmt.Sprintf("@bosun-supervisor-%d-%s", os.Getpid(), t.Name())
	ln, err := net.Listen("unix", name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	useFakeSocket(t, name)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var req struct{ Op, Short string }
				if json.NewDecoder(conn).Decode(&req) != nil {
					return
				}
				for _, l := range serve(req.Op, req.Short) {
					if l == "" {
						// A blank line holds the connection open until the test ends.
						<-t.Context().Done()
						return
					}
					_, _ = conn.Write([]byte(l + "\n"))
				}
			}()
		}
	}()
}

// followFast makes a follow read the transcript every 10 ms, and resubscribe at once.
func followFast(t *testing.T) {
	poll, wait := followPoll, resubscribeWait
	followPoll, resubscribeWait = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { followPoll, resubscribeWait = poll, wait })
}

func TestFollow(t *testing.T) {
	d, _ := testDaemon(t)
	followFast(t)
	fakeSupervisor(t, func(op, short string) []string { return nil })
	data := writeProbe(t, "s-probe", 9)
	d.mu.Lock()
	d.nodes["a"].SessionID = "s-probe"
	d.mu.Unlock()

	next := convDial(t, Request{Op: "follow", Name: "a", Limit: 2})
	if got := next(); got.Type != "reset" || got.Session != "s-probe" {
		t.Fatalf("follow began with %+v", got)
	}
	got := next()
	if want := []string{`a-text text "I'll run the tests first."`, `a-test tool Bash`}; got.Type != "entries" || !got.More || !slices.Equal(lines(got.Entries), want) {
		t.Fatalf("follow's first entries are %v, more %v", lines(got.Entries), got.More)
	}

	// The result lands on the call, which goes out again; a line still being written waits for its end.
	full, _ := os.ReadFile("testdata/probe.jsonl")
	rest := bytes.SplitAfter(full[len(data):], []byte("\n"))
	half := len(rest[1]) / 2
	appendProbeFile(t, "s-probe", slices.Concat(rest[0], rest[1][:half]))
	got = next()
	if want := []string{`a-test tool Bash → "FAIL\tbosun\t0.2s" error`}; got.Type != "entries" || !slices.Equal(lines(got.Entries), want) {
		t.Fatalf("the result came as %v", lines(got.Entries))
	}
	appendProbeFile(t, "s-probe", rest[1][half:])
	if got := next(); len(got.Entries) != 1 || got.Entries[0].ID != "a-edit" || got.Entries[0].Result != nil {
		t.Fatalf("the edit came as %v", lines(got.Entries))
	}

	// A clear moves the session to a new conversation, which follow starts over with.
	writeProbeFile(t, "s-cleared", []byte(`{"type":"user","message":{"content":"<command-name>/clear</command-name>\n<command-args></command-args>"},"uuid":"u-clear","timestamp":"2026-10-26T22:00:00Z"}`+"\n"))
	d.mu.Lock()
	d.nodes["a"].Conversation = "s-cleared"
	d.mu.Unlock()
	if got := next(); got.Type != "reset" || got.Session != "s-cleared" {
		t.Fatalf("the clear came as %+v", got)
	}
	if got := next(); got.Type != "entries" || got.More || !slices.Equal(lines(got.Entries), []string{`u-clear command /clear ""`}) {
		t.Fatalf("the cleared conversation began with %v", lines(got.Entries))
	}

	// Once the session is gone, follow says so and ends.
	d.mu.Lock()
	delete(d.nodes, "a")
	d.mu.Unlock()
	if got := next(); got.Type != "result" || got.Error == "" {
		t.Fatalf("follow of a session gone sent %+v", got)
	}
}

// Claude Code's <task-notification> goes out as a task entry with its fields read, and its text whole.
func TestConversationReadsTaskNotifications(t *testing.T) {
	data, err := os.ReadFile("testdata/tasks.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	c := newConversation(nil)
	for _, l := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		c.add(l)
	}
	exit := func(n int) *int { return &n }
	want := map[string]Said{
		"u-done":  {Kind: "task", From: "claude", Task: "bcx0xj4v7", Call: "toolu_01SwBmyVY5JU4JasJoKjrMxh", OutputFile: "/tmp/claude-1000/-home-egor-Projects-bosun/26b1c4e0/tasks/bcx0xj4v7.output", Status: "completed", Summary: `Background command "Run the bosun daemon on haiku for the e2e test" completed (exit code 0)`, Exit: exit(0)},
		"u-fail":  {Kind: "task", From: "claude", Task: "k7f2", Call: "toolu_02Failed", OutputFile: "/tmp/claude-1000/tasks/k7f2.output", Status: "failed", Summary: `Background command "make" failed (exit code 2)`, Exit: exit(2)},
		"u-agent": {Kind: "task", From: "claude", Task: "ae2c8306d03d40f96", Status: "completed", Summary: `Agent "Review doc edits" finished`},
	}
	for id, w := range want {
		i, ok := c.at[id]
		if !ok {
			t.Fatalf("no entry %s", id)
		}
		g := c.entries[i]
		if !strings.HasPrefix(g.Text, "<task-notification>") || !strings.Contains(g.Text, "<task-id>"+w.Task+"</task-id>") {
			t.Fatalf("%s lost its text: %q", id, g.Text)
		}
		g.ID, g.At, g.Text = "", time.Time{}, ""
		gj, _ := json.Marshal(g)
		wj, _ := json.Marshal(w)
		if string(gj) != string(wj) {
			t.Fatalf("%s is %s, want %s", id, gj, wj)
		}
	}
	if e := c.entries[0]; e.Kind != "queued" {
		t.Fatalf("the first entry is %+v", e)
	}
}
