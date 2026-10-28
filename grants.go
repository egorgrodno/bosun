package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"
)

// grantClass is a kind of decision that a standing grant may settle, narrow enough for bosun to check by itself.
type grantClass struct {
	name, what string
}

// grantClasses are the only classes a grant names. Each covers a permission of the master's own, which waits on the user since nobody sits above the master.
var grantClasses = []grantClass{
	{"push", "the master's push to a remote of the repository, as one whole command: git -C <repo> push [<remote> [<refspec>...]], with no option but -C and " + strings.Join(grantPushFlags, ", ") + ", from a repository whose git setup is as bosun recorded it"},
	{"rebuild", "the master's nixos-rebuild test or build with --flake on /etc/nixos itself, or nxt or nxc, as one whole command"},
}

// grantMax bounds how long a grant lasts.
const grantMax = 24 * time.Hour

// grantPushFlags are the options of git push that a grant's push may carry.
var grantPushFlags = []string{"-u", "--set-upstream", "--follow-tags", "-q", "--quiet", "-v", "--verbose", "--porcelain"}

// grantClass names the class of grantClasses that the master's tool call falls in, or "".
// It reads the command alone, so a call that a grant covers is one the user could read off the grant.
func (g gate) grantClass(tool string, input json.RawMessage) string {
	if tool != "Bash" {
		return ""
	}
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	argv := strings.Split(in.Command, " ")
	// rebuild checks each word itself, and takes the # of a host attribute, which plainWord does not.
	if rebuild(argv) {
		// A worktree's configuration is code that only the master has read.
		if slices.ContainsFunc(argv, func(w string) bool { return strings.Contains(w, "/.claude/worktrees/") }) {
			return ""
		}
		return "rebuild"
	}
	if argv[0] != "git" || slices.ContainsFunc(argv, func(w string) bool { return !plainWord.MatchString(w) }) {
		return ""
	}
	cd, sub, rest, opt := gitCommand(argv[1:])
	if sub != "push" || opt != "" {
		return ""
	}
	if slices.ContainsFunc(rest, func(a string) bool { return strings.HasPrefix(a, "-") && !slices.Contains(grantPushFlags, a) }) {
		return ""
	}
	named, r := pushArgs(rest)
	if r.reason != "" {
		return ""
	}
	dir := at(g.cwd, cd)
	if dir == "" {
		return ""
	}
	for _, name := range named {
		if !slices.Contains(g.repos.remotes(dir), name) {
			return ""
		}
	}
	if g.repos.changed(dir) != "" {
		return ""
	}
	return "push"
}

func classWhat(name string) string {
	if i := slices.IndexFunc(grantClasses, func(c grantClass) bool { return c.name == name }); i >= 0 {
		return grantClasses[i].what
	}
	return ""
}

// classList lists each class with what it covers, one to a line.
func classList() string {
	var b strings.Builder
	for _, c := range grantClasses {
		fmt.Fprintf(&b, "\n- %s: %s", c.name, c.what)
	}
	return b.String()
}

func classNames() string {
	var names []string
	for _, c := range grantClasses {
		names = append(names, c.name)
	}
	return strings.Join(names, ", ")
}

// parseUntil reads when a grant ends: a time of day such as 09:00, its next coming, or a duration such as 8h, or an RFC 3339 time.
func parseUntil(s string, now time.Time) (time.Time, error) {
	var until time.Time
	if t, err := time.ParseInLocation("15:04", s, now.Location()); err == nil {
		until = time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
		if !until.After(now) {
			until = until.AddDate(0, 0, 1)
		}
	} else if dur, err := time.ParseDuration(s); err == nil {
		until = now.Add(dur)
	} else if t, err := time.Parse(time.RFC3339, s); err == nil {
		until = t
	} else {
		return time.Time{}, fmt.Errorf("until %q: give a time of day such as 09:00, a duration such as 8h, or an RFC 3339 time", s)
	}
	switch {
	case !until.After(now):
		return time.Time{}, fmt.Errorf("until %q has passed", s)
	case until.Sub(now) > grantMax:
		return time.Time{}, fmt.Errorf("until %q is more than %v away, which is as long as a grant lasts", s, grantMax)
	}
	return until, nil
}

// grantRequest builds a grant request from a frontend's words: a class, when it ends, and the user's words, if any.
func grantRequest(args []string) (Request, error) {
	if len(args) < 2 {
		return Request{}, fmt.Errorf("grant takes a class (%s) and when it ends, such as 09:00 or 8h", classNames())
	}
	return Request{Op: "grant", Name: args[0], Text: args[1], Words: strings.Join(args[2:], " ")}, nil
}

// onUserWordLocked checks that caller acts on the user's word: the user from a frontend, or the desk quoting words from the user's latest message. what completes "only the user and the desk …".
func (d *daemon) onUserWordLocked(caller, words, what string) error {
	switch caller {
	case "":
		return nil
	case deskNode:
		if squash(words) == "" {
			return errors.New("quote the user's words that ask for it")
		}
		if d.said.at.IsZero() || !strings.Contains(squash(d.said.text), squash(words)) {
			return fmt.Errorf("the user's latest message does not hold %q: quote their words exactly", words)
		}
		return nil
	}
	return errors.New("only the user and the desk " + what)
}

