package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

func goAway(t *testing.T, words string) string {
	t.Helper()
	return mustCall(t, Request{Op: "away", Name: "on", Words: words}).Text
}

// parkedDeny is what a hook prints for a call bosun parked.
func parkedDeny(event string) string {
	reason, _ := json.Marshal(parkedCall)
	if event == "PermissionRequest" {
		return fmt.Sprintf(`{"hookSpecificOutput":{"decision":{"behavior":"deny","message":%s},"hookEventName":"PermissionRequest"}}`, reason)
	}
	return fmt.Sprintf(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":%s}}`, reason)
}

// awaitParked waits until the daemon has parked n decisions, and returns them.
func awaitParked(t *testing.T, n int) []Parked {
	t.Helper()
	for range 500 {
		if p := mustCall(t, Request{Op: "status"}).Parked; len(p) == n {
			return p
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("not %d parked: %+v", n, mustCall(t, Request{Op: "status"}).Parked)
	return nil
}

func noDecisions(t *testing.T) {
	t.Helper()
	if decs := mustCall(t, Request{Op: "status"}).Decisions; len(decs) != 0 {
		t.Fatalf("decisions open: %+v", decs)
	}
}

// While the user is away, what would wait on them is denied or answered at once and listed, and the frontends see no decision.
func TestAwayParks(t *testing.T) {
	d, _ := testDesk(t)
	_, next := watchKinds(t)
	if text := goAway(t, "off to bed"); !strings.Contains(text, "Away mode is on") {
		t.Fatalf("away on said %q", text)
	}
	awaitEvent(t, next, "away")

	// A reserved action on the user's list.
	if out := receive(t, runHookAsync("a", hookEvent("PreToolUse", "t1", "Bash", map[string]any{"command": "git push --force origin HEAD"}))); out != parkedDeny("PreToolUse") {
		t.Fatalf("a force push printed %s", out)
	}
	awaitEvent(t, next, "parked")
	// The master's and the desk's own permissions.
	for _, node := range []string{masterNode, deskNode} {
		if out := receive(t, runHookAsync(node, hookEvent("PermissionRequest", "", "Bash", bashTest))); out != parkedDeny("PermissionRequest") {
			t.Fatalf("%s's permission printed %s", node, out)
		}
	}
	// A permission the master escalates.
	hook := runHookAsync("a", hookEvent("PermissionRequest", "", "Bash", map[string]any{"command": "rm -rf build"}))
	dec := mustCall(t, Request{Op: "next_event", Node: masterNode}).Events[0].Decision
	if text := mustCall(t, Request{Op: "escalate", Node: masterNode, ID: dec.ID, Text: "deletes a lot"}).Text; !strings.Contains(text, "parked decision "+dec.ID) {
		t.Fatalf("escalate said %q", text)
	}
	if out := receive(t, hook); out != parkedDeny("PermissionRequest") {
		t.Fatalf("the escalated permission printed %s", out)
	}
	// A question the master escalates.
	got := escalatedQuestion(t, 1, "X or Y?")
	if answer := receive(t, got); answer != parkedQuestion {
		t.Fatalf("the escalated question got %q", answer)
	}
	// A notice.
	if text := mustCall(t, Request{Op: "notify", Node: masterNode, Text: "b failed"}).Text; !strings.Contains(text, "parked") {
		t.Fatalf("notify said %q", text)
	}
	noDecisions(t)

	parked := awaitParked(t, 6)
	var why []string
	for _, p := range parked {
		why = append(why, p.Decision.Node+": "+p.Why)
	}
	want := []string{
		"a: it is a force push, which is on your list",
		"master: it is the master's own permission",
		"desk: it is the desk's own permission",
		"a: the master escalated it: deletes a lot",
		"a: the master escalated it",
		"master: it is a notice from the master",
	}
	if !slices.Equal(why, want) {
		t.Fatalf("parked\n%q\nwant\n%q", why, want)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	kinds := map[string]int{}
	for _, e := range d.logged {
		kinds[e.Kind]++
	}
	if kinds["away"] != 1 || kinds["parked"] != 6 || kinds["decision"] != 0 {
		t.Fatalf("logged %v", kinds)
	}
}

// Turning the away mode on parks what waits on the user already, and leaves a notice to the user.
func TestAwayParksWaiting(t *testing.T) {
	testDaemon(t)
	hook := runHookAsync("a", hookEvent("PreToolUse", "t1", "Bash", map[string]any{"command": "git push --force origin HEAD"}))
	openDecision(t, 1)
	mustCall(t, Request{Op: "notify", Node: masterNode, Text: "b failed"})
	openDecision(t, 2)
	if text := goAway(t, ""); !strings.Contains(text, "parked the 1 decisions") {
		t.Fatalf("away on said %q", text)
	}
	if out := receive(t, hook); out != parkedDeny("PreToolUse") {
		t.Fatalf("the waiting push printed %s", out)
	}
	if dec := openDecision(t, 1); dec.Kind != "notice" {
		t.Fatalf("left open %+v", dec)
	}
	awaitParked(t, 1)
}

// The away mode and what it parked outlast a restart of the daemon.
func TestAwayOutlastsRestart(t *testing.T) {
	d, _ := testDaemon(t)
	goAway(t, "logging off")
	receive(t, runHookAsync(masterNode, hookEvent("PermissionRequest", "", "Bash", bashTest)))
	awaitParked(t, 1)
	restart(t, d)

	s := mustCall(t, Request{Op: "status"})
	if s.Away == nil || s.Away.Words != "logging off" || s.Away.By != "user" || len(s.Parked) != 1 {
		t.Fatalf("after a restart away = %+v, parked = %+v", s.Away, s.Parked)
	}
	if out := receive(t, runHookAsync(masterNode, hookEvent("PermissionRequest", "", "Bash", bashTest))); out != parkedDeny("PermissionRequest") {
		t.Fatalf("after a restart the master's permission printed %s", out)
	}
}

// The master hears with each batch of events that the user is away.
func TestAwayNextEvent(t *testing.T) {
	testDaemon(t)
	goAway(t, "bye")
	mustCall(t, Request{Op: "tell", Text: "fix it"})
	out, err := newMCPServer(masterNode, io.Discard).run(context.Background(), "next_event", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "bosun: away mode is on") || !strings.Contains(out, `saying "bye"`) || !strings.Contains(out, "fix it") {
		t.Fatalf("next_event returned %s", out)
	}
	mustCall(t, Request{Op: "away", Name: "off"})
	mustCall(t, Request{Op: "tell", Text: "and b"})
	if out, _ := newMCPServer(masterNode, io.Discard).run(context.Background(), "next_event", nil); strings.Contains(out, "away mode") {
		t.Fatalf("next_event after the return returned %s", out)
	}
}

// Turning the away mode off hands the user the list, through the desk and as a notice, the push first.
func TestAwayOff(t *testing.T) {
	d, fc := testDesk(t)
	if _, err := call(context.Background(), Request{Op: "away", Name: "off"}); err == nil {
		t.Fatal("away off went through while it was off")
	}
	goAway(t, "")
	if text := mustCall(t, Request{Op: "away", Name: "off"}).Text; text != "Away mode is off. Nothing was parked." {
		t.Fatalf("an empty return said %q", text)
	}
	if dec := len(mustCall(t, Request{Op: "status"}).Decisions); dec != 0 {
		t.Fatalf("an empty return opened %d decisions", dec)
	}

	goAway(t, "")
	escalatedQuestion(t, 1, "which db?")
	escalatedQuestion(t, 1, "which db?")
	receive(t, runHookAsync(masterNode, hookEvent("PermissionRequest", "", "Bash", map[string]any{"command": "git -C /repo push origin master"})))
	awaitParked(t, 3)
	fc.mu.Lock()
	starts := len(fc.starts)
	fc.mu.Unlock()

	text := mustCall(t, Request{Op: "away", Name: "off"}).Text
	push, db := strings.Index(text, "1. master"), strings.Index(text, "2. a, at")
	if push < 0 || db < push || !strings.Contains(text, "A push") || strings.Count(text, "which db?") != 1 {
		t.Fatalf("away off said\n%s", text)
	}
	s := mustCall(t, Request{Op: "status"})
	if s.Away != nil || len(s.Parked) != 0 || len(s.Decisions) != 1 || s.Decisions[0].Kind != "notice" || s.Decisions[0].Note != text {
		t.Fatalf("after the return status = %+v", s)
	}
	list := text[strings.Index(text, "1. "):]
	if args := awaitStarts(t, fc, starts+1); args[len(args)-1] != deskReport([]Event{{Kind: "back", Text: list}}) {
		t.Fatalf("the desk got %q", args[len(args)-1])
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.said.at.IsZero() {
		t.Fatalf("the list counts as the user's message %+v", d.said)
	}
}

// The desk switches the away mode only on the user's words.
func TestDeskAway(t *testing.T) {
	testDesk(t)
	desk := newMCPServer(deskNode, io.Discard)
	if _, err := desk.run(context.Background(), "away", json.RawMessage(`{"on":true,"words":"I'm logging off"}`)); err == nil {
		t.Fatal("the desk turned away mode on with no word of the user's")
	}
	deskSays(t, "ok, I'm logging off for the night")
	if _, err := desk.run(context.Background(), "away", json.RawMessage(`{"on":true,"words":"I'm logging off"}`)); err != nil {
		t.Fatal(err)
	}
	if a := mustCall(t, Request{Op: "status"}).Away; a == nil || a.By != "desk" || a.Words != "I'm logging off" {
		t.Fatalf("away = %+v", a)
	}
	if _, err := newMCPServer(masterNode, io.Discard).run(context.Background(), "away", json.RawMessage(`{"on":false}`)); err == nil {
		t.Fatal("the master has away")
	}
	receive(t, runHookAsync(masterNode, hookEvent("PermissionRequest", "", "Bash", bashTest)))
	deskSays(t, "back now")
	out, err := desk.run(context.Background(), "away", json.RawMessage(`{"on":false,"words":"back now"}`))
	if err != nil || !strings.Contains(out, "1. master") {
		t.Fatalf("away off returned %q, %v", out, err)
	}
}

func TestParkedList(t *testing.T) {
	at := time.Date(2026, 10, 28, 23, 0, 0, 0, time.Local)
	p := func(id, node, kind, command, question string) Parked {
		dec := Decision{ID: id, Node: node, Kind: kind, Question: question}
		if command != "" {
			dec.Tool, dec.Input = "Bash", json.RawMessage(fmt.Sprintf(`{"command":%q}`, command))
		}
		return Parked{Decision: dec, Why: "why " + id, At: at}
	}
	got := parkedList([]Parked{
		p("1", "a", "question", "", "which db?"),
		p("2", "b", "permission", "npm publish", ""),
		p("3", "b", "question", "", "which db?"),
		p("4", "master", "permission", "cd /repo && git push", ""),
		{Decision: Decision{ID: "5", Node: masterNode, Kind: "notice", Note: "b failed"}, Why: "why 5", At: at},
	})
	var heads []string
	for line := range strings.SplitSeq(got, "\n") {
		if !strings.HasPrefix(line, " ") {
			heads = append(heads, line)
		}
	}
	want := []string{
		"1. master, at 23:00 (4): Bash: cd /repo && git push",
		"2. a, b, at 23:00 (1, 3): asks: which db?",
		"3. b, at 23:00 (2): Bash: npm publish",
		"4. master, at 23:00 (5): notice: b failed",
	}
	if !slices.Equal(heads, want) {
		t.Fatalf("list\n%s", got)
	}
	if !strings.Contains(got, "Parked because why 1.") || strings.Count(got, "A push") != 1 {
		t.Fatalf("list\n%s", got)
	}
}

func TestPushes(t *testing.T) {
	for script, want := range map[string]bool{
		"git push":                       true,
		"cd /repo && git -C . push o m":  true,
		"sudo git push":                  true,
		"env X=1 git push":               true,
		"sh -c 'git push origin master'": true,
		"git status && echo push":        false,
		"npm publish":                    false,
	} {
		if got := pushes(script); got != want {
			t.Errorf("pushes(%q) = %v", script, got)
		}
	}
}
