package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
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

// transcript is what the transcript Claude Code keeps of a session tells: its last turns in its own conversation, the last text it wrote there, when it last wrote, and whether it then ended its turn.
type transcript struct {
	turns []turn
	text  string
	at    time.Time
	ended bool
}

// turn is the prompt that began a turn, then the session's text and a line for each of its tool calls, in order.
type turn struct {
	prompt string
	steps  []string
}

// keptPrompt and keptText clip each prompt and each text of a turn, in runes.
const (
	keptPrompt = 1000
	keptText   = 2000
)

// readTranscript reads the transcript of Claude Code session sid, with its last keep turns, through the entries of its conversation.
func readTranscript(sid string, keep int) (transcript, error) {
	var s transcript
	c, err := readConversation(sid, nil)
	if err != nil {
		return s, err
	}
	s.text, s.at, s.ended = c.lastText, c.lastAt, c.ended
	if keep == 0 {
		return s, nil
	}
	step := func(line string) {
		if len(s.turns) == 0 {
			s.turns = append(s.turns, turn{})
		}
		t := &s.turns[len(s.turns)-1]
		t.steps = append(t.steps, line)
	}
	begin := func(prompt string) {
		s.turns = append(s.turns, turn{prompt: clip(prompt, keptPrompt)})
		s.turns = s.turns[max(0, len(s.turns)-keep):]
	}
	for _, e := range c.entries {
		switch e.Kind {
		case "text":
			step(clip(e.Text, keptText))
		case "tool":
			detail, more := inputDetail(e.Input), ""
			if i := strings.IndexByte(detail, '\n'); i >= 0 {
				detail, more = detail[:i], " …"
			}
			step(fmt.Sprintf("[%s] %s%s", e.Name, detail, more))
		case "command":
			if e.Name != "" {
				begin(strings.TrimSpace(e.Name + " " + e.Args))
			}
		case "prompt":
			text := e.Text
			if e.From != "user" && e.From != "bosun" {
				text = "[from " + e.From + "] " + text
			}
			switch {
			case e.Text == "":
			// What the user types while the session runs a tool reaches the turn between tool calls, as other sessions' messages may.
			case e.MidTurn || e.peer:
				step(quote(clip(text, keptPrompt)))
			// Claude Code records the user's interruption of a turn as a prompt, which begins no turn.
			case strings.HasPrefix(text, "[Request interrupted by user"):
				step(quote(text))
			default:
				begin(text)
			}
		}
	}
	return s, nil
}

// block is one block of a message's content.
type block struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// contentBlocks reads a message's content, which is a string or a list of blocks.
func contentBlocks(raw json.RawMessage) []block {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []block{{Type: "text", Text: text}}
	}
	var blocks []block
	_ = json.Unmarshal(raw, &blocks)
	return blocks
}

// commandName and commandArgs are how a transcript records a slash command the session ran, such as a skill or the /clear that begins a cleared conversation.
var (
	commandName = regexp.MustCompile(`<command-name>([^<]*)</command-name>`)
	commandArgs = regexp.MustCompile(`<command-args>([^<]*)</command-args>`)
)

// quote marks each line of text as what the session was told.
func quote(text string) string {
	return "> " + strings.ReplaceAll(text, "\n", "\n> ")
}

// render lays out the turns: each prompt quoted, then the session's text and a line for each tool call.
func (s transcript) render() string {
	var b strings.Builder
	for i, t := range s.turns {
		if i > 0 {
			b.WriteString("\n")
		}
		if t.prompt != "" {
			b.WriteString(quote(t.prompt) + "\n")
		}
		for _, line := range t.steps {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

func run(cmd, id string, args ...string) error {
	out, err := exec.Command("claude", append([]string{cmd, id}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("claude %s %s: %w: %s", cmd, id, err, strings.TrimSpace(string(out)))
	}
	return nil
}
