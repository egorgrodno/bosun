package main

import (
	"encoding/json"
	"net"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestParseNeeds(t *testing.T) {
	for needs, want := range map[string]*Need{
		"":                                    nil,
		"approve Bash: echo needs-permission": {Kind: "permission", Tool: "Bash", Detail: "echo needs-permission"},
		"approve mcp__bosun__spawn: a: b":     {Kind: "permission", Tool: "mcp__bosun__spawn", Detail: "a: b"},
		"answer: red or blue? (Red · Blue)":   {Kind: "question", Question: "red or blue?", Options: []string{"Red", "Blue"}},
		"answer: which one?":                  {Kind: "question", Question: "which one?"},
		"pick a plan":                         {Kind: "other", Text: "pick a plan"},
	} {
		if got := parseNeeds(needs); !reflect.DeepEqual(got, want) {
			t.Errorf("parseNeeds(%q) = %+v, want %+v", needs, got, want)
		}
	}
}

// followLines follows session name and returns a reader of its progress lines, which skips every other line.
func followLines(t *testing.T, name string) func() Progress {
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
	return func() Progress {
		t.Helper()
		for {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				t.Fatal(err)
			}
			var p Progress
			_ = json.Unmarshal(raw, &p)
			if p.Type == "progress" {
				return p
			}
		}
	}
}

const pong = `{"ok":true,"op":"ping","version":"2.1.310","proto":1}`

func TestFollowProgress(t *testing.T) {
	d, _ := testDaemon(t)
	followFast(t)
	writeProbe(t, "s-probe", 9)
	d.mu.Lock()
	d.nodes["a"].SessionID = "s-probe"
	d.mu.Unlock()
	var mu sync.Mutex
	subscribed := map[string]int{}
	fakeSupervisor(t, func(op, short string) []string {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case op == "ping":
			return []string{pong}
		case op != "subscribe":
			return nil
		case short == "aaaaaaaa" && subscribed[short] == 0:
			subscribed[short]++
			// The supervisor's own lines, from a 2.1.310 probe; the subscription then drops.
			return []string{
				`{"type":"snapshot","record":{"short":"aaaaaaaa","sessionId":"s-probe","pid":1196445,"tempo":"active","state":"running","detail":"Running the tests","name":"a","cliVersion":"2.1.310"},"streamTail":["\u001b[?2026h"]}`,
				`{"type":"stream","line":"\u001b]0;◐ a\u0007"}`,
				`{"type":"state","patch":{"pid":1226791}}`,
				`{"type":"state","patch":{"tempo":"blocked","needs":"approve Bash: echo needs-permission"}}`,
			}
		case short == "bbbbbbbb":
			return []string{`{"type":"snapshot","record":{"short":"bbbbbbbb","tempo":"idle","state":"running","detail":""}}`, ""}
		}
		return []string{""}
	})

	next := followLines(t, "a")
	if got, want := next(), (Progress{Type: "progress", Name: "a", Tempo: "active", State: "running", Detail: "Running the tests"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("the snapshot came as %+v", got)
	}
	want := Progress{Type: "progress", Name: "a", Tempo: "blocked", State: "running", Detail: "Running the tests", Needs: "approve Bash: echo needs-permission", Need: &Need{Kind: "permission", Tool: "Bash", Detail: "echo needs-permission"}}
	if got := next(); !reflect.DeepEqual(got, want) {
		t.Fatalf("the dialog came as %+v", got)
	}
	// The subscription dropped, so the state it sent no longer holds.
	if got := next(); !reflect.DeepEqual(got, Progress{Type: "progress", Name: "a"}) {
		t.Fatalf("the drop came as %+v", got)
	}

	// A switch moves the worker to a new short id, which follow subscribes to.
	d.mu.Lock()
	d.nodes["a"].ID = "bbbbbbbb"
	d.mu.Unlock()
	if got := next(); got.Tempo != "idle" {
		t.Fatalf("the switched worker came as %+v", got)
	}
}

// A supervisor that speaks another protocol leaves the conversation without progress.
func TestFollowWithoutProgress(t *testing.T) {
	d, _ := testDaemon(t)
	followFast(t)
	data := writeProbe(t, "s-probe", 9)
	d.mu.Lock()
	d.nodes["a"].SessionID = "s-probe"
	d.mu.Unlock()
	fakeSupervisor(t, func(op, short string) []string {
		if op == "ping" {
			return []string{`{"ok":true,"op":"ping","version":"9.0.0","proto":2}`}
		}
		t.Errorf("bosun sent %s to a supervisor of another protocol", op)
		return nil
	})
	next := convDial(t, Request{Op: "follow", Name: "a"})
	next()
	next()
	full, _ := os.ReadFile("testdata/probe.jsonl")
	appendProbeFile(t, "s-probe", lineAfter(full[len(data):]))
	if got := next(); got.Type != "entries" || len(got.Entries) != 1 || got.Entries[0].ID != "a-test" {
		t.Fatalf("follow went on with %+v", got)
	}
}

// lineAfter returns the first line of b, with its newline.
func lineAfter(b []byte) []byte {
	for i, c := range b {
		if c == '\n' {
			return b[:i+1]
		}
	}
	return b
}
