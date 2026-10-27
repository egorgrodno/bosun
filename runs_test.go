package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestModelName(t *testing.T) {
	for id, want := range map[string]string{
		"claude-opus-5-5": "Opus 5.5", "claude-sonnet-4-5-20250929": "Sonnet 4.5", "claude-fable-5": "Fable 5", "gpt-4": "",
	} {
		if got := modelName(id); got != want {
			t.Errorf("modelName(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestSettingsEffort(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if got := settingsEffort("claude-opus-5-5"); got != "" {
		t.Fatalf("no settings gave %q", got)
	}
	settings := `{"effortLevel":"low","modelSettings":{"claude-opus-5-5":{"effortLevel":"xhigh"}}}`
	if err := os.WriteFile(filepath.Join(claudeDir(), "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := settingsEffort("claude-opus-5-5"); got != "xhigh" {
		t.Errorf("the model's entry gave %q", got)
	}
	if got := settingsEffort("claude-haiku-5"); got != "low" {
		t.Errorf("the top-level level gave %q", got)
	}
}

func TestDaemonReportsRuns(t *testing.T) {
	d, _ := testDaemon(t)
	dir := filepath.Join(claudeDir(), "projects", "-repo--claude-worktrees-a")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	settings := `{"modelSettings":{"claude-opus-5-5":{"effortLevel":"xhigh"}}}`
	if err := os.WriteFile(filepath.Join(claudeDir(), "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.nodes["a"].SessionID = "s-a"
	d.mu.Unlock()
	appendTo(t, filepath.Join(dir, "s-a.jsonl"), userLine+reply("claude-opus-5-5", 4, 1000, 83000))
	receive(t, runHookAsync("a", hookEvent("PostToolUse", "t1", "Bash", bashTest)))
	want := Runs{"claude-opus-5-5", "Opus 5.5", "xhigh"}
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		for _, s := range mustCall(t, Request{Op: "status"}).Sessions {
			if s.Node == "a" && s.Runs != nil && *s.Runs == want {
				return
			}
		}
		if time.Now().After(end) {
			t.Fatalf("the snapshot never held %+v", want)
		}
	}
}
