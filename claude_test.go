package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTurns writes a transcript of session sid in the 2.1.310 format, holding two turns of a worker's own conversation among entries that begin none.
func writeTurns(t *testing.T, sid string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", "-repo--claude-worktrees-a")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	for _, entry := range []map[string]any{
		{"type": "user", "message": map[string]any{"content": "the first task"}},
		{"type": "assistant", "message": map[string]any{"stop_reason": "end_turn", "content": []map[string]any{{"type": "text", "text": "Done with the first."}}}},
		{"type": "user", "message": map[string]any{"content": "fix the parser"}},
		{"type": "user", "isMeta": true, "message": map[string]any{"content": []map[string]any{{"type": "text", "text": "a skill's text"}}}},
		{"type": "assistant", "message": map[string]any{"content": []map[string]any{{"type": "thinking", "thinking": "hmm"}}}},
		{"type": "assistant", "message": map[string]any{"content": []map[string]any{{"type": "text", "text": "Reading it first."}}}},
		{"type": "assistant", "message": map[string]any{"stop_reason": "tool_use", "content": []map[string]any{{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": "go test ./...\necho done"}}}}},
		{"type": "user", "message": map[string]any{"content": []map[string]any{{"type": "tool_result", "content": "ok"}}}},
		{"type": "assistant", "isSidechain": true, "message": map[string]any{"content": []map[string]any{{"type": "text", "text": "a subagent's"}}}},
		{"type": "attachment", "attachment": map[string]any{"type": "queued_command", "prompt": "also the lexer"}},
		{"type": "assistant", "message": map[string]any{"stop_reason": "tool_use", "content": []map[string]any{{"type": "tool_use", "name": "Edit", "input": map[string]any{"file_path": "/repo/parse.go"}}}}},
	} {
		line, _ := json.Marshal(entry)
		b.Write(append(line, '\n'))
	}
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// lastTurn is how the last turn that writeTurns writes renders.
const lastTurn = "> fix the parser\nReading it first.\n[Bash] go test ./... …\n> also the lexer\n[Edit] /repo/parse.go\n"

func TestReadTranscript(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	writeTurns(t, "s-a")
	if got, err := readTranscript("s-a", 1); err != nil || got.render() != lastTurn {
		t.Fatalf("the last turn renders as\n%s\nwant\n%s(%v)", got.render(), lastTurn, err)
	}
	if got, _ := readTranscript("s-a", 3); !strings.HasPrefix(got.render(), "> the first task\nDone with the first.\n\n"+lastTurn) {
		t.Fatalf("both turns render as\n%s", got.render())
	}
	if tail, err := readTranscript("s-a", 0); err != nil || tail.text != "Reading it first." || tail.ended || len(tail.turns) != 0 {
		t.Fatalf("tail = %+v, %v", tail, err)
	}

	// A cleared conversation begins with the /clear, and Claude Code's caveat about it. A skill is a slash command too, and an interruption begins no turn.
	cleared := `{"type":"user","isMeta":true,"message":{"content":"<local-command-caveat>The command below was run directly in Claude Code.</local-command-caveat>"}}
{"type":"user","message":{"content":"<command-name>/clear</command-name>\n            <command-message>clear</command-message>\n            <command-args></command-args>"}}
{"type":"system","subtype":"local_command","content":"<local-command-stdout></local-command-stdout>"}
{"type":"user","message":{"content":"<command-message>mkissue</command-message>\n<command-name>/mkissue</command-name>\n<command-args>a bug</command-args>"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Filing it."}]}}
{"type":"attachment","attachment":{"type":"queued_command","prompt":"first line\nsecond line"}}
{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]}}
`
	if err := os.WriteFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", "-repo--claude-worktrees-a", "s-c.jsonl"), []byte(cleared), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := func() string { s, _ := readTranscript("s-c", 3); return s.render() }(), "> /clear\n\n> /mkissue a bug\nFiling it.\n> first line\n> second line\n> [Request interrupted by user]\n"; got != want {
		t.Fatalf("a cleared conversation renders as\n%s\nwant\n%s", got, want)
	}
}
