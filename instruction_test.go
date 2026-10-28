package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// Each tell_master gets the next number, in its result, its log entry and its event. The numbers carry on after a restart.
func TestInstructionNumbers(t *testing.T) {
	d, _ := testDesk(t)
	desk := newMCPServer(deskNode, nil)
	for i, want := range []string{"instruction 1", "instruction 2"} {
		got, err := desk.run(context.Background(), "tell_master", json.RawMessage(`{"text":"do it"}`))
		if err != nil || !strings.Contains(got, want) {
			t.Fatalf("tell_master %d = %q, %v", i, got, err)
		}
	}
	// The user's own tell to the desk is no instruction.
	mustCall(t, Request{Op: "tell", Text: "hello"})
	var nums []int
	for _, e := range logOf(d, "tell") {
		nums = append(nums, e.Instruction)
	}
	if len(nums) != 3 || nums[0] != 1 || nums[1] != 2 || nums[2] != 0 {
		t.Fatalf("tell entries carry %v", nums)
	}
	evs := mustCall(t, Request{Op: "next_event", Node: masterNode}).Events
	if len(evs) != 2 || evs[0].Instruction != 1 || evs[1].Instruction != 2 {
		t.Fatalf("events = %+v", evs)
	}

	d = restart(t, d)
	got, err := newMCPServer(deskNode, nil).run(context.Background(), "tell_master", json.RawMessage(`{"text":"again"}`))
	if err != nil || !strings.Contains(got, "instruction 3") {
		t.Fatalf("after a restart tell_master = %q, %v", got, err)
	}
}

// The master's tell_desk, spawn, resume and merge carry the instruction into the log, and the desk is told which one the master answers.
func TestInstructionReachesLog(t *testing.T) {
	d, fc := testDesk(t)
	master := newMCPServer(masterNode, nil)
	if _, err := master.run(context.Background(), "tell_desk", json.RawMessage(`{"text":"a took it.","instruction":7}`)); err != nil {
		t.Fatal(err)
	}
	if args := awaitStarts(t, fc, 3); args[len(args)-1] != "bosun: the master answers instruction 7:\na took it." {
		t.Fatalf("claude ran with %q", args)
	}
	if tells := logOf(d, "tell"); len(tells) != 1 || tells[0].Instruction != 7 || tells[0].Node != deskNode {
		t.Fatalf("tells = %+v", tells)
	}

	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	mustCall(t, Request{Op: "spawn", Node: masterNode, Name: "s", Repo: repo, Text: "x", Instruction: 5})
	if es := logOf(d, "spawned"); len(es) != 1 || es[0].Instruction != 5 {
		t.Fatalf("spawned = %+v", es)
	}

	b, _, _ := mergeable(t, d, fc)
	fc.mu.Lock()
	fc.agents[len(fc.agents)-1].SessionID = "s-b"
	fc.mu.Unlock()
	d.mu.Lock()
	d.nodes["b"] = b
	d.mu.Unlock()
	mustCall(t, Request{Op: "resume", Node: masterNode, Name: "b", Text: "more", Instruction: 6})
	if es := logOf(d, "resumed"); len(es) != 1 || es[0].Instruction != 6 {
		t.Fatalf("resumed = %+v", es)
	}
	fc.setStatus(b.ID, "idle")
	d.poll()
	mustCall(t, Request{Op: "merge", Node: masterNode, Name: "b", Instruction: 6})
	if es := logOf(d, "merged"); len(es) != 1 || es[0].Instruction != 6 {
		t.Fatalf("merged = %+v", es)
	}
}

// The MCP tools pass instruction on for the master alone.
func TestInstructionArgument(t *testing.T) {
	for node, want := range map[string]bool{masterNode: true, "a": false} {
		var found bool
		for _, tl := range tools(node) {
			if tl.Name == "spawn" || tl.Name == "merge" || tl.Name == "tell_desk" || tl.Name == "resume" {
				_, ok := tl.InputSchema["properties"].(map[string]any)["instruction"]
				found = found || ok
			}
		}
		if found != want {
			t.Errorf("%s: instruction offered = %v", node, found)
		}
	}
}
