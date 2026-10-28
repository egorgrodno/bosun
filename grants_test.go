package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
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
		"git -C r push origin main":                       "",
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
	g.repos = fakeRepos{change: map[string]string{"/etc/nixos": "whose config's core.fsmonitor changed"}}
	if got := g.grantClass("Bash", json.RawMessage(`{"command":"nxt"}`)); got != "" {
		t.Errorf("nxt with /etc/nixos changed falls in %q", got)
	}
}

// recordNixos points nixosRepo at a repository that d has a record of.
func recordNixos(t *testing.T, d *daemon) {
	t.Helper()
	_, repo, _ := recorded(t)
	if err := d.repos().record(repo); err != nil {
		t.Fatal(err)
	}
	was := nixosRepo
	nixosRepo = repo
	t.Cleanup(func() { nixosRepo = was })
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
	recordNixos(t, d)
	_, next := watchKinds(t)
	if text := grant(t, "rebuild", "8h"); !strings.Contains(text, "Grant g1") || !strings.Contains(text, "acts only once it goes on") {
		t.Fatalf("grant said %q", text)
	}
	awaitEvent(t, next, "grants")

	hook := runHookAsync(masterNode, hookEvent("PermissionRequest", "", "Bash", map[string]any{"command": "nxt"}))
	dec := openDecision(t, 1)
	if dec.Holder != "user" || dec.Class != "rebuild" {
		t.Fatalf("with away mode off, the master's rebuild opened %+v", dec)
	}
	// Its class was read before away mode went on, so it parks.
	goAway(t, "off to bed")
	if out := receive(t, hook); out != parkedDeny("PermissionRequest") {
		t.Fatalf("the rebuild already waiting printed %s", out)
	}
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
	recordNixos(t, d)
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
	recordNixos(t, d)
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

// Only the user gives grants: from a frontend, or through the desk on their yes. The desk revokes one on their quoted words.
func TestGrantCallers(t *testing.T) {
	testDesk(t)
	if _, err := call(context.Background(), Request{Op: "grant", Node: masterNode, Name: "push", Text: "8h"}); err == nil {
		t.Fatal("the master gave a grant")
	}
	if _, err := call(context.Background(), Request{Op: "grant", Name: "anything", Text: "8h"}); err == nil {
		t.Fatal("a grant of an unknown class")
	}
	if _, err := deskGrant(`{"class":"anything","until":"8h"}`); err == nil {
		t.Fatal("the desk proposed a grant of an unknown class")
	}
	if _, err := deskGrant(`{"class":"push","until":"25h"}`); err == nil {
		t.Fatal("the desk proposed a grant longer than a day")
	}
	deskSays(t, "push what you merge tonight, until 9")
	deskGrant(`{"class":"push","until":"09:00"}`)
	deskSays(t, "yes")
	if text, err := deskGrant(`{"class":"push","until":"09:00","words":"yes"}`); err != nil || !strings.HasPrefix(text, "Grant g1") {
		t.Fatalf("the grant on the user's yes said %q, %v", text, err)
	}
	deskSays(t, "stop pushing, revoke g1")
	if text, err := newMCPServer(deskNode, nil).run(context.Background(), "revoke", json.RawMessage(`{"id":"g1","words":"revoke g1"}`)); err != nil || text != "Revoked grant g1." {
		t.Fatalf("the desk's revoke said %q, %v", text, err)
	}
}

// deskGrant calls the desk's grant tool with the JSON arguments args.
func deskGrant(args string) (string, error) {
	return newMCPServer(deskNode, nil).run(context.Background(), "grant", json.RawMessage(args))
}

// No message of the user's gives a grant before the desk has shown it to them, however it reads, and a refusal holds no yes.
func TestDeskGrantNeedsYes(t *testing.T) {
	d, _ := testDesk(t)
	noGrant := func(args, why string) string {
		t.Helper()
		before := len(mustCall(t, Request{Op: "status"}).Grants)
		text, err := deskGrant(args)
		if err != nil {
			t.Fatalf("%s: %v", why, err)
		}
		if !strings.Contains(text, "bosun has not given the grant") {
			t.Fatalf("%s: the desk's grant said %q", why, text)
		}
		if g := mustCall(t, Request{Op: "status"}).Grants; len(g) != before {
			t.Fatalf("%s: grants %+v", why, g)
		}
		return text
	}
	// The first call gives nothing, whatever words it quotes.
	deskSays(t, "don't push anything tonight")
	noGrant(`{"class":"push","until":"8h","words":"push anything tonight"}`, "a grant on words from before it was shown")
	if text := noGrant(`{"class":"push","until":"8h","words":"push anything tonight"}`, "a grant on the same words again"); !strings.HasPrefix(text, "The user has said nothing since you showed it.") {
		t.Fatalf("a grant on the same words again said %q", text)
	}

	shown := noGrant(`{"class":"push","until":"09:00"}`, "the first call")
	for _, want := range []string{"A standing grant of push, until ", "git -C <repo> push", "never a worker's call", "Away mode is off"} {
		if !strings.Contains(shown, want) {
			t.Fatalf("the proposal %q lacks %q", shown, want)
		}
	}
	// A reply that refuses or changes the grant holds words of a yes, but is not one.
	for reply, words := range map[string]string{
		"no, don't push anything tonight": "push anything tonight",
		"yes, but only until 07:00":       "yes",
	} {
		deskSays(t, reply)
		if text, err := deskGrant(fmt.Sprintf(`{"class":"push","until":"09:00","words":%q}`, words)); err == nil || !strings.Contains(err.Error(), "is not "+strconv.Quote(words)) {
			t.Fatalf("a grant on %q from %q said %q, %v", words, reply, text, err)
		}
		if g := mustCall(t, Request{Op: "status"}).Grants; len(g) != 0 {
			t.Fatalf("a grant on %q from %q: grants %+v", words, reply, g)
		}
		noGrant(`{"class":"push","until":"09:00"}`, "proposing anew")
	}
	if text := noGrant(`{"class":"push","until":"07:00"}`, "proposing another end"); !strings.HasPrefix(text, "It differs from what you last showed") {
		t.Fatalf("proposing another end said %q", text)
	}
	deskSays(t, "what does it push?")
	deskSays(t, "ok, yes")
	if text := noGrant(`{"class":"push","until":"07:00","words":"ok, yes"}`, "a grant two messages after it was shown"); !strings.HasPrefix(text, "The user has written to you more than once") {
		t.Fatalf("a grant two messages after it was shown said %q", text)
	}

	deskSays(t, "Yes,\n  give it.")
	text, err := deskGrant(`{"class":"push","until":"07:00","words":"Yes, give it."}`)
	if err != nil || !strings.HasPrefix(text, "Grant g1: while away mode is on, until ") {
		t.Fatalf("the grant on the user's yes said %q, %v", text, err)
	}
	g := mustCall(t, Request{Op: "status"}).Grants
	if len(g) != 1 || g[0].Class != "push" || g[0].By != "desk" || g[0].Words != "Yes, give it." || g[0].Until.Local().Format("15:04") != "07:00" {
		t.Fatalf("grants %+v", g)
	}
	// The yes gave one grant.
	if text := noGrant(`{"class":"push","until":"07:00","words":"Yes, give it."}`, "a second grant on the same yes"); !strings.HasPrefix(text, "bosun has not given") {
		t.Fatalf("a second grant on the same yes said %q", text)
	}
	if text := noGrant(`{"class":"rebuild","until":"07:00","words":"Yes, give it."}`, "a grant of a class not shown"); !strings.HasPrefix(text, "bosun has not given") {
		t.Fatalf("a grant of a class not shown said %q", text)
	}

	var b strings.Builder
	printStatus(&b, mustCall(t, Request{Op: "status"}))
	if !strings.Contains(b.String(), "g1   push     until ") || !strings.Contains(b.String(), `07:00, on your words "Yes, give it."`) {
		t.Fatalf("status shows\n%s", b.String())
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if e := d.logged[len(d.logged)-1]; e.Kind != "granted" || e.By != "desk" || e.Text != "Yes, give it." || !strings.Contains(logLine(e), `on your words "Yes, give it."`) {
		t.Fatalf("logged %+v", e)
	}
}

// The grant the user said yes to ends when the desk showed them, and one that ended already is not given.
func TestDeskGrantKeepsShownEnd(t *testing.T) {
	d, _ := testDesk(t)
	deskGrant(`{"class":"push","until":"8h"}`)
	d.mu.Lock()
	p := d.proposed[grantKey+"push"]
	p.at = p.at.Add(-time.Hour)
	d.proposed[grantKey+"push"] = p
	d.mu.Unlock()
	deskSays(t, "yes")
	if _, err := deskGrant(`{"class":"push","until":"8h","words":"yes"}`); err != nil {
		t.Fatal(err)
	}
	if g := mustCall(t, Request{Op: "status"}).Grants; len(g) != 1 || g[0].Until.Sub(p.at) != 8*time.Hour {
		t.Fatalf("grants %+v, shown at %v", g, p.at)
	}

	deskGrant(`{"class":"rebuild","until":"1h"}`)
	d.mu.Lock()
	p = d.proposed[grantKey+"rebuild"]
	p.at = p.at.Add(-2 * time.Hour)
	d.proposed[grantKey+"rebuild"] = p
	d.said, d.saidBefore = userMessage{}, time.Time{}
	d.mu.Unlock()
	deskSays(t, "yes")
	if text, err := deskGrant(`{"class":"rebuild","until":"1h","words":"yes"}`); err == nil || !strings.Contains(err.Error(), "ended already") {
		t.Fatalf("a grant that ended already said %q, %v", text, err)
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
