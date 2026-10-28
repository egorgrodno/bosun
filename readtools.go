package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// LogQuery picks the log entries a log request returns: each field left empty matches every entry.
// Since is an RFC 3339 time, or a duration back from now such as 2h. Node matches the session an entry is about or that acted. By matches who acted, which for a settled entry is who settled the decision.
type LogQuery struct {
	Since       string `json:"since,omitempty"`
	Node        string `json:"node,omitempty"`
	Kind        string `json:"kind,omitempty"`
	By          string `json:"by,omitempty"`
	ReachedUser bool   `json:"reachedUser,omitempty"`
	// Limit keeps the latest entries that match, logLimit when 0.
	Limit int `json:"limit,omitempty"`
}

// logLimit is how many entries a log request returns when it names no limit.
const logLimit = 50

// scopeLocked returns the nodes caller may read: a worker's own subtree, itself and its descendants. It returns nil for the master, the desk and a frontend, which read everything.
func (d *daemon) scopeLocked(caller string) map[string]bool {
	if caller == "" || !isWorker(caller) {
		return nil
	}
	in := map[string]bool{caller: true}
	// A node's parent may come later in the map, so repeat until no node joins.
	for grew := true; grew; {
		grew = false
		for name, n := range d.nodes {
			if !in[name] && in[n.Parent] {
				in[name], grew = true, true
			}
		}
	}
	return in
}

// checkReadLocked returns an error when caller may not read session name.
func (d *daemon) checkReadLocked(caller, name string) error {
	if in := d.scopeLocked(caller); in != nil && !in[name] {
		return errOutOfScope(name)
	}
	return nil
}

// errOutOfScope tells a worker that name lies outside its subtree.
func errOutOfScope(name string) error {
	return fmt.Errorf("no session %q in your subtree: you can read yourself and your descendants", name)
}

// statusFor is the snapshot that caller may read: all of it, or for a worker the part about its subtree, with the worker as the root of its tree.
func (d *daemon) statusFor(caller string) Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	resp := d.snapshotLocked()
	in := d.scopeLocked(caller)
	if in == nil {
		return resp
	}
	resp.Sessions = slices.DeleteFunc(resp.Sessions, func(n Session) bool { return !in[n.Node] })
	for i := range resp.Sessions {
		if resp.Sessions[i].Node == caller {
			resp.Sessions[i].Parent = ""
		}
	}
	resp.Decisions = slices.DeleteFunc(resp.Decisions, func(dec Decision) bool { return !in[dec.Node] })
	resp.Messages = slices.DeleteFunc(resp.Messages, func(e Event) bool { return !in[e.Node] && !in[e.From] })
	resp.Answered = slices.DeleteFunc(resp.Answered, func(a Answered) bool { return !in[a.Decision.Node] })
	resp.Grants = nil
	resp.Log = slices.DeleteFunc(resp.Log, func(e Entry) bool { return !inScope(in, e) })
	return resp
}

// inScope reports whether log entry e concerns a session in the set in.
func inScope(in map[string]bool, e Entry) bool {
	return in[e.Node] || in[e.From] || e.Decision != nil && in[e.Decision.Node]
}

// readLog returns the entries of the fleet's log that caller may read and q matches, oldest first, the latest q.Limit of them.
func (d *daemon) readLog(caller string, q LogQuery) ([]Entry, error) {
	var since time.Time
	if q.Since != "" {
		var err error
		if since, err = parseSince(q.Since, time.Now()); err != nil {
			return nil, err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	in := d.scopeLocked(caller)
	if q.Node != "" && in != nil && !in[q.Node] {
		return nil, errOutOfScope(q.Node)
	}
	var out []Entry
	for _, e := range d.logged {
		switch {
		case e.At.Before(since):
		case in != nil && !inScope(in, e):
		case q.Node != "" && e.Node != q.Node && e.From != q.Node:
		case q.Kind != "" && e.Kind != q.Kind:
		case q.By != "" && e.By != q.By:
		case q.ReachedUser && !reachedUser(e):
		default:
			out = append(out, e)
		}
	}
	limit := cmp.Or(q.Limit, logLimit)
	return out[max(0, len(out)-limit):], nil
}

// activityReport shows what session name, or with no name every session caller may read, is doing: its latest activity and the tool calls still open.
func (d *daemon) activityReport(caller, name string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var nodes []*Session
	if name != "" {
		n := d.nodes[name]
		if n == nil {
			return "", fmt.Errorf("no session %q", name)
		}
		if err := d.checkReadLocked(caller, name); err != nil {
			return "", err
		}
		nodes = []*Session{n}
	} else {
		in := d.scopeLocked(caller)
		for _, n := range d.sortedLocked() {
			if in == nil || in[n.Node] {
				nodes = append(nodes, n)
			}
		}
	}
	var b strings.Builder
	for _, n := range nodes {
		fmt.Fprintf(&b, "%s is %s", n.Node, sessionState(n.public()))
		if n.Turn {
			b.WriteString(", in its turn")
		}
		b.WriteString(".\n")
		if line := activityLine(n.Activity); line != "" {
			fmt.Fprintf(&b, "  latest, %s ago: %s\n", time.Since(n.Activity.At).Round(time.Second), clip(line, 200))
		}
		// pre holds the calls that began and did not end, including one that waits at a permission prompt.
		calls := make([]string, 0, len(n.pre))
		for _, c := range n.pre {
			calls = append(calls, fmt.Sprintf("  open call: %s %s", c.tool, clip(c.input, 200)))
		}
		slices.Sort(calls)
		for _, c := range calls {
			fmt.Fprintln(&b, c)
		}
	}
	return b.String(), nil
}

// mcpConvLimit is how many entries the conversation tool returns when it names no limit, fewer than the command line's, since the result rides in the session's context.
const mcpConvLimit = 20

// conversation asks the daemon for a session's conversation and returns it as a JSON line, without the bookkeeping entries.
func (s *mcpServer) conversation(ctx context.Context, req Request) (string, error) {
	var line ConvLine
	if err := exchange(ctx, req, &line); err != nil {
		return "", err
	}
	if line.Error != "" {
		return "", errors.New(line.Error)
	}
	line.Entries = slices.DeleteFunc(line.Entries, func(e Said) bool { return e.Kind == "meta" })
	data, err := json.Marshal(line)
	return string(data), err
}

// logText shows log entries for a session: a line each, or with full as JSON.
func logText(entries []Entry, full bool) string {
	if len(entries) == 0 {
		return "No entries match."
	}
	if full {
		data, _ := json.MarshalIndent(entries, "", "  ")
		return string(data)
	}
	var b strings.Builder
	for _, e := range entries {
		route := ""
		if e.Route != "" {
			route = " [" + e.Route + "]"
		}
		fmt.Fprintf(&b, "%d %s %s%s\n", e.Seq, e.At.Local().Format("2006-01-02 15:04:05"), logLine(e), route)
	}
	return b.String()
}
