package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestStatusShowsStates(t *testing.T) {
	var out bytes.Buffer
	printStatus(&out, Response{Paused: true, Sessions: []Session{
		{Node: masterNode, ID: "mmmmmmmm", Status: "waiting", State: "idle"},
		{Node: "a", Parent: masterNode, ID: "aaaaaaaa", Status: "stopped", Killed: true},
		{Node: "b", Parent: masterNode, ID: "bbbbbbbb", Status: "stopped"},
		{Node: "c", Parent: masterNode, ID: "cccccccc", Status: "exited", Killed: true},
		{Node: "d", Parent: masterNode, ID: "dddddddd", Status: "stopped", Paused: true},
	}})
	want := map[string]string{masterNode: "waiting/idle", "a": "killed", "b": "stopped", "c": "killed", "d": "paused"}
	if !strings.HasPrefix(out.String(), "Paused") {
		t.Fatalf("status printed %q", out.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")[2:]
	if len(lines) != len(want) {
		t.Fatalf("status printed %q", out.String())
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[2] != want[fields[0]] {
			t.Errorf("status printed %q, want %s %s", line, fields[0], want[fields[0]])
		}
	}
}
