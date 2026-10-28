package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
)

// runLog prints the fleet's log as log.jsonl and log.1.jsonl hold it, which needs no daemon.
func runLog(args []string) error {
	flags := flag.NewFlagSet("log", flag.ExitOnError)
	reached := flags.Bool("reached-user", false, "group the decisions that reached the user by the rule that sent them there")
	since := flags.String("since", "", "only entries since `T`: a duration such as 24h, a date, or a time such as 2026-10-27T09:00")
	asJSON := flags.Bool("json", false, "print the entries as JSON lines")
	_ = flags.Parse(args)
	if flags.NArg() != 0 {
		return errors.New(usage)
	}
	from, err := parseSince(*since, time.Now())
	if err != nil {
		return err
	}
	var entries []Entry
	err = readLogs(stateDir(), func(e Entry) {
		if !e.At.Before(from) && (!*reached || e.Kind == "settled" && reachedUser(e)) {
			entries = append(entries, e)
		}
	})
	if err != nil {
		return err
	}
	switch {
	case *asJSON:
		enc := json.NewEncoder(os.Stdout)
		for _, e := range entries {
			if err := enc.Encode(e); err != nil {
				return err
			}
		}
	case *reached:
		printReached(os.Stdout, entries)
	default:
		for _, e := range entries {
			fmt.Printf("%s %s\n", e.At.Local().Format("01-02"), logLine(e))
		}
	}
	return nil
}

// parseSince reads --since as a duration back from now, an RFC 3339 time, or a local date or time.
func parseSince(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02", "2006-01-02T15:04", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("--since %q is neither a duration, a date nor a time", s)
}

// reachedUser reports whether log entry e is about a decision that reached the user.
func reachedUser(e Entry) bool {
	r := entryRoute(e)
	return r != "" && r != routeMaster
}

// entryRoute is the route of the decision e is about, or "". An entry from before routes were logged gets it from its decision.
func entryRoute(e Entry) string {
	if e.Route != "" || e.Decision == nil {
		return e.Route
	}
	switch e.Kind {
	case "decision", "escalated", "settled":
		return routeOf(e.Decision)
	}
	return ""
}

// ruleOf is what printReached groups e under: its rule, or for an entry without one, its decision's reserved reason or kind.
func ruleOf(e Entry) string {
	switch {
	case e.Rule != "":
		return e.Rule
	case e.Decision.Reserved != "":
		return "reserved: " + e.Decision.Reserved
	}
	return e.Decision.Kind
}

// printReached prints the settled decisions in entries grouped by rule, the most frequent first, each with its routes, how they were settled and an example.
func printReached(w io.Writer, entries []Entry) {
	groups := map[string][]Entry{}
	var rules []string
	for _, e := range entries {
		r := ruleOf(e)
		if groups[r] == nil {
			rules = append(rules, r)
		}
		groups[r] = append(groups[r], e)
	}
	slices.SortStableFunc(rules, func(a, b string) int {
		return cmp.Or(len(groups[b])-len(groups[a]), strings.Compare(a, b))
	})
	fmt.Fprintf(w, "%d decisions reached you\n", len(entries))
	for _, r := range rules {
		g := groups[r]
		fmt.Fprintf(w, "\n%s: %d\n", r, len(g))
		if g[0].Rule != "" {
			fmt.Fprintf(w, "  why: %s\n", tally(g, func(e Entry) string { return e.Decision.Reserved }))
		}
		fmt.Fprintf(w, "  routes: %s\n", tally(g, entryRoute))
		fmt.Fprintf(w, "  settled: %s\n", tally(g, settledHow))
		if s := waits(g); s != "" {
			fmt.Fprintf(w, "  waited: %s\n", s)
		}
		// The latest is the likeliest to show the rule as it stands.
		e := g[len(g)-1]
		var what string
		switch dec := *e.Decision; dec.Kind {
		case "permission":
			what = dec.Tool + ": " + inputDetail(dec.Input)
		case "notice":
			what = dec.Note
		default:
			what = describe(dec)
		}
		fmt.Fprintf(w, "  e.g. %s %s, %s: %s\n", e.At.Local().Format("01-02 15:04"), e.Decision.ID, e.Node, clip(squash(what), 160))
		if e.Match != "" {
			fmt.Fprintf(w, "  matched: %s\n", e.Match)
		}
	}
}

// settledHow says who settled e and how, as "you allowed".
func settledHow(e Entry) string {
	who := e.By
	if who == "user" {
		who = "you"
	}
	how := given(Answered{Behavior: e.Behavior})
	switch {
	case e.Withdrawn:
		how = "withdrawn"
	case e.Decision.Kind == "notice":
		how = "dismissed"
	}
	return who + " " + how
}

// waits says how long the decisions in entries waited, as the median and the longest, or returns "" when no entry says.
func waits(entries []Entry) string {
	var ds []time.Duration
	for _, e := range entries {
		if e.Waited > 0 {
			ds = append(ds, time.Duration(e.Waited*float64(time.Second)).Round(time.Second))
		}
	}
	if len(ds) == 0 {
		return ""
	}
	slices.Sort(ds)
	return fmt.Sprintf("median %s, longest %s", ds[len(ds)/2], ds[len(ds)-1])
}

// tally counts entries by what key gives, as "userOnly 5, escalated 1", the most frequent first.
func tally(entries []Entry, key func(Entry) string) string {
	counts := map[string]int{}
	var keys []string
	for _, e := range entries {
		k := key(e)
		if counts[k] == 0 {
			keys = append(keys, k)
		}
		counts[k]++
	}
	slices.SortStableFunc(keys, func(a, b string) int { return counts[b] - counts[a] })
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, counts[k])
	}
	return strings.Join(parts, ", ")
}
