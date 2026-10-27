package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Context is how full a session's context window is: Used tokens of Total.
// Used is what Claude Code's own meter counts for the latest request: its input tokens plus those read from and written to the cache, which leave out the reply.
type Context struct {
	Used  int `json:"used"`
	Total int `json:"total"`
}

// defaultWindow is the context window of a model that none of the rules below name, as Claude Code assumes.
const defaultWindow = 200_000

// longWindow is the window of a model with a 1M context.
const longWindow = 1_000_000

// modelVersion splits a model id such as claude-sonnet-4-5-20250929 into its family and its version, 4.5 here.
var modelVersion = regexp.MustCompile(`^claude-([a-z]+)-(\d+)(?:-(\d{1,2}))?(?:-\d{8})?`)

// contextWindow is the context window of the model that answered, from the id in the transcript, and of flag, the model the session was started with.
// A transcript holds the bare id, so a 1M window that a model gets only through the "[1m]" suffix shows only in flag.
// The rules follow the model catalog in Claude Code 2.1.311: Opus from 4.7, Sonnet and Haiku from 5, Fable and Mythos run a 1M window of their own, and every other model 200k.
func contextWindow(model, flag string) int {
	if strings.Contains(flag, "[1m]") || strings.Contains(model, "[1m]") {
		return longWindow
	}
	m := modelVersion.FindStringSubmatch(model)
	if m == nil {
		return defaultWindow
	}
	major, _ := strconv.Atoi(m[2])
	minor, _ := strconv.Atoi(m[3])
	switch m[1] {
	case "fable", "mythos":
		return longWindow
	case "opus":
		if major > 4 || major == 4 && minor >= 7 {
			return longWindow
		}
	case "sonnet", "haiku":
		if major >= 5 {
			return longWindow
		}
	}
	return defaultWindow
}

// contextLabel shows c as "84k/1M" for a frontend without a bar, or "" for none.
func contextLabel(c *Context) string {
	if c == nil {
		return ""
	}
	return tokenCount(c.Used) + "/" + tokenCount(c.Total)
}

func tokenCount(n int) string {
	switch {
	case n >= 1_000_000 && n%1_000_000 == 0:
		return fmt.Sprintf("%dM", n/1_000_000)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%dk", (n+500)/1000)
	}
	return strconv.Itoa(n)
}

// tailStart is how much of a transcript's end a first read looks at for an assistant message, and tailMax the most it widens that to.
const (
	tailStart = 256 << 10
	tailMax   = 64 << 20
)

// usageTail follows the end of one conversation's transcript for the usage of its latest assistant message.
// It reads each byte of the transcript once, from where the last read stopped, and starts over when the transcript is replaced or cut short.
type usageTail struct {
	mu sync.Mutex
	// path is the transcript the tail follows and off how far into it the whole lines reach. A zero off means no read yet.
	path string
	off  int64
	// used is the input, cache-read and cache-creation tokens of the latest assistant message since the last compaction, and model the id that answered it, kept across a new conversation since a clear does not change the model.
	used  int
	model string
	// seen says the conversation holds an assistant message or a compaction, so used is a reading and not the zero of a conversation with none.
	seen bool
}

