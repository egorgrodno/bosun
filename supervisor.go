package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// supervisorProto is the version of the supervisor's control protocol that bosun speaks. Claude Code 2.1.310 reports 1.
const supervisorProto = 1

// controlSocket returns the path of the control socket of Claude Code's supervisor: in /tmp/cc-daemon-<uid>/, the folder named by the first 8 hex digits of the SHA-256 of the absolute config directory.
var controlSocket = func() (string, error) {
	base := fmt.Sprintf("/tmp/cc-daemon-%d", os.Getuid())
	dir, err := filepath.Abs(claudeDir())
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(dir))
	path := filepath.Join(base, hex.EncodeToString(sum[:])[:8], "control.sock")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	// The derivation comes from the 2.1.310 binary, so a lone socket stands in when it misses.
	if paths, _ := filepath.Glob(filepath.Join(base, "*", "control.sock")); len(paths) == 1 {
		return paths[0], nil
	}
	return "", fmt.Errorf("found no control socket of Claude Code's supervisor for %s", dir)
}

// controlRequest sends req on a connection of its own to the supervisor, and returns the connection and a reader of its reply lines.
func controlRequest(ctx context.Context, req map[string]any) (net.Conn, *bufio.Reader, error) {
	path, err := controlSocket()
	if err != nil {
		return nil, nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, nil, err
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, bufio.NewReaderSize(conn, 64<<10), nil
}

// checkProto pings the supervisor and fails unless it speaks supervisorProto.
func checkProto(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, r, err := controlRequest(ctx, map[string]any{"op": "ping"})
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	line, err := r.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("the supervisor's ping: %w", err)
	}
	var pong struct {
		OK    bool `json:"ok"`
		Proto int  `json:"proto"`
	}
	if err := json.Unmarshal(line, &pong); err != nil || !pong.OK {
		return fmt.Errorf("the supervisor's ping answered %s", strings.TrimSpace(string(line)))
	}
	if pong.Proto != supervisorProto {
		return fmt.Errorf("the supervisor speaks protocol %d, and bosun %d", pong.Proto, supervisorProto)
	}
	return nil
}

// jobState is what the supervisor reports of a session's turn: its tempo, such as active, idle or blocked, its state, a line on what the turn does, and the dialog it waits on.
type jobState struct {
	Tempo  string
	State  string
	Detail string
	Needs  string
}

// subscribe sends each change of session short's jobState to fn, from the supervisor's snapshot on, until ctx ends or the supervisor drops the subscription.
// The supervisor's stream messages carry raw terminal output, which it skips.
func subscribe(ctx context.Context, short string, fn func(jobState)) error {
	if err := checkProto(ctx); err != nil {
		return err
	}
	conn, r, err := controlRequest(ctx, map[string]any{"proto": supervisorProto, "op": "subscribe", "short": short, "tail": 1})
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	var st jobState
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("the supervisor's subscription to %s: %w", short, err)
		}
		var msg struct {
			Type   string                     `json:"type"`
			OK     *bool                      `json:"ok"`
			Error  string                     `json:"error"`
			Record map[string]json.RawMessage `json:"record"`
			Patch  map[string]json.RawMessage `json:"patch"`
		}
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		fields := msg.Patch
		switch {
		case msg.OK != nil && !*msg.OK:
			return fmt.Errorf("the supervisor refused the subscription to %s: %s", short, msg.Error)
		case msg.Type == "snapshot":
			st, fields = jobState{}, msg.Record
		case msg.Type != "state":
			continue
		}
		next := st
		for key, field := range map[string]*string{"tempo": &next.Tempo, "state": &next.State, "detail": &next.Detail, "needs": &next.Needs} {
			if raw, ok := fields[key]; ok {
				*field = ""
				_ = json.Unmarshal(raw, field)
			}
		}
		if next != st || msg.Type == "snapshot" {
			st = next
			fn(st)
		}
	}
}

// Need is the dialog a session waits on, read from the supervisor's needs: a permission for Tool with Detail, a Question with its Options, or Other text.
type Need struct {
	Kind     string   `json:"kind"`
	Tool     string   `json:"tool,omitempty"`
	Detail   string   `json:"detail,omitempty"`
	Question string   `json:"question,omitempty"`
	Options  []string `json:"options,omitempty"`
	Text     string   `json:"text,omitempty"`
}

var (
	needsApprove = regexp.MustCompile(`(?s)^approve ([^:\s]+): (.*)$`)
	needsAnswer  = regexp.MustCompile(`(?s)^answer: (.*?)(?: \(([^()]*)\))?$`)
)

// parseNeeds reads needs as Claude Code 2.1.310 writes it, as `approve Bash: echo hi` or `answer: red or blue? (Red · Blue)`.
func parseNeeds(needs string) *Need {
	switch {
	case needs == "":
		return nil
	case needsApprove.MatchString(needs):
		m := needsApprove.FindStringSubmatch(needs)
		return &Need{Kind: "permission", Tool: m[1], Detail: m[2]}
	case needsAnswer.MatchString(needs):
		m := needsAnswer.FindStringSubmatch(needs)
		n := &Need{Kind: "question", Question: m[1]}
		if m[2] != "" {
			n.Options = strings.Split(m[2], " · ")
		}
		return n
	}
	return &Need{Kind: "other", Text: needs}
}

// Progress is a "progress" line of a follow: the latest jobState of session Name, all of it empty once bosun has none.
type Progress struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	Tempo  string `json:"tempo"`
	State  string `json:"state"`
	Detail string `json:"detail"`
	Needs  string `json:"needs"`
	Need   *Need  `json:"need,omitempty"`
}

func progressOf(name string, st jobState) Progress {
	return Progress{Type: "progress", Name: name, Tempo: st.Tempo, State: st.State, Detail: st.Detail, Needs: st.Needs, Need: parseNeeds(st.Needs)}
}

// resubscribeWait is how long a follow waits to subscribe again after the supervisor drops or refuses it, as it does while no supervisor runs.
var resubscribeWait = 5 * time.Second

// progress sends each change of session name's jobState as a progress line until ctx ends, subscribing again after resubscribeWait whenever the subscription ends.
// A worker's switch moves it to a new short id, which it subscribes to at once.
// A subscription that ends after it sent a state sends an empty one, since the state it sent no longer holds.
func (d *daemon) progress(ctx context.Context, o *out, name string, poll, wait time.Duration) {
	var warned bool
	for {
		_, short, err := d.conversationID(name)
		if err != nil {
			return
		}
		sent := false
		if short != "" {
			sctx, scancel := context.WithCancel(ctx)
			go func() {
				t := time.NewTicker(poll)
				defer t.Stop()
				for {
					select {
					case <-sctx.Done():
						return
					case <-t.C:
						if _, now, _ := d.conversationID(name); now != short {
							scancel()
							return
						}
					}
				}
			}()
			err = subscribe(sctx, short, func(st jobState) {
				sent = true
				o.send(progressOf(name, st))
			})
			scancel()
		}
		if ctx.Err() != nil {
			return
		}
		if sent {
			o.send(progressOf(name, jobState{}))
		}
		if _, now, _ := d.conversationID(name); now != short {
			continue
		}
		if err != nil && !warned && !errors.Is(err, context.Canceled) {
			log.Printf("following %s without progress: %v", name, err)
			warned = true
		}
		pause := wait
		if short == "" {
			pause = poll
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
	}
}
