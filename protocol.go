package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Request is a line a client sends the daemon. Node names the calling session, and is empty for a frontend.
// A hook's Socket is the messaging socket of the session's process, which other sessions address it by.
type Request struct {
	Op     string `json:"op"`
	Node   string `json:"node,omitempty"`
	Socket string `json:"socket,omitempty"`
	ID     string `json:"id,omitempty"`
	Name   string `json:"name,omitempty"`
	Repo   string `json:"repo,omitempty"`
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
	Text   string `json:"text,omitempty"`
	// Turns is how many of a session's last turns a transcript shows.
	Turns int `json:"turns,omitempty"`
	// Before and Limit pick the entries of a session's conversation that a conversation or follow request returns: the last Limit before the one with ID Before.
	Before string          `json:"before,omitempty"`
	Limit  int             `json:"limit,omitempty"`
	Hook   json.RawMessage `json:"hook,omitempty"`
	// Words quotes the user's message that the desk's answer to a decision acts on.
	Words string `json:"words,omitempty"`
	// Plugins and Connectors are what a spawn adds to the worker's lean start.
	Plugins    []string `json:"plugins,omitempty"`
	Connectors bool     `json:"connectors,omitempty"`

	// Key names a request that its client sends again, unchanged, once the daemon is back from a restart.
	Key string `json:"key,omitempty"`
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
	Answered  []Answered `json:"answered,omitempty"`
	Paused    bool       `json:"paused,omitempty"`
	// Log holds the fleet's latest entries, oldest first.
	Log []Entry `json:"log,omitempty"`
}

// ConvLine is a line the daemon sends for a session's conversation: the "result" of a conversation request, or on a follow a "reset", then "entries" and "progress" lines.
// Session is the Claude Code session id of the conversation, and More marks entries older than those sent.
type ConvLine struct {
	Type    string `json:"type"`
	Error   string `json:"error,omitempty"`
	Session string `json:"session,omitempty"`
	Entries []Said `json:"entries,omitempty"`
	More    bool   `json:"more,omitempty"`
}

// Event is what the master, the desk or a frontend is told. The master gets decision, turn_end, exit, tell and message; the desk gets tell; a frontend gets decision, settled, answered, sessions, activity, message, paused, unpaused and log.
// A message is what session From told session Node through Claude Code's SendMessage. A tell is From's text: the user's when From is empty, or the desk's or the master's.
// Again marks an event the master took once already, in a turn that was cut short.
type Event struct {
	Kind     string    `json:"kind"`
	Node     string    `json:"node,omitempty"`
	From     string    `json:"from,omitempty"`
	ID       string    `json:"id,omitempty"`
	Text     string    `json:"text,omitempty"`
	Decision *Decision `json:"decision,omitempty"`
	Sessions []Session `json:"sessions,omitempty"`
	Answered *Answered `json:"answered,omitempty"`
	Entry    *Entry    `json:"entry,omitempty"`
	Again    bool      `json:"again,omitempty"`
	// Activity is the latest activity of session Node, in an activity event.
	Activity *Activity `json:"activity,omitempty"`

	// sent is when the user sent a tell to the desk, which may hand it over only after an answer it proposed since.
	sent time.Time
}