// usageRecord is the part of a transcript record that carries the usage of a request.
type usageRecord struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	Sidechain bool   `json:"isSidechain"`
	Message   struct {
		Model string `json:"model"`
		Usage *struct {
			Input         int `json:"input_tokens"`
			CacheCreation int `json:"cache_creation_input_tokens"`
			CacheRead     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

var (
	usageMark   = []byte(`"usage"`)
	compactMark = []byte(`compact_boundary`)
)

// read brings the tail up to the end of the transcript at path and returns the reading: the used tokens, the model, and whether the conversation holds a reading at all.
// A transcript that is not the one read last starts the reading over, with no used tokens until it holds a message.
func (t *usageTail) read(path string) (used int, model string, seen bool, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		return 0, t.model, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, t.model, false, err
	}
	size := st.Size()
	if path != t.path || size < t.off {
		t.path, t.off, t.used, t.seen = path, 0, 0, false
	}
	if t.off == 0 {
		for window := int64(tailStart); ; window *= 4 {
			start := max(0, size-window)
			t.used, t.seen = 0, false
			err = t.scan(f, start, size, start > 0)
			if err != nil || t.seen || start == 0 || window >= tailMax {
				break
			}
		}
	} else {
		err = t.scan(f, t.off, size, false)
	}
	return t.used, t.model, t.seen, err
}

// scan takes in the whole lines of f from start to size, skipping a first one that start cut in two, and moves off past them.
func (t *usageTail) scan(f *os.File, start, size int64, partial bool) error {
	if size <= start {
		return nil
	}
	data := make([]byte, size-start)
	n, err := f.ReadAt(data, start)
	if err != nil && err != io.EOF {
		return err
	}
	data = data[:n]
	skipped := 0
	if partial {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return nil
		}
		skipped = i + 1
	}
	end := bytes.LastIndexByte(data, '\n') + 1
	if end <= skipped {
		return nil
	}
	for _, line := range bytes.Split(data[skipped:end], []byte{'\n'}) {
		t.add(line)
	}
	t.off = start + int64(end)
	return nil
}

// add takes in one record: an assistant message sets the reading, and a compaction resets it to zero until the next message.
func (t *usageTail) add(line []byte) {
	isUsage, isCompact := bytes.Contains(line, usageMark), bytes.Contains(line, compactMark)
	if !isUsage && !isCompact {
		return
	}
	var r usageRecord
	if json.Unmarshal(line, &r) != nil || r.Sidechain {
		return
	}
	switch {
	case r.Type == "assistant" && r.Message.Usage != nil && r.Message.Model != "<synthetic>":
		u := r.Message.Usage
		t.used, t.seen = u.Input+u.CacheCreation+u.CacheRead, true
		t.model = cmp.Or(r.Message.Model, t.model)
	case r.Type == "system" && r.Subtype == "compact_boundary":
		t.used, t.seen = 0, true
	}
}

// contextFollowDelay is how long after a Stop the daemon reads the transcript once more, since Claude Code writes the last message a moment after the hook.
var contextFollowDelay = 1500 * time.Millisecond

// contextEvents are the hooks after which a session's transcript may hold a new assistant message, or begin a new conversation.
var contextEvents = map[string]bool{
	"SessionStart": true, "UserPromptSubmit": true, "PostToolUse": true, "PostToolUseFailure": true,
	"Stop": true, "StopFailure": true, "PostCompact": true,
}

// touchContextLocked has the daemon read session n's context use, and again a moment later at the end of a turn.
func (d *daemon) touchContextLocked(n *Session, event string) {
	if n.usage == nil {
		n.usage = &usageTail{}
	}
	go d.refreshContext(n.Node)
	if event == "Stop" || event == "StopFailure" {
		time.AfterFunc(contextFollowDelay, func() { d.refreshContext(n.Node) })
	}
}

// refreshContext reads the end of session node's transcript, and tells the frontends if its context use changed.
// It reads outside the lock, only what the transcript gained since its last read.
func (d *daemon) refreshContext(node string) {
	d.mu.Lock()
	n := d.nodes[node]
	if n == nil || n.usage == nil {
		d.mu.Unlock()
		return
	}
	tail, flag := n.usage, n.Model
	sid := cmp.Or(n.Conversation, n.SessionID)
	d.mu.Unlock()
	path, err := transcriptPath(sid)
	if err != nil {
		return
	}
	used, model, seen, err := tail.read(path)
	if err != nil || model == "" && !seen {
		return
	}
	// A conversation without a message yet, as a clear leaves it, starts from zero in the window of the model before it.
	next := &Context{Used: used, Total: contextWindow(model, flag)}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.nodes[node] != n || n.usage != tail || cmp.Or(n.Conversation, n.SessionID) != sid {
		return
	}
	if prev := n.Context; prev != nil && *prev == *next {
		return
	}
	n.Context = next
	d.broadcastLocked(Event{Kind: "context", Node: node, Context: next})
}
