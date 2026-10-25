package main

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestResetTime(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip(err)
	}
	now := time.Date(2026, 10, 25, 18, 0, 0, 0, berlin)
	for text, want := range map[string]time.Time{
		"You've hit your limit · resets 9pm (Europe/Berlin)":                         time.Date(2026, 10, 25, 21, 0, 0, 0, berlin),
		"You've hit your limit · resets 3:30am (Europe/Berlin)":                      time.Date(2026, 10, 26, 3, 30, 0, 0, berlin),
		"You've hit your limit · resets 12pm (UTC)":                                  time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC),
		"You've hit your weekly limit · resets Oct 29, 6pm (Europe/Berlin)":          time.Date(2026, 10, 29, 18, 0, 0, 0, berlin),
		"You've hit your weekly limit · resets Jan 2, 2027, 11:15am (Europe/Berlin)": time.Date(2027, 1, 2, 11, 15, 0, 0, berlin),
	} {
		if got, ok := resetTime(text, now); !ok || !got.Equal(want) {
			t.Errorf("%s: %v %v, want %v", text, got, ok, want)
		}
	}
	if _, ok := resetTime("API Error: 529 overloaded", now); ok {
		t.Error("read a reset from a message without one")
	}
}

func stopFailure(t *testing.T, node, kind, message string) {
	t.Helper()
	raw := []byte(`{"hook_event_name":"StopFailure","session_id":"s","error":"` + kind + `","last_assistant_message":"` + message + `"}`)
	receive(t, runHookAsync(node, raw))
}

func TestUsageLimit(t *testing.T) {
	d, fc := testDaemon(t)
	fc.add(agent{ID: "aaaaaaaa", SessionID: "s-a", Name: "a", Status: "idle"})
	fc.mu.Lock()
	fc.agents[0].SessionID = "s-m"
	fc.mu.Unlock()
	d.poll()

	stopFailure(t, "a", "rate_limit", "You've hit your limit · resets 11pm (UTC)")
	stopFailure(t, masterNode, "rate_limit", "You've hit your limit · resets 11pm (UTC)")
	fc.setStatus("00000001", "idle")
	if dec := openDecision(t, 1); dec.Kind != "notice" || !strings.Contains(dec.Note, "usage limit") {
		t.Fatalf("decision = %+v", dec)
	}
	// A limit that holds through its probes brings one notice, not one per probe.
	stopFailure(t, "a", "rate_limit", "You've hit your limit")
	if n := len(mustCall(t, Request{Op: "status"}).Decisions); n != 1 {
		t.Fatalf("%d decisions open", n)
	}
	// An event while the limit holds would only meet it again.
	mustCall(t, Request{Op: "tell", Text: "x"})
	time.Sleep(50 * time.Millisecond)
	fc.mu.Lock()
	starts := len(fc.starts)
	fc.mu.Unlock()
	d.mu.Lock()
	events := len(d.events)
	limited := d.nodes["a"].LimitedUntil != nil && d.nodes[masterNode].LimitedUntil != nil
	d.mu.Unlock()
	if starts != 1 || events != 1 || !limited {
		t.Fatalf("%d starts, %d events, both limited: %v", starts, events, limited)
	}

	d.recoverLimited(time.Now().Add(25 * time.Hour))
	for i := 0; ; i++ {
		fc.mu.Lock()
		starts = len(fc.starts)
		fc.mu.Unlock()
		if starts == 3 {
			break
		}
		if i == 500 {
			t.Fatalf("%d starts after the reset", starts)
		}
		time.Sleep(10 * time.Millisecond)
	}
	fc.mu.Lock()
	first, second := fc.starts[1], fc.starts[2]
	fc.mu.Unlock()
	if !slices.Equal(first, []string{"--bg", "--resume", "s-m", "--", limitResetMaster}) || !slices.Equal(second, []string{"--bg", "--resume", "s-a", "--", limitReset}) {
		t.Fatalf("claude ran with %q, then %q", first, second)
	}
}

func TestTurnFailure(t *testing.T) {
	d, _ := testDaemon(t)
	stopFailure(t, "a", "server_error", "API Error: 500")
	events := mustCall(t, Request{Op: "next_event", Node: masterNode}).Events
	if len(events) != 1 || events[0].Kind != "turn_end" || !strings.Contains(events[0].Text, "server_error") {
		t.Fatalf("events = %+v", events)
	}
	stopFailure(t, masterNode, "authentication_failed", "Please run /login")
	if dec := openDecision(t, 1); dec.Kind != "notice" {
		t.Fatalf("decision = %+v", dec)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.asleep {
		t.Fatal("a master whose turn failed waits for no event")
	}
}
