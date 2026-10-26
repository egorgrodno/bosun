package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"
)

// keptLog is how many of the log's latest entries the daemon keeps and a snapshot carries.
const keptLog = 200

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
	if err != nil {
		log.Printf("appending to the log: %v", err)
	}
}

// loadLog reads the latest entries of log.jsonl, and rewrites the file with only those once it holds twice as many.
func (d *daemon) loadLog() error {
	data, err := os.ReadFile(d.logPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	lines := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(nil, len(data)+1)
	for sc.Scan() {
		var e Entry
		// A crash may cut the last line short.
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		lines++
		d.logged = append(d.logged, e)
		d.logged = d.logged[max(0, len(d.logged)-keptLog):]
		d.logSeq = max(d.logSeq, e.Seq)
	}
	if lines < 2*keptLog {
		if len(data) > 0 && data[len(data)-1] != '\n' {
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
	var buf bytes.Buffer
	for _, e := range d.logged {
		line, _ := json.Marshal(e)
		buf.Write(append(line, '\n'))
	}
	return writeDurably(d.logPath(), buf.Bytes())
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

// settledEntry is the log's entry for dec, settled with a as o says.
func settledEntry(dec *Decision, a answer, o outcome) Entry {
	e := Entry{Kind: "settled", By: o.by, Node: dec.Node, Decision: dec, Words: o.words}
	switch o.by {
	case "master", "desk":
		e.From = o.by
	case "session":
		e.From = dec.Node
	}
	switch {
	case dec.Kind == "notice" && a.withdrawn:
		e.Withdrawn, e.Cause = true, o.cause
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
