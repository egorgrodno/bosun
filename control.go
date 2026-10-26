package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// errNoControl wraps every reason the supervisor protocol is not as bosun knows it, on which an action falls back to stopping and resuming the session.
var errNoControl = errors.New("the supervisor's protocol is not the one bosun speaks")

// controlWait bounds each exchange with the supervisor, and keysSettle how long an attach stays open after its keys, since a lone Esc reads as a key only once no more bytes follow it.
var (
	controlWait = 5 * time.Second
	keysSettle  = 500 * time.Millisecond
)

// The size an attach reports for its terminal, which the supervisor requires.
const (
	attachCols = 120
	attachRows = 40
)

// controlKey reads the key that Claude Code's supervisor takes as auth from a client acting as the user.
// The user allowed only the daemon to read it, and only for an action the user takes from a frontend on one of bosun's own sessions. It must reach no session, frontend, log or error.
func controlKey() (string, error) {
	path := filepath.Join(claudeDir(), "daemon", "control.key")
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("%w: Claude Code keeps no control key", errNoControl)
	case err != nil:
		return "", fmt.Errorf("%w: reading Claude Code's control key failed", errNoControl)
	case !fi.Mode().IsRegular() || fi.Size() > 4096:
		return "", fmt.Errorf("%w: Claude Code's control key is not a small regular file", errNoControl)
	}
	data, err := os.ReadFile(path)
	key := strings.TrimSpace(string(data))
	if err != nil || key == "" {
		return "", fmt.Errorf("%w: reading Claude Code's control key failed", errNoControl)
	}
	return key, nil
}

// controlReply is the supervisor's answer to a request.
type controlReply struct {
	OK    bool   `json:"ok"`
	Op    string `json:"op"`
	Error string `json:"error"`
	Code  string `json:"code"`
}

// control is a session of the supervisor protocol, for one action of the user's on one of bosun's sessions.
type control struct {
	key string
}

// newControl checks that the supervisor speaks supervisorProto, and only then reads the control key.
func newControl(ctx context.Context) (*control, error) {
	if err := checkProto(ctx); err != nil {
		return nil, fmt.Errorf("%w: %v", errNoControl, err)
	}
	key, err := controlKey()
	if err != nil {
		return nil, err
	}
	return &control{key: key}, nil
}

// send sends req and reads the line the supervisor answers, leaving the connection open for what follows an attach.
func (c *control) send(req map[string]any) (net.Conn, controlReply, error) {
	var r controlReply
	ctx, cancel := context.WithTimeout(context.Background(), controlWait)
	defer cancel()
	conn, lines, err := controlRequest(ctx, req)
	if err != nil {
		return nil, r, fmt.Errorf("%w: its control socket does not answer", errNoControl)
	}
	_ = conn.SetDeadline(time.Now().Add(controlWait))
	reply, err := lines.ReadBytes('\n')
	if err == nil {
		err = json.Unmarshal(reply, &r)
	}
	if err != nil {
		conn.Close()
		return nil, r, fmt.Errorf("%w: its answer is not a line of JSON", errNoControl)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, r, nil
}

// refusal turns the supervisor's refusal into an error, which never quotes the key: a code bosun does not expect means the protocol changed.
func (c *control) refusal(op string, r controlReply) error {
	msg := strings.ReplaceAll(r.Error, c.key, "…")
	switch r.Code {
	case "EPROTO", "EUNKNOWN", "EAUTH", "":
		return fmt.Errorf("%w: it refused %s: %s (%s)", errNoControl, op, msg, r.Code)
	}
	return fmt.Errorf("Claude Code's supervisor refused %s: %s (%s)", op, msg, r.Code)
}

// reply types text into session short as the user, as its terminal would take it.
func (c *control) reply(short, text string) error {
	conn, r, err := c.send(map[string]any{"proto": supervisorProto, "op": "reply", "short": short, "text": text, "auth": c.key})
	if err != nil {
		return err
	}
	conn.Close()
	if !r.OK || r.Op != "reply" {
		return c.refusal("the reply", r)
	}
	return nil
}

// keys presses keys in session short's terminal, through an attach that turns the connection into the terminal once the supervisor answers.
func (c *control) keys(short, keys string) error {
	conn, r, err := c.send(map[string]any{"proto": supervisorProto, "op": "attach", "short": short, "auth": c.key, "cols": attachCols, "rows": attachRows})
	if err != nil {
		return err
	}
	defer conn.Close()
	if !r.OK || r.Op != "attach" {
		return c.refusal("the attach", r)
	}
	// The supervisor drops an attacher whose output it cannot write.
	go func() { _, _ = io.Copy(io.Discard, conn) }()
	if _, err := conn.Write([]byte(keys)); err != nil {
		return fmt.Errorf("pressing keys in %s's terminal failed: %w", short, err)
	}
	time.Sleep(keysSettle)
	return nil
}
