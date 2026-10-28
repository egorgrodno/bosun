package main

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// scopedTree adds b under the master, c under a, and d under c to testDaemon's master and worker a.
func scopedTree(t *testing.T) *daemon {
	d, _ := testDaemon(t)
	d.mu.Lock()
	for name, parent := range map[string]string{"b": masterNode, "c": "a", "e": "c"} {
		d.nodes[name] = &Session{Node: name, Parent: parent, Repo: "/repo", Dir: "/repo/.claude/worktrees/" + name, Depth: 1, ID: strings.Repeat(name, 8), Status: "idle"}
	}
	d.mu.Unlock()
	return d
}

func nodesOf(r Response) []string {
	var out []string
	for _, n := range r.Sessions {
		out = append(out, n.Node)
	}
	slices.Sort(out)
	return out
}

func TestReadScope(t *testing.T) {
	scopedTree(t)
	for _, tc := range []struct {
		caller string
		want   []string
	}{
		{"", []string{"a", "b", "c", "e", masterNode}},
		{masterNode, []string{"a", "b", "c", "e", masterNode}},
		{"a", []string{"a", "c", "e"}},
		{"c", []string{"c", "e"}},
		{"b", []string{"b"}},
		{"e", []string{"e"}},
	} {
		resp := mustCall(t, Request{Op: "status", Node: tc.caller})
		if got := nodesOf(resp); !slices.Equal(got, tc.want) {
			t.Errorf("%q sees %v, want %v", tc.caller, got, tc.want)
		}
		for _, n := range resp.Sessions {
			if n.Node == tc.caller && n.Parent != "" && tc.caller != "" && isWorker(tc.caller) {
				t.Errorf("%q is not the root of its own tree: parent %q", tc.caller, n.Parent)
			}
		}
	}
}

func TestReadScopeDeskSeesAll(t *testing.T) {
	d, _ := testDesk(t)
	d.mu.Lock()
	d.nodes["b"] = &Session{Node: "b", Parent: masterNode, Dir: "/repo/.claude/worktrees/b", ID: "bbbbbbbb", Status: "idle"}
	d.mu.Unlock()
	if got := nodesOf(mustCall(t, Request{Op: "status", Node: deskNode})); !slices.Contains(got, "b") || !slices.Contains(got, "a") {
		t.Fatalf("the desk sees %v", got)
	}
}

// A worker's status leaves out the decisions, messages and log entries of sessions outside its subtree.
func TestReadScopeStatusFiltersDecisionsAndLog(t *testing.T) {
	d := scopedTree(t)
	d.mu.Lock()
	d.logLocked(Entry{Kind: "spawned", By: "worker", From: "a", Node: "c"})
	d.logLocked(Entry{Kind: "spawned", By: "user", Node: "b"})
	d.logLocked(Entry{Kind: "paused", By: "user"})
	d.messageLocked(Event{Kind: "message", Node: "b", From: masterNode, Text: "to b"})
	d.messageLocked(Event{Kind: "message", Node: "c", From: "a", Text: "to c"})
	d.mu.Unlock()
	hook := runHookAsync("b", hookEvent("PermissionRequest", "", "Bash", bashTest))
	openDecision(t, 1)

	resp := mustCall(t, Request{Op: "status", Node: "a"})
	if len(resp.Decisions) != 0 {
		t.Errorf("a sees b's decision %+v", resp.Decisions)
	}
	if len(resp.Messages) != 1 || resp.Messages[0].Text != "to c" {
		t.Errorf("a sees the messages %+v", resp.Messages)
	}
	if len(resp.Log) != 2 || resp.Log[0].Node != "c" || resp.Log[1].Kind != "message" || resp.Log[1].Text != "to c" {
		t.Errorf("a sees the log %+v", resp.Log)
	}
	if got := mustCall(t, Request{Op: "status", Node: "b"}); len(got.Decisions) != 1 {
		t.Errorf("b does not see its own decision: %+v", got.Decisions)
	}
	if got := mustCall(t, Request{Op: "status", Node: masterNode}); len(got.Decisions) != 1 || len(got.Log) < 3 {
		t.Errorf("the master sees %d decisions, %d log entries", len(got.Decisions), len(got.Log))
	}
	mustCall(t, Request{Op: "deny", ID: "1"})
	receive(t, hook)
}

