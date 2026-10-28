package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// keptLog is how many of the log's latest entries the daemon keeps and a snapshot carries.
const keptLog = 200

// logRotate is how many entries log.jsonl holds before the daemon moves it to log.1.jsonl, so that the two files hold a run of days.
var logRotate = 10000

// logInputCut bounds each string of a logged decision's input, such as the content of a Write, which every snapshot carries.
const logInputCut = 2000

func (d *daemon) logPath() string {
	return logFile(d.stateDir)
}

// logLocked adds e to the fleet's log, shows it to the frontends and appends it to log.jsonl.
func (d *daemon) logLocked(e Entry) {
	d.logSeq++
	e.Seq, e.At = d.logSeq, time.Now()
	if e.Decision != nil {
		c := *e.Decision
		c.Input = cutInput(c.Input)
		e.Decision = &c
	}
	d.logged = append(d.logged, e)
	d.logged = d.logged[max(0, len(d.logged)-keptLog):]
	d.broadcastLocked(Event{Kind: "log", Entry: &e})
	if d.stopping {
		return
	}
	line, _ := json.Marshal(e)
	f, err := os.OpenFile(d.logPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err == nil {
		_, err = f.Write(append(line, '\n'))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err == nil {
		if d.logLines++; d.logLines >= logRotate {
			err = d.rotateLog()
		}
	}
	if err != nil {
		log.Printf("appending to the log: %v", err)
	}
}

// logFile is the log in stateDir, and oldLogFile where it goes once it holds logRotate entries.
func logFile(stateDir string) string    { return filepath.Join(stateDir, "log.jsonl") }
func oldLogFile(stateDir string) string { return filepath.Join(stateDir, "log.1.jsonl") }

// rotateLog moves log.jsonl to log.1.jsonl, in place of the one before. The next entry starts a new log.jsonl.
func (d *daemon) rotateLog() error {
	d.logLines = 0
	return os.Rename(d.logPath(), oldLogFile(d.stateDir))
}

// loadLog reads the latest entries of log.jsonl, and of log.1.jsonl where log.jsonl holds fewer, and rotates log.jsonl once it holds logRotate entries.
func (d *daemon) loadLog() error {
	data, err := os.ReadFile(d.logPath())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	d.logged = lastEntries(data, keptLog)
	if len(d.logged) < keptLog {
		old, err := os.ReadFile(oldLogFile(d.stateDir))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		d.logged = append(lastEntries(old, keptLog-len(d.logged)), d.logged...)
	}
	for _, e := range d.logged {
		d.logSeq = max(d.logSeq, e.Seq)
	}
	cut := len(data) > 0 && data[len(data)-1] != '\n'
	d.logLines = bytes.Count(data, []byte("\n"))
	if cut {
		d.logLines++
	}
	switch {
	case d.logLines >= logRotate:
		return d.rotateLog()
	case cut:
		// The next entry would otherwise join the line a crash cut short.
		f, err := os.OpenFile(d.logPath(), os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return err
		}
		_, err = f.Write([]byte{'\n'})
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	}
	return nil
}

// lastEntries returns the latest n entries of a log file's data, oldest first. It skips any line that is no entry, as a crash may cut the last one short, and parses no line older than it needs.
func lastEntries(data []byte, n int) []Entry {
	lines := bytes.Split(data, []byte("\n"))
	var out []Entry
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		var e Entry
		if json.Unmarshal(lines[i], &e) == nil {
			out = append(out, e)
		}
	}
	slices.Reverse(out)
	return out
}

// readLogs returns every entry that log.1.jsonl and log.jsonl in stateDir hold, oldest first.
func readLogs(stateDir string) ([]Entry, error) {
	var out []Entry
	for _, path := range []string{oldLogFile(stateDir), logFile(stateDir)} {
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		out = append(out, lastEntries(data, math.MaxInt)...)
	}
	return out, nil
}

// cutInput cuts each long string in a tool call's input short.
func cutInput(input json.RawMessage) json.RawMessage {
	if len(input) <= logInputCut {
		return input
	}
	var v any
	if json.Unmarshal(input, &v) != nil {
		return input
	}
	out, err := json.Marshal(cutStrings(v))
	if err != nil {
		return input
	}
	return out
}

func cutStrings(v any) any {
	switch v := v.(type) {
	case string:
		if r := []rune(v); len(r) > logInputCut {
			return string(r[:logInputCut]) + "…"
		}
	case map[string]any:
		for k, x := range v {
			v[k] = cutStrings(x)
		}
	case []any:
		for i, x := range v {
			v[i] = cutStrings(x)
		}
	}
	return v
}

// role is who a request from node is, for the log's By.
func role(node string) string {
	switch {
	case node == "":
		return "user"
	case isWorker(node):
		return "worker"
	}
	return node
}

// outcome is how a decision was settled, for the log: by whom, the answer where the session's own prompt gave it, for a withdrawal the cause, which completes "withdrawn when …", and the user's words the desk acted on, or the standing grant that settled it.
type outcome struct {
	by       string
	behavior string
	cause    string
	words    string
	grant    *Grant
}

// The routes by which a decision reaches its holder. Every route but routeMaster reaches the user.
const (
	routeMaster    = "master"
	routeEscalated = "escalated"
	// routeRequester is a decision of the master's or the desk's own, which nobody sits above.
	routeRequester = "requester"
	routeUserOnly  = "userOnly"
	// routePrompt is a notice of a permission that the daemon left to its session's own prompt.
	routePrompt = "prompt"
	routeNotice = "notice"
)

// routeOf says how dec reached its holder. It reads only what state.json and the log keep of dec, so it serves entries of earlier runs too.
func routeOf(dec *Decision) string {
	switch {
	case dec.Kind == "notice" && dec.Prompt:
		return routePrompt
	case dec.Kind == "notice":
		return routeNotice
	case dec.Holder == "master":
		return routeMaster
	case dec.UserOnly:
		return routeUserOnly
	case !isWorker(dec.Node):
		return routeRequester
	}
	// A worker's decision reaches the user otherwise only through the master's escalate.
	return routeEscalated
}

// why records the rule that caught dec, and its route.
func (e *Entry) why(dec *Decision) {
	e.Rule, e.Match, e.Route = dec.rule, dec.match, routeOf(dec)
}

// settledEntry is the log's entry for dec, settled with a as o says.
func settledEntry(dec *Decision, a answer, o outcome) Entry {
	e := Entry{Kind: "settled", By: o.by, Node: dec.Node, Decision: dec, Words: o.words, Grant: o.grant}
	e.why(dec)
	if !dec.opened.IsZero() {
		e.Waited = math.Round(time.Since(dec.opened).Seconds()*10) / 10
	}
	switch o.by {
	case "master", "desk":
		e.From = o.by
	case "session":
		e.From = dec.Node
	}
	switch {
	case dec.Kind == "notice" && a.withdrawn:
		e.Withdrawn, e.Cause = true, o.cause
	case dec.Kind == "notice" && o.behavior != "":
		e.Behavior = o.behavior
	case dec.Kind == "notice":
	case o.behavior != "":
		e.Behavior = o.behavior
	case a.withdrawn:
		e.Withdrawn, e.Cause = true, o.cause
	default:
		e.Behavior, e.Text = a.behavior, a.text
	}
	return e
}
