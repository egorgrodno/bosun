package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// agent is one entry of `claude agents --json`.
type agent struct {
	PID       int    `json:"pid"`
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Cwd       string `json:"cwd"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	State     string `json:"state"`
}

// claudeCLI is the part of the claude command the daemon runs.
type claudeCLI interface {
	// Start runs `claude args...` in dir, where args hold --bg, and returns the short id it prints.
	Start(dir string, args []string) (string, error)
	// Agents lists every session, one that stopped included: a session stopped by `claude stop`, or by the supervisor once it sat idle for an hour, stays listed without a PID or Status until `claude rm`.
	Agents() ([]agent, error)
	Stop(id string) error
	// Remove runs `claude rm id`, with discard as --discard-unpushed when set: the <commit>@<worktree-id> that a refusing `claude rm` printed.
	Remove(id, discard string) error
	// Supervisor returns the pid of Claude Code's supervisor, which runs every background session.
	Supervisor() (int, error)
}

type claudeExec struct{}

var (
	ansi          = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	discardHint   = regexp.MustCompile(`--discard-unpushed ([0-9a-f]+)@([0-9a-f]+)`)
	backgrounded  = regexp.MustCompile(`backgrounded · ([0-9a-f]+)`)
	supervisorPID = regexp.MustCompile(`(?m)^pid:\s+([0-9]+)`)
)

func (claudeExec) Start(dir string, args []string) (string, error) {
	cmd := exec.Command("claude", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(ansi.ReplaceAllString(string(out), ""))
	if err != nil {
		return "", fmt.Errorf("claude --bg: %w: %s", err, text)
	}
	m := backgrounded.FindStringSubmatch(text)
	if m == nil {
		return "", fmt.Errorf("claude --bg printed no session id: %s", text)
	}
	return m[1], nil
}

func (claudeExec) Agents() ([]agent, error) {
	out, err := exec.Command("claude", "agents", "--json", "--all").Output()
	if err != nil {
		return nil, fmt.Errorf("claude agents --json --all: %w", err)
	}
	var agents []agent
	if err := json.Unmarshal(out, &agents); err != nil {
		return nil, fmt.Errorf("claude agents --json --all: %w", err)
	}
	return agents, nil
}

func (claudeExec) Supervisor() (int, error) {
	out, err := exec.Command("claude", "daemon", "status").Output()
	if err != nil {
		return 0, fmt.Errorf("claude daemon status: %w", err)
	}
	m := supervisorPID.FindSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("claude daemon status printed no pid: %s", strings.TrimSpace(string(out)))
	}
	return strconv.Atoi(string(m[1]))
}

func (claudeExec) Stop(id string) error {
	return run("stop", id)
}

func (claudeExec) Remove(id, discard string) error {
	if discard == "" {
		return run("rm", id)
	}
	return run("rm", id, "--discard-unpushed", discard)
}

// transcriptTail is what the transcript Claude Code keeps of a session tells of the session's last turn.
type transcriptTail struct {
	// text is the last text the session wrote in its own conversation, at is when it last wrote, and ended whether it then ended its turn.
	text  string
	at    time.Time
	ended bool
}

// readTranscript reads the transcript of Claude Code session sid.
func readTranscript(sid string) (transcriptTail, error) {
	var s transcriptTail
	if sid == "" {
		return s, errors.New("bosun knows no Claude Code session id for it")
	}
	paths, _ := filepath.Glob(filepath.Join(claudeDir(), "projects", "*", sid+".jsonl"))
	if len(paths) == 0 {
		return s, fmt.Errorf("Claude Code keeps no transcript of session %s", sid)
	}
	f, err := os.Open(paths[0])
	if err != nil {
		return s, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		var entry struct {
			Type      string    `json:"type"`
			Sidechain bool      `json:"isSidechain"`
			Timestamp time.Time `json:"timestamp"`
			Message   struct {
				StopReason string `json:"stop_reason"`
				Content    []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &entry) == nil && entry.Type == "assistant" && !entry.Sidechain {
			s.at, s.ended = entry.Timestamp, entry.Message.StopReason == "end_turn"
			var parts []string
			for _, c := range entry.Message.Content {
				if c.Type == "text" {
					parts = append(parts, c.Text)
				}
			}
			if len(parts) > 0 {
				s.text = strings.Join(parts, "\n")
			}
		}
		if err != nil {
			return s, nil
		}
	}
}

func run(cmd, id string, args ...string) error {
	out, err := exec.Command("claude", append([]string{cmd, id}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("claude %s %s: %w: %s", cmd, id, err, strings.TrimSpace(string(out)))
	}
	return nil
}