func TestReadScopeConversationAndTranscript(t *testing.T) {
	d := scopedTree(t)
	writeProbe(t, "s-probe", 0)
	d.mu.Lock()
	d.nodes["a"].SessionID = "s-probe"
	d.mu.Unlock()

	// A worker reads its own conversation, and the master and the desk read it too.
	for _, caller := range []string{"a", masterNode, deskNode, ""} {
		if got := convDial(t, Request{Op: "conversation", Node: caller, Name: "a", Limit: 3})(); got.Error != "" || len(got.Entries) == 0 {
			t.Errorf("%q reads a: %+v", caller, got)
		}
	}
	// It cannot read its parent, a sibling or the master, nor a stranger's transcript.
	for _, tc := range [][2]string{{"c", "a"}, {"c", masterNode}, {"c", "b"}, {"a", "b"}, {"b", "a"}, {"e", "c"}} {
		if got := convDial(t, Request{Op: "conversation", Node: tc[0], Name: tc[1]})(); !strings.Contains(got.Error, "your subtree") || len(got.Entries) != 0 {
			t.Errorf("%s reads %s's conversation: %+v", tc[0], tc[1], got)
		}
		if _, err := call(context.Background(), Request{Op: "transcript", Node: tc[0], Name: tc[1]}); err == nil || !strings.Contains(err.Error(), "your subtree") {
			t.Errorf("%s reads %s's transcript: %v", tc[0], tc[1], err)
		}
	}
	// Its child's, however, it reads.
	if got := convDial(t, Request{Op: "conversation", Node: "a", Name: "c"})(); strings.Contains(got.Error, "your subtree") {
		t.Errorf("a cannot read its child: %+v", got)
	}
}

func TestReadToolConversationDropsMeta(t *testing.T) {
	d := scopedTree(t)
	writeProbe(t, "s-probe", 0)
	d.mu.Lock()
	d.nodes["a"].SessionID = "s-probe"
	d.mu.Unlock()
	text, err := newMCPServer("a", nil).run(context.Background(), "conversation", json.RawMessage(`{"name":"a"}`))
	if err != nil {
		t.Fatal(err)
	}
	var line ConvLine
	if err := json.Unmarshal([]byte(text), &line); err != nil || len(line.Entries) == 0 {
		t.Fatalf("the tool returned %q (%v)", text, err)
	}
	for _, e := range line.Entries {
		if e.Kind == "meta" {
			t.Fatalf("a meta entry came through: %+v", e)
		}
	}
	if _, err := newMCPServer("a", nil).run(context.Background(), "conversation", json.RawMessage(`{"name":"b"}`)); err == nil || !strings.Contains(err.Error(), "your subtree") {
		t.Fatalf("a read b's conversation: %v", err)
	}
}

// logFixture fills the log with entries of every kind a filter tells apart.
func logFixture(t *testing.T) *daemon {
	d := scopedTree(t)
	d.mu.Lock()
	defer d.mu.Unlock()
	dec := func(id, node, holder string) *Decision {
		return &Decision{ID: id, Kind: "permission", Node: node, Holder: holder, Tool: "Bash"}
	}
	for _, e := range []Entry{
		{Kind: "spawned", By: "user", Node: "b"},
		{Kind: "decision", By: "worker", From: "a", Node: "a", Decision: dec("1", "a", "user")},
		{Kind: "settled", By: "user", Node: "a", Behavior: "deny", Decision: dec("1", "a", "user")},
		{Kind: "settled", By: "master", From: masterNode, Node: "c", Behavior: "allow", Decision: dec("2", "c", "master")},
		{Kind: "escalated", By: "master", From: masterNode, Node: "b", Text: "touches CI", Decision: dec("3", "b", "user")},
		{Kind: "settled", By: "user", Node: "b", Behavior: "allow", Decision: dec("3", "b", "user")},
		{Kind: "settled", By: "master", From: masterNode, Node: "e", Behavior: "allow", Route: "master", Decision: dec("4", "e", "master")},
		{Kind: "settled", By: "user", Node: "e", Behavior: "allow", Route: "userOnly", Decision: dec("5", "e", "user")},
		{Kind: "say", By: "user", Node: "a", Text: "hello"},
		{Kind: "decision", By: "bosun", Node: masterNode, Decision: &Decision{ID: "6", Kind: "notice", Node: masterNode, Holder: "user"}},
	} {
		d.logLocked(e)
	}
	return d
}

