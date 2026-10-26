package main

import (
	"slices"
	"strings"
	"testing"
)

func testTUI() *tui {
	return &tui{
		connected: true,
		sessions: []Session{
			{Node: "a", Parent: masterNode, ID: "aaaaaaaa", Status: "idle", Depth: 1},
			{Node: masterNode, ID: "mmmmmmmm", Status: "busy"},
			{Node: "b", Parent: "a", ID: "bbbbbbbb", Status: "stopped", Depth: 2},
		},
		decisions: []Decision{
			{ID: "2", Kind: "question", Node: "a", Holder: "master", Question: "X or Y?"},
			{ID: "3", Kind: "permission", Node: "a", Holder: "user", Tool: "Bash", Input: []byte(`{"command":"git push"}`), Reserved: "a git push"},
			{ID: "4", Kind: "notice", Node: masterNode, Holder: "user", Note: "spawn failed"},
		},
	}
}

func TestTUIItems(t *testing.T) {
	var got []string
	for _, it := range testTUI().items() {
		if it.decision != nil {
			got = append(got, it.decision.ID)
		} else {
			got = append(got, it.session.Node)
		}
	}
	if want := []string{"3", "4", "2", masterNode, "a", "b"}; !slices.Equal(got, want) {
		t.Fatalf("items %v, want the user's decisions, the master's, then the tree: %v", got, want)
	}
}

func TestTUIKeys(t *testing.T) {
	tu := testTUI()
	if a, ok := tu.key("a"); !ok || a != (tuiAction{op: "approve", id: "3"}) {
		t.Fatalf("a on a permission: %+v %v", a, ok)
	}
	if _, ok := tu.key("x"); ok {
		t.Fatal("dismissed a permission")
	}
	tu.key("j")
	if a, ok := tu.key("x"); !ok || a != (tuiAction{op: "dismiss", id: "4"}) {
		t.Fatalf("x on a notice: %+v %v", a, ok)
	}
	tu.key("\x1b[B")
	tu.key("\r")
	for _, k := range splitKeys([]byte("Yes!\x7f")) {
		tu.key(k)
	}
	if a, ok := tu.key("\r"); !ok || a != (tuiAction{op: "reply", id: "2", text: "Yes"}) {
		t.Fatalf("an answer: %+v %v", a, ok)
	}
	tu.key("j")
	if a, ok := tu.key("\r"); !ok || a != (tuiAction{op: "attach", id: "mmmmmmmm"}) {
		t.Fatalf("enter on a session: %+v %v", a, ok)
	}
	if a, ok := tu.key("q"); !ok || a.op != "quit" {
		t.Fatalf("q: %+v %v", a, ok)
	}
}

func TestTUIRender(t *testing.T) {
	tu := testTUI()
	screen := strings.Join(tu.render(80, 20), "\n")
	for _, want := range []string{"Waiting on you", "git push", "[a git push]", "spawn failed", "Waiting on the master", "X or Y?", "Sessions", "    b ", "a approve"} {
		if !strings.Contains(screen, want) {
			t.Errorf("the screen lacks %q:\n%s", want, screen)
		}
	}
	if lines := tu.render(80, 5); len(lines) != 5 || !strings.Contains(strings.Join(lines, "\n"), reverse) {
		t.Fatalf("a short screen loses the selected line: %q", lines)
	}
}

func TestTUIApply(t *testing.T) {
	tu := testTUI()
	tu.sel = 2
	tu.apply(Response{Type: "event", Event: &Event{Kind: "settled", ID: "2"}})
	tu.apply(Response{Type: "event", Event: &Event{Kind: "decision", Decision: &Decision{ID: "10", Kind: "notice", Node: masterNode, Holder: "user"}}})
	var ids []string
	for _, d := range tu.decisions {
		ids = append(ids, d.ID)
	}
	if !slices.Equal(ids, []string{"3", "4", "10"}) {
		t.Fatalf("decisions %v", ids)
	}

	tu.apply(Response{Type: "event", Event: &Event{Kind: "message", Node: "a", From: "b", Text: "b merges first.\nThen rebase."}})
	screen := strings.Join(tu.render(80, 30), "\n")
	if !strings.Contains(screen, "b → a: b merges first.") || strings.Contains(screen, "Then rebase") {
		t.Fatalf("the screen shows the message as:\n%s", screen)
	}
}

func TestSplitKeys(t *testing.T) {
	if got := splitKeys([]byte("j\x1b[Bé\r")); !slices.Equal(got, []string{"j", "\x1b[B", "é", "\r"}) {
		t.Fatalf("keys %q", got)
	}
}
