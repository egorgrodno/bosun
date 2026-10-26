package main

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

func TestSpawnEffort(t *testing.T) {
	d, fc := testDaemon(t)
	d.workerEffort = "medium"
	repo := t.TempDir()
	git(t, repo, "init", "-q")
	for _, tc := range []struct{ name, effort, want string }{
		{"b", "", "medium"},
		{"c", "low", "low"},
	} {
		s := mustCall(t, Request{Op: "spawn", Name: tc.name, Repo: repo, Effort: tc.effort, Text: "x"}).Session
		if got := argAfter(fc.lastStart(), "--effort"); s.Effort != tc.want || got != tc.want {
			t.Errorf("spawn %s with effort %q: session effort %q, --effort %q, want %q", tc.name, tc.effort, s.Effort, got, tc.want)
		}
	}
	if _, err := call(context.Background(), Request{Op: "spawn", Name: "e", Repo: repo, Effort: "huge", Text: "x"}); err == nil {
		t.Error("spawn with an unknown effort succeeded")
	}
}

// A switch moves the worker to a copy of its conversation under a new id, which the daemon follows, and merge removes the first id too.
func TestResumeSwitch(t *testing.T) {
	d, fc := testDaemon(t)
	b, _, _ := mergeable(t, d, fc)
	fc.mu.Lock()
	fc.agents[len(fc.agents)-1].SessionID = "s-b"
	fc.mu.Unlock()
	d.mu.Lock()
	b.Model, b.Effort = "sonnet", "low"
	d.mu.Unlock()

	mustCall(t, Request{Op: "resume", Node: masterNode, Name: "b", Effort: "high", Text: "again"})
	args := fc.lastStart()
	if argAfter(args, "--resume") != "s-b" || argAfter(args, "--model") != "sonnet" || argAfter(args, "--effort") != "high" || argAfter(args, "--settings") == "" || args[len(args)-1] != "again" {
		t.Fatalf("claude ran with %q", args)
	}
	d.mu.Lock()
	got := *d.nodes["b"]
	d.mu.Unlock()
	if got.ID == "bbbbbbbb" || got.Effort != "high" || got.Model != "sonnet" || !slices.Equal(got.Retired, []string{"bbbbbbbb"}) || got.Status != "starting" {
		t.Fatalf("b = %+v", got)
	}

	fc.mu.Lock()
	fc.agents[slices.IndexFunc(fc.agents, func(a agent) bool { return a.ID == got.ID })].SessionID = "s-b2"
	fc.mu.Unlock()
	fc.setStatus(got.ID, "idle")
	d.poll()
	mustCall(t, Request{Op: "resume", Node: masterNode, Name: "b", Effort: "high", Text: "more"})
	if args := fc.lastStart(); slices.Contains(args, "--effort") {
		t.Fatalf("a resume at the effort it runs at ran with %q", args)
	}

	fc.setStatus(got.ID, "idle")
	d.poll()
	mustCall(t, Request{Op: "merge", Node: masterNode, Name: "b"})
	fc.mu.Lock()
	removed := slices.Clone(fc.removed)
	fc.mu.Unlock()
	if !slices.Contains(removed, "bbbbbbbb") || !slices.Contains(removed, got.ID) {
		t.Fatalf("removed %v, want both ids", removed)
	}
}

func TestResumeSwitchFailureResumesOld(t *testing.T) {
	d, fc := testDaemon(t)
	b, _, _ := mergeable(t, d, fc)
	fc.mu.Lock()
	fc.agents[len(fc.agents)-1].SessionID = "s-b"
	fc.fail = func(args []string) error {
		if slices.Contains(args, "--effort") {
			return fmt.Errorf("bad effort")
		}
		return nil
	}
	fc.mu.Unlock()
	if _, err := call(context.Background(), Request{Op: "resume", Node: masterNode, Name: "b", Effort: "max", Text: "again"}); err == nil {
		t.Fatal("the failing switch succeeded")
	}
	if args := fc.lastStart(); !slices.Equal(args, []string{"--bg", "--resume", "s-b"}) {
		t.Fatalf("after the failure claude ran with %q, want the old session resumed idle", args)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if b.ID != "bbbbbbbb" || b.Effort != "" || len(b.Retired) != 0 {
		t.Fatalf("b = %+v", b)
	}
}
