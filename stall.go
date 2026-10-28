package main

import (
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"
)

// defaultStallAfter is how long a worker's turn goes without a hook, or a decision waits on the user, before the master hears of it.
// A long build is normal, so it is a report and never a kill.
const defaultStallAfter = 10 * time.Minute

// checkStalls queues a stalled event for the master, as of now, for each worker whose turn has run no hook for stallAfter and each decision the user has held for stallAfter.
// A worker's stall goes where its turn_end goes, to its parent or else the master. It is reported once, and again only after a hook has ended the quiet and the worker has gone quiet anew.
// A decision is reported once. The master cannot settle one the user holds, but it can tell the user through notify.
func (d *daemon) checkStalls(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stallAfter <= 0 || d.paused {
		return
	}
	var held []*Decision
	waiting := map[string]bool{}
	for _, dec := range d.decisions {
		waiting[dec.Node] = true
		// A notice is for the user to dismiss, except one that says a session waits in its own prompt.
		if dec.Holder == "user" && dec.Node != masterNode && (dec.Kind != "notice" || dec.Prompt) && !dec.stalled && now.Sub(dec.opened) > d.stallAfter {
			held = append(held, dec)
		}
	}
	slices.SortFunc(held, func(a, b *Decision) int { return a.opened.Compare(b.opened) })
	for _, dec := range held {
		dec.stalled = true
		text := fmt.Sprintf("%s's decision %s has waited on the user for %s. %s", dec.Node, dec.ID, since(now, dec.opened), waitingOn(*dec))
		d.logLocked(Entry{Kind: "stalled", By: "bosun", Node: dec.Node, Text: text})
		d.pushLocked(Event{Kind: "stalled", Node: dec.Node, ID: dec.ID, Text: text})
		log.Printf("decision %s: waited on the user for %s", dec.ID, since(now, dec.opened))
	}
	for _, n := range d.sortedLocked() {
		if !isWorker(n.Node) || !n.Turn || n.Killed || n.Paused || n.held || n.LimitedUntil != nil || waiting[n.Node] || n.Status == "starting" || betweenTurns(n.Status) {
			continue
		}
		// A daemon that restarted in the worker's turn cannot know when its last hook ran.
		last := n.hookAt
		if last.Before(d.began) {
			last = d.began
		}
		if now.Sub(last) <= d.stallAfter || n.stallFrom.Equal(last) {
			continue
		}
		n.stallFrom = last
		d.reportLocked(Event{Kind: "stalled", Node: n.Node, Text: stallText(n, now.Sub(last), now)})
	}
}

// stallText says what worker n did last and which calls are open, which a long build holds open for good reason.
func stallText(n *Session, quiet time.Duration, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "No hook of %s's turn has run for %s, though the turn goes on. A long build or a script that polls may be the reason, so look before you act.", n.Node, since(now, now.Add(-quiet)))
	if line := activityLine(n.Activity); line != "" {
		fmt.Fprintf(&b, "\nLatest activity, %s ago: %s", since(now, n.Activity.At), clip(line, 200))
	}
	calls := make([]string, 0, len(n.pre))
	for _, c := range n.pre {
		calls = append(calls, c.tool+" "+clip(c.input, 200))
	}
	slices.Sort(calls)
	for _, c := range calls {
		fmt.Fprintf(&b, "\nOpen call: %s", c)
	}
	return b.String()
}

// waitingOn says what dec is, and that only the user settles it.
func waitingOn(dec Decision) string {
	what := describe(dec)
	if dec.Kind == "notice" {
		what = "A session waits in its own prompt, which only the user can answer: " + clip(dec.Note, 200)
	}
	return fmt.Sprintf("%s. You cannot settle it, but notify tells the user.", clip(what, 300))
}

// since shows how long before now t was, to the second under a minute and to the minute above.
func since(now, t time.Time) string {
	age := max(now.Sub(t), 0)
	if age < time.Minute {
		return age.Round(time.Second).String()
	}
	return strconv.Itoa(int(age.Round(time.Minute)/time.Minute)) + "m"
}