// grant gives a standing grant of class until the time that until names, for caller acting on the user's words.
func (d *daemon) grant(caller, class, until, words string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.onUserWordLocked(caller, words, "give grants"); err != nil {
		return "", err
	}
	if classWhat(class) == "" {
		return "", fmt.Errorf("no grant class %q: the classes are %s", class, classNames())
	}
	now := time.Now()
	end, err := parseUntil(until, now)
	if err != nil {
		return "", err
	}
	d.nextGrant++
	g := Grant{ID: "g" + strconv.Itoa(d.nextGrant), Class: class, Since: now, Until: end, By: role(caller), Words: words}
	d.grants = append(d.grants, g)
	d.saveLocked()
	d.broadcastGrantsLocked()
	d.logLocked(Entry{Kind: "granted", By: role(caller), From: caller, Grant: &g, Text: words})
	log.Printf("grant %s: %s until %s", g.ID, g.Class, g.Until.Format(time.RFC3339))
	text := fmt.Sprintf("Grant %s: while away mode is on, until %s, bosun allows %s.", g.ID, untilLabel(g.Until, now), classWhat(class))
	if d.away == nil {
		text += " Away mode is off, so it applies once it goes on."
	}
	return text, nil
}

// revoke ends grant id, for caller acting on the user's words.
func (d *daemon) revoke(caller, id, words string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.onUserWordLocked(caller, words, "revoke grants"); err != nil {
		return "", err
	}
	i := slices.IndexFunc(d.grants, func(g Grant) bool { return g.ID == id })
	if i < 0 {
		return "", fmt.Errorf("no grant %s", id)
	}
	g := d.grants[i]
	d.grants = slices.Delete(d.grants, i, i+1)
	d.saveLocked()
	d.broadcastGrantsLocked()
	d.logLocked(Entry{Kind: "revoked", By: role(caller), From: caller, Grant: &g, Text: words})
	log.Printf("grant %s: revoked", g.ID)
	return fmt.Sprintf("Revoked grant %s.", g.ID), nil
}

// expireGrants ends each grant whose time is up.
func (d *daemon) expireGrants(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var gone []Grant
	d.grants = slices.DeleteFunc(d.grants, func(g Grant) bool {
		if g.Until.After(now) {
			return false
		}
		gone = append(gone, g)
		return true
	})
	if len(gone) == 0 {
		return
	}
	d.saveLocked()
	d.broadcastGrantsLocked()
	for _, g := range gone {
		d.logLocked(Entry{Kind: "expired", By: "bosun", Grant: &g})
		log.Printf("grant %s: expired", g.ID)
	}
}

func (d *daemon) broadcastGrantsLocked() {
	d.broadcastLocked(Event{Kind: "grants", Grants: slices.Clone(d.grants)})
}

// grantLocked allows dec under a live grant of its class, and reports whether one did.
// Grants act only while the user is away, and never on a reserved action on the user's list, whatever its class.
func (d *daemon) grantLocked(dec *Decision) bool {
	if !d.awayLocked() || dec.UserOnly || dec.Kind != "permission" || dec.Node != masterNode || dec.Class == "" {
		return false
	}
	now := time.Now()
	i := slices.IndexFunc(d.grants, func(g Grant) bool { return g.Class == dec.Class && g.Until.After(now) })
	if i < 0 {
		return false
	}
	g := d.grants[i]
	d.decisions[dec.ID] = dec
	d.settleLocked(dec, answer{behavior: "allow"}, outcome{by: "bosun", grant: &g})
	log.Printf("decision %s: allowed under grant %s", dec.ID, g.ID)
	return true
}

// untilLabel shows when a grant ends, with the day when it is not today.
func untilLabel(until, now time.Time) string {
	until, now = until.Local(), now.Local()
	if y, m, d := until.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return until.Format("15:04")
	}
	return until.Format("Jan 2 15:04")
}

// grantLine shows grant g on one line, as of now.
func grantLine(g Grant, now time.Time) string {
	s := fmt.Sprintf("%-4s %-8s until %s", g.ID, g.Class, untilLabel(g.Until, now))
	if g.Words != "" {
		s += fmt.Sprintf(", on your words %q", squash(g.Words))
	}
	return s
}

// grantsHeader heads a frontend's list of standing grants.
func grantsHeader(away bool) string {
	if away {
		return "Standing grants, which bosun acts on while you are away:"
	}
	return "Standing grants, which bosun acts on once away mode is on:"
}

// grantEntryLine shows a granted, revoked or expired log entry.
func grantEntryLine(e Entry) string {
	if e.Grant == nil {
		return e.Kind + " a grant"
	}
	g := *e.Grant
	switch e.Kind {
	case "granted":
		s := fmt.Sprintf("granted %s: %s until %s", g.ID, g.Class, g.Until.Local().Format("Jan 2 15:04"))
		if e.Text != "" {
			s += fmt.Sprintf(", on your words %q", squash(e.Text))
		}
		return s
	case "revoked":
		return "revoked grant " + g.ID
	}
	return fmt.Sprintf("grant %s (%s) expired", g.ID, g.Class)
}

// grantsLine is what tells the master, in each batch while the user is away, which of its calls bosun allows.
func grantsLine(grants []Grant) string {
	if len(grants) == 0 {
		return ""
	}
	var parts []string
	for _, g := range grants {
		parts = append(parts, fmt.Sprintf("%s until %s: %s", g.ID, g.Until.Local().Format("Jan 2 15:04"), classWhat(g.Class)))
	}
	return "The user gave standing grants, so bosun allows these calls of yours without them, but only as described: " + strings.Join(parts, "; ") + ". Run such a call that way and outside the sandbox, rather than park it."
}
