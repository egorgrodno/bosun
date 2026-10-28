package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// bosun log --reached-user groups the decisions that reached the user by rule, the most frequent first, and leaves out what the master settled.
func TestPrintReached(t *testing.T) {
	bash := func(cmd string) json.RawMessage {
		raw, _ := json.Marshal(map[string]string{"command": cmd})
		return raw
	}
	at := time.Date(2026, 10, 27, 9, 0, 0, 0, time.Local)
	settled := func(id, rule, match, route, by, behavior string, dec Decision) Entry {
		dec.ID, dec.Kind, dec.Node, dec.Tool = id, "permission", "visual-check", "Bash"
		return Entry{Kind: "settled", At: at, By: by, Node: dec.Node, Behavior: behavior, Rule: rule, Match: match, Route: route, Waited: 90, Decision: &dec}
	}
	entries := []Entry{
		settled("175", ruleUnboxNames, "/home/u/.claude", routeUserOnly, "user", "allow", Decision{Holder: "user", UserOnly: true, Reserved: "a command outside the sandbox that names a folder that holds Claude Code's configuration", Input: bash("bosun status --json")}),
		settled("177", ruleUnboxNames, "/home/u/.claude", routeUserOnly, "user", "deny", Decision{Holder: "user", UserOnly: true, Reserved: "a command outside the sandbox that names a folder that holds Claude Code's configuration", Input: bash("bosun conversation x\n| jq .")}),
		settled("180", ruleUnboxed, "ls", routeMaster, "master", "allow", Decision{Holder: "master", Reserved: "a command outside the sandbox", Input: bash("ls")}),
		// An entry from before routes were logged.
		{Kind: "settled", At: at, By: "user", Node: "desk", Behavior: "allow", Decision: &Decision{ID: "195", Kind: "permission", Node: "desk", Holder: "user", Tool: "Bash", Reserved: "a command outside the sandbox", Input: bash("bosun status --json")}},
		{Kind: "decision", At: at, By: "worker", Node: "a", Decision: &Decision{ID: "200", Kind: "permission", Node: "a", Holder: "user"}},
	}
	var reached []Entry
	for _, e := range entries {
		if e.Kind == "settled" && reachedUser(e) {
			reached = append(reached, e)
		}
	}
	var b strings.Builder
	printReached(&b, reached)
	want := `3 decisions reached you

unboxed.names: 2
  why: a command outside the sandbox that names a folder that holds Claude Code's configuration 2
  routes: userOnly 2
  settled: you allowed 1, you denied 1
  waited: median 1m30s, longest 1m30s
  e.g. 10-27 09:00 177, visual-check: Bash: bosun conversation x | jq .
  matched: /home/u/.claude

reserved: a command outside the sandbox: 1
  routes: requester 1
  settled: you allowed 1
  e.g. 10-27 09:00 195, desk: Bash: bosun status --json
`
	if got := b.String(); got != want {
		t.Errorf("printReached printed\n%s\nwant\n%s", got, want)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.Local)
	for in, want := range map[string]time.Time{
		"":                     {},
		"24h":                  now.Add(-24 * time.Hour),
		"2026-10-27":           time.Date(2026, 10, 27, 0, 0, 0, 0, time.Local),
		"2026-10-27T09:30":     time.Date(2026, 10, 27, 9, 30, 0, 0, time.Local),
		"2026-10-27T09:30:00Z": time.Date(2026, 10, 27, 9, 30, 0, 0, time.UTC),
	} {
		if got, err := parseSince(in, now); err != nil || !got.Equal(want) {
			t.Errorf("parseSince(%q) = %v, %v, want %v", in, got, err, want)
		}
	}
	if _, err := parseSince("yesterday", now); err == nil {
		t.Error("parseSince took yesterday")
	}
}
