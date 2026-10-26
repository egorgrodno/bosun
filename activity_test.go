package main

import (
	"strings"
	"testing"
	"time"
)

func activityWatcher(d *daemon) chan Event {
	w := &watcher{ch: make(chan Event, 256), cancel: func() {}}
	d.mu.Lock()
	d.watchers[w] = true
	d.mu.Unlock()
	return w.ch
}

func nextActivity(t *testing.T, ch chan Event) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no activity event")
	}
	return Event{}
}

func TestActivityOf(t *testing.T) {
	long := strings.Repeat("x", 300)
	for _, tc := range []struct {
		tool  string
		input string
		arg   string
	}{
		{"Bash", `{"command":"go test ./...","description":"run"}`, "go test ./..."},
		{"Edit", `{"file_path":"/r/daemon.go","old_string":"a"}`, "/r/daemon.go"},
		{"Bash", `{"command":"a\n  b"}`, "a b"},
		{"Task", `{}`, ""},
		{"Bash", `{"command":"` + long + `"}`, strings.Repeat("x", activityWidth-1) + "…"},
	} {
		if got := activityOf(tc.tool, []byte(tc.input)); got.Tool != tc.tool || got.Arg != tc.arg {
			t.Errorf("activityOf(%s, %s) = %+v, want arg %q", tc.tool, tc.input, got, tc.arg)
		}
	}
}

// A burst of tool calls sends the first change at once and the last of the rest when the window ends, and the snapshot holds the latest throughout.
func TestActivityCoalesces(t *testing.T) {
	window := activityWindow
	activityWindow = 200 * time.Millisecond
	t.Cleanup(func() { activityWindow = window })
	d, _ := testDaemon(t)
	ch := activityWatcher(d)

	for i, cmd := range []string{"one", "two", "three", "four"} {
		id := string(rune('a' + i))
		receive(t, runHookAsync("a", hookEvent("PreToolUse", id, "Bash", map[string]any{"command": cmd})))
		receive(t, runHookAsync("a", hookEvent("PostToolUse", id, "Bash", map[string]any{"command": cmd})))
	}
	first := nextActivity(t, ch)
	if first.Kind != "activity" || first.Node != "a" || first.Activity.Tool != "Bash" || first.Activity.Arg != "one" {
		t.Fatalf("first event %+v %+v", first, first.Activity)
	}
	d.mu.Lock()
	snap := d.snapshotLocked()
	d.mu.Unlock()
	for _, s := range snap.Sessions {
		if s.Node == "a" && (s.Activity == nil || s.Activity.Arg != "four") {
			t.Fatalf("snapshot activity %+v", s.Activity)
		}
	}
	if last := nextActivity(t, ch); last.Activity.Arg != "four" {
		t.Fatalf("last event %+v", last.Activity)
	}
	select {
	case ev := <-ch:
		t.Fatalf("extra event %+v", ev)
	case <-time.After(400 * time.Millisecond):
	}
}

func TestActivityStopText(t *testing.T) {
	d, _ := testDaemon(t)
	ch := activityWatcher(d)
	stopHook(t, "a", "All done.\nThe tests pass.")
	ev := nextActivity(t, ch)
	if ev.Activity.Tool != "" || ev.Activity.Text != "All done. The tests pass." {
		t.Fatalf("event %+v", ev.Activity)
	}
}

func TestTUIActivity(t *testing.T) {
	tu := testTUI()
	tu.apply(Response{Type: "event", Event: &Event{Kind: "activity", Node: "a", Activity: &Activity{Tool: "Edit", Arg: "daemon.go"}}})
	tu.apply(Response{Type: "event", Event: &Event{Kind: "activity", Node: "b", Activity: &Activity{Text: "Rebasing now"}}})
	screen := strings.Join(tu.render(80, 30), "\n")
	for _, want := range []string{"Edit daemon.go", "Rebasing now"} {
		if !strings.Contains(screen, want) {
			t.Errorf("the screen lacks %q:\n%s", want, screen)
		}
	}
	var b strings.Builder
	printStatus(&b, Response{Sessions: tu.sessions})
	if !strings.Contains(b.String(), "Edit daemon.go") {
		t.Errorf("status lacks the line:\n%s", b.String())
	}
}
