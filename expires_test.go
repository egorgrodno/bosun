package main

import (
	"context"
	"testing"
	"time"
)

func parseExpires(t *testing.T, dec Decision) time.Time {
	t.Helper()
	e, err := time.Parse(time.RFC3339, dec.Expires)
	if err != nil {
		t.Fatalf("decision %s expires %q (%v)", dec.ID, dec.Expires, err)
	}
	return e
}

// Only a permission that no reserved rule caught carries an expiry, promptAfter after it opened.
func TestDecisionExpires(t *testing.T) {
	testDaemon(t)
	go func() { _, _ = call(context.Background(), Request{Op: "ask", Node: "a", Text: "X or Y?"}) }()
	openDecision(t, 1)
	runHookAsync("a", hookEvent("PreToolUse", "t1", "Bash", map[string]any{"command": "git push --force origin main"}))
	openDecision(t, 2)
	runHookAsync("a", hookEvent("PermissionRequest", "", "Bash", bashTest))
	openDecision(t, 3)
	mustCall(t, Request{Op: "notify", Node: masterNode, Text: "b failed"})
	openDecision(t, 4)

	decs := mustCall(t, Request{Op: "status"}).Decisions
	if decs[1].Reserved == "" || decs[0].Expires != "" || decs[1].Expires != "" || decs[3].Expires != "" {
		t.Fatalf("decisions = %+v", decs)
	}
	asked, err := time.Parse(time.RFC3339, decs[2].Asked)
	if err != nil || !parseExpires(t, decs[2]).Equal(asked.Add(promptAfter)) {
		t.Fatalf("permission asked %q, expires %q (%v)", decs[2].Asked, decs[2].Expires, err)
	}

	// Settle them all, so no request outlives the test.
	mustCall(t, Request{Op: "reply", ID: decs[0].ID, Text: "X"})
	mustCall(t, Request{Op: "approve", ID: decs[1].ID})
	mustCall(t, Request{Op: "approve", ID: decs[2].ID})
	mustCall(t, Request{Op: "dismiss", ID: decs[3].ID})
	for range 500 {
		if len(mustCall(t, Request{Op: "status"}).Decisions) == 0 {
			time.Sleep(50 * time.Millisecond)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// After a restart the daemon holds an orphaned decision only until it gives up on the request coming back, and a permission whose request returns is held to its own deadline again.
func TestRestartMovesExpires(t *testing.T) {
	d, _ := testDaemon(t)
	reconnectWait = time.Minute
	t.Cleanup(func() { reconnectWait = 30 * time.Second })
	go func() { _, _ = call(context.Background(), Request{Op: "ask", Node: "a", Text: "X or Y?"}) }()
	openDecision(t, 1)
	hook := Request{Op: "hook", Node: "a", Key: "k", Hook: hookEvent("PermissionRequest", "", "Bash", bashTest)}
	go func() { _, _ = call(context.Background(), hook) }()
	before := openDecision(t, 2)

	restart(t, d)
	decs := mustCall(t, Request{Op: "status"}).Decisions
	for _, dec := range decs {
		if left := time.Until(parseExpires(t, dec)); left > time.Minute || left < 30*time.Second {
			t.Fatalf("orphan %s expires in %v", dec.ID, left)
		}
	}

	ctx := t.Context()
	go func() { _, _ = callAgain(ctx, hook) }()
	for range 500 {
		decs = mustCall(t, Request{Op: "status"}).Decisions
		if decs[1].Expires == before.Expires {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if decs[1].Expires != before.Expires {
		t.Fatalf("returned permission expires %q, was %q", decs[1].Expires, before.Expires)
	}
	// Settle both, so the daemon saves no more state while the test cleans up.
	mustCall(t, Request{Op: "reply", ID: decs[0].ID, Text: "X"})
	mustCall(t, Request{Op: "approve", ID: decs[1].ID})
	time.Sleep(100 * time.Millisecond)
}
