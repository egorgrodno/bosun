package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Request is a line a client sends the daemon. Node names the calling session, and is empty for a frontend.
type Request struct {
	Op    string          `json:"op"`
	Node  string          `json:"node,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Repo  string          `json:"repo,omitempty"`
	Model string          `json:"model,omitempty"`
	Text  string          `json:"text,omitempty"`
	Hook  json.RawMessage `json:"hook,omitempty"`
}

// Response is a line the daemon sends: a "result" for each request, and on a watch a "snapshot" followed by "event"s.
type Response struct {
	Type      string     `json:"type"`
	Error     string     `json:"error,omitempty"`
	Behavior  string     `json:"behavior,omitempty"`
	Text      string     `json:"text,omitempty"`
	Session   *Session   `json:"session,omitempty"`
	Events    []Event    `json:"events,omitempty"`
	Event     *Event     `json:"event,omitempty"`
	Sessions  []Session  `json:"sessions,omitempty"`
	Decisions []Decision `json:"decisions,omitempty"`
	Messages  []Event    `json:"messages,omitempty"`
}

// Event is what the master or a frontend is told. The master gets decision, turn_end, exit, tell and message; a frontend gets decision, settled, sessions and message.
// A message is what session From told session Node through Claude Code's SendMessage.
type Event struct {
	Kind     string    `json:"kind"`
	Node     string    `json:"node,omitempty"`
	From     string    `json:"from,omitempty"`
	ID       string    `json:"id,omitempty"`
	Text     string    `json:"text,omitempty"`
	Decision *Decision `json:"decision,omitempty"`
	Sessions []Session `json:"sessions,omitempty"`
}

// Decision is a permission or a question waiting on its holder, the master or the user, or a notice from the master waiting on the user. A notice's text is its Note.
type Decision struct {
	ID       string          `json:"id"`
	Kind     string          `json:"kind"`
	Node     string          `json:"node"`
	Holder   string          `json:"holder"`
	Reserved string          `json:"reserved,omitempty"`
	Tool     string          `json:"tool,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	Question string          `json:"question,omitempty"`
	Note     string          `json:"note,omitempty"`

	toolUseID string
	answer    chan answer
}

type answer struct {
	behavior  string
	text      string
	withdrawn bool
}

// Session is a node of the tree. Status and State come from `claude agents --json --all`, with Status "stopped" while it lists the session without a process, and "exited" once it lists the session no more.
// An empty Model is the user's default. Killed marks a worker the user stopped, which the master may not resume.
// A worker's branch began at the HEAD of BaseBranch, checked out in Base, which merge fast-forwards.
type Session struct {
	Node      string `json:"node"`
	Parent    string `json:"parent,omitempty"`
	Repo      string `json:"repo,omitempty"`
	Dir       string `json:"dir"`
	ID        string `json:"id,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	Model     string `json:"model,omitempty"`
	Depth     int    `json:"depth"`
	Status    string `json:"status"`
	State     string `json:"state,omitempty"`
	Killed    bool   `json:"killed,omitempty"`
	// LimitedUntil is when the usage limit that stopped the session resets, and the daemon resumes it.
	LimitedUntil *time.Time `json:"limitedUntil,omitempty"`

	Base       string `json:"base,omitempty"`
	BaseBranch string `json:"baseBranch,omitempty"`

	pre map[string]preCall
	// held keeps poll, resume, merge and kill off the session while one of them stops or resumes it.
	held bool
}

func socketPath() string {
	if p := os.Getenv("BOSUN_SOCKET"); p != "" {
		return p
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "bosun.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("bosun-%d.sock", os.Getuid()))
}

// call sends req to the daemon and waits for its result, giving up when ctx ends.
func call(ctx context.Context, req Request) (Response, error) {
	var resp Response
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath())
	if err != nil {
		return resp, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return resp, err
	}
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		if ctx.Err() != nil {
			return resp, ctx.Err()
		}
		return resp, err
	}
	if resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}
