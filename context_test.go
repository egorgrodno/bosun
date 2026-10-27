package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContextWindow(t *testing.T) {
	for _, c := range []struct {
		model, flag string
		want        int
	}{
		{"claude-opus-5-5", "", longWindow},
		{"claude-opus-5", "opus", longWindow},
		{"claude-opus-4-8", "", longWindow},
		{"claude-opus-4-7", "", longWindow},
		{"claude-opus-4-6", "", defaultWindow},
		{"claude-opus-4-6", "opus[1m]", longWindow},
		{"claude-opus-4-1-20250805", "", defaultWindow},
		{"claude-sonnet-5-5", "sonnet", longWindow},
		{"claude-sonnet-4-5-20250929", "", defaultWindow},
		{"claude-sonnet-4-5-20250929[1m]", "", longWindow},
		{"claude-haiku-5-5", "haiku", longWindow},
		{"claude-haiku-4-5-20251001", "", defaultWindow},
		{"claude-fable-5-1", "", longWindow},
		{"claude-mythos-5", "", longWindow},
		{"some-other-model", "", defaultWindow},
		{"", "", defaultWindow},
	} {
		if got := contextWindow(c.model, c.flag); got != c.want {
			t.Errorf("contextWindow(%q, %q) = %d, want %d", c.model, c.flag, got, c.want)
		}
	}
}

func TestContextLabel(t *testing.T) {
	for _, c := range []struct {
		c    *Context
		want string
	}{
		{nil, ""},
		{&Context{0, 200_000}, "0/200k"},
		{&Context{84_300, 200_000}, "84k/200k"},
		{&Context{999, 1_000_000}, "999/1M"},
		{&Context{1_250_000, 1_500_000}, "1.2M/1.5M"},
	} {
		if got := contextLabel(c.c); got != c.want {
			t.Errorf("contextLabel(%v) = %q, want %q", c.c, got, c.want)
		}
	}
}

// reply is an assistant record that answered with the usage of in input tokens, cr read from and cc written to the cache.
func reply(model string, in, cc, cr int, extra ...string) string {
	rest := strings.Join(extra, "")
	return fmt.Sprintf(`{"type":"assistant"%s,"message":{"model":%q,"content":[{"type":"text","text":"x"}],"usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":999}}}`+"\n", rest, model, in, cc, cr)
}

const (
	userLine     = `{"type":"user","message":{"role":"user","content":"go"}}` + "\n"
	compactLine  = `{"type":"system","subtype":"compact_boundary","content":"Conversation compacted"}` + "\n"
	textMentions = `{"type":"user","message":{"role":"user","content":"the \"usage\" of a compact_boundary"}}` + "\n"
)

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func readTail(t *testing.T, tail *usageTail, path string) (used int, model string, seen bool) {
	t.Helper()
	used, model, seen, err := tail.read(path)
	if err != nil {
		t.Fatal(err)
	}
	return
}

func TestUsageTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	tail := &usageTail{}
	appendTo(t, path, userLine)
	if _, _, seen := readTail(t, tail, path); seen {
		t.Fatal("a conversation without a reply holds a reading")
	}

	// The reading sums the input and both cache counts, and leaves the reply out.
	appendTo(t, path, reply("claude-opus-5-5", 4, 2212, 47064))
	if used, model, seen := readTail(t, tail, path); !seen || used != 49280 || model != "claude-opus-5-5" {
		t.Fatalf("read %d tokens of %q, seen %v", used, model, seen)
	}

	// A subagent's message, a synthetic one and a record that only mentions the words are no reading, and a line still being written waits.
	appendTo(t, path, reply("claude-haiku-5-5", 1, 1, 1, `,"isSidechain":true`)+reply("<synthetic>", 0, 0, 0)+textMentions+`{"type":"assistant","message":{"usage":{"input_tokens":`)
	if used, model, _ := readTail(t, tail, path); used != 49280 || model != "claude-opus-5-5" {
		t.Fatalf("read %d tokens of %q past records that carry no reading", used, model)
	}
	appendTo(t, path, `7,"cache_creation_input_tokens":0,"cache_read_input_tokens":0},"model":"claude-opus-5-5"}}`+"\n")
	if used, _, _ := readTail(t, tail, path); used != 7 {
		t.Fatalf("the finished line read %d tokens", used)
	}

	// A compaction drops the use to zero until the next reply, which is smaller than the one before it.
	appendTo(t, path, reply("claude-opus-5-5", 3, 90000, 0)+compactLine)
	if used, _, seen := readTail(t, tail, path); used != 0 || !seen {
		t.Fatalf("after the compaction used = %d, seen %v", used, seen)
	}
	appendTo(t, path, reply("claude-opus-5-5", 2, 1000, 5000))
	if used, _, _ := readTail(t, tail, path); used != 6002 {
		t.Fatalf("after the compaction's first reply used = %d", used)
	}

	// A boundary that reports the tokens it left sets the use to them.
	appendTo(t, path, `{"type":"system","subtype":"compact_boundary","compactMetadata":{"trigger":"manual","preTokens":694742,"postTokens":25594}}`+"\n")
	if used, _, seen := readTail(t, tail, path); used != 25594 || !seen {
		t.Fatalf("after a compaction that left 25594 tokens used = %d, seen %v", used, seen)
	}

	// A transcript replaced by a shorter one starts the reading over.
	if err := os.WriteFile(path, []byte(userLine+reply("claude-opus-5-5", 1, 1, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if used, _, _ := readTail(t, tail, path); used != 3 {
		t.Fatalf("after the replacement used = %d", used)
	}

	// Another conversation starts from zero, and keeps the model the session ran.
	clear := filepath.Join(t.TempDir(), "clear.jsonl")
	appendTo(t, clear, userLine)
	if used, model, seen := readTail(t, tail, clear); used != 0 || seen || model != "claude-opus-5-5" {
		t.Fatalf("a new conversation reads %d tokens of %q, seen %v", used, model, seen)
	}
}

// A first read of a long transcript starts at its end, and widens the window until it finds a reply.
func TestUsageTailFirstRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	big := `{"type":"user","message":{"role":"user","content":"` + strings.Repeat("x", 3*tailStart) + `"}}` + "\n"
	appendTo(t, path, userLine+reply("claude-sonnet-5-5", 10, 20, 30)+big+big)
	tail := &usageTail{}
	if used, model, seen := readTail(t, tail, path); !seen || used != 60 || model != "claude-sonnet-5-5" {
		t.Fatalf("read %d tokens of %q, seen %v", used, model, seen)
	}
	appendTo(t, path, reply("claude-sonnet-5-5", 1, 1, 1))
	if used, _, _ := readTail(t, tail, path); used != 3 {
		t.Fatalf("the next read took %d tokens", used)
	}
}

// watchContext follows the watch stream and returns a function that reads it until the next context event.
func watchContext(t *testing.T) func() Event {
	t.Helper()
	conn, err := net.Dial("unix", socketPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := json.NewEncoder(conn).Encode(Request{Op: "watch"}); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(conn)
	var snap Response
	if err := dec.Decode(&snap); err != nil {
		t.Fatal(err)
	}
	return func() Event {
		t.Helper()
		for {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			var r Response
			if err := dec.Decode(&r); err != nil {
				t.Fatal(err)
			}
			if r.Event != nil && r.Event.Kind == "context" {
				return *r.Event
			}
		}
	}
}

func contextOf(t *testing.T, node string) *Context {
	t.Helper()
	for _, s := range mustCall(t, Request{Op: "status"}).Sessions {
		if s.Node == node {
			return s.Context
		}
	}
	t.Fatalf("no session %s", node)
	return nil
}

func TestDaemonReportsContext(t *testing.T) {
	d, _ := testDaemon(t)
	contextDelay := contextFollowDelay
	contextFollowDelay = 10 * time.Millisecond
	t.Cleanup(func() { contextFollowDelay = contextDelay })
	next := watchContext(t)
	dir := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", "-repo--claude-worktrees-a")
	path := filepath.Join(dir, "s-a.jsonl")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.nodes["a"].SessionID = "s-a"
	d.mu.Unlock()
	appendTo(t, path, userLine+reply("claude-opus-5-5", 4, 1000, 83000))

	receive(t, runHookAsync("a", hookEvent("PostToolUse", "t1", "Bash", bashTest)))
	if e, want := next(), (Context{84004, longWindow}); e.Node != "a" || e.Context == nil || *e.Context != want {
		t.Fatalf("the first context event is %+v", e)
	}
	if got := contextOf(t, "a"); got == nil || got.Used != 84004 {
		t.Fatalf("the snapshot holds %+v", got)
	}

	// A reading that has not changed sends nothing, and a reply that lands after the Stop is found by the read that follows it.
	receive(t, runHookAsync("a", hookEvent("PostToolUse", "t2", "Bash", bashTest)))
	appendTo(t, path, reply("claude-opus-5-5", 1, 0, 90000))
	receive(t, runHookAsync("a", hookEvent("Stop", "", "", nil)))
	if e := next(); e.Context == nil || e.Context.Used != 90001 {
		t.Fatalf("the context event after the Stop is %+v", e)
	}

	// A boundary lands a moment after the PostCompact, and the read that follows it finds it.
	receive(t, runHookAsync("a", hookEvent("PostCompact", "", "", nil)))
	appendTo(t, path, `{"type":"system","subtype":"compact_boundary","compactMetadata":{"postTokens":25594}}`+"\n")
	if e := next(); e.Context == nil || e.Context.Used != 25594 {
		t.Fatalf("the context event after the PostCompact is %+v", e)
	}

	// A clear starts the new conversation from zero.
	d.mu.Lock()
	d.nodes["a"].SessionID = "s-clear"
	d.mu.Unlock()
	appendTo(t, filepath.Join(dir, "s-clear.jsonl"), userLine)
	receive(t, runHookAsync("a", hookEvent("UserPromptSubmit", "", "", nil)))
	if e, want := next(), (Context{0, longWindow}); e.Context == nil || *e.Context != want {
		t.Fatalf("after the clear the context event is %+v", e)
	}
}

// A session whose transcript holds no reply has no context to report.
func TestNoContextWithoutReply(t *testing.T) {
	d, _ := testDaemon(t)
	d.mu.Lock()
	d.nodes["a"].SessionID = "s-a"
	d.mu.Unlock()
	writeTranscript(t, "s-a")
	receive(t, runHookAsync("a", hookEvent("PostToolUse", "t1", "Bash", bashTest)))
	time.Sleep(100 * time.Millisecond)
	if got := contextOf(t, "a"); got != nil {
		t.Fatalf("a transcript without usage gave the context %+v", got)
	}
}
