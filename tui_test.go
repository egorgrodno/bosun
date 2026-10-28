package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func testTUI() *tui {
	return &tui{
		connected: true,
		sessions: []Session{
			{Node: "a", Parent: masterNode, ID: "aaaaaaaa", Status: "idle", Depth: 1},
			{Node: masterNode, ID: "mmmmmmmm", Status: "busy"},
			{Node: "b", Parent: "a", ID: "bbbbbbbb", Status: "stopped", Depth: 2},
			{Node: deskNode, ID: "dddddddd", Status: "idle"},
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
	if want := []string{"3", "4", "2", deskNode, masterNode, "a", "b"}; !slices.Equal(got, want) {
		t.Fatalf("items %v, want the user's decisions, the master's, then the desk and the tree: %v", got, want)
	}
}

func TestTUIKeys(t *testing.T) {
	tu := testTUI()
	if _, ok := tu.key("y"); ok {
		t.Fatal("one y approved")
	}
	if a, ok := tu.key("y"); !ok || a != (tuiAction{op: "approve", id: "3"}) {
		t.Fatalf("y on a permission: %+v %v", a, ok)
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
	if a, ok := tu.key("\r"); !ok || a != (tuiAction{op: "attach", id: "dddddddd"}) {
		t.Fatalf("enter on the desk: %+v %v", a, ok)
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
	for _, want := range []string{"Waiting on you", "git push", "[a git push]", "spawn failed", "Waiting on the master", "X or Y?", "Sessions", "    b ", "y approve"} {
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

	at := time.Date(2026, 10, 26, 17, 43, 0, 0, time.Local)
	tu.apply(Response{Type: "event", Event: &Event{Kind: "log", Entry: &Entry{Seq: 1, At: at, Kind: "message", By: "worker", From: "b", Node: "a", Text: "b merges first.\nThen rebase."}}})
	screen := strings.Join(tu.render(80, 30), "\n")
	if !strings.Contains(screen, "Log") || !strings.Contains(screen, "17:43 b            → a: b merges first.") || strings.Contains(screen, "Then rebase") {
		t.Fatalf("the screen shows the message as:\n%s", screen)
	}

	tu.apply(Response{Type: "event", Event: &Event{Kind: "answered", Answered: &Answered{Decision: Decision{ID: "2", Kind: "question", Node: "a", Question: "X or Y?"}, Text: "Y", Words: "yes,\nY"}}})
	screen = strings.Join(tu.render(100, 30), "\n")
	if want := `answered asks: X or Y? → Y, on your words "yes, Y"`; !strings.Contains(screen, "Answered by the desk on your word") || !strings.Contains(screen, want) {
		t.Fatalf("the screen shows the desk's answer as:\n%s", screen)
	}
}

func TestTUIPause(t *testing.T) {
	tu := testTUI()
	if _, ok := tu.key("p"); ok {
		t.Fatal("one p paused")
	}
	if a, ok := tu.key("p"); !ok || a.op != "pause" {
		t.Fatalf("p: %+v %v", a, ok)
	}
	tu.apply(Response{Type: "event", Event: &Event{Kind: "paused"}})
	tu.apply(Response{Type: "event", Event: &Event{Kind: "sessions", Sessions: []Session{{Node: masterNode, ID: "mmmmmmmm", Status: "stopped", Paused: true}}}})
	screen := strings.Join(tu.render(80, 20), "\n")
	if !strings.Contains(screen, "Paused") || !strings.Contains(screen, "paused ") {
		t.Fatalf("the screen does not show the pause:\n%s", screen)
	}
	if _, ok := tu.key("p"); ok {
		t.Fatal("one p unpaused")
	}
	if a, ok := tu.key("p"); !ok || a.op != "unpause" {
		t.Fatalf("p while paused: %+v %v", a, ok)
	}
	tu.apply(Response{Type: "snapshot"})
	if tu.paused {
		t.Fatal("a snapshot without the pause left it shown")
	}
}

func TestTUIAway(t *testing.T) {
	tu := testTUI()
	tu.key("w")
	if a, ok := tu.key("w"); !ok || a != (tuiAction{op: "away", name: "on"}) {
		t.Fatalf("w: %+v %v", a, ok)
	}
	tu.apply(Response{Type: "event", Event: &Event{Kind: "away", Away: &Away{Since: time.Now(), By: "user", Words: "bed"}}})
	tu.apply(Response{Type: "event", Event: &Event{Kind: "parked", Parked: &Parked{Decision: Decision{ID: "7", Node: masterNode, Kind: "permission", Tool: "Bash", Input: []byte(`{"command":"git push"}`)}, Why: "it is the master's own permission"}}})
	screen := strings.Join(tu.render(100, 30), "\n")
	if !strings.Contains(screen, `Away since`) || !strings.Contains(screen, `"bed"`) || !strings.Contains(screen, "Bash: git push") || !strings.Contains(screen, "because it is the master's own permission") {
		t.Fatalf("the screen does not show the away mode:\n%s", screen)
	}
	tu.key("w")
	if a, ok := tu.key("w"); !ok || a != (tuiAction{op: "away", name: "off"}) {
		t.Fatalf("w while away: %+v %v", a, ok)
	}
	tu.apply(Response{Type: "event", Event: &Event{Kind: "back"}})
	if tu.away != nil || len(tu.parked) != 0 {
		t.Fatalf("after the return away = %+v, parked = %+v", tu.away, tu.parked)
	}
}

// n takes a grant to give, and x on a grant revokes it.
func TestTUIGrants(t *testing.T) {
	tu := testTUI()
	tu.key("n")
	for _, k := range strings.Split("push 8h", "") {
		tu.key(k)
	}
	if a, ok := tu.key("\r"); !ok || a != (tuiAction{op: "grant", text: "push 8h"}) {
		t.Fatalf("n: %+v %v", a, ok)
	}
	tu.apply(Response{Type: "event", Event: &Event{Kind: "grants", Grants: []Grant{{ID: "g1", Class: "push", Until: time.Now().Add(time.Hour), By: "user"}}}})
	screen := strings.Join(tu.render(100, 30), "\n")
	if !strings.Contains(screen, "Standing grants, which bosun acts on once away mode is on") || !strings.Contains(screen, "g1   push") {
		t.Fatalf("the screen does not show the grant:\n%s", screen)
	}
	for tu.selected().grant == nil {
		tu.key("j")
	}
	if a, ok := tu.key("x"); !ok || a != (tuiAction{op: "revoke", id: "g1"}) {
		t.Fatalf("x on a grant: %+v %v", a, ok)
	}
}

// m on the master or the desk takes a model and an effort to switch it to, and does nothing on a worker.
func TestTUISwitch(t *testing.T) {
	tu := testTUI()
	for range 3 {
		tu.key("j")
	}
	tu.key("m")
	for _, k := range splitKeys([]byte("opus high")) {
		tu.key(k)
	}
	if screen := strings.Join(tu.render(80, 20), "\n"); !strings.Contains(screen, "switch desk to: opus high_") {
		t.Fatalf("the screen lacks the switch:\n%s", screen)
	}
	if a, ok := tu.key("\r"); !ok || a != (tuiAction{op: "model", name: deskNode, text: "opus high"}) {
		t.Fatalf("a switch: %+v %v", a, ok)
	}
	tu.key("j")
	if tu.key("m"); tu.switching != masterNode {
		t.Fatalf("m on the master switches %q", tu.switching)
	}
	tu.key("\x1b")
	tu.key("j")
	if tu.key("m"); tu.switching != "" {
		t.Fatalf("m on a worker switches %q", tu.switching)
	}
}

func TestSplitKeys(t *testing.T) {
	if got := splitKeys([]byte("j\x1b[Bé\r")); !slices.Equal(got, []string{"j", "\x1b[B", "é", "\r"}) {
		t.Fatalf("keys %q", got)
	}
}

// Only the movement keys follow the layout: j/k on qwerty, the default, and a/e on hallmack. Arrows work on both, and y approves on both.
func TestTUILayouts(t *testing.T) {
	for _, c := range []struct{ layout, down, up, other string }{
		{"", "j", "k", "a"},
		{"qwerty", "j", "k", "a"},
		{"bogus", "j", "k", "a"},
		{layoutHallmack, "a", "e", "j"},
	} {
		tu := testTUI()
		tu.layout = c.layout
		tu.key(c.down)
		if tu.sel != 1 {
			t.Fatalf("%q: %s left sel at %d", c.layout, c.down, tu.sel)
		}
		tu.key(c.up)
		if tu.sel != 0 {
			t.Fatalf("%q: %s left sel at %d", c.layout, c.up, tu.sel)
		}
		tu.key(c.other)
		if tu.sel != 0 {
			t.Fatalf("%q: the other layout key %s moved to %d", c.layout, c.other, tu.sel)
		}
		tu.key("\x1b[B")
		if tu.sel != 1 {
			t.Fatalf("%q: the down arrow left sel at %d", c.layout, tu.sel)
		}
		tu.key("\x1b[A")
		tu.key("y")
		if a, ok := tu.key("y"); !ok || a != (tuiAction{op: "approve", id: "3"}) {
			t.Fatalf("%q: y: %+v %v", c.layout, a, ok)
		}
		if a, ok := tu.key("a"); ok && a.op == "approve" {
			t.Fatalf("%q: a still approves", c.layout)
		}
		tu.sel = 0
		if got := strings.Join(tu.render(80, 20), "\n"); !strings.Contains(got, "y approve") || !strings.Contains(got, c.down+"/"+c.up+" move") {
			t.Fatalf("%q: the hint lacks the keys:\n%s", c.layout, got)
		}
	}
}

func TestLayoutFor(t *testing.T) {
	t.Setenv("BOSUN_LAYOUT", "hallmack")
	if layoutFor("") != layoutHallmack || layoutFor("qwerty") != "qwerty" {
		t.Fatal("the variable sets the layout and the flag overrides it")
	}
	t.Setenv("BOSUN_LAYOUT", "dvorak")
	if layoutFor("") != "qwerty" || layoutFor("hallmack") != layoutHallmack {
		t.Fatal("an unknown layout is qwerty")
	}
}

// A first y, d or p arms the key and shows a hint. A second press within three seconds acts, and another key or the timeout disarms it.
func TestTUIArmed(t *testing.T) {
	tu := testTUI()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	tu.now = func() time.Time { return now }
	tu.key("y")
	if screen := strings.Join(tu.render(80, 20), "\n"); !strings.Contains(screen, "Press y again to approve") {
		t.Fatalf("no hint after the first y:\n%s", screen)
	}
	if _, ok := tu.key("d"); ok {
		t.Fatal("y then d denied")
	}
	if a, ok := tu.key("d"); !ok || a.op != "deny" {
		t.Fatalf("d d: %+v %v", a, ok)
	}
	tu.key("y")
	now = now.Add(armTimeout)
	if screen := strings.Join(tu.render(80, 20), "\n"); strings.Contains(screen, "again to") {
		t.Fatalf("the hint outlived the timeout:\n%s", screen)
	}
	if _, ok := tu.key("y"); ok {
		t.Fatal("a y after the timeout approved")
	}
	tu.key("j")
	if _, ok := tu.key("y"); ok {
		t.Fatal("a movement key left y armed")
	}
}
