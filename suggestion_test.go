package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeJob replaces the job state of session short with the fixture in testdata, changed by edit, as Claude Code writes it.
func writeJob(t *testing.T, short string, edit func(map[string]any)) {
	t.Helper()
	data, err := os.ReadFile("testdata/job-suggested.json")
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	edit(st)
	data, _ = json.Marshal(st)
	dir := filepath.Join(claudeDir(), "jobs", short)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.tmp"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "state.tmp"), filepath.Join(dir, "state.json")); err != nil {
		t.Fatal(err)
	}
}

func TestJobSuggestion(t *testing.T) {
	testDaemon(t)
	for name, c := range map[string]struct {
		edit func(map[string]any)
		want string
	}{
		"blocked":         {func(map[string]any) {}, "keep the stacked one"},
		"in a new turn":   {func(st map[string]any) { st["tempo"] = "active" }, ""},
		"in a question":   {func(st map[string]any) { st["block"] = map[string]any{"questions": []any{}} }, ""},
		"with a null one": {func(st map[string]any) { st["block"] = nil }, "keep the stacked one"},
		"without one":     {func(st map[string]any) { delete(st, "suggestedReply") }, ""},
	} {
		writeJob(t, "aaaaaaaa", c.edit)
		if got := jobSuggestion("aaaaaaaa"); got != c.want {
			t.Errorf("%s: the suggestion is %q, want %q", name, got, c.want)
		}
	}
	if got := jobSuggestion("bbbbbbbb"); got != "" {
		t.Errorf("a session without job state suggests %q", got)
	}
}

// suggestionLines follows session name and returns a reader of its suggestion lines, which skips every other line.
func suggestionLines(t *testing.T, name string) func() Suggestion {
	t.Helper()
	conn, err := net.Dial("unix", socketPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := json.NewEncoder(conn).Encode(Request{Op: "follow", Name: name}); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(conn)
	return func() Suggestion {
		t.Helper()
		for {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				t.Fatal(err)
			}
			var s Suggestion
			_ = json.Unmarshal(raw, &s)
			if s.Type == "suggestion" {
				return s
			}
		}
	}
}

func TestFollowSuggestion(t *testing.T) {
	d, _ := testDaemon(t)
	followFast(t)
	fakeSupervisor(t, func(op, short string) []string { return nil })
	writeProbe(t, "s-probe", 9)
	writeJob(t, "aaaaaaaa", func(map[string]any) {})
	d.mu.Lock()
	d.nodes["a"].SessionID = "s-probe"
	d.mu.Unlock()

	next := suggestionLines(t, "a")
	if got, want := next(), (Suggestion{Type: "suggestion", Name: "a", Text: "keep the stacked one"}); got != want {
		t.Fatalf("the suggestion came as %+v", got)
	}
	// A turn's start clears the suggestion, as Claude Code writes it.
	writeJob(t, "aaaaaaaa", func(st map[string]any) {
		st["tempo"] = "active"
		delete(st, "suggestedReply")
	})
	if got := next(); got.Text != "" {
		t.Fatalf("the new turn left the suggestion %q", got.Text)
	}
	writeJob(t, "aaaaaaaa", func(st map[string]any) { st["suggestedReply"] = "the tabbed one" })
	if got := next(); got.Text != "the tabbed one" {
		t.Fatalf("the next suggestion came as %q", got.Text)
	}
	// A new conversation's reset drops what the frontend held, so the suggestion goes out again.
	writeProbe(t, "s-clear", 9)
	d.mu.Lock()
	d.nodes["a"].Conversation = "s-clear"
	d.mu.Unlock()
	if got := next(); got.Text != "the tabbed one" {
		t.Fatalf("after the reset the suggestion came as %q", got.Text)
	}
}