func readLogSeqs(t *testing.T, caller string, q LogQuery) []int {
	t.Helper()
	resp := mustCall(t, Request{Op: "log", Node: caller, Log: &q})
	var seqs []int
	for _, e := range resp.Log {
		seqs = append(seqs, e.Seq)
	}
	return seqs
}

func TestReadLogFilters(t *testing.T) {
	d := logFixture(t)
	d.mu.Lock()
	last := d.logSeq
	// Entries 1 to 4 are an hour old.
	for i := range d.logged {
		if d.logged[i].Seq <= 4 {
			d.logged[i].At = time.Now().Add(-time.Hour)
		}
	}
	d.mu.Unlock()
	first := last - 9
	seq := func(i ...int) []int {
		var out []int
		for _, n := range i {
			out = append(out, first+n)
		}
		return out
	}
	for name, tc := range map[string]struct {
		q    LogQuery
		want []int
	}{
		"all":          {LogQuery{}, seq(0, 1, 2, 3, 4, 5, 6, 7, 8, 9)},
		"kind":         {LogQuery{Kind: "settled"}, seq(2, 3, 5, 6, 7)},
		"node":         {LogQuery{Node: "a"}, seq(1, 2, 8)},
		"node by from": {LogQuery{Node: masterNode}, seq(3, 4, 6, 9)},
		"by":           {LogQuery{By: "master"}, seq(3, 4, 6)},
		"kind and by":  {LogQuery{Kind: "settled", By: "user"}, seq(2, 5, 7)},
		"duration":     {LogQuery{Since: "30m"}, seq(4, 5, 6, 7, 8, 9)},
		"rfc3339":      {LogQuery{Since: time.Now().Add(-2 * time.Minute).Format(time.RFC3339)}, seq(4, 5, 6, 7, 8, 9)},
		"limit":        {LogQuery{Limit: 2}, seq(8, 9)},
		// Entry 6 took the master's route. Entries without a route count by the route their decision took, and a notice reaches the user.
		"reached user":      {LogQuery{ReachedUser: true}, seq(1, 2, 4, 5, 7, 9)},
		"reached user kind": {LogQuery{ReachedUser: true, Kind: "settled"}, seq(2, 5, 7)},
	} {
		if got := readLogSeqs(t, "", tc.q); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
	if _, err := call(context.Background(), Request{Op: "log", Log: &LogQuery{Since: "yesterday"}}); err == nil {
		t.Error("an unreadable since went through")
	}
}

func TestReadLogScope(t *testing.T) {
	logFixture(t)
	nodesOfLog := func(caller string, q LogQuery) []string {
		var out []string
		for _, e := range mustCall(t, Request{Op: "log", Node: caller, Log: &q}).Log {
			out = append(out, e.Node)
		}
		return out
	}
	// a reads the entries about itself and its descendants c and e.
	got := nodesOfLog("a", LogQuery{})
	want := []string{"a", "a", "c", "e", "e", "a"}
	if !slices.Equal(got, want) {
		t.Errorf("a reads the log of %v, want %v", got, want)
	}
	if got := nodesOfLog("c", LogQuery{}); !slices.Equal(got, []string{"c", "e", "e"}) {
		t.Errorf("c reads the log of %v", got)
	}
	// A filter on a node outside the subtree is refused, rather than answered empty.
	if _, err := call(context.Background(), Request{Op: "log", Node: "c", Log: &LogQuery{Node: "a"}}); err == nil || !strings.Contains(err.Error(), "your subtree") {
		t.Errorf("c filtered the log by its parent: %v", err)
	}
	if got := nodesOfLog("b", LogQuery{Kind: "settled"}); !slices.Equal(got, []string{"b"}) {
		t.Errorf("b reads the settled entries of %v", got)
	}
	if n := len(readLogSeqs(t, masterNode, LogQuery{})); n != 10 {
		t.Errorf("the master reads %d entries", n)
	}
	if n := len(readLogSeqs(t, deskNode, LogQuery{})); n != 10 {
		t.Errorf("the desk reads %d entries", n)
	}
}

func TestReadToolLogText(t *testing.T) {
	logFixture(t)
	srv := newMCPServer(masterNode, nil)
	text, err := srv.run(context.Background(), "log", json.RawMessage(`{"kind":"escalated"}`))
	if err != nil || !strings.Contains(text, "b") || strings.Count(text, "\n") != 1 {
		t.Fatalf("the log tool shows %q (%v)", text, err)
	}
	full, err := srv.run(context.Background(), "log", json.RawMessage(`{"reached_user":true,"kind":"settled","full":true}`))
	var entries []Entry
	if err != nil || json.Unmarshal([]byte(full), &entries) != nil || len(entries) != 3 {
		t.Fatalf("the full log is %q (%v)", full, err)
	}
	if none, _ := srv.run(context.Background(), "log", json.RawMessage(`{"node":"nobody"}`)); none != "No entries match." {
		t.Fatalf("an empty log shows %q", none)
	}
}

func TestReadActivity(t *testing.T) {
	scopedTree(t)
	receive(t, runHookAsync("a", hookEvent("PreToolUse", "t1", "Bash", map[string]any{"command": "go test ./..."})))
	receive(t, runHookAsync("b", hookEvent("PreToolUse", "t2", "Read", map[string]any{"file_path": "/repo/secret"})))

	srv := newMCPServer("a", nil)
	text, err := srv.run(context.Background(), "activity", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"a is busy", "Bash go test ./...", "open call: Bash", "c is idle"} {
		if !strings.Contains(text, want) {
			t.Errorf("a's activity lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "secret") || strings.Contains(text, "b is") {
		t.Errorf("a's activity shows b:\n%s", text)
	}
	if _, err := srv.run(context.Background(), "activity", json.RawMessage(`{"name":"b"}`)); err == nil || !strings.Contains(err.Error(), "your subtree") {
		t.Errorf("a read b's activity: %v", err)
	}
	if _, err := newMCPServer("c", nil).run(context.Background(), "activity", json.RawMessage(`{"name":"a"}`)); err == nil {
		t.Error("c read its parent's activity")
	}
	one, err := newMCPServer(masterNode, nil).run(context.Background(), "activity", json.RawMessage(`{"name":"b"}`))
	if err != nil || !strings.Contains(one, "Read /repo/secret") || strings.Contains(one, "a is") {
		t.Errorf("the master's activity of b is %q (%v)", one, err)
	}
}

// Each role gets the read tools, and none gets a tool that steers.
func TestReadToolsPerRole(t *testing.T) {
	for _, node := range []string{masterNode, deskNode, "a"} {
		names := map[string]bool{}
		for _, tl := range tools(node) {
			names[tl.Name] = true
		}
		for _, want := range []string{"status", "conversation", "log", "activity"} {
			if !names[want] {
				t.Errorf("%s lacks %s", node, want)
			}
		}
		for _, banned := range []string{"say", "interrupt", "approve", "deny", "compact", "clear", "kill", "attach"} {
			if names[banned] {
				t.Errorf("%s has the steering tool %s", node, banned)
			}
		}
	}
	// The ops behind the tools steer nothing, so no read tool maps to one that does.
	for _, op := range []string{"say", "interrupt", "clear", "compact"} {
		if _, err := newMCPServer("a", nil).run(context.Background(), op, nil); err == nil {
			t.Errorf("a worker runs %s", op)
		}
	}
}
