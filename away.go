package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// parkedCall and parkedQuestion answer a permission and a question that bosun parked for the user's return.
const (
	parkedCall     = "bosun: the user is away, so bosun parked this call on the list they get when they come back, instead of asking them now. Do not try it again, nor another way to the same end: carry on with the work that does not need it, and say what waits on it when you end your turn."
	parkedQuestion = "bosun: the user is away, so bosun parked this question on the list they get when they come back, instead of asking them now. Do not ask it again: carry on with what you can do without the answer, and when nothing is left, end your turn saying what waits on it."
)

// awayLocked reports whether the user is away, so that what would wait on them parks instead.
func (d *daemon) awayLocked() bool {
	return d.away != nil
}

// parkLocked is where every decision that would wait on the user goes, as it opens or as the master escalates it.
// While the user is away, it parks dec: it answers a permission or question with parkedCall or parkedQuestion, keeps it on the list for their return, and reports true. A notice that a permission waits in its session's own prompt never parks, since only an answer there settles it.
func (d *daemon) parkLocked(dec *Decision) bool {
	if !d.awayLocked() || dec.Prompt {
		return false
	}
	c := *dec
	c.Input, c.Expires = cutInput(c.Input), ""
	p := Parked{Decision: c, Why: reachesUser(dec), At: time.Now()}
	d.parked = append(d.parked, p)
	if d.decisions[dec.ID] == dec {
		d.settleLocked(dec, parkedAnswer(dec), outcome{by: "bosun"})
	} else {
		if dec.Kind != "notice" {
			dec.answer <- parkedAnswer(dec)
		}
		d.saveLocked()
	}
	d.broadcastLocked(Event{Kind: "parked", Parked: &p})
	d.logLocked(Entry{Kind: "parked", By: "bosun", From: dec.from, Node: dec.Node, Decision: dec, Text: p.Why})
	log.Printf("decision %s: parked, since the user is away", dec.ID)
	return true
}

func parkedAnswer(dec *Decision) answer {
	if dec.Kind == "question" {
		return answer{text: parkedQuestion}
	}
	return answer{behavior: "deny", text: parkedCall}
}

// reachesUser says why dec waits on the user rather than the master, completing "parked because …".
func reachesUser(dec *Decision) string {
	switch {
	case dec.Kind == "notice" && dec.from == masterNode:
		return "it is a notice from the master"
	case dec.Kind == "notice":
		return "it is a notice from bosun"
	case dec.UserOnly:
		return "it is " + dec.Reserved + ", which is on your list"
	case dec.Node == masterNode:
		return "it is the master's own permission"
	case dec.Node == deskNode:
		return "it is the desk's own permission"
	case dec.Note != "":
		return "the master escalated it: " + dec.Note
	}
	return "the master escalated it"
}

