package main

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"time"
)

// convLimit is how many entries a conversation or follow request returns when it names no limit.
const convLimit = 100

// followPoll is how often a follow reads the transcript for new records.
var followPoll = 250 * time.Millisecond

// conversationID returns the Claude Code session id of session name's current conversation.
func (d *daemon) conversationID(name string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := d.nodes[name]
	if n == nil {
		return "", fmt.Errorf("no session %q", name)
	}
	return cmp.Or(n.Conversation, n.SessionID), nil
}

// conversation returns up to limit entries of session name's conversation before the one with ID before, or its last ones.
func (d *daemon) conversation(name, before string, limit int) ConvLine {
	line := ConvLine{Type: "result"}
	if limit <= 0 {
		limit = convLimit
	}
	sid, err := d.conversationID(name)
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
// A new conversation, as a clear or a switch begins, sends a reset and its own last entries.
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
		read  int64
		paged bool
		first int
	)
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	tick := time.NewTicker(followPoll)
	defer tick.Stop()
	for {
		next, err := d.conversationID(name)
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
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
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
