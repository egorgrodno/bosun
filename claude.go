package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// agent is one entry of `claude agents --json`.
type agent struct {
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
	Agents() ([]agent, error)
	Stop(id string) error
	Remove(id string) error
}

type claudeExec struct{}

var (
	ansi         = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	backgrounded = regexp.MustCompile(`backgrounded · ([0-9a-f]+)`)
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
	out, err := exec.Command("claude", "agents", "--json").Output()
	if err != nil {
		return nil, fmt.Errorf("claude agents --json: %w", err)
	}
	var agents []agent
	if err := json.Unmarshal(out, &agents); err != nil {
		return nil, fmt.Errorf("claude agents --json: %w", err)
	}
	return agents, nil
}

func (claudeExec) Stop(id string) error {
	return run("stop", id)
}

func (claudeExec) Remove(id string) error {
	return run("rm", id)
}

func run(cmd, id string) error {
	out, err := exec.Command("claude", cmd, id).CombinedOutput()
	if err != nil {
		return fmt.Errorf("claude %s %s: %w: %s", cmd, id, err, strings.TrimSpace(string(out)))
	}
	return nil
}
