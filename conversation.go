package main

import (
	"bufio"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Said is an entry of a session's conversation, as the frontends get it. Kind picks the fields that apply:
//   - prompt: From, Text and MidTurn
//   - task: From, Text, Task, Call, OutputFile, Status, Summary and Exit, for Claude Code's <task-notification>
//   - queued: Text and Taken
//   - text, thinking: Text
//   - tool: Call, Name, Input and, once it lands, Result
//   - compact: Summary
//   - command: Name, Args and Output
//   - meta, unsupported: RecordType and Raw
//
// An entry that changes, as a tool's result lands, goes out again under the same ID.
type Said struct {
	ID      string    `json:"id"`
	At      time.Time `json:"at,omitzero"`
	Kind    string    `json:"kind"`
	From    string    `json:"from,omitempty"`
	Text    string    `json:"text,omitempty"`
	MidTurn bool      `json:"midTurn,omitempty"`
	Taken   bool      `json:"taken,omitempty"`

	Call   string          `json:"call,omitempty"`
	Name   string          `json:"name,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
	Result *ToolResult     `json:"result,omitempty"`

	Summary string `json:"summary,omitempty"`
	// Task, OutputFile, Status and Exit read a <task-notification>. Exit is the exit code that a finished background command's summary names, and nil for any other task.
	Task       string `json:"task,omitempty"`
	OutputFile string `json:"outputFile,omitempty"`
	Status     string `json:"status,omitempty"`
	Exit       *int   `json:"exit,omitempty"`

	Args   string `json:"args,omitempty"`
	Output string `json:"output,omitempty"`

	RecordType string          `json:"recordType,omitempty"`
	Raw        json.RawMessage `json:"raw,omitempty"`

	// peer marks another session's message, whose record does not say whether it began a turn or reached one between tool calls.
	peer bool
}

// ToolResult is what a tool call returned. Diff holds an edit's or a write's hunks, each {oldStart, oldLines, newStart, newLines, lines}.
type ToolResult struct {
	At          time.Time       `json:"at,omitzero"`
	Text        string          `json:"text"`
	Error       bool            `json:"error,omitempty"`
	Interrupted bool            `json:"interrupted,omitempty"`
	Diff        json.RawMessage `json:"diff,omitempty"`
}

// metaTypes, metaAttachments and metaSystem are the records of Claude Code 2.1.310 that hold bookkeeping, which a frontend need not show.
// A record outside them, and outside what conversation.add maps, goes out as unsupported.
var (
	metaTypes = []string{
		"api-request", "api-request-blob", "api-request-shape", "cost-state", "last-prompt", "atis-latch", "mode", "permission-mode",
		"agent-name", "custom-title", "ai-title", "worktree-state", "file-history-delta", "file-history-snapshot", "summary",
		"tools-used", "bridge-session", "agent-setting", "continued-in",
	}
	metaAttachments = []string{
		"total_tokens_reminder", "deferred_tools_record", "deferred_tools_delta", "task_reminder", "silent_turn_reminder", "diagnostics",
		"prompt_snapshot", "environment", "hook_success", "hook_additional_context", "hook_permission_decision", "hook_blocking_error",
		"hook_non_blocking_error", "edited_text_file", "mcp_instructions_delta", "command_permissions", "instructions", "model",
		"skill_listing", "skill_mention", "date", "agent_listing_delta", "sandbox_instructions", "credential_org", "session_context",
		"auto_mode", "nested_memory", "thinking_drop", "read_truncation_notice", "compact_file_reference",
		"file", "remote_session_change", "hook_system_message", "session_cron_carry", "plan_mode_exit", "invoked_skills",
	}
	metaSystem = []string{"stop_hook_summary", "turn_duration", "away_summary", "informational", "bridge_status"}
)

var localStdout = regexp.MustCompile(`(?s)<local-command-stdout>(.*?)</local-command-stdout>`)

// record is the part of a transcript line that the conversation reads.
type record struct {
	Type       string                         `json:"type"`
	Subtype    string                         `json:"subtype"`
	UUID       string                         `json:"uuid"`
	Timestamp  time.Time                      `json:"timestamp"`
	Sidechain  bool                           `json:"isSidechain"`
	Meta       bool                           `json:"isMeta"`
	Summary    bool                           `json:"isCompactSummary"`
	Origin     origin                         `json:"origin"`
	Operation  string                         `json:"operation"`
	Content    json.RawMessage                `json:"content"`
	CommandRun struct{ Command, Args string } `json:"commandRun"`
	ToolResult json.RawMessage                `json:"toolUseResult"`
	Message    struct {
		StopReason string          `json:"stop_reason"`
		Content    json.RawMessage `json:"content"`
	} `json:"message"`
	Attachment struct {
		Type   string          `json:"type"`
		Prompt json.RawMessage `json:"prompt"`
		Origin origin          `json:"origin"`
	} `json:"attachment"`
}

// origin says who a prompt came from: kind human for typed text, peer for another session's message, with its name and body.
type origin struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Body string `json:"body"`
}

// conversation turns the records of a transcript into entries, one line at a time.
// Every reader of a transcript goes through it, so a change in Claude Code's format breaks it alone.
type conversation struct {
	entries []Said
	// at maps an entry's ID to its place, calls a tool call's id, and queued holds the queued entries not yet taken, oldest first.
	at     map[string]int
	calls  map[string]int
	queued []int
	// command is the place of the last slash command still without its output, and compacted of the last compaction without its summary, or -1.
	command, compacted int
	line               int
	changed            []int
	// sender names who sent a prompt that bosun handed the session, or returns "" for one it did not.
	sender func(text string) string

	// lastAt is when the session last wrote an assistant message, lastText its last text, and ended whether that message ended its turn.
	lastAt   time.Time
	lastText string
	ended    bool
}

func newConversation(sender func(string) string) *conversation {
	return &conversation{at: map[string]int{}, calls: map[string]int{}, command: -1, compacted: -1, sender: sender}
}

// transcriptPath is the transcript Claude Code keeps of session sid.
func transcriptPath(sid string) (string, error) {
	if sid == "" {
		return "", errors.New("bosun knows no Claude Code session id for it")
	}
	paths, _ := filepath.Glob(filepath.Join(claudeDir(), "projects", "*", sid+".jsonl"))
	if len(paths) == 0 {
		return "", fmt.Errorf("Claude Code keeps no transcript of session %s", sid)
	}
	return paths[0], nil
}

// readConversation reads the whole transcript of session sid.
func readConversation(sid string, sender func(string) string) (*conversation, error) {
	path, err := transcriptPath(sid)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	c := newConversation(sender)
	_, err = c.read(f)
	c.changed = nil
	return c, err
}

// read adds each whole line r holds, and returns how many bytes they took. A line still being written stays for the next read.
func (c *conversation) read(r io.Reader) (int64, error) {
	br := bufio.NewReader(r)
	var n int64
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return n, err
		}
		n += int64(len(line))
		c.add(line)
	}
}

// flush returns the entries from place first on that were added or changed since the last flush, in order.
func (c *conversation) flush(first int) []Said {
	slices.Sort(c.changed)
	var out []Said
	for _, p := range slices.Compact(c.changed) {
		if p >= first {
			out = append(out, c.entries[p])
		}
	}
	c.changed = nil
	return out
}

func (c *conversation) put(e Said) int {
	if p, ok := c.at[e.ID]; ok {
		c.entries[p] = e
		c.changed = append(c.changed, p)
		return p
	}
	p := len(c.entries)
	c.at[e.ID] = p
	c.entries = append(c.entries, e)
	c.changed = append(c.changed, p)
	return p
}

func (c *conversation) touch(p int) {
	c.changed = append(c.changed, p)
}

// add maps one transcript line to entries.
func (c *conversation) add(line []byte) {
	c.line++
	line = []byte(strings.TrimSpace(string(line)))
	if len(line) == 0 {
		return
	}
	var r record
	if err := json.Unmarshal(line, &r); err != nil {
		c.put(Said{ID: fmt.Sprintf("L%d", c.line), Kind: "unsupported", RecordType: "invalid JSON", Raw: rawString(line)})
		return
	}
	id := r.UUID
	if id == "" {
		id = fmt.Sprintf("L%d", c.line)
	}
	base := Said{ID: id, At: r.Timestamp}
	other := func(kind string) {
		e := base
		e.Kind, e.RecordType, e.Raw = kind, recordType(r), cutInput(line)
		c.put(e)
	}
	switch {
	case r.Sidechain:
		other("meta")
	case r.Type == "assistant":
		c.assistant(r, base, line)
	case r.Type == "user":
		c.user(r, base, line)
	case r.Type == "attachment" && r.Attachment.Type == "queued_command":
		e := base
		c.prompt(&e, r.Attachment.Origin, plainText(r.Attachment.Prompt))
		e.MidTurn = true
		c.put(e)
	case r.Type == "attachment" && slices.Contains(metaAttachments, r.Attachment.Type):
		other("meta")
	case r.Type == "queue-operation":
		if kind := c.queue(r, base); kind != "queued" {
			other(kind)
		}
	case r.Type == "system" && r.Subtype == "compact_boundary":
		e := base
		e.Kind = "compact"
		c.compacted = c.put(e)
	case r.Type == "system" && r.Subtype == "local_command":
		var text string
		_ = json.Unmarshal(r.Content, &text)
		c.output(base, r.CommandRun.Command, r.CommandRun.Args, text)
	case r.Type == "system" && slices.Contains(metaSystem, r.Subtype):
		other("meta")
	case slices.Contains(metaTypes, r.Type):
		other("meta")
	default:
		other("unsupported")
	}
}

func (c *conversation) assistant(r record, base Said, line []byte) {
	c.lastAt, c.ended = r.Timestamp, r.Message.StopReason == "end_turn"
	var raws []json.RawMessage
	if json.Unmarshal(r.Message.Content, &raws) != nil {
		// A string content is a single text.
		raws = []json.RawMessage{r.Message.Content}
	}
	if len(raws) == 0 || string(raws[0]) == "null" {
		e := base
		e.Kind, e.RecordType, e.Raw = "unsupported", "assistant", cutInput(line)
		c.put(e)
		return
	}
	var texts []string
	for i, raw := range raws {
		e := base
		if i > 0 {
			e.ID = fmt.Sprintf("%s#%d", base.ID, i)
		}
		var b struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		}
		var s string
		switch {
		case json.Unmarshal(raw, &s) == nil:
			e.Kind, e.Text = "text", s
			texts = append(texts, s)
		case json.Unmarshal(raw, &b) != nil:
			e.Kind, e.RecordType, e.Raw = "unsupported", "assistant", cutInput(raw)
		case b.Type == "text":
			e.Kind, e.Text = "text", b.Text
			texts = append(texts, b.Text)
		case b.Type == "thinking":
			e.Kind, e.Text = "thinking", b.Thinking
		case b.Type == "tool_use":
			e.Kind, e.Call, e.Name, e.Input = "tool", b.ID, b.Name, b.Input
			if p, ok := c.calls[b.ID]; ok {
				e.Result = c.entries[p].Result
			}
		default:
			e.Kind, e.RecordType, e.Raw = "unsupported", "assistant/"+b.Type, cutInput(raw)
		}
		p := c.put(e)
		if e.Kind == "tool" {
			c.calls[e.Call] = p
		}
	}
	if len(texts) > 0 {
		c.lastText = strings.Join(texts, "\n")
	}
}

func (c *conversation) user(r record, base Said, line []byte) {
	content := r.Message.Content
	var raws []json.RawMessage
	if json.Unmarshal(content, &raws) != nil {
		raws = nil
	}
	results := slices.ContainsFunc(raws, func(raw json.RawMessage) bool { return blockType(raw) == "tool_result" })
	switch {
	case r.Summary:
		e := base
		if c.compacted >= 0 {
			e = c.entries[c.compacted]
		}
		e.Kind, e.Summary = "compact", plainText(content)
		c.put(e)
		c.compacted = -1
	case results:
		for i, raw := range raws {
			if blockType(raw) != "tool_result" {
				e := base
				e.ID = fmt.Sprintf("%s#%d", base.ID, i)
				e.Kind, e.RecordType, e.Raw = "unsupported", "user/"+blockType(raw), cutInput(raw)
				c.put(e)
				continue
			}
			c.result(r, base, i, raw)
		}
	case r.Origin.Kind == "peer":
		e := base
		c.prompt(&e, r.Origin, plainText(content))
		c.put(e)
	case r.Meta:
		e := base
		e.Kind, e.RecordType, e.Raw = "meta", recordType(r), cutInput(line)
		c.put(e)
	default:
		text := plainText(content)
		if out := localStdout.FindStringSubmatch(text); out != nil && strings.HasPrefix(text, "<local-command-") {
			c.output(base, "", "", text)
			return
		}
		if m := commandName.FindStringSubmatch(text); m != nil && strings.HasPrefix(text, "<command-") {
			e := base
			e.Kind, e.Name = "command", m[1]
			if a := commandArgs.FindStringSubmatch(text); a != nil {
				e.Args = strings.TrimSpace(a[1])
			}
			c.command = c.put(e)
			return
		}
		e := base
		c.prompt(&e, r.Origin, text)
		c.put(e)
		for i, raw := range raws {
			if t := blockType(raw); t != "text" {
				o := base
				o.ID = fmt.Sprintf("%s#%d", base.ID, i)
				o.Kind, o.RecordType, o.Raw = "unsupported", "user/"+t, cutInput(raw)
				c.put(o)
			}
		}
	}
}

// result lands a tool call's result on its tool entry, the call's block i of record r.
func (c *conversation) result(r record, base Said, i int, raw json.RawMessage) {
	var b struct {
		ID      string          `json:"tool_use_id"`
		Content json.RawMessage `json:"content"`
		Error   bool            `json:"is_error"`
	}
	_ = json.Unmarshal(raw, &b)
	res := &ToolResult{At: r.Timestamp, Text: resultText(b.Content), Error: b.Error}
	var tr struct {
		Interrupted     bool            `json:"interrupted"`
		Type            string          `json:"type"`
		Content         string          `json:"content"`
		StructuredPatch json.RawMessage `json:"structuredPatch"`
	}
	if json.Unmarshal(r.ToolResult, &tr) == nil {
		res.Interrupted = tr.Interrupted
		if len(tr.StructuredPatch) > 2 {
			res.Diff = tr.StructuredPatch
		} else if tr.Type == "create" {
			res.Diff = created(tr.Content)
		}
	}
	// A stop leaves a call without a result until the next prompt writes this one.
	if strings.HasPrefix(res.Text, "[Tool call interrupted") {
		res.Interrupted = true
	}
	if p, ok := c.calls[b.ID]; ok {
		c.entries[p].Result = res
		c.touch(p)
		return
	}
	// The call itself never reached this transcript, as one before a compaction may not.
	e := base
	if i > 0 {
		e.ID = fmt.Sprintf("%s#%d", base.ID, i)
	}
	e.Kind, e.Call, e.Result = "tool", b.ID, res
	c.calls[b.ID] = c.put(e)
}

// output lands a slash command's output on its command entry, or begins one for output with no command before it.
func (c *conversation) output(base Said, name, args, text string) {
	out := text
	if m := localStdout.FindStringSubmatch(text); m != nil {
		out = m[1]
	}
	out = ansi.ReplaceAllString(out, "")
	if c.command >= 0 {
		c.entries[c.command].Output = out
		c.touch(c.command)
		c.command = -1
		return
	}
	e := base
	e.Kind, e.Output = "command", out
	if name != "" {
		e.Name, e.Args = "/"+name, args
	}
	c.put(e)
}

// queue maps a queue-operation: an enqueue begins a queued entry, and each other operation takes queued entries in.
// It returns the kind of entry the record itself must go out as: meta for an operation that took nothing, unsupported for one it does not know, or queued once it is mapped.
func (c *conversation) queue(r record, base Said) string {
	var text string
	_ = json.Unmarshal(r.Content, &text)
	take := func(i int) {
		p := c.queued[i]
		c.queued = slices.Delete(c.queued, i, i+1)
		c.entries[p].Taken = true
		c.touch(p)
	}
	i := -1
	switch r.Operation {
	case "enqueue":
		e := base
		e.Kind, e.Text = "queued", text
		c.queued = append(c.queued, c.put(e))
		return "queued"
	case "remove":
		i = slices.IndexFunc(c.queued, func(p int) bool { return c.entries[p].Text == text })
	case "dequeue", "popAll":
		if len(c.queued) > 0 {
			i = 0
		}
	default:
		return "unsupported"
	}
	if i < 0 {
		return "meta"
	}
	take(i)
	for r.Operation == "popAll" && len(c.queued) > 0 {
		take(0)
	}
	return "queued"
}

// prompt makes e the prompt text, from whoever sent it: the session a cross-session message names, the session bosun handed it for, bosun itself, Claude Code for its own notifications, or the user.
func (c *conversation) prompt(e *Said, o origin, text string) {
	e.Kind, e.From, e.Text = "prompt", "user", text
	if o.Kind == "peer" && o.Name != "" {
		e.From, e.Text, e.peer = o.Name, cmp.Or(o.Body, text), true
		return
	}
	if i := strings.Index(text, "<cross-session-message"); i >= 0 {
		if from, body, ok := peerMessage(text[i:]); ok {
			e.From, e.Text, e.peer = from, body, true
			return
		}
	}
	from := ""
	if c.sender != nil {
		from = c.sender(text)
	}
	switch {
	case from != "":
		e.From = from
	case bosunPrompt(text):
		e.From = "bosun"
	case o.Kind == "task-notification":
		e.From = "claude"
	}
	taskNotification(e, text)
}

var (
	taskField = regexp.MustCompile(`(?s)<(task-id|tool-use-id|output-file|status|summary)>(.*?)</(?:task-id|tool-use-id|output-file|status|summary)>`)
	taskExit  = regexp.MustCompile(`\(exit code (-?\d+)\)\s*$`)
)

// taskNotification turns e into a task entry when text is Claude Code's <task-notification>, which reports a finished background command, a monitor or an agent. Text stays whole.
func taskNotification(e *Said, text string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "<task-notification>") || !strings.HasSuffix(text, "</task-notification>") {
		return
	}
	e.Kind, e.From = "task", "claude"
	for _, m := range taskField.FindAllStringSubmatch(text, -1) {
		v := strings.TrimSpace(m[2])
		switch m[1] {
		case "task-id":
			e.Task = v
		case "tool-use-id":
			e.Call = v
		case "output-file":
			e.OutputFile = v
		case "status":
			e.Status = v
		case "summary":
			e.Summary = v
		}
	}
	if m := taskExit.FindStringSubmatch(e.Summary); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			e.Exit = &n
		}
	}
}

// bosunPrompt reports whether bosun wrote prompt itself, as a resume's prompt.
func bosunPrompt(prompt string) bool {
	prompt = strings.TrimSpace(prompt)
	return deskPromptFromBosun(prompt) && prompt != "/clear" || prompt == masterStart || prompt == eventsWaiting || strings.HasSuffix(prompt, masterCarryOn)
}

// recordType names a record for a frontend: its type, and the kind of attachment or system record it is.
func recordType(r record) string {
	switch {
	case r.Type == "attachment" && r.Attachment.Type != "":
		return r.Type + "/" + r.Attachment.Type
	case r.Subtype != "":
		return r.Type + "/" + r.Subtype
	case r.Operation != "":
		return r.Type + "/" + r.Operation
	}
	return r.Type
}

// plainText is the text of a message's content, a string or a list of blocks.
func plainText(raw json.RawMessage) string {
	var parts []string
	for _, c := range contentBlocks(raw) {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func blockType(raw json.RawMessage) string {
	var b struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &b)
	return b.Type
}

// resultText is the text of a tool result's content, a string or a list of blocks.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &blocks)
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		} else {
			parts = append(parts, "["+b.Type+"]")
		}
	}
	return strings.Join(parts, "\n")
}

// created is the hunk of a file a write created, which Claude Code records without one.
func created(content string) json.RawMessage {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "+" + l
	}
	out, _ := json.Marshal([]map[string]any{{"oldStart": 0, "oldLines": 0, "newStart": 1, "newLines": len(lines), "lines": lines}})
	return out
}

func rawString(line []byte) json.RawMessage {
	out, _ := json.Marshal(clip(string(line), logInputCut))
	return out
}

// page returns up to limit entries before the one with ID before, or the last ones when before is empty, and whether older entries remain.
// Meta entries count against no limit: those among the page come along, so that a page holds what a frontend shows.
func page(entries []Said, before string, limit int) ([]Said, bool, error) {
	end := len(entries)
	if before != "" {
		end = slices.IndexFunc(entries, func(e Said) bool { return e.ID == before })
		if end < 0 {
			return nil, false, fmt.Errorf("no entry %s in the conversation", before)
		}
	}
	start, n := end, 0
	for start > 0 && (n < limit || entries[start-1].Kind == "meta") {
		start--
		if entries[start].Kind != "meta" {
			n++
		}
	}
	return entries[start:end], start > 0, nil
}

// promptSum identifies a prompt bosun handed a session, without keeping its text.
func promptSum(text string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(text)))
	return hex.EncodeToString(sum[:8])
}