// Entry is a line of the fleet's log. Seq numbers the entries in order, across restarts of the daemon.
// By is who acted: user, master, desk, worker, session for a decision's session through its own prompt, or bosun. From names the session that acted, and Node the session the entry is about.
type Entry struct {
	Seq  int       `json:"seq"`
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	By   string    `json:"by"`
	From string    `json:"from,omitempty"`
	Node string    `json:"node,omitempty"`
	Text string    `json:"text,omitempty"`
	// Decision is what a decision, escalated or settled entry is about, with each long string of its input cut short.
	Decision *Decision `json:"decision,omitempty"`
	// Behavior is a settled permission's answer, allow or deny. Withdrawn marks a decision nobody answered, and Cause completes "withdrawn when …".
	Behavior  string `json:"behavior,omitempty"`
	Withdrawn bool   `json:"withdrawn,omitempty"`
	Cause     string `json:"cause,omitempty"`
	// Words quotes the user's message that the desk settled a decision on.
	Words string `json:"words,omitempty"`
	// Via is how the user's action reached the session: supervisor where the daemon presented Claude Code's control key, resume where it stopped or resumed the session instead. Error is why the action failed.
	Via    string `json:"via,omitempty"`
	Error  string `json:"error,omitempty"`
	Repo   string `json:"repo,omitempty"`
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// Answered is a decision the desk answered on the user's word: Behavior is allow or deny for a permission, and Text a denial's message or a question's answer.
// Words quotes the user's message that said yes to the answer, which the desk had shown them in full.
type Answered struct {
	Decision Decision  `json:"decision"`
	Behavior string    `json:"behavior,omitempty"`
	Text     string    `json:"text,omitempty"`
	Words    string    `json:"words"`
	At       time.Time `json:"at"`
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
	// Refused is the reason auto mode gave for refusing the session's last call. After a run of refusals, it asks about a call instead of refusing it.
	Refused string `json:"refused,omitempty"`
	// UserOnly marks a reserved action on the user's list, which only the user's own answer in a frontend settles.
	UserOnly bool `json:"userOnly,omitempty"`

	toolUseID string
	// key is the Key of the request that waits on the decision.
	key    string
	opened time.Time
	// prompt marks a notice that a permission waits in its session's own prompt, which the session moving on withdraws.
	prompt bool
	// from is the session that sent a notice, for the log, and empty for bosun's own.
	from   string
	answer chan answer
}

type answer struct {
	behavior  string
	text      string
	withdrawn bool
}

// Session is a node of the tree. Status and State come from `claude agents --json --all`, with Status "stopped" while it lists the session without a process, and "exited" once it lists the session no more.
// An empty Model or Effort is the user's default. Killed marks a worker the user stopped, which the master may not resume.
// Turn marks a turn whose end the master has not heard of, and Heard is when the master last heard the end of one.
// Paused marks a session that a pause stopped in its turn, which unpausing resumes.
// A worker's branch began at the HEAD of BaseBranch, checked out in Base, which merge fast-forwards.
type Session struct {
	Node      string    `json:"node"`
	Parent    string    `json:"parent,omitempty"`
	Repo      string    `json:"repo,omitempty"`
	Dir       string    `json:"dir"`
	ID        string    `json:"id,omitempty"`
	SessionID string    `json:"sessionId,omitempty"`
	Model     string    `json:"model,omitempty"`
	Effort    string    `json:"effort,omitempty"`
	Depth     int       `json:"depth"`
	Status    string    `json:"status"`
	State     string    `json:"state,omitempty"`
	Killed    bool      `json:"killed,omitempty"`
	Turn      bool      `json:"turn,omitempty"`
	Heard     time.Time `json:"heard,omitzero"`
	Paused    bool      `json:"paused,omitempty"`
	// Conversation is the one a clear began, which a copy of the session must name.
	Conversation string `json:"conversation,omitempty"`
	// Retired holds the ids a worker had before a switch of its model or effort moved it to a new one. The first began the worker's worktree, so merge removes them too.
	Retired []string `json:"retired,omitempty"`
	// Plugins and Connectors are what the parent added to the worker's lean start, which each start of it repeats.
	Plugins    []string `json:"plugins,omitempty"`
	Connectors bool     `json:"connectors,omitempty"`
	// Activity is the latest tool call of the session or, with none in its turn, the start of its last message.
	Activity *Activity `json:"activity,omitempty"`
	// Socket is the messaging socket of the session's running process, forgotten once the process stops, since another process may take the path.
	Socket string `json:"socket,omitempty"`
	// Events holds what waits for a session that runs no next_event until bosun hands it over: the turn ends and exits of a worker's children, or the desk's tells. Taken holds the reports handed to a worker in its current turn.
	Events []Event `json:"events,omitempty"`
	Taken  []Event `json:"taken,omitempty"`
	// LimitedUntil is when the usage limit that stopped the session resets, and the daemon resumes it.
	LimitedUntil *time.Time `json:"limitedUntil,omitempty"`
	// Prompts holds the latest prompts that another session or the user handed the session through bosun, for its conversation to name who sent each.
	Prompts []sentPrompt `json:"prompts,omitempty"`
	// Unseen marks a desk that bosun handed a tell or an answer since the user last opened it, so its reply may wait there unread.
	Unseen bool `json:"unseen,omitempty"`

	Base       string `json:"base,omitempty"`
	BaseBranch string `json:"baseBranch,omitempty"`

	pre map[string]preCall
	// turnAt is when a hook of the session's turn last ran.
	turnAt time.Time
	// held keeps poll, resume, merge and kill off the session while one of them stops or resumes it.
	held bool
	// refused is the reason auto mode gave for refusing the session's last call, until a call runs.
	refused string
	// actSent is when the frontends last heard of the session's activity, and actTimer the send held back until the window ends.
	actSent  time.Time
	actTimer *time.Timer
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

// reconnectWait bounds how long callAgain waits for a daemon that went away, as one does while it restarts.
var reconnectWait = 30 * time.Second

// callAgain sends req as call does, and sends it again under the same Key while the daemon is away, for up to reconnectWait since the daemon last took it.
func callAgain(ctx context.Context, req Request) (Response, error) {
	if req.Key == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		req.Key = hex.EncodeToString(b)
	}
	until := time.Now().Add(reconnectWait)
	for {
		resp, err := call(ctx, req)
		var op *net.OpError
		switch {
		case err == nil || ctx.Err() != nil:
			return resp, err
		case errors.As(err, &op) && op.Op == "dial":
		case op != nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
			until = time.Now().Add(reconnectWait)
		default:
			return resp, err
		}
		if time.Now().After(until) {
			return resp, err
		}
		select {
		case <-ctx.Done():
			return resp, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
