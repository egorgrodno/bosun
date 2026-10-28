package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"math"
	"os"
	"path/filepath"
	"time"
)

// keptLog is how many of the log's latest entries the daemon keeps and a snapshot carries.
const keptLog = 200

// logRotate is how many entries log.jsonl holds before the daemon moves it to log.1.jsonl, so that the two files hold a run of days.
var logRotate = 10000

// logInputCut bounds each string of a logged decision's input, such as the content of a Write, which every snapshot carries.
const logInputCut = 2000

func (d *daemon) logPath() string {
	return filepath.Join(d.stateDir, "log.jsonl")
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

// oldLogPath is where log.jsonl goes once it holds logRotate entries.
func oldLogPath(stateDir string) string {
	return filepath.Join(stateDir, "log.1.jsonl")
}

// rotateLog moves log.jsonl to log.1.jsonl, in place of the one before, and starts log.jsonl anew with the entries the daemon keeps.
// The two files then share those entries, which a reader of both tells apart by Seq.
func (d *daemon) rotateLog() error {
	if err := os.Rename(d.logPath(), oldLogPath(d.stateDir)); err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, e := range d.logged {
		line, _ := json.Marshal(e)
		buf.Write(append(line, '\n'))
	}
	d.logLines = len(d.logged)
	return writeDurably(d.logPath(), buf.Bytes())
}

// loadLog reads the latest entries of log.jsonl, and rotates it once it holds logRotate entries.
func (d *daemon) loadLog() error {
	keep := func(e Entry) {
		d.logged = append(d.logged, e)
		d.logged = d.logged[max(0, len(d.logged)-keptLog):]
		d.logSeq = max(d.logSeq, e.Seq)
	}
	lines, cut, err := readLog(d.logPath(), keep)
	if errors.Is(err, fs.ErrNotExist) {
		// A crash between a rotation's two steps leaves only log.1.jsonl, whose Seq the next entry must pass.
		if _, _, err := readLog(oldLogPath(d.stateDir), keep); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	d.logLines = lines
	switch {
	case lines >= logRotate:
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

// readLog hands each entry of the log file at path to fn, and returns how many lines it holds and whether its last line lacks its newline. A crash may cut that line short, and readLog skips any line that is no entry.
func readLog(path string, fn func(Entry)) (lines int, cut bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, len(data)+1)
	for sc.Scan() {
		lines++
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			fn(e)
		}
	}
	return lines, len(data) > 0 && data[len(data)-1] != '\n', sc.Err()
}

// readLogs hands fn every entry that log.1.jsonl and log.jsonl in stateDir hold, oldest first, each once.
func readLogs(stateDir string, fn func(Entry)) error {
	last := 0
	for _, path := range []string{oldLogPath(stateDir), filepath.Join(stateDir, "log.jsonl")} {
		_, _, err := readLog(path, func(e Entry) {
			if e.Seq > last {
				last = e.Seq
				fn(e)
			}
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
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

// outcome is how a decision was settled, for the log: by whom, the answer where the session's own prompt gave it, for a withdrawal the cause, which completes "withdrawn when …", and the user's words the desk acted on.
type outcome struct {
	by       string
	behavior string
	cause    string
	words    string
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
	e := Entry{Kind: "settled", By: o.by, Node: dec.Node, Decision: dec, Words: o.words}
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