// setAway turns the away mode on or off for caller: the user from a frontend, or the desk on the user's words, which must appear in their latest message.
// Turning it on parks what waits on the user already, other than notices. Turning it off hands the user what was parked, as one list through the desk and as a notice.
func (d *daemon) setAway(caller string, on bool, words string) (string, error) {
	switch caller {
	case "":
	case deskNode:
		if words == "" {
			return "", errors.New("quote the user's words that ask for it")
		}
	default:
		return "", errors.New("only the user and the desk switch the away mode")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if caller == deskNode && (d.said.at.IsZero() || !strings.Contains(squash(d.said.text), squash(words))) {
		return "", fmt.Errorf("the user's latest message does not hold %q: quote their words exactly", words)
	}
	if on {
		return d.goAwayLocked(caller, words)
	}
	return d.comeBackLocked(caller)
}

func (d *daemon) goAwayLocked(caller, words string) (string, error) {
	if d.away != nil {
		return "", fmt.Errorf("away mode is already on, since %s", d.away.Since.Local().Format("Jan 2 15:04"))
	}
	d.away = &Away{Since: time.Now(), By: role(caller), Words: words}
	d.parked = nil
	a := *d.away
	d.broadcastLocked(Event{Kind: "away", Away: &a})
	d.logLocked(Entry{Kind: "away", By: role(caller), From: caller, Text: words})
	log.Print("away mode on")
	var waiting []*Decision
	for _, dec := range d.decisions {
		if dec.Holder == "user" && dec.Kind != "notice" {
			waiting = append(waiting, dec)
		}
	}
	slices.SortFunc(waiting, func(a, b *Decision) int { return byID(a.ID, b.ID) })
	for _, dec := range waiting {
		d.parkLocked(dec)
	}
	d.saveLocked()
	text := "Away mode is on: bosun parks what would wait on the user, and the master carries on with the rest."
	if len(waiting) > 0 {
		text += fmt.Sprintf(" It parked the %d decisions that waited on the user already.", len(waiting))
	}
	return text, nil
}

func (d *daemon) comeBackLocked(caller string) (string, error) {
	if d.away == nil {
		return "", errors.New("away mode is off")
	}
	list := parkedList(d.parked)
	text := "Away mode is off. Nothing was parked."
	if list != "" {
		text = "Away mode is off. While the user was away, bosun parked these, the one whose approval unblocks the most first:\n\n" + list
	}
	d.away, d.parked = nil, nil
	d.broadcastLocked(Event{Kind: "back"})
	d.logLocked(Entry{Kind: "back", By: role(caller), From: caller, Text: text})
	log.Print("away mode off")
	if list == "" {
		d.saveLocked()
		return text, nil
	}
	// The desk that turned it off reads the list in its tool's result.
	if desk := d.deskLocked(); caller != deskNode && desk != nil {
		d.queueLocked(desk, Event{Kind: "back", Text: list})
	}
	d.openLocked(&Decision{Kind: "notice", Node: masterNode, Holder: "user", Note: text})
	return text, nil
}

// byID orders decision ids, which count up from 1, by number.
func byID(a, b string) int {
	x, _ := strconv.Atoi(a)
	y, _ := strconv.Atoi(b)
	return cmp.Compare(x, y)
}

// parkedList shows what was parked, one group to each request that several sessions or several tries share, the group whose approval unblocks the most first.
// A push comes first, since what the fleet built since waits on it, then the larger groups, then permissions before questions before notices, then the oldest.
func parkedList(parked []Parked) string {
	type group struct {
		items []Parked
		push  bool
		rank  int
	}
	var groups []*group
	byKey := map[string]*group{}
	for _, p := range parked {
		dec := p.Decision
		// An escalation's note is the master's, so it does not tell requests apart.
		key := strings.Join([]string{dec.Kind, dec.Tool, compact(dec.Input), dec.Question}, "\x00")
		if dec.Kind == "notice" {
			key += "\x00" + dec.Note
		}
		g := byKey[key]
		if g == nil {
			g = &group{push: isPush(dec), rank: slices.Index([]string{"permission", "question", "notice"}, dec.Kind)}
			byKey[key] = g
			groups = append(groups, g)
		}
		g.items = append(g.items, p)
	}
	slices.SortStableFunc(groups, func(a, b *group) int {
		return cmp.Or(compareBool(b.push, a.push), cmp.Compare(len(b.items), len(a.items)), cmp.Compare(a.rank, b.rank))
	})
	var b strings.Builder
	for i, g := range groups {
		var nodes, ids []string
		for _, p := range g.items {
			if !slices.Contains(nodes, p.Decision.Node) {
				nodes = append(nodes, p.Decision.Node)
			}
			ids = append(ids, p.Decision.ID)
		}
		fmt.Fprintf(&b, "%d. %s, at %s (%s): %s\n", i+1, strings.Join(nodes, ", "), g.items[0].At.Local().Format("15:04"), strings.Join(ids, ", "), parkedLine(g.items[0]))
		fmt.Fprintf(&b, "   Parked because %s.\n", g.items[0].Why)
		if g.push {
			b.WriteString("   A push: the work since may wait on it.\n")
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// compareBool orders false before true.
func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// isPush reports whether dec runs git push anywhere in its script.
func isPush(dec Decision) bool {
	if dec.Kind != "permission" || dec.Tool != "Bash" {
		return false
	}
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(dec.Input, &in)
	return pushes(in.Command)
}

// pushes walks script as the gate's command and argv do, finding git behind wrappers such as sudo or env and inside sh -c or eval.
func pushes(script string) bool {
	for _, c := range simpleCommands(script) {
		for i, w := range c.argv {
			if filepath.Base(w) == "git" {
				if _, sub, _, _ := gitCommand(c.argv[i+1:]); sub == "push" {
					return true
				}
			}
			if s, ok := inner(c.argv, i); ok {
				if pushes(s) {
					return true
				}
				break
			}
		}
	}
	return false
}

// awayLine is what heads each batch of events the master gets while the user is away.
func awayLine(a *Away) string {
	s := "bosun: away mode is on: the user went away at " + a.Since.Local().Format("Jan 2 15:04")
	if a.Words != "" {
		s += fmt.Sprintf(", saying %q", a.Words)
	}
	return s + ". Escalate and notify park what they get for the user's return, and next_event says how to work until then."
}
