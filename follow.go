package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// convLimit is how many entries a conversation or follow request returns when it names no limit.
const convLimit = 100

// followPoll is how often a follow reads the transcript for new records.
var followPoll = 250 * time.Millisecond

// conversationID returns the Claude Code session id of session name's current conversation, and the short id the supervisor knows the session by.
func (d *daemon) conversationID(name string) (sid, short string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := d.nodes[name]
	if n == nil {
		return "", "", fmt.Errorf("no session %q", name)
	}
	return cmp.Or(n.Conversation, n.SessionID), n.ID, nil
}

// conversation returns up to limit entries of session name's conversation before the one with ID before, or its last ones.
func (d *daemon) conversation(caller, name, before string, limit int) ConvLine {
	line := ConvLine{Type: "result"}
	if limit <= 0 {
		limit = convLimit
	}
	d.mu.Lock()
	in := d.scopeLocked(caller)
	d.mu.Unlock()
	if in != nil && !in[name] {
		line.Error = errOutOfScope(name).Error()
		return line
	}
	sid, _, err := d.conversationID(name)
	if err == nil {
		var c *conversation
		if c, err = readConversation(sid, d.senderOf(name)); err == nil {
			line.Session = sid
			line.Entries, line.More, err = page(c.entries, before, limit)
		}
	}
	if err != nil {
		line.Error = err.Error()
	}
	return line
}

// follow sends session name's conversation until ctx ends or the session is gone: a reset and the last limit entries, then each entry as it lands or changes.
// A new conversation, as a clear or a switch begins, sends a reset and its own last entries. Beside them go progress lines from the supervisor, and a suggestion line each time the reply Claude Code suggests changes.
func (d *daemon) follow(ctx context.Context, cancel context.CancelFunc, o *out, name string, limit int) {
	defer cancel()
	if limit <= 0 {
		limit = convLimit
	}
	var (
		sid string
		c   *conversation
		f   *os.File
		// read is how far into f the whole lines reach, paged whether the reset's page went out, and first the place of the page's first entry.
		read        int64
		paged       bool
		first       int
		progressing bool
		// suggested is the suggestion the frontend holds since the reset.
		suggested string
	)
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	poll := followPoll
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		next, short, err := d.conversationID(name)
		if err != nil {
			o.send(ConvLine{Type: "result", Error: err.Error()})
			return
		}
		if next != "" && (next != sid || f != nil && rewritten(f, read)) {
			if f != nil {
				f.Close()
				f = nil
			}
			sid, c, read, paged = next, newConversation(d.senderOf(name)), 0, false
			suggested = ""
			o.send(ConvLine{Type: "reset", Session: sid})
		}
		if c != nil && f == nil {
			if path, err := transcriptPath(sid); err == nil {
				f, _ = os.Open(path)
			}
		}
		if f != nil {
			if _, err := f.Seek(read, io.SeekStart); err == nil {
				n, _ := c.read(f)
				read += n
			}
		}
		switch {
		case c == nil:
		case !paged:
			line := ConvLine{Type: "entries"}
			line.Entries, line.More, _ = page(c.entries, "", limit)
			c.changed = nil
			o.send(line)
			paged, first = true, len(c.entries)-len(line.Entries)
		default:
			// A change to an entry before the page has no entry at the frontend to replace. A conversation request reads it as it is now.
			if es := c.flush(first); len(es) > 0 {
				o.send(ConvLine{Type: "entries", Entries: es})
			}
		}
		if s := jobSuggestion(short); paged && s != suggested {
			suggested = s
			o.send(Suggestion{Type: "suggestion", Name: name, Text: s})
		}
		if paged && !progressing {
			progressing = true
			go d.progress(ctx, o, name, poll, resubscribeWait)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Suggestion is a "suggestion" line of a follow: the reply Claude Code suggests to what session Name's turn ended asking, empty once it suggests none.
type Suggestion struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Text string `json:"text"`
}

// jobSuggestion reads the reply that Claude Code's job state for session short suggests, or "" for none.
// Claude Code's agents view shows suggestedReply only while the session is blocked outside a question dialog, and so does bosun.
func jobSuggestion(short string) string {
	data, err := os.ReadFile(filepath.Join(claudeDir(), "jobs", short, "state.json"))
	if err != nil {
		return ""
	}
	var st struct {
		Tempo          string          `json:"tempo"`
		Block          json.RawMessage `json:"block"`
		SuggestedReply string          `json:"suggestedReply"`
	}
	if json.Unmarshal(data, &st) != nil || st.Tempo != "blocked" || len(st.Block) > 0 && string(st.Block) != "null" {
		return ""
	}
	return st.SuggestedReply
}

// rewritten reports whether the transcript that f holds, read up to read, was cut short or replaced, so that its entries must be read again.
func rewritten(f *os.File, read int64) bool {
	was, err := f.Stat()
	if err != nil {
		return true
	}
	now, err := os.Stat(f.Name())
	return err != nil || !os.SameFile(was, now) || now.Size() < read
}
