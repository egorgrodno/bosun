package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
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

func TestMasterAndDeskEffort(t *testing.T) {
	d, fc := testDaemon(t)
	d.masterEffort, d.deskEffort = "low", "medium"
	fc.mu.Lock()
	fc.agents = nil
	fc.mu.Unlock()
	if err := d.ensureMaster(); err != nil {
		t.Fatal(err)
	}
	if got := argAfter(fc.lastStart(), "--effort"); got != "low" {
		t.Errorf("a new master runs --effort %q, want low", got)
	}
	if err := d.ensureDesk(); err != nil {
		t.Fatal(err)
	}
	if got := argAfter(fc.lastStart(), "--effort"); got != "medium" {
		t.Errorf("a new desk runs --effort %q, want medium", got)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.nodes[masterNode].Effort != "low" || d.nodes[deskNode].Effort != "medium" {
		t.Errorf("master %+v, desk %+v", d.nodes[masterNode], d.nodes[deskNode])
	}
}

// A switch of the effort alone keeps the model the master runs.
func TestSwitchMasterEffort(t *testing.T) {
	d, fc := testDaemon(t)
	d.mu.Lock()
	d.nodes[masterNode].SessionID, d.nodes[masterNode].Model = "s-master", "sonnet"
	d.mu.Unlock()
	mustCall(t, Request{Op: "model", Name: masterNode, Effort: "high"})
	args := fc.lastStart()
	if argAfter(args, "--resume") != "s-master" || argAfter(args, "--model") != "sonnet" || argAfter(args, "--effort") != "high" || !strings.HasPrefix(argAfter(args, "--append-system-prompt"), masterPrompt) {
		t.Fatalf("claude ran with %q", args)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if m := d.nodes[masterNode]; m.Model != "sonnet" || m.Effort != "high" {
		t.Fatalf("master = %+v", m)
	}
}

// The desk moves to a copy of its conversation as the master does, and its old id goes, since it holds no worktree.
func TestSwitchDesk(t *testing.T) {
	d, fc := testDesk(t)
	mustCall(t, Request{Op: "model", Name: deskNode, Model: "opus", Effort: "low"})
	args := fc.lastStart()
	if argAfter(args, "--resume") != "s-d" || argAfter(args, "--model") != "opus" || argAfter(args, "--effort") != "low" || !strings.HasPrefix(argAfter(args, "--append-system-prompt"), deskPrompt) || !deskPromptFromBosun(args[len(args)-1]) {
		t.Fatalf("claude ran with %q", args)
	}
	d.mu.Lock()
	desk := *d.nodes[deskNode]
	d.mu.Unlock()
	if desk.ID == "00000002" || desk.Model != "opus" || desk.Effort != "low" || len(desk.Retired) != 0 || desk.Status != "starting" {
		t.Fatalf("desk = %+v", desk)
	}
	for range 100 {
		fc.mu.Lock()
		removed := slices.Contains(fc.removed, "00000002")
		fc.mu.Unlock()
		if removed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the desk's old id stayed")
}

func TestSwitchRefuses(t *testing.T) {
	d, _ := testDaemon(t)
	d.mu.Lock()
	d.nodes[masterNode].SessionID, d.nodes[masterNode].Model = "s-master", "opus"
	d.mu.Unlock()
	for _, r := range []Request{
		{Op: "model"},
		{Op: "model", Model: "opus"},
		{Op: "model", Effort: "huge"},
		{Op: "model", Name: "a", Model: "sonnet"},
		{Op: "model", Name: deskNode, Model: "sonnet"},
	} {
		if _, err := call(context.Background(), r); err == nil {
			t.Errorf("%+v succeeded", r)
		}
	}
}

func TestModelRequest(t *testing.T) {
	for _, tc := range []struct {
		words         []string
		model, effort string
		fails         bool
	}{
		{words: []string{"opus"}, model: "opus"},
		{words: []string{"high"}, effort: "high"},
		{words: []string{"high", "sonnet"}, model: "sonnet", effort: "high"},
		{words: nil, fails: true},
		{words: []string{"opus", "sonnet"}, fails: true},
		{words: []string{"low", "max"}, fails: true},
	} {
		req, err := modelRequest(deskNode, tc.words)
		if (err != nil) != tc.fails || err == nil && (req.Op != "model" || req.Name != deskNode || req.Model != tc.model || req.Effort != tc.effort) {
			t.Errorf("modelRequest(%q) = %+v, %v", tc.words, req, err)
		}
	}
}
