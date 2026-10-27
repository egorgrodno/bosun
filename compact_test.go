package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

// compacted runs node's SessionStart hook for the /compact it ran.
func compacted(node, sid string) {
	raw, _ := json.Marshal(map[string]any{"hook_event_name": "SessionStart", "session_id": sid, "source": "compact"})
	hookOutput(node, "", bytes.NewReader(raw))
}

// callAsync sends req and hands back its response once it comes.
func callAsync(req Request) chan Response {
	ch := make(chan Response, 1)
	go func() {
		resp, err := call(context.Background(), req)
		if err != nil {
			resp.Error = err.Error()
		}
		ch <- resp
	}()
	return ch
}

func receiveResponse(t *testing.T, ch chan Response) Response {
	t.Helper()
	select {
	case resp := <-ch:
		return resp
	case <-time.After(5 * time.Second):
		t.Fatal("no response")
		return Response{}
	}
}

// awaitOps waits until the supervisor has got n requests.
func awaitOps(t *testing.T, sup *controlFake, n int) []string {
	t.Helper()
	for range 500 {
		if ops := sup.ops(); len(ops) >= n {
			return ops
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the supervisor got only %v", sup.ops())
	return nil
}

// Only the user compacts or clears, and only the master's or the desk's conversation.
func TestCompactAndClearRefuse(t *testing.T) {
	d, _ := testDesk(t)
	sup := fakeControl(t, d)
	for _, op := range []string{"compact", "clear"} {
		for _, node := range []string{masterNode, deskNode, "a"} {
			if _, err := call(context.Background(), Request{Op: op, Node: node, Name: deskNode}); err == nil || !strings.Contains(err.Error(), "only the user") {
				t.Fatalf("%s from %s: %v", op, node, err)
			}
			for _, tool := range tools(node) {
				if tool.Name == op {
					t.Fatalf("%s has the tool %s", node, op)
				}
			}
			if _, err := newMCPServer(node, io.Discard).run(context.Background(), op, json.RawMessage(`{"name":"desk"}`)); err == nil {
				t.Fatalf("%s's MCP server ran %s", node, op)
			}
		}
		for _, name := range []string{"a", "", "stranger"} {
			if _, err := call(context.Background(), Request{Op: op, Name: name}); err == nil || !strings.Contains(err.Error(), "only the master and the desk") {
				t.Fatalf("%s of %q: %v", op, name, err)
			}
		}
	}
	_, err := call(context.Background(), Request{Op: "clear", Name: masterNode})
	if err == nil || err.Error() != "bosun clears the master only once no worker is left, since its conversation keeps track of them: 1 left" {
		t.Fatalf("clearing the master with worker a left: %v", err)
	}
	if ops := sup.ops(); len(ops) != 0 {
		t.Fatalf("the supervisor got %v", ops)
	}
	if n := len(logOf(d, "compact")) + len(logOf(d, "clear")); n != 0 {
		t.Fatalf("%d uses logged", n)
	}
}

// A running desk takes /compact through the supervisor, and a tell that comes meanwhile waits until it has compacted.
func TestCompactRunningDesk(t *testing.T) {
	d, fc := testDesk(t)
	sup := fakeControl(t, d)
	done := callAsync(Request{Op: "compact", Name: deskNode})
	if ops := awaitOps(t, sup, 2); ops[1] != "reply" || sup.last()["text"] != "/compact" || sup.last()["short"] != "00000002" {
		t.Fatalf("the supervisor got %v, last %v", ops, sup.last())
	}
	mustCall(t, Request{Op: "tell", Text: "x"})
	time.Sleep(100 * time.Millisecond)
	if got := resumes(fc); len(got) != 1 {
		t.Fatalf("claude ran with %q while the desk compacted", got)
	}
	compacted(deskNode, "s-d")
	if resp := receiveResponse(t, done); resp.Error != "" || resp.Text != "Compacted desk's conversation." {
		t.Fatalf("compact answered %+v", resp)
	}
	if args := awaitStarts(t, fc, 3); !slices.Equal(args, []string{"--bg", "--resume", "s-d", "--", deskReport([]Event{{Kind: "tell", Text: "x"}})}) {
		t.Fatalf("the tell resumed the desk with %q", args)
	}
	if logged := logOf(d, "compact"); len(logged) != 1 || logged[0].By != "user" || logged[0].Node != deskNode || logged[0].Via != "supervisor" || logged[0].Error != "" {
		t.Fatalf("logged %+v", logged)
	}
	if len(fc.stops) != 1 {
		t.Fatalf("stops %v", fc.stops)
	}
	assertKeyKept(t, d)
}

// A stopped master compacts in a resume, without the key.
func TestCompactStoppedMaster(t *testing.T) {
	d, fc := staleMaster(t, "", time.Time{}, true)
	sup := fakeControl(t, d)
	done := callAsync(Request{Op: "compact", Name: masterNode})
	awaitStarts(t, fc, 2)
	if args := fc.lastStart(); strings.Join(args, " ") != "--bg --resume s-m -- /compact" {
		t.Fatalf("started %q", args)
	}
	compacted(masterNode, "s-m")
	if resp := receiveResponse(t, done); resp.Error != "" || resp.Text != "Compacted master's conversation." {
		t.Fatalf("compact answered %+v", resp)
	}
	if ops := sup.ops(); len(ops) != 0 {
		t.Fatalf("the supervisor got %v", ops)
	}
	if logged := logOf(d, "compact"); len(logged) != 1 || logged[0].Via != "resume" {
		t.Fatalf("logged %+v", logged)
	}
}

// /compact waits for the turn's Stop, so the master's next_event returns at once, and the events it took stay with it.
func TestCompactMasterInTurn(t *testing.T) {
	d, _, sup := steerDaemon(t, "busy")
	d.mu.Lock()
	d.nodes[masterNode].Status, d.nodes[masterNode].Turn = "busy", true
	d.mu.Unlock()
	wait := compactWait
	compactWait = 100 * time.Millisecond
	t.Cleanup(func() { compactWait = wait })
	next := callAsync(Request{Op: "next_event", Node: masterNode})
	time.Sleep(50 * time.Millisecond)
	resp := mustCall(t, Request{Op: "compact", Name: masterNode})
	if !strings.Contains(resp.Text, "compacts once its turn ends") {
		t.Fatalf("compact answered %q", resp.Text)
	}
	if evs := receiveResponse(t, next).Events; len(evs) != 0 {
		t.Fatalf("next_event returned %+v", evs)
	}
	if sup.last()["op"] != "reply" || sup.last()["text"] != "/compact" {
		t.Fatalf("the supervisor got %v", sup.last())
	}
}

// A master in its turn is cut short and cleared, and the events its turn took wait again for the new conversation.
func TestClearMasterInTurn(t *testing.T) {
	d, fc := staleMaster(t, "busy", time.Time{}, false)
	d.mu.Lock()
	m := d.nodes[masterNode]
	m.Status, m.Turn = "busy", true
	d.taken, d.takenKey = []Event{{Kind: "tell", Text: "x"}}, "k"
	dec := &Decision{Kind: "permission", Node: masterNode, Holder: "user", Tool: "Bash"}
	d.openLocked(dec)
	d.mu.Unlock()
	resp := mustCall(t, Request{Op: "clear", Name: masterNode})
	if resp.Text != "Cleared master's conversation." {
		t.Fatalf("clear answered %q", resp.Text)
	}
	awaitWoken(t, d, fc, 3)
	if got, want := resumes(fc), []string{"--bg --resume s-m -- /clear", "--bg --resume s-m -- " + eventsWaiting}; !slices.Equal(got, want) {
		t.Fatalf("claude ran with %q, want %q", got, want)
	}
	d.mu.Lock()
	conversation, open := m.Conversation, d.decisions[dec.ID]
	d.mu.Unlock()
	if conversation != "s-m-2" || open != nil {
		t.Fatalf("conversation %q, decision %+v", conversation, open)
	}
	if evs := mustCall(t, Request{Op: "next_event", Node: masterNode}).Events; len(evs) != 1 || evs[0].Text != "x" || !evs[0].Again {
		t.Fatalf("events = %+v", evs)
	}
	if logged := logOf(d, "clear"); len(logged) != 1 || logged[0].By != "user" || logged[0].Node != masterNode || logged[0].Via != "resume" || logged[0].Error != "" {
		t.Fatalf("logged %+v", logged)
	}
}

// The desk clears whatever runs in the fleet, and the tells its cut turn took go to its new conversation.
func TestClearDeskInTurn(t *testing.T) {
	d, fc := staleDesk(t)
	fc.setStatus("00000002", "busy")
	d.mu.Lock()
	desk := d.nodes[deskNode]
	desk.Status, desk.Turn, desk.Unseen = "busy", true, true
	desk.Taken = []Event{{Kind: "tell", Text: "x"}}
	d.mu.Unlock()
	if resp := mustCall(t, Request{Op: "clear", Name: deskNode}); resp.Text != "Cleared desk's conversation." {
		t.Fatalf("clear answered %q", resp.Text)
	}
	args := awaitStarts(t, fc, 4)
	if got := resumes(fc)[1]; got != "--bg --resume s-d -- /clear" {
		t.Fatalf("the clear ran claude with %q", got)
	}
	if !strings.Contains(args[len(args)-1], "x") {
		t.Fatalf("the tell resumed the desk with %q", args)
	}
	d.mu.Lock()
	conversation := desk.Conversation
	d.mu.Unlock()
	if conversation != "s-d-3" {
		t.Fatalf("the desk's conversation is %q", conversation)
	}
}
