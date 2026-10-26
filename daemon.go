package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	masterNode = "master"
	// deskNode is the front desk, the session the user talks to, which reads the fleet and acts on it only through the master.
	deskNode = "desk"

	// holdTimeout is the timeout of the PreToolUse and PermissionRequest hooks, so how long a decision waits.
	// The hook gives up holdMargin before it, while Claude Code still reads what it prints.
	holdTimeout = time.Hour
	holdMargin  = 30 * time.Second

	// idleReturn bounds next_event, because text typed into the master waits until the call returns.
	idleReturn = 4 * time.Minute
	pollEvery  = 3 * time.Second
	// quietPollEvery spaces the polls while no session is in its turn, since every turn begins with a hook.
	quietPollEvery = time.Minute
	// startGrace covers a session `claude --bg` started that `claude agents --json` does not list yet.
	startGrace = 30 * time.Second

	keptMessages = 10
)

// idleWait bounds how long resume waits for a worker to be listed idle, which it is not until its Stop hook returns.
var idleWait = 30 * time.Second

// openReturn bounds next_event while the user has the master open through bosun attach, where what they type waits for the call to return.
var openReturn = 10 * time.Second

// promptAfter is how long a permission waits on the master or the user before the daemon leaves it to the session's own prompt, ahead of the hook giving up.
var promptAfter = holdTimeout - 2*holdMargin

// stopWait bounds how long a resume waits for the process of the session it stopped to exit.
const stopWait = 10 * time.Second

// cacheLife is how long Claude Code's prompt cache keeps a session's transcript.
const cacheLife = time.Hour

// clearWait bounds how long a clear waits for the master's SessionStart.
var clearWait = 30 * time.Second

var (
	errInTurn   = errors.New("still in its turn")
	errStarting = errors.New("still starting")
	errPaused   = errors.New("bosun is paused: bosun unpause resumes it")
)

// eventsWaiting is the prompt that wakes the master, and the reason a block of its Stop gives.
const eventsWaiting = "Events are waiting: call next_event."

var nodeName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// isWorker reports whether node is a worker rather than the master or the desk, which the daemon starts itself and which no parent hears of.
func isWorker(node string) bool {
	return node != masterNode && node != deskNode
}

type daemon struct {
	exe          string
	socket       string
	stateDir     string
	masterDir    string
	masterModel  string
	deskModel    string
	workerModel  string
	workerEffort string
	// maxDepth caps the depth of the tree, where the master's children are at depth 1, and maxChildren the live children of each session.
	maxDepth    int
	maxChildren int
	claude      claudeCLI

	// masterLock is held while the master is woken, switched or resumed idle, so that no two of them resume it at once.
	masterLock sync.Mutex
	// deskLock is held while the desk is handed what waits for it or readied for claude attach, each of which may clear it first.
	deskLock sync.Mutex
	// pauseLock is held through a pause or an unpause, each of which stops or resumes one session after another.
	pauseLock sync.Mutex
	mu        sync.Mutex
	nodes     map[string]*Session
	started   map[string]time.Time
	decisions map[string]*Decision
	nextID    int
	events    []Event
	wake      chan struct{}
	watchers  map[*watcher]bool
	// messages holds the latest messages between sessions, for a frontend's snapshot.
	messages []Event
	// said is the user's latest message to the desk, typed into it or told through bosun, and saidBefore when the one before it came.
	said       userMessage
	saidBefore time.Time
	// proposed holds by decision id the answer the desk last showed the user, which only the user's next message can say yes to.
	proposed map[string]proposal
	// answered holds the latest decisions the desk answered on the user's word.
	answered []Answered
	// stopBlocked holds from a block of the master's Stop until its next call to next_event.
	// Claude Code's stop_hook_active cannot stand in: it stays true for the rest of the turn a block continued.
	stopBlocked bool
	// asleep holds from a Stop that ends the master's turn with no events waiting until the next event resumes it.
	asleep bool
	// cleared holds by node a channel that closes once the session's SessionStart reports the conversation that a clear began.
	cleared map[string]chan struct{}
	// recovering holds while the daemon resumes the sessions that a usage limit stopped, and limitNotice is the last notice of a limit.
	recovering  bool
	limitNotice string
	// lastPoll and stirred decide whether the next tick polls: stirred holds from a hook or a kill until the poll after it.
	lastPoll time.Time
	stirred  bool
	// masterPID is the master's process when the daemon last checked the supervisor, and foreign the cgroup of a supervisor it last told the user of.
	masterPID int
	foreign   string
	// taken is the batch of events that next_event, called with takenKey, last handed the master, which its next call or Stop acknowledges.
	taken    []Event
	takenKey string
	// masterOpen counts the frontends running claude attach on the master, and masterOpenAt is when the first of them began.
	masterOpen   int
	masterOpenAt time.Time
	// orphans holds by id the decisions of an earlier run until orphansUntil, for the hooks and asks that wait on them to send their requests again.
	orphans      map[string]*Decision
	orphansUntil time.Time
	// paused holds while the user has paused the fleet, and stopping once the daemon shuts down.
	paused   bool
	stopping bool
}

type watcher struct {
	ch     chan Event
	cancel context.CancelFunc
}

type userMessage struct {
	at   time.Time
	text string
}

// proposal is an answer the desk showed the user at at.
type proposal struct {
	answer answer
	at     time.Time
}

// preCall is a PreToolUse not yet followed by its PostToolUse.
type preCall struct {
	tool  string
	input string
}

type savedState struct {
	NextID    int             `json:"nextId"`
	Sessions  []*Session      `json:"sessions"`
	Events    []Event         `json:"events,omitempty"`
	Taken     []Event         `json:"taken,omitempty"`
	TakenKey  string          `json:"takenKey,omitempty"`
	Decisions []savedDecision `json:"decisions,omitempty"`
	Answered  []Answered      `json:"answered,omitempty"`
	Paused    bool            `json:"paused,omitempty"`
}

// savedDecision is a decision with what the protocol leaves out of it.
type savedDecision struct {
	Decision
	ToolUseID string    `json:"toolUseId,omitempty"`
	Key       string    `json:"key,omitempty"`
	Opened    time.Time `json:"opened"`
	Prompt    bool      `json:"prompt,omitempty"`
}

func newDaemon(exe, socket, stateDir, masterDir string, claude claudeCLI) *daemon {
	return &daemon{
		exe:         exe,
		socket:      socket,
		stateDir:    stateDir,
		masterDir:   masterDir,
		maxDepth:    defaultMaxDepth,
		maxChildren: defaultMaxChildren,
		claude:      claude,
		nodes:       map[string]*Session{},
		started:     map[string]time.Time{},
		decisions:   map[string]*Decision{},
		orphans:     map[string]*Decision{},
		cleared:     map[string]chan struct{}{},
		proposed:    map[string]proposal{},
		wake:        make(chan struct{}),
		watchers:    map[*watcher]bool{},
	}
}

// The default caps keep a worker that hands its whole task to a child, which does the same, from spawning until the account's usage limit stops every session.
const (
	defaultMaxDepth    = 3
	defaultMaxChildren = 4
)

func stateDir() string {
	if p := os.Getenv("BOSUN_STATE"); p != "" {
		return p
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "bosun")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "bosun")
}

func claudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

func runDaemon(args []string) error {
	flags := flag.NewFlagSet("daemon", flag.ExitOnError)
	masterDir := flags.String("master-dir", filepath.Join(stateDir(), "master"), "directory the master runs in, which Claude Code must trust")
	masterModel := flags.String("master-model", "", "model of a master the daemon starts, such as fable, opus or sonnet; the user's default when empty")
	deskModel := flags.String("desk-model", "", "model of a front desk the daemon starts; the user's default when empty")
	workerModel := flags.String("worker-model", "", "model of a worker started without one; the user's default when empty")
	workerEffort := flags.String("worker-effort", "", "effort of a worker started without one: low, medium, high, xhigh or max; the user's default when empty")
	maxDepth := flags.Int("max-depth", defaultMaxDepth, "deepest level of the tree, where the master's children are at level 1")
	maxChildren := flags.Int("max-children", defaultMaxChildren, "most children each session runs at once, a stopped one included until it is killed")
	_ = flags.Parse(args)
	if *maxDepth < 1 || *maxChildren < 1 {
		return errors.New("-max-depth and -max-children must be at least 1")
	}
	if err := checkEffort(*workerEffort); err != nil {
		return fmt.Errorf("-worker-effort: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	d := newDaemon(exe, socketPath(), stateDir(), *masterDir, claudeExec{})
	d.masterModel, d.deskModel, d.workerModel = *masterModel, *deskModel, *workerModel
	d.workerEffort = *workerEffort
	d.maxDepth, d.maxChildren = *maxDepth, *maxChildren
	if err := d.fitsSockets(masterNode); err != nil {
		return fmt.Errorf("the state path %s is too long, so set BOSUN_STATE to a shorter one: %w", d.stateDir, err)
	}
	ln, err := listen(d.socket)
	if err != nil {
		return err
	}
	defer ln.Close()
	if d.exe, err = linkExe(d.stateDir, exe); err != nil {
		return err
	}
	if err := d.load(); err != nil {
		return err
	}
	go d.serve(ln)
	if err := d.ensureMaster(); err != nil {
		return err
	}
	if err := d.ensureDesk(); err != nil {
		log.Printf("starting the desk: %v", err)
		d.mu.Lock()
		d.openLocked(&Decision{Kind: "notice", Node: deskNode, Holder: "user", Note: fmt.Sprintf("The front desk could not start: %v. Until a restart of the daemon starts it, bosun tell goes to the master.", err)})
		d.mu.Unlock()
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			// The connections that close as the process exits must not withdraw the saved decisions.
			d.mu.Lock()
			d.stopping = true
			d.mu.Unlock()
			return nil
		case <-tick.C:
			if d.pollDue(time.Now()) {
				d.poll()
			}
			d.recoverLimited(time.Now())
			d.expireOrphans(time.Now())
		}
	}
}

func listen(path string) (net.Listener, error) {
	if conn, err := net.Dial("unix", path); err == nil {
		conn.Close()
		return nil, fmt.Errorf("a daemon already listens on %s", path)
	}
	_ = os.Remove(path)
	return net.Listen("unix", path)
}

// linkExe points dir/bosun at exe and returns the link, which every session's hooks and MCP server run.
// A session started by an earlier build so runs this one, and never a path that garbage collection removed.
func linkExe(dir, exe string) (string, error) {
	link := filepath.Join(dir, "bosun")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	_ = os.Remove(link + ".new")
	if err := os.Symlink(exe, link+".new"); err != nil {
		return "", err
	}
	return link, os.Rename(link+".new", link)
}

// load adopts the sessions, events and decisions of an earlier run, and rewrites the sessions' settings for this build, which each reads when its process next starts.
// The first poll then reports each turn that ended or stopped while no daemon ran.
func (d *daemon) load() error {
	data, err := os.ReadFile(filepath.Join(d.stateDir, "state.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st savedState
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("state.json: %w", err)
	}
	d.nextID = st.NextID
	now := time.Now()
	for _, s := range st.Sessions {
		d.nodes[s.Node] = s
		if s.Status == "starting" {
			d.started[s.Node] = now
		}
		if _, _, err := d.writeConfig(s); err != nil {
			log.Printf("%s: %v", s.Node, err)
		}
		switch {
		case !isWorker(s.Node) || s.Killed || s.Paused:
		case s.Heard.IsZero():
			// An earlier build kept no such time, and told the master of every turn it saw end.
			s.Heard = now
		default:
			// The transcript holds a turn whose start no daemon saw, as when it ran while none did.
			if tail, err := readTranscript(s.SessionID, 0); err == nil && tail.at.After(s.Heard) {
				s.Turn = true
			}
		}
	}
	d.events, d.taken, d.takenKey, d.paused, d.answered = st.Events, st.Taken, st.TakenKey, st.Paused, st.Answered
	for _, sd := range st.Decisions {
		dec := sd.Decision
		dec.toolUseID, dec.key, dec.opened, dec.prompt = sd.ToolUseID, sd.Key, sd.Opened, sd.Prompt
		dec.answer = make(chan answer, 1)
		d.decisions[dec.ID] = &dec
		if dec.Kind != "notice" {
			d.orphans[dec.ID] = &dec
		}
	}
	d.orphansUntil = now.Add(reconnectWait)
	return nil
}

// saveLocked writes the daemon's state, which must survive a power loss, as the next start reports what it lost.
func (d *daemon) saveLocked() {
	if d.stopping {
		return
	}
	st := savedState{NextID: d.nextID, Events: d.events, Taken: d.taken, TakenKey: d.takenKey, Answered: d.answered, Paused: d.paused}
	for _, n := range d.sortedLocked() {
		st.Sessions = append(st.Sessions, n)
	}
	for _, dec := range d.snapshotLocked().Decisions {
		o := d.decisions[dec.ID]
		st.Decisions = append(st.Decisions, savedDecision{Decision: dec, ToolUseID: o.toolUseID, Key: o.key, Opened: o.opened, Prompt: o.prompt})
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	if err := writeDurably(filepath.Join(d.stateDir, "state.json"), data); err != nil {
		log.Printf("saving state: %v", err)
	}
}

// writeDurably replaces the file at path with data, which a crash or power loss leaves whole, old or new.
func writeDurably(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(path+".tmp", path)
	}
	if err != nil {
		return err
	}
	if df, err := os.Open(dir); err == nil {
		_ = df.Sync()
		df.Close()
	}
	return nil
}

func (d *daemon) sortedLocked() []*Session {
	nodes := make([]*Session, 0, len(d.nodes))
	for _, n := range d.nodes {
		nodes = append(nodes, n)
	}
	slices.SortFunc(nodes, func(a, b *Session) int { return strings.Compare(a.Node, b.Node) })
	return nodes
}

func (s *Session) public() Session {
	c := *s
	c.pre, c.Events, c.Taken = nil, nil, nil
	return c
}

var notAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

// projectDir is the folder of Claude Code's config directory for the session's repository, or for its directory outside one: the transcripts of a session outside a repository land there.
// Claude Code adds a hash to a name over 200 characters and this does not, so such a repository's sessions keep memory apart from the user's.
func (s *Session) projectDir() string {
	return filepath.Join(claudeDir(), "projects", notAlnum.ReplaceAllString(cmp.Or(s.Repo, s.Dir), "-"))
}

// memoryDir is the auto memory directory Claude Code picks for the session, which the user's own sessions in the same repository or directory share.
func (s *Session) memoryDir() string {
	return filepath.Join(s.projectDir(), "memory")
}

// memoryDir is the auto memory directory of session s: the master's for the desk, which shares it, and Claude Code's own pick for every other session.
func (d *daemon) memoryDir(s *Session) string {
	if s.Node == deskNode {
		return (&Session{Dir: d.masterDir}).memoryDir()
	}
	return s.memoryDir()
}

// deskDir is where the desk runs. Claude Code trusts a directory outside a repository when it trusts one that holds it, so the master's trust covers it.
func (d *daemon) deskDir() string {
	return filepath.Join(d.masterDir, deskNode)
}

// tempDir is the node's TMPDIR.
func (d *daemon) tempDir(node string) string {
	return filepath.Join(d.stateDir, "nodes", node, "tmp")
}

// tempDirRoom is the most bytes a TMPDIR may hold beside the sockets Claude Code's sandbox creates in it.
// The longest is claude-socks-<16 hex>.sock, and Linux binds a unix socket path of at most 108 bytes.
var tempDirRoom = 108 - len("/claude-socks-0123456789abcdef.sock")

// fitsSockets fails when node's TMPDIR is too long for the sandbox's sockets, without which every Bash call fails.
func (d *daemon) fitsSockets(node string) error {
	if dir := d.tempDir(node); len(dir) > tempDirRoom {
		return fmt.Errorf("%s runs to %d bytes, and the sandbox's sockets need it at most %d", dir, len(dir), tempDirRoom)
	}
	return nil
}

// scratchDir is the folder within tempDir that Claude Code makes the sandbox's TMPDIR.
// The rest of tempDir, which holds files the session's own Claude Code process loads, stays read-only to the sandbox.
func (d *daemon) scratchDir(node string) string {
	return filepath.Join(d.tempDir(node), fmt.Sprintf("claude-%d", os.Getuid()))
}

// gate is the reserved check for node's calls that run in cwd, or nil for an unknown node.
func (d *daemon) gate(node, cwd string) *gate {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := d.nodes[node]
	if n == nil {
		return nil
	}
	home, _ := os.UserHomeDir()
	return &gate{cwd: cmp.Or(cwd, n.Dir), writable: d.writable(n), fenced: d.fenced(n), list: newUserList(home, claudeDir(), d.stateDir, os.Getenv), repos: d.repos()}
}

// fenced lists the folders within the session's own directory that it may not write: for the master, the desk's, whose settings and hooks would run outside the master's sandbox once the desk starts.
func (d *daemon) fenced(s *Session) []string {
	if s.Node == masterNode {
		return []string{d.deskDir()}
	}
	return nil
}

// repos holds the record of each repository's git setup from before bosun's first worker in it.
func (d *daemon) repos() gitRepos {
	return gitRepos{dir: filepath.Join(d.stateDir, "repos")}
}

// writable lists where the session's file tools write without the user: its worktree, its scratch folder, and the memory and job directories Claude Code's system prompt sends it to.
func (d *daemon) writable(s *Session) []string {
	dirs := []string{s.Dir, d.scratchDir(s.Node), d.memoryDir(s)}
	if s.ID != "" {
		dirs = append(dirs, filepath.Join(claudeDir(), "jobs", s.ID, "tmp"))
	}
	return dirs
}

func (d *daemon) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go d.handleConn(conn)
	}
}

type out struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (o *out) send(r Response) {
	o.mu.Lock()
	defer o.mu.Unlock()
	_ = o.enc.Encode(r)
}

// handleConn answers each request on conn in turn. The connection closing cancels the request in flight, which withdraws its decision.
func (d *daemon) handleConn(conn net.Conn) {
	defer conn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reqs := make(chan Request)
	go func() {
		defer cancel()
		dec := json.NewDecoder(conn)
		for {
			var r Request
			if dec.Decode(&r) != nil {
				return
			}
			select {
			case reqs <- r:
			case <-ctx.Done():
				return
			}
		}
	}()
	o := &out{enc: json.NewEncoder(conn)}
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-reqs:
			if r.Op == "watch" {
				d.watch(ctx, cancel, o)
				continue
			}
			o.send(d.handle(ctx, r))
		}
	}
}

func (d *daemon) handle(ctx context.Context, r Request) Response {
	resp := Response{Type: "result"}
	var err error
	switch r.Op {
	case "hook":
		resp, err = d.hook(ctx, r.Node, r.Socket, r.Key, r.Hook)
	case "ask":
		resp.Text, err = d.ask(ctx, r.Node, r.Key, r.Text)
	case "next_event":
		if r.Node != masterNode {
			err = errors.New("only the master waits for events")
		} else {
			resp.Events = d.nextEvent(ctx, r.Key)
		}
	case "approve":
		resp.Text, err = d.answer(r.Node, r.ID, r.Words, "permission", answer{behavior: "allow"})
	case "deny":
		resp.Text, err = d.answer(r.Node, r.ID, r.Words, "permission", answer{behavior: "deny", text: r.Text})
	case "reply":
		resp.Text, err = d.answer(r.Node, r.ID, r.Words, "question", answer{text: r.Text})
	case "escalate":
		err = d.escalate(r.Node, r.ID, r.Text)
	case "notify":
		err = d.notify(r.Node, r.Text)
	case "dismiss":
		resp.Text, err = d.answer(r.Node, r.ID, r.Words, "notice", answer{})
	case "tell":
		err = d.tell(r.Node, r.Text)
	case "transcript":
		resp.Text, err = d.transcript(r.Name, r.Turns)
	case "spawn":
		resp.Session, err = d.spawn(r.Node, r.Name, r.Repo, r.Model, r.Effort, r.Text)
	case "resume":
		err = d.resume(ctx, r.Node, r.Name, r.Model, r.Effort, r.Text)
	case "attach":
		err = d.attach(r.ID)
	case "attached":
		d.attached(ctx, r.ID)
	case "kill":
		err = d.kill(r.Name)
	case "merge":
		resp.Text, err = d.merge(ctx, r.Node, r.Name)
	case "model":
		err = d.switchModel(r.Model)
	case "pause":
		resp.Text, err = d.pause(ctx)
	case "unpause":
		resp.Text, err = d.unpause()
	case "status":
		d.mu.Lock()
		resp = d.snapshotLocked()
		d.mu.Unlock()
	default:
		err = fmt.Errorf("unknown op %q", r.Op)
	}
	resp.Type = cmp.Or(resp.Type, "result")
	if err != nil {
		resp.Error = err.Error()
	}
	return resp
}

type hookInput struct {
	Event       string          `json:"hook_event_name"`
	SessionID   string          `json:"session_id"`
	ToolName    string          `json:"tool_name"`
	ToolInput   json.RawMessage `json:"tool_input"`
	ToolUseID   string          `json:"tool_use_id"`
	LastMessage string          `json:"last_assistant_message"`
	Error       string          `json:"error"`
	Prompt      string          `json:"prompt"`
	Source      string          `json:"source"`
	Reason      string          `json:"reason"`
	Cwd         string          `json:"cwd"`
}

// hook handles one hook event of a session, whose process has the messaging socket socket, sent under key. It blocks while a decision it opened waits, and answers with no behavior when the decision is withdrawn.
func (d *daemon) hook(ctx context.Context, node, socket, key string, raw json.RawMessage) (Response, error) {
	resp := Response{Type: "result"}
	var in hookInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return resp, fmt.Errorf("hook input: %w", err)
	}
	var reserved reservation
	// The check may run git, which a worker's repository can hold up, so it runs outside the lock.
	if in.Event == "PreToolUse" {
		if g := d.gate(node, in.Cwd); g != nil {
			reserved = g.reserved(in.ToolName, in.ToolInput)
		}
	}
	d.mu.Lock()
	n := d.nodes[node]
	if n == nil {
		d.mu.Unlock()
		return resp, fmt.Errorf("unknown node %q", node)
	}
	if n.SessionID == "" {
		n.SessionID = in.SessionID
	}
	if socket != "" && socket != n.Socket {
		n.Socket = socket
		d.saveLocked()
	}
	d.stirred = true
	switch in.Event {
	case "UserPromptSubmit", "PreToolUse", "PermissionRequest", "PostToolUse", "PostToolUseFailure":
		d.turnLocked(n)
	}
	var dec *Decision
	var worktree string
	// peer is the node and session id a SendMessage goes to, which must run to take it, and peerSocket whether the message names its process's socket.
	var peer, peerID string
	var peerSocket bool
	switch in.Event {
	case "SessionStart":
		if n.Repo != "" {
			worktree = n.Dir
		}
		if in.Source == "clear" && !isWorker(node) {
			n.Conversation = in.SessionID
			d.saveLocked()
			if ch := d.cleared[node]; ch != nil {
				close(ch)
				delete(d.cleared, node)
			}
		}
		if node == deskNode && (in.Source == "clear" || in.Source == "startup") {
			// The desk's new conversation holds neither the answers it showed the user nor the user's yes to one.
			d.said, d.saidBefore = userMessage{}, time.Time{}
			clear(d.proposed)
		}
	case "UserPromptSubmit":
		if from, text, ok := peerMessage(in.Prompt); ok {
			d.messageLocked(Event{Kind: "message", Node: node, From: from, Text: text})
		} else if node == deskNode && !deskPromptFromBosun(in.Prompt) {
			d.saidLocked(in.Prompt, time.Now())
		}
	case "PreToolUse":
		// A permission still open from an earlier call was denied in the session's own prompt, which runs no tool.
		d.withdrawLocked(node, func(dec *Decision) bool { return dec.toolUseID != in.ToolUseID })
		if n.pre == nil {
			n.pre = map[string]preCall{}
		}
		n.pre[in.ToolUseID] = preCall{in.ToolName, compact(in.ToolInput)}
		if reserved.reason != "" {
			// The master's and the desk's own go to the user, since nobody sits above them.
			holder := "master"
			if reserved.user || !isWorker(node) {
				holder = "user"
			}
			dec = &Decision{Kind: "permission", Node: node, Holder: holder, Reserved: reserved.reason, UserOnly: reserved.user, Tool: in.ToolName, Input: in.ToolInput, Refused: n.refused, toolUseID: in.ToolUseID, key: key}
		}
		if in.ToolName != "SendMessage" {
			break
		}
		var msg struct {
			To string `json:"to"`
		}
		_ = json.Unmarshal(in.ToolInput, &msg)
		to, refusal := d.recipientLocked(msg.To)
		switch {
		case refusal != "":
			resp.Behavior, resp.Text = "deny", refusal
		case to == nil:
		case node == deskNode:
			resp.Behavior, resp.Text = "deny", "bosun: the desk changes nothing in the fleet itself, and a message would start a session's turn. Pass the user's instruction to the master with tell_master."
		case to.Node == deskNode && node == masterNode:
			resp.Behavior, resp.Text = "deny", "bosun: the desk takes no messages. Answer it with tell_desk."
		case to.Node == deskNode:
			resp.Behavior, resp.Text = "deny", "bosun: the desk takes no messages from workers. Put questions to the master with ask."
		case to.Node == masterNode:
			// The master refuses messages, but SendMessage reports success all the same, and the refusal starts the sender's next turn.
			resp.Behavior, resp.Text = "deny", "bosun: the master takes no messages. Ask it with ask, which waits for its answer."
		case to.Killed:
			resp.Behavior, resp.Text = "deny", fmt.Sprintf("bosun: the user stopped %s, so it takes no messages.", to.Node)
		// SendMessage would take the name of a session without an inbox as a prefix of another session's name.
		case to.Status == "exited":
			resp.Behavior, resp.Text = "deny", fmt.Sprintf("bosun: %s has exited, so it takes no messages.", to.Node)
		case to.ID == "" || to.held:
			resp.Behavior, resp.Text = "deny", fmt.Sprintf("bosun: %s is %v, so send the message again in a moment.", to.Node, errStarting)
		default:
			peer, peerID, peerSocket = to.Node, to.ID, socketAddress(msg.To)
		}
	case "PermissionRequest":
		holder := "master"
		if !isWorker(node) {
			holder = "user"
		}
		dec = &Decision{Kind: "permission", Node: node, Holder: holder, Tool: in.ToolName, Input: in.ToolInput, Refused: n.refused, toolUseID: n.match(in.ToolName, in.ToolInput), key: key}
	case "PermissionDenied":
		n.refused = cmp.Or(in.Reason, "it gave no reason")
	case "PostToolUse", "PostToolUseFailure":
		n.refused = ""
		delete(n.pre, in.ToolUseID)
		d.withdrawLocked(node, func(dec *Decision) bool { return dec.toolUseID == in.ToolUseID })
	case "StopFailure":
		// The turn ended on an API error, and Stop does not run.
		n.pre = nil
		d.withdrawLocked(node, func(*Decision) bool { return true })
		if node == masterNode {
			n.Turn = false
			d.requeueLocked()
		} else {
			d.heardLocked(n)
		}
		switch {
		case n.Paused && d.paused:
		case in.Error == "rate_limit":
			d.limitLocked(n, in.LastMessage)
			if node == masterNode {
				d.asleep = true
			}
			// The master may run on another model, which the limit spares.
			if node == deskNode {
				d.strandLocked(n, "stopped at a usage limit")
			}
		case node == deskNode:
			// The user may have told the desk through bosun tell, and so not be watching it. The notice stands in for the tells the turn took.
			n.Taken = nil
			d.openLocked(&Decision{Kind: "notice", Node: deskNode, Holder: "user", Note: fmt.Sprintf("The front desk's turn failed on an API error, %s: %s. bosun attach %s shows what it was doing.", in.Error, in.LastMessage, n.ID)})
		case node != masterNode:
			d.reportLocked(Event{Kind: "turn_end", Node: node, Text: fmt.Sprintf("Its turn failed on an API error, %s: %s", in.Error, in.LastMessage)})
		default:
			// Waking it at once could fail the same way without end, so the next event retries it.
			d.asleep = true
			d.openLocked(&Decision{Kind: "notice", Node: masterNode, Holder: "user", Note: fmt.Sprintf("The master's turn failed on an API error, %s: %s. The next event resumes it.", in.Error, in.LastMessage)})
		}
	case "Stop":
		n.pre = nil
		n.LimitedUntil = nil
		d.withdrawLocked(node, func(*Decision) bool { return true })
		switch {
		case n.Paused && d.paused:
			// The pause stopped the session in its turn, which unpausing resumes.
		case node == deskNode && len(n.Events) > 0:
			resp.Behavior, resp.Text = "block", deskReport(n.Events)
			d.toldLocked(n.Events)
			n.Taken, n.Events, n.Unseen = append(n.Taken, n.Events...), nil, true
			d.saveLocked()
		case node == deskNode:
			n.Turn, n.Taken = false, nil
			d.saveLocked()
		case node != masterNode && len(n.Events) > 0:
			// A worker runs no next_event, so news of its children waits for the end of its turn, which goes on with it.
			resp.Behavior, resp.Text = "block", childReport(n.Events)
			n.Taken, n.Events = append(n.Taken, n.Events...), nil
			d.saveLocked()
		case node != masterNode:
			n.Taken = nil
			d.heardLocked(n)
			d.reportLocked(Event{Kind: "turn_end", Node: node, Text: in.LastMessage})
		case !needsMaster(d.pendingLocked()):
			d.endMasterTurnLocked()
			d.asleep = true
			log.Print("the master sleeps until the next event")
		case !d.stopBlocked:
			resp.Behavior, resp.Text = "block", eventsWaiting
			d.stopBlocked = true
		default:
			// The block did not send the master back to next_event, and blocking it again might never end.
			d.endMasterTurnLocked()
			d.asleep = true
			d.openLocked(&Decision{Kind: "notice", Node: masterNode, Holder: "user", Note: fmt.Sprintf("The master ended its turn twice in a row while events waited, so it has not read them. claude attach %s shows why, and the next event resumes it.", n.ID)})
		}
	}
	if dec != nil {
		dec = d.adoptLocked(dec)
	}
	d.mu.Unlock()
	if worktree != "" {
		if err := standInGitmodules(worktree); err != nil {
			log.Printf("%s: .gitmodules: %v", node, err)
		}
	}
	if peer != "" && peerSocket {
		// A socket names one process, and a session that stopped comes back in another.
		if _, a, err := d.listed(peerID); err != nil || a.Status == "" {
			resp.Behavior, resp.Text = "deny", fmt.Sprintf("bosun: the process of %s at that address has stopped. Message it by its node name, which resumes it.", peer)
		}
		return resp, nil
	}
	if peer != "" {
		// A stopped session has no inbox, and SendMessage looks for one only after this hook returns.
		// The listing, which resumeIdle reads, may show a retirement that the last poll missed.
		switch err := d.resumeIdle(peerID); {
		case errors.Is(err, errStarting):
			resp.Behavior, resp.Text = "deny", fmt.Sprintf("bosun: %s is %v, so send the message again in a moment.", peer, errStarting)
		case err != nil:
			resp.Behavior, resp.Text = "deny", fmt.Sprintf("bosun: %s has stopped, and resuming it to take the message failed: %v", peer, err)
		}
		return resp, nil
	}
	if dec == nil {
		return resp, nil
	}
	if a, ok := d.wait(ctx, dec); ok {
		resp.Behavior, resp.Text = a.behavior, a.text
	}
	return resp, nil
}

// standInGitmodules gives a worktree that lacks one an empty .gitmodules, which the session's excludes keep out of commits.
// The sandbox of Claude Code 2.1.310 mounts /dev/null over a missing .gitmodules, which libgit2 cannot open, so every nix flake command in the repository fails.
func standInGitmodules(worktree string) error {
	path := filepath.Join(worktree, ".gitmodules")
	if _, err := os.Lstat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, nil, 0o644)
}

// sandboxStandIns are what the sandbox of Claude Code 2.1.310 mounts /dev/null over in a session's directory, where missing, plus its own bookkeeping.
// Git lists each as an untracked file it cannot add, so `git add -A` fails unless they are ignored.
var sandboxStandIns = []string{
	"/.bash_profile", "/.bashrc", "/.gitconfig", "/.gitmodules", "/.idea", "/.mcp.json", "/.profile", "/.ripgreprc", "/.vscode", "/.zprofile", "/.zshrc",
	"/.claude/.cc-writes/", "/.claude/agents", "/.claude/commands", "/.claude/hooks", "/.claude/launch.json", "/.claude/loop.md", "/.claude/output-styles",
	"/.claude/routines", "/.claude/scheduled_tasks.json", "/.claude/settings.json", "/.claude/settings.local.json", "/.claude/skills", "/.claude/workflows",
}

// writeExcludes writes the excludes file a session's git reads: the user's own, which core.excludesFile in the session's environment replaces, then the sandbox's stand-ins.
func writeExcludes(path string) error {
	own := ""
	if out, err := exec.Command("git", "config", "--global", "--path", "--get", "core.excludesFile").Output(); err == nil {
		own = strings.TrimSpace(string(out))
	} else if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		own = filepath.Join(dir, "git", "ignore")
	} else if home, err := os.UserHomeDir(); err == nil {
		own = filepath.Join(home, ".config", "git", "ignore")
	}
	data, err := os.ReadFile(own)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		data = append(data, '\n')
	}
	data = append(data, strings.Join(sandboxStandIns, "\n")+"\n"...)
	return os.WriteFile(path, data, 0o600)
}

// match finds the tool_use_id of the PreToolUse a PermissionRequest follows, which carries none of its own.
func (n *Session) match(tool string, input json.RawMessage) string {
	want := compact(input)
	var id string
	same := 0
	for k, p := range n.pre {
		if p.tool == tool && p.input == want {
			return k
		}
		if p.tool == tool {
			id = k
			same++
		}
	}
	if same == 1 {
		return id
	}
	return ""
}

var (
	crossSession = regexp.MustCompile(`(?s)^<cross-session-message\b([^>]*)>\n(.*)\n</cross-session-message>$`)
	attribute    = regexp.MustCompile(`([a-z-]+)="([^"]*)"`)
	// ref is what ListAgents shows after each name, and SendMessage takes along with it to pick one of the sessions of that name.
	ref = regexp.MustCompile(` \[[0-9A-Za-z]+\]$`)
	// subagentID and bareSocket are what SendMessage in Claude Code 2.1.310 takes for a subagent's id, which it looks up only among the sender's own subagents, and for a uds: address without its prefix.
	subagentID = regexp.MustCompile(`^a(?:[\w-]{1,63}-)?[0-9a-f]{16}$`)
	bareSocket = regexp.MustCompile(`^/\S*\.sock$`)
)

// peerMessage returns the sender and text of a prompt that Claude Code submits for another session's message.
func peerMessage(prompt string) (from, text string, ok bool) {
	m := crossSession.FindStringSubmatch(strings.TrimSpace(prompt))
	if m == nil {
		return "", "", false
	}
	attrs := map[string]string{}
	for _, a := range attribute.FindAllStringSubmatch(m[1], -1) {
		attrs[a[1]] = a[2]
	}
	return cmp.Or(attrs["from-name"], attrs["from"]), m[2], true
}

// socketAddress reports whether SendMessage takes to for a socket's path.
func socketAddress(to string) bool {
	return strings.HasPrefix(to, "uds:") || bareSocket.MatchString(to)
}

// recipientLocked returns the bosun session that a SendMessage's to addresses, by its node name or its socket, or nil for one of the sender's own subagents.
// Any other recipient may be a session outside bosun, whose auto mode would act on the message without bosun's reserved check, so it returns why the message may not go.
func (d *daemon) recipientLocked(to string) (*Session, string) {
	switch {
	case to == "main" || subagentID.MatchString(to):
		return nil, ""
	case socketAddress(to):
		path := strings.TrimPrefix(to, "uds:")
		for _, n := range d.nodes {
			if n.Socket != "" && n.Socket == path {
				return n, ""
			}
		}
		return nil, fmt.Sprintf("bosun: %s is the address of no running bosun session. Message a worker by its node name.", to)
	}
	if n := d.nodes[to]; n != nil {
		return n, ""
	}
	name := ref.ReplaceAllString(to, "")
	if !isWorker(name) && d.nodes[name] != nil {
		return d.nodes[name], ""
	}
	if name != to && d.nodes[name] != nil {
		// SendMessage asks for a ref only when another session has the node's name, and bosun cannot tell which session a ref picks.
		return nil, fmt.Sprintf("bosun: message %s by its bare node name, %q, without the ref. If SendMessage refuses the bare name because another session has it too, ask instead.", name, name)
	}
	var workers []string
	for _, n := range d.sortedLocked() {
		if isWorker(n.Node) && !n.Killed && n.Status != "exited" {
			workers = append(workers, n.Node)
		}
	}
	return nil, fmt.Sprintf("bosun: %q is not a bosun worker, and workers message only each other, by node name: %s.", to, strings.Join(workers, ", "))
}

func compact(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}

func (d *daemon) ask(ctx context.Context, node, key, question string) (string, error) {
	if question == "" {
		return "", errors.New("the question is empty")
	}
	d.mu.Lock()
	if d.nodes[node] == nil || !isWorker(node) {
		d.mu.Unlock()
		return "", errors.New("only a worker asks")
	}
	dec := d.adoptLocked(&Decision{Kind: "question", Node: node, Holder: "master", Question: question, key: key})
	d.mu.Unlock()
	a, ok := d.wait(ctx, dec)
	if !ok {
		return "", errors.New("the question was withdrawn")
	}
	return a.text, nil
}

// adoptLocked opens dec, unless a decision of an earlier run waits for the request that sends dec again under its key, and returns the decision to wait on.
func (d *daemon) adoptLocked(dec *Decision) *Decision {
	for id, o := range d.orphans {
		if dec.key != "" && o.key == dec.key {
			delete(d.orphans, id)
			log.Printf("decision %s: taken up again by %s", id, o.Node)
			return o
		}
	}
	d.openLocked(dec)
	return dec
}

func (d *daemon) openLocked(dec *Decision) {
	d.nextID++
	dec.ID = strconv.Itoa(d.nextID)
	dec.opened = time.Now()
	dec.answer = make(chan answer, 1)
	d.decisions[dec.ID] = dec
	d.saveLocked()
	c := *dec
	d.broadcastLocked(Event{Kind: "decision", Decision: &c})
	if dec.Holder == "master" {
		d.pushLocked(Event{Kind: "decision", Decision: &c})
	}
	log.Printf("decision %s: %s from %s, for the %s", dec.ID, dec.Kind, dec.Node, dec.Holder)
}

func (d *daemon) wait(ctx context.Context, dec *Decision) (answer, bool) {
	var expired <-chan time.Time
	if dec.Kind == "permission" && dec.Reserved == "" {
		t := time.NewTimer(time.Until(dec.opened.Add(promptAfter)))
		defer t.Stop()
		expired = t.C
	}
	select {
	case a := <-dec.answer:
		return a, !a.withdrawn
	case <-expired:
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.decisions[dec.ID] != dec {
			a := <-dec.answer
			return a, !a.withdrawn
		}
		id := ""
		if n := d.nodes[dec.Node]; n != nil {
			id = n.ID
		}
		d.promptLocked(dec, fmt.Sprintf("%s has waited for permission to run %s as long as bosun can hold a permission, so only its own prompt can answer it now: bosun attach %s", dec.Node, describe(*dec), id))
		return answer{}, false
	case <-ctx.Done():
		d.mu.Lock()
		if d.decisions[dec.ID] == dec {
			d.settleLocked(dec, answer{withdrawn: true})
		}
		d.mu.Unlock()
		return answer{}, false
	}
}

func (d *daemon) settleLocked(dec *Decision, a answer) {
	delete(d.decisions, dec.ID)
	delete(d.proposed, dec.ID)
	dec.answer <- a
	d.saveLocked()
	d.broadcastLocked(Event{Kind: "settled", ID: dec.ID})
	switch {
	case a.withdrawn:
		log.Printf("decision %s: withdrawn", dec.ID)
	case dec.Kind == "notice":
		log.Printf("decision %s: dismissed", dec.ID)
	default:
		log.Printf("decision %s: answered %s", dec.ID, cmp.Or(a.behavior, "with text"))
	}
}

// withdrawLocked settles the node's open permissions that match, except reserved ones, whose hook still blocks the call, and the notices that match of a permission waiting in the node's own prompt.
func (d *daemon) withdrawLocked(node string, match func(*Decision) bool) {
	for _, dec := range d.decisions {
		if dec.Node == node && (dec.Kind == "permission" && dec.Reserved == "" || dec.prompt) && match(dec) {
			d.settleLocked(dec, answer{withdrawn: true})
		}
	}
}

// promptLocked settles permission dec, whose hook can no longer answer it, and opens in its place a notice for the user, with note, until the session moves on.
func (d *daemon) promptLocked(dec *Decision, note string) {
	d.settleLocked(dec, answer{withdrawn: true})
	d.openLocked(&Decision{Kind: "notice", Node: dec.Node, Holder: "user", Note: note, toolUseID: dec.toolUseID, prompt: true})
}

// expireOrphans settles each decision of an earlier run whose hook or ask did not send its request again by orphansUntil, and so no longer waits on bosun.
// A permission whose session is still waiting then waits in the session's own prompt.
func (d *daemon) expireOrphans(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.orphans) == 0 || now.Before(d.orphansUntil) {
		return
	}
	for id, dec := range d.orphans {
		delete(d.orphans, id)
		n := d.nodes[dec.Node]
		switch {
		case d.decisions[id] != dec:
		case dec.Kind == "permission" && dec.Reserved == "" && n != nil && n.Status == "waiting":
			d.promptLocked(dec, fmt.Sprintf("bosun restarted while %s waited for permission to run %s, and the request did not come back, so only its own prompt can answer it now: bosun attach %s", dec.Node, describe(*dec), n.ID))
		default:
			d.settleLocked(dec, answer{withdrawn: true})
		}
	}
}

// answer settles a decision for caller: the master for a decision it holds, "" for the user, who may answer any, or the desk for the user, on the user's words.
func (d *daemon) answer(caller, id, words, kind string, a answer) (string, error) {
	if caller != "" && caller != masterNode && caller != deskNode {
		return "", errors.New("only the master, the desk or the user answers")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	dec := d.decisions[id]
	switch {
	case dec == nil:
		return "", fmt.Errorf("no open decision %s", id)
	case dec.Kind != kind:
		return "", fmt.Errorf("decision %s is a %s", id, dec.Kind)
	case kind == "question" && a.text == "":
		return "", errors.New("the answer is empty")
	case caller == masterNode && dec.Holder != "master":
		return "", fmt.Errorf("decision %s waits on the user", id)
	}
	if a.behavior == "deny" && a.text == "" {
		a.text = "The user denied this."
		if caller == masterNode {
			a.text = "The bosun master denied this."
		}
	}
	if caller == deskNode {
		return d.deskAnswerLocked(dec, words, a)
	}
	d.settleLocked(dec, a)
	return "", nil
}

// inFrontend is where the user answers what the desk may not.
const inFrontend = "so only the user answers it, in the bosun panel or bosun tui, or with bosun approve, deny or reply"

// deskAnswerLocked gives a to dec for the desk once the user has said yes to it: the desk proposes a, which hands it dec in full to show the user, and the user's next message, which words quote, says yes.
// A decision that opened after that message cannot have been shown before it. Without the yes, the call proposes a.
func (d *daemon) deskAnswerLocked(dec *Decision, words string, a answer) (string, error) {
	switch {
	case dec.Kind == "notice":
		return "", errors.New("only the user dismisses a notice")
	case dec.Holder != "user":
		return "", fmt.Errorf("decision %s waits on the master, which settles it. Pass on what the user wants done with it with tell_master", dec.ID)
	case dec.Node == deskNode:
		return "", fmt.Errorf("decision %s is your own, %s", dec.ID, inFrontend)
	case dec.UserOnly:
		return "", fmt.Errorf("decision %s is %s, which is on the user's list, %s", dec.ID, dec.Reserved, inFrontend)
	}
	p, ok := d.proposed[dec.ID]
	why := ""
	switch {
	case !ok:
	case p.answer != a:
		why = "It differs from the answer you last showed the user. "
	case words == "":
	case !p.at.Before(d.said.at):
		why = "The user has said nothing since you showed it. "
	case p.at.Before(d.saidBefore):
		// The user's latest message may answer whatever they wrote of in between.
		why = "The user has written to you more than once since you showed it. "
	case !strings.Contains(squash(d.said.text), squash(words)):
		return "", fmt.Errorf("the user's latest message does not hold %q: quote their words exactly", words)
	default:
		rec := Answered{Decision: *dec, Behavior: a.behavior, Text: a.text, Words: words, At: time.Now()}
		d.answered = append(d.answered, rec)
		d.answered = d.answered[max(0, len(d.answered)-keptMessages):]
		d.broadcastLocked(Event{Kind: "answered", Answered: &rec})
		log.Printf("decision %s: the desk answers it on the user's words %q", dec.ID, words)
		d.settleLocked(dec, a)
		return fmt.Sprintf("bosun %s decision %s on the user's word.", given(rec), dec.ID), nil
	}
	d.proposed[dec.ID] = proposal{a, time.Now()}
	data, _ := json.MarshalIndent(dec, "", "  ")
	return fmt.Sprintf("%sbosun has not answered decision %s, and holds your answer until the user says yes to it. Show the user the whole decision below, and that you will %s. Ask them whether to, and end your turn. Once their next message says yes, call answer again with the same answer and words quoted from that message.\n\n%s", why, dec.ID, proposed(dec.Kind, a), data), nil
}

// proposed says what answer a to a decision of kind does, completing "you will …".
func proposed(kind string, a answer) string {
	switch {
	case kind == "question":
		return "answer: " + a.text
	case a.behavior == "allow":
		return "allow it"
	}
	return "deny it, telling the session: " + a.text
}

// given says what the desk's answer did, as a verb.
func given(rec Answered) string {
	switch rec.Behavior {
	case "allow":
		return "allowed"
	case "deny":
		return "denied"
	}
	return "answered"
}

// squash collapses each run of white space in s to a space, since the desk may wrap a quote otherwise than the user did.
func squash(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// deskPromptFromBosun reports whether bosun, rather than the user, sent the desk prompt: the desk's first prompt, the tells and answers bosun hands it, the prompts that unpausing and a usage limit's reset resume it with, or a clear.
func deskPromptFromBosun(prompt string) bool {
	prompt = strings.TrimSpace(prompt)
	return strings.HasPrefix(prompt, "bosun: ") || slices.Contains([]string{deskStart, pauseReset, limitReset, "/clear"}, prompt)
}

// saidLocked notes a message of the user's to the desk, sent at at.
func (d *daemon) saidLocked(text string, at time.Time) {
	d.saidBefore = d.said.at
	d.said = userMessage{at: at, text: text}
}

// toldLocked notes each of the user's tells among evs, which bosun hands the desk, as a message of the user's. A tell given back from a cut turn was the user's message once already.
// A tell that a restart reloaded lost when it was sent, and so comes before every answer the desk proposes.
func (d *daemon) toldLocked(evs []Event) {
	for _, ev := range evs {
		if ev.From == "" && !ev.Again {
			d.saidLocked(ev.Text, ev.sent)
		}
	}
}

func (d *daemon) escalate(caller, id, note string) error {
	if caller != masterNode {
		return errors.New("only the master escalates")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	dec := d.decisions[id]
	if dec == nil {
		return fmt.Errorf("no open decision %s", id)
	}
	if dec.Holder != "master" {
		return fmt.Errorf("decision %s already waits on the user", id)
	}
	dec.Holder, dec.Note = "user", note
	d.saveLocked()
	c := *dec
	d.broadcastLocked(Event{Kind: "decision", Decision: &c})
	log.Printf("decision %s: escalated", id)
	return nil
}

// notify queues a notice from the master, which waits for the user to dismiss it and holds up no session.
func (d *daemon) notify(caller, text string) error {
	if caller != masterNode {
		return errors.New("only the master notifies the user")
	}
	if text == "" {
		return errors.New("the notice is empty")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.openLocked(&Decision{Kind: "notice", Node: masterNode, Holder: "user", Note: text})
	return nil
}

// tell passes on text from caller: the user's from a frontend to the desk, the desk's to the master, and the master's to the desk.
// When the desk could not start, its session is gone or a usage limit stopped it, the user's goes to the master and the master's to the user.
func (d *daemon) tell(caller, text string) error {
	if text == "" {
		return errors.New("nothing to tell")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	desk := d.nodes[deskNode]
	if desk != nil && (desk.Status == "exited" || desk.LimitedUntil != nil) {
		desk = nil
	}
	switch {
	case caller == deskNode:
		d.pushLocked(Event{Kind: "tell", From: deskNode, Text: text})
	case caller == masterNode && desk == nil:
		d.openLocked(&Decision{Kind: "notice", Node: masterNode, Holder: "user", Note: "The master answers: " + text})
	case caller == masterNode:
		d.queueLocked(desk, Event{Kind: "tell", From: masterNode, Text: text})
	case caller != "":
		return errors.New("only the user, the desk and the master tell")
	case desk == nil:
		d.pushLocked(Event{Kind: "tell", Text: text})
	default:
		d.queueLocked(desk, Event{Kind: "tell", Text: text, sent: time.Now()})
	}
	return nil
}

// deskReport is what bosun hands the desk: the user's tells and the master's answers.
func deskReport(evs []Event) string {
	var parts []string
	for _, ev := range evs {
		part := "bosun: the user sent this through bosun tell, so they may not be watching this session:\n"
		if ev.From == masterNode {
			part = "bosun: the master answers:\n"
		}
		if ev.Again {
			part = "bosun: a turn of yours that took this was cut short, so you may have acted on it already.\n" + part
		}
		parts = append(parts, part+ev.Text)
	}
	return strings.Join(parts, "\n\n")
}

// strandLocked hands on what waits for the desk, which cannot take it: the user's tells go to the master, and the master's answers to the user.
// The tells that a turn it was in took go with them.
func (d *daemon) strandLocked(desk *Session, why string) {
	evs := slices.Concat(desk.Taken, desk.Events)
	desk.Taken = nil
	if len(evs) == 0 {
		return
	}
	note := fmt.Sprintf("The front desk %s.", why)
	told := false
	for _, ev := range evs {
		if ev.From == masterNode {
			note += "\n\nThe master answered: " + ev.Text
		} else {
			d.pushLocked(Event{Kind: "tell", Text: ev.Text})
			told = true
		}
	}
	if told {
		note += "\n\nThe master got instead what you told the desk through bosun tell."
	}
	desk.Events = nil
	d.openLocked(&Decision{Kind: "notice", Node: deskNode, Holder: "user", Note: note})
}

// giveBackLocked puts the tells that the desk's cut turn took back in front of its queue, marked again, for the next poll to hand over.
// Tells cut short once already go on as strandLocked hands them, so that a desk whose every turn fails cannot take them again and again.
func (d *daemon) giveBackLocked(desk *Session) {
	switch {
	case len(desk.Taken) == 0:
		return
	case slices.ContainsFunc(desk.Taken, func(ev Event) bool { return ev.Again }):
		d.strandLocked(desk, "was cut short twice in a turn that took your tells")
		return
	}
	for i := range desk.Taken {
		desk.Taken[i].Again = true
	}
	log.Printf("the desk's turn was cut short, so %d tells it took wait again", len(desk.Taken))
	desk.Events, desk.Taken = append(desk.Taken, desk.Events...), nil
}

// messageLocked shows the frontends a message between sessions, and queues it for the master without waking it.
func (d *daemon) messageLocked(ev Event) {
	d.messages = append(d.messages, ev)
	d.messages = d.messages[max(0, len(d.messages)-keptMessages):]
	d.broadcastLocked(ev)
	if ev.From != masterNode {
		d.events = append(d.events, ev)
		d.saveLocked()
	}
	log.Printf("message from %s to %s", ev.From, ev.Node)
}

// needsMaster reports whether evs hold an event for the master to act on. A message waits for one, and comes along with it.
func needsMaster(evs []Event) bool {
	return slices.ContainsFunc(evs, func(ev Event) bool { return ev.Kind != "message" })
}

func (d *daemon) pushLocked(ev Event) {
	d.events = append(d.events, ev)
	d.saveLocked()
	close(d.wake)
	d.wake = make(chan struct{})
	// A master that a usage limit stopped wakes once the limit resets, and a paused one once the user unpauses.
	if m := d.nodes[masterNode]; d.asleep && !d.paused && (m == nil || m.LimitedUntil == nil) {
		d.asleep = false
		go d.wakeMaster()
	}
}

// reportLocked tells the parent of worker ev.Node that it ended its turn or stopped. The master hears through its events.
// A worker parent runs no next_event, so it hears at the end of its turn, or between turns in a new one. A parent the user stopped, or one gone, leaves its children to the master.
func (d *daemon) reportLocked(ev Event) {
	var p *Session
	if n := d.nodes[ev.Node]; n != nil {
		p = d.nodes[n.Parent]
	}
	if p == nil || p.Node == masterNode || p.Killed || p.Status == "exited" {
		d.pushLocked(ev)
		return
	}
	d.queueLocked(p, ev)
}

// queueLocked holds ev for n, a worker parent or the desk, which runs no next_event: n gets it at the end of its turn, or between turns in a new one.
func (d *daemon) queueLocked(n *Session, ev Event) {
	n.Events = append(n.Events, ev)
	d.saveLocked()
	log.Printf("%s of %s, for %s", ev.Kind, cmp.Or(ev.Node, ev.From, "the user"), n.Node)
	if betweenTurns(n.Status) {
		go d.deliver(n.Node)
	}
}

// orphanLocked leaves n's children to the master, since n will not act on their reports: it hands over those n has not read, or read in a turn still open, and tells of each other child between turns.
func (d *daemon) orphanLocked(n *Session) {
	told := map[string]bool{}
	for _, ev := range slices.Concat(n.Taken, n.Events) {
		d.pushLocked(ev)
		told[ev.Node] = true
	}
	n.Taken, n.Events = nil, nil
	for _, c := range d.sortedLocked() {
		if c.Parent == n.Node && !told[c.Node] && !c.Killed && c.Status != "exited" && betweenTurns(c.Status) {
			d.pushLocked(Event{Kind: "turn_end", Node: c.Node, Text: fmt.Sprintf("Its parent, %s, has stopped, so it reports to you now. Its last turn ended before then.", n.Node)})
		}
	}
}

// deliver starts a turn of name, a worker or the desk, between turns, that hands it what waits for it: a worker's children's reports, or the desk's tells.
// While it cannot start one, its next Stop, or the poll that finds it between turns, hands them over. When starting one fails, the master gets a worker's reports, and strandLocked the desk's tells.
// A stale desk moves to a new conversation first, as clearStaleDesk allows.
func (d *daemon) deliver(name string) {
	if name == deskNode {
		d.deskLock.Lock()
		defer d.deskLock.Unlock()
		d.mu.Lock()
		n := d.nodes[name]
		waiting := n != nil && len(n.Events) > 0
		d.mu.Unlock()
		if waiting {
			d.clearStaleDesk()
		}
	}
	d.mu.Lock()
	n := d.nodes[name]
	if n == nil || n.Killed || n.LimitedUntil != nil || d.paused || len(n.Events) == 0 {
		d.mu.Unlock()
		return
	}
	evs := n.Events
	n.Events = nil
	d.mu.Unlock()
	report := childReport(evs)
	if name == deskNode {
		report = deskReport(evs)
	}
	err := d.carryOn(context.Background(), name, report)
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case err == nil && name == deskNode:
		n.Taken, n.Unseen = append(n.Taken, evs...), true
		d.toldLocked(evs)
	case err == nil:
		n.Taken = append(n.Taken, evs...)
	case errors.Is(err, errInTurn) || errors.Is(err, errStarting) || errors.Is(err, errPaused):
		n.Events = append(evs, n.Events...)
	case name == deskNode:
		log.Printf("handing the desk its tells: %v", err)
		n.Events = append(evs, n.Events...)
		d.strandLocked(n, fmt.Sprintf("could not be resumed: %v", err))
	default:
		log.Printf("telling %s of its children: %v", name, err)
		n.Events = append(evs, n.Events...)
		d.orphanLocked(n)
	}
	d.saveLocked()
}

// childReport is what bosun tells a worker of its children.
func childReport(evs []Event) string {
	var b strings.Builder
	b.WriteString("bosun: news of your children.")
	for _, ev := range evs {
		switch {
		case ev.Kind == "turn_end":
			fmt.Fprintf(&b, "\n\n%s ended its turn:\n%s", ev.Node, ev.Text)
		case ev.Text != "":
			fmt.Fprintf(&b, "\n\n%s has stopped. %s", ev.Node, ev.Text)
		default:
			fmt.Fprintf(&b, "\n\n%s stopped in the middle of its turn. A message to it carries it on.", ev.Node)
		}
	}
	b.WriteString("\n\nOnce a child's work is right, merge it. To give it more to do, send it a message.")
	return b.String()
}

// requeueLocked puts the events the master took back in front of its queue, marked again, as the turn that took them was cut short.
func (d *daemon) requeueLocked() {
	if len(d.taken) == 0 {
		return
	}
	for i := range d.taken {
		d.taken[i].Again = true
	}
	d.events = append(d.taken, d.events...)
	log.Printf("the master's turn was cut short, so %d events it took wait again", len(d.taken))
	d.taken, d.takenKey = nil, ""
	d.saveLocked()
	close(d.wake)
	d.wake = make(chan struct{})
}

// endMasterTurnLocked notes that the master ended its turn of itself, having handled the events it took.
func (d *daemon) endMasterTurnLocked() {
	d.nodes[masterNode].Turn = false
	d.taken, d.takenKey = nil, ""
	d.saveLocked()
}

// heardLocked notes that worker n's parent has heard the end of its turn, which a restart then need not find in n's transcript.
func (d *daemon) heardLocked(n *Session) {
	n.Turn, n.Heard = false, time.Now()
}

// turnLocked notes a hook of n's turn, and saves the turn's start, which a power loss must not lose.
func (d *daemon) turnLocked(n *Session) {
	n.turnAt = time.Now()
	if !n.Turn {
		n.Turn = true
		d.saveLocked()
	}
}

// wakeMaster resumes the master asleep. A master found in a turn, as one is once the user types into it, or starting needs no waking: it reads the events before its turn ends.
// A stale master wakes in a new conversation.
func (d *daemon) wakeMaster() {
	d.masterLock.Lock()
	defer d.masterLock.Unlock()
	d.mu.Lock()
	if d.paused {
		// unpause wakes it.
		d.asleep = true
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	if d.stale(masterNode, time.Now()) {
		if err := d.clear(context.Background(), masterNode); err != nil {
			log.Printf("clearing the master's conversation: %v", err)
		}
	}
	err := d.carryOn(context.Background(), masterNode, eventsWaiting)
	if err == nil || errors.Is(err, errInTurn) || errors.Is(err, errStarting) || errors.Is(err, errPaused) {
		return
	}
	log.Printf("waking the master: %v", err)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.asleep = true
	id := d.nodes[masterNode].ID
	d.openLocked(&Decision{Kind: "notice", Node: masterNode, Holder: "user", Note: fmt.Sprintf("Waking the master failed, so its events wait unread: %v. The next event tries again. If it keeps failing, `claude rm %s` and a restart of the daemon start a new master, without the old one's conversation.", err, id)})
}

// stale reports whether node, the master or the desk, should wake in a new conversation: it has outlasted its prompt cache, so its next turn would read the whole transcript uncached.
// The master also needs no worker left for it to remember.
// Each runs in a directory of its own, outside any repository, so the transcripts in its project folder are its own.
func (d *daemon) stale(node string, now time.Time) bool {
	d.mu.Lock()
	n := d.nodes[node]
	busy := node == masterNode && slices.ContainsFunc(slices.Collect(maps.Keys(d.nodes)), isWorker)
	if n == nil || n.ID == "" || busy {
		d.mu.Unlock()
		return false
	}
	dir := n.projectDir()
	d.mu.Unlock()
	last, ok := lastWrite(dir)
	return ok && now.Sub(last) >= cacheLife
}

// lastWrite returns when Claude Code last wrote a transcript in dir, a project folder of its config directory, and whether it holds one.
func lastWrite(dir string) (time.Time, bool) {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	var last time.Time
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.ModTime().After(last) {
			last = fi.ModTime()
		}
	}
	return last, !last.IsZero()
}

// clear moves node, the master or the desk, to a new conversation in the same session, as /clear does, and leaves it idle.
func (d *daemon) clear(ctx context.Context, node string) error {
	cleared := make(chan struct{})
	d.mu.Lock()
	n := d.nodes[node]
	if n == nil {
		d.mu.Unlock()
		return fmt.Errorf("no node %q", node)
	}
	id, dir := n.ID, n.Dir
	d.cleared[node] = cleared
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		if d.cleared[node] == cleared {
			delete(d.cleared, node)
		}
		d.mu.Unlock()
	}()
	if _, err := d.restart(ctx, n, id, dir, "/clear", nil); err != nil {
		return err
	}
	timer := time.NewTimer(clearWait)
	defer timer.Stop()
	select {
	case <-cleared:
		log.Printf("cleared %s's conversation", node)
		return nil
	case <-timer.C:
		return fmt.Errorf("%s began no new conversation within %v of /clear", node, clearWait)
	}
}

// sleepLocked lets the master sleep until the next event, or wakes it now when events already wait and the fleet is not paused, since only a new event wakes it.
func (d *daemon) sleepLocked() {
	d.asleep = d.paused || !needsMaster(d.pendingLocked())
	if !d.asleep {
		go d.wakeMaster()
	}
}

// pendingLocked returns the master's events, leaving out decisions settled or escalated since.
func (d *daemon) pendingLocked() []Event {
	var evs []Event
	for _, ev := range d.events {
		if ev.Decision != nil {
			if cur := d.decisions[ev.Decision.ID]; cur == nil || cur.Holder != "master" {
				continue
			}
		}
		evs = append(evs, ev)
	}
	return evs
}

// nextEvent waits for an event the master acts on, and returns it with every other event waiting, the user's tells first.
// The call acknowledges the events the last call took, unless it is that call sent again under its key, which gets them again.
func (d *daemon) nextEvent(ctx context.Context, key string) []Event {
	begun := time.Now()
	d.mu.Lock()
	if key != "" && key == d.takenKey {
		evs := d.taken
		d.mu.Unlock()
		return evs
	}
	if d.taken != nil {
		d.taken, d.takenKey = nil, ""
		d.saveLocked()
	}
	d.stopBlocked, d.asleep = false, false
	if m := d.nodes[masterNode]; m != nil {
		m.LimitedUntil = nil
	}
	d.mu.Unlock()
	for {
		d.mu.Lock()
		evs := d.pendingLocked()
		ready := needsMaster(evs)
		d.events = nil
		if !ready {
			d.events = evs
		} else {
			evs = tellsFirst(evs)
			d.taken, d.takenKey = evs, key
			d.saveLocked()
		}
		wake := d.wake
		wait := idleReturn - time.Since(begun)
		if d.masterOpen > 0 {
			wait = min(wait, openReturn-min(time.Since(begun), time.Since(d.masterOpenAt)))
		}
		d.mu.Unlock()
		if ready {
			return evs
		}
		timer := time.NewTimer(wait)
		select {
		case <-wake:
			timer.Stop()
		case <-timer.C:
			return nil
		case <-ctx.Done():
			timer.Stop()
			return nil
		}
	}
}

// tellsFirst moves the tells in evs ahead of the other events, keeping the order within each.
func tellsFirst(evs []Event) []Event {
	var tells, rest []Event
	for _, ev := range evs {
		if ev.Kind == "tell" {
			tells = append(tells, ev)
		} else {
			rest = append(rest, ev)
		}
	}
	return append(tells, rest...)
}

func (d *daemon) watch(ctx context.Context, cancel context.CancelFunc, o *out) {
	w := &watcher{ch: make(chan Event, 256), cancel: cancel}
	d.mu.Lock()
	d.watchers[w] = true
	snap := d.snapshotLocked()
	d.mu.Unlock()
	o.send(snap)
	go func() {
		defer func() {
			d.mu.Lock()
			delete(d.watchers, w)
			d.mu.Unlock()
		}()
		for {
			select {
			case ev := <-w.ch:
				o.send(Response{Type: "event", Event: &ev})
			case <-ctx.Done():
				return
			}
		}
	}()
}

// broadcastLocked tells every frontend, and drops one too slow to keep up rather than block.
func (d *daemon) broadcastLocked(ev Event) {
	for w := range d.watchers {
		select {
		case w.ch <- ev:
		default:
			w.cancel()
			delete(d.watchers, w)
		}
	}
}

func (d *daemon) broadcastSessionsLocked() {
	ev := Event{Kind: "sessions"}
	for _, n := range d.sortedLocked() {
		ev.Sessions = append(ev.Sessions, n.public())
	}
	d.broadcastLocked(ev)
}

func (d *daemon) snapshotLocked() Response {
	resp := Response{Type: "snapshot", Sessions: []Session{}, Decisions: []Decision{}, Paused: d.paused}
	for _, n := range d.sortedLocked() {
		resp.Sessions = append(resp.Sessions, n.public())
	}
	for _, dec := range d.decisions {
		resp.Decisions = append(resp.Decisions, *dec)
	}
	resp.Messages = slices.Clone(d.messages)
	resp.Answered = slices.Clone(d.answered)
	slices.SortFunc(resp.Decisions, func(a, b Decision) int {
		x, _ := strconv.Atoi(a.ID)
		y, _ := strconv.Atoi(b.ID)
		return x - y
	})
	return resp
}

// spawn starts a child of caller, or of the master when a frontend asks. A child of the master needs repo; a deeper one branches from its parent's worktree.
func (d *daemon) spawn(caller, name, repo, model, effort, prompt string) (*Session, error) {
	caller = cmp.Or(caller, masterNode)
	if caller == deskNode {
		return nil, errors.New("the desk starts no sessions: pass the user's instruction to the master with tell_master")
	}
	if !nodeName.MatchString(name) || !isWorker(name) {
		return nil, fmt.Errorf("invalid node name %q: use lowercase letters, digits and hyphens, other than master and desk", name)
	}
	if err := d.fitsSockets(name); err != nil {
		return nil, fmt.Errorf("node name %q is too long: %w", name, err)
	}
	if prompt == "" {
		return nil, errors.New("the prompt is empty")
	}
	if err := checkEffort(effort); err != nil {
		return nil, err
	}
	d.mu.Lock()
	parent := d.nodes[caller]
	if parent == nil {
		d.mu.Unlock()
		return nil, fmt.Errorf("unknown node %q", caller)
	}
	if d.paused {
		d.mu.Unlock()
		return nil, errPaused
	}
	dir, root := parent.Dir, parent.Repo
	d.mu.Unlock()
	if caller == masterNode {
		if repo == "" {
			return nil, errors.New("a worker the master starts needs a repo")
		}
		var err error
		if dir, err = filepath.Abs(repo); err != nil {
			return nil, err
		}
		if root, err = repoRoot(dir); err != nil {
			return nil, err
		}
	}
	worktree := filepath.Join(root, ".claude", "worktrees", name)
	if _, err := os.Stat(worktree); err == nil {
		return nil, fmt.Errorf("%s already exists", worktree)
	}

	// An empty branch, for a detached HEAD, leaves the worker nothing to merge into.
	branch, _ := gitOut(dir, "symbolic-ref", "--short", "-q", "HEAD")

	d.mu.Lock()
	err := d.admitLocked(caller, name)
	// Once a worker runs in the repository, its sandbox may have changed the setup.
	fresh := !slices.ContainsFunc(slices.Collect(maps.Values(d.nodes)), func(s *Session) bool { return s.Repo == root })
	if err == nil {
		d.nodes[name] = &Session{Node: name, Parent: caller, Repo: root, Dir: worktree, Base: dir, BaseBranch: branch, Model: cmp.Or(model, d.workerModel), Effort: cmp.Or(effort, d.workerEffort), Depth: d.nodes[caller].Depth + 1, Status: "starting", Heard: time.Now()}
		d.started[name] = time.Now()
	}
	n := d.nodes[name]
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if fresh {
		if err := d.repos().record(root); err != nil {
			log.Printf("recording the git setup of %s: %v", root, err)
		}
	}

	id, err := d.start(n, dir, []string{"-w", name}, workerPrompt(name), prompt)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		delete(d.nodes, name)
		return nil, err
	}
	n.ID = id
	d.saveLocked()
	d.broadcastSessionsLocked()
	log.Printf("spawned %s (%s) under %s", name, id, caller)
	c := n.public()
	return &c, nil
}

// admitLocked applies the caps on depth and fan-out. Node names are unique across the daemon, which keeps them unique within each repository.
func (d *daemon) admitLocked(parent, name string) error {
	if d.nodes[name] != nil {
		return fmt.Errorf("node %q exists", name)
	}
	if d.nodes[parent].Depth >= d.maxDepth {
		return fmt.Errorf("%s is at the depth cap of %d", parent, d.maxDepth)
	}
	live := 0
	for _, n := range d.nodes {
		if n.Parent == parent && n.Status != "exited" && !n.Killed {
			live++
		}
	}
	if live >= d.maxChildren {
		return fmt.Errorf("%s already runs %d children", parent, d.maxChildren)
	}
	return nil
}

func repoRoot(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return "", fmt.Errorf("%s is not in a git repository", dir)
	}
	return filepath.Dir(strings.TrimSpace(string(out))), nil
}

// start runs `claude --bg` for n in dir, with bosun's settings and MCP server.
func (d *daemon) start(n *Session, dir string, extra []string, system, prompt string) (string, error) {
	settings, mcp, err := d.writeConfig(n)
	if err != nil {
		return "", err
	}
	args := append([]string{"--bg", "-n", n.Node}, extra...)
	if n.Model != "" {
		args = append(args, "--model", n.Model)
	}
	if n.Effort != "" {
		args = append(args, "--effort", n.Effort)
	}
	if n.Node == masterNode {
		// The settings name the connectors and plugins they turn off, and this keeps out the MCP servers they cannot name.
		args = append(args, "--strict-mcp-config")
	}
	args = append(args, "--settings", settings, "--mcp-config", mcp, "--append-system-prompt", system+"\n\n"+tempFiles(d.scratchDir(n.Node)), "--", prompt)
	return d.claude.Start(dir, args)
}

type hookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

type hookGroup struct {
	Hooks []hookCommand `json:"hooks"`
}

func (d *daemon) writeConfig(n *Session) (settingsPath, mcpPath string, err error) {
	node := n.Node
	dir := filepath.Join(d.stateDir, "nodes", node)
	if err := os.MkdirAll(d.tempDir(node), 0o700); err != nil {
		return "", "", err
	}
	command := fmt.Sprintf("BOSUN_NODE=%s BOSUN_SOCKET=%s %s hook", node, shellQuote(d.socket), shellQuote(d.exe))
	hold := []hookGroup{{Hooks: []hookCommand{{Type: "command", Command: command, Timeout: int(holdTimeout / time.Second)}}}}
	quick := []hookGroup{{Hooks: []hookCommand{{Type: "command", Command: command}}}}
	permissions := map[string][]string{"allow": {"mcp__bosun"}, "ask": rebuildRules}
	if isWorker(node) {
		permissions["deny"] = []string{"AskUserQuestion"}
	}
	excludes := filepath.Join(dir, "gitignore")
	if err := writeExcludes(excludes); err != nil {
		return "", "", err
	}
	filesystem := map[string]any{
		"allowWrite": []string{"~/.cache/go-build", "~/.cache/nix", "~/.cache/treefmt"},
		"denyRead":   d.hiddenSockets(),
	}
	// Claude Code's sandbox guards the .claude and .mcp.json of the session's own directory alone.
	if fenced := d.fenced(n); len(fenced) > 0 {
		filesystem["denyWrite"] = fenced
	}
	settings := map[string]any{
		"worktree": map[string]string{"baseRef": "head"},
		// Claude Code's default, or the master's for the desk, set so that the reserved check spares exactly the directory the session writes.
		// Claude Code reads it from --settings, which only managed settings outrank.
		"autoMemoryDirectory": d.memoryDir(n),
		"permissions":         permissions,
		"env": map[string]string{
			// Claude Code moves an MCP call still running after 120 seconds to the background, so ask and next_event would stop blocking the turn.
			// The server option disableAutoBackground does not help: it applies only to SDK servers.
			"CLAUDE_CODE_MCP_AUTO_BACKGROUND_MS": "0",
			// Only this session's git reads it, unlike the repository's info/exclude, which every worktree and the main checkout share.
			"GIT_CONFIG_COUNT":   "1",
			"GIT_CONFIG_KEY_0":   "core.excludesFile",
			"GIT_CONFIG_VALUE_0": excludes,
			// Otherwise every session shares the sandbox's temporary folder, and each writes there.
			// CLAUDE_CODE_TMPDIR would move Claude Code's own files but leave the sandbox's folder in /tmp.
			"TMPDIR": d.tempDir(node),
		},
		"sandbox": map[string]any{
			"enabled":           true,
			"failIfUnavailable": true,
			// The sandbox confines where a command writes, not what it does to the worktree, so auto mode and the master still judge it.
			"autoAllowBashIfSandboxed": false,
			// Linux cannot open a single socket path, and nix needs its daemon's, so the sockets a command could act through are hidden instead.
			"network":    map[string]any{"allowAllUnixSockets": true},
			"filesystem": filesystem,
		},
		"hooks": map[string]any{
			"SessionStart":       quick,
			"UserPromptSubmit":   quick,
			"PreToolUse":         hold,
			"PermissionRequest":  hold,
			"PermissionDenied":   quick,
			"PostToolUse":        quick,
			"PostToolUseFailure": quick,
			"Stop":               quick,
			"StopFailure":        quick,
		},
	}
	if !isWorker(node) {
		// A message would start a turn outside the next_event loop, or wait unread until next_event returns, so workers reach the master through ask.
		// The desk passes on to the master what it is told, so only bosun and the user reach it, and a session outside bosun cannot pose as the user.
		settings["crossSessionInbound"] = "refuse"
	}
	if node == masterNode {
		// Every request carries each tool the session holds, and a clear starts its new conversation from these settings.
		permissions["deny"] = masterDenied
		settings["disableClaudeAiConnectors"] = true
		plugins := map[string]bool{}
		for _, p := range userPlugins() {
			plugins[p] = false
		}
		settings["enabledPlugins"] = plugins
	}
	mcp := map[string]any{"mcpServers": map[string]any{"bosun": map[string]any{
		"type":       "stdio",
		"command":    d.exe,
		"args":       []string{"mcp"},
		"env":        map[string]string{"BOSUN_NODE": node, "BOSUN_SOCKET": d.socket},
		"alwaysLoad": true,
	}}}
	settingsPath = filepath.Join(dir, "settings.json")
	mcpPath = filepath.Join(dir, "mcp.json")
	if err := writeJSON(settingsPath, settings); err != nil {
		return "", "", err
	}
	return settingsPath, mcpPath, writeJSON(mcpPath, mcp)
}

// masterDenied are the built-in tools the master goes without. The desk and the workers keep them.
var masterDenied = []string{
	// Its listing of the user's skills rides on every request, and the desk does the work they serve.
	// Without it, Artifact and Workflow take the guidance a skill would load into their own descriptions, so all three go together.
	"Skill",
	// It publishes pages for the user, whom the desk talks to.
	"Artifact",
	// It orchestrates subagents on the user's word, and the master orchestrates workers.
	"Workflow",
	// It drafts feedback on Claude Code for a person to send.
	"SendFeedback",
	// It paces /loop, and next_event wakes the master.
	"ScheduleWakeup",
	// It reports a code review that a skill runs.
	"ReportFindings",
	// The master works in no repository.
	"EnterWorktree",
	// Nobody watches the master, which reaches the user through escalate.
	"AskUserQuestion",
}

// userPlugins lists the plugins of the user's Claude Code: those installed, and those the user's settings enable, built-in ones included.
// A file that is missing or does not parse adds none.
func userPlugins() []string {
	var installed struct {
		Plugins map[string]json.RawMessage `json:"plugins"`
	}
	var settings struct {
		Enabled map[string]json.RawMessage `json:"enabledPlugins"`
	}
	for path, v := range map[string]any{
		filepath.Join(claudeDir(), "plugins", "installed_plugins.json"): &installed,
		filepath.Join(claudeDir(), "settings.json"):                     &settings,
	} {
		if data, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(data, v)
		}
	}
	plugins := slices.Concat(slices.Collect(maps.Keys(installed.Plugins)), slices.Collect(maps.Keys(settings.Enabled)))
	slices.Sort(plugins)
	return slices.Compact(plugins)
}

// hiddenSockets lists the sockets, and folders of them, through which a sandboxed command could act outside the sandbox.
func (d *daemon) hiddenSockets() []string {
	uid := os.Getuid()
	return []string{
		// A worker would answer its own decisions.
		d.socket,
		// The user's services: systemd, the session bus, the compositor, the ssh agent, and each Claude Code session's peer socket.
		cmp.Or(os.Getenv("XDG_RUNTIME_DIR"), fmt.Sprintf("/run/user/%d", uid)),
		// Claude Code's daemon, which starts sessions, and the terminals of the sessions it runs.
		filepath.Join(os.TempDir(), fmt.Sprintf("cc-daemon-%d", uid)),
		"/tmp/.X11-unix",
		// The system bus, whose services act for the user wherever polkit allows it.
		"/run/dbus",
		// systemd's local sshd.
		"/run/ssh-unix-local",
		// The singletons of Chromium-based browsers, which open what they are sent. Claude Code expands the glob.
		"/tmp/.com.*",
		"/tmp/.org.*",
	}
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// kill stops a session. Claude Code lists a stopped worker just as it lists one it retired, so the daemon marks it, and the master may not resume it.
// The master stays stopped only until it has events to read, as poll finds, and the desk until a tell or an answer waits for it.
func (d *daemon) kill(name string) error {
	d.mu.Lock()
	n := d.nodes[name]
	var id string
	var killed, held bool
	if n != nil {
		id, killed, held = n.ID, n.Killed, n.held
		// Marked first, so that a poll that sees the worker stopped tells its parent nothing of its own.
		n.Killed = killed || (id != "" && !held && isWorker(name))
		if n.Killed {
			d.heardLocked(n)
			n.Paused = false
		}
		// Given back, the tells its turn took would resume the desk the user stopped.
		if name == deskNode {
			n.Taken = nil
		}
	}
	d.stirred = true
	d.mu.Unlock()
	switch {
	case n == nil:
		return fmt.Errorf("unknown node %q", name)
	case id == "" || held:
		return fmt.Errorf("%s is %w", name, errStarting)
	}
	err := d.claude.Stop(id)
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case err != nil:
		n.Killed = killed
	case n.Killed && !killed:
		d.reportLocked(Event{Kind: "exit", Node: name, Text: "The user stopped it."})
		d.orphanLocked(n)
		d.saveLocked()
		d.broadcastSessionsLocked()
	}
	return err
}

// merge fast-forwards the branch that worker name began from to the worker's branch, then removes the worker: its session, worktree and branch.
// Its parent or the master may merge it, since the master hears of a worker whose parent the user stopped.
func (d *daemon) merge(ctx context.Context, caller, name string) (string, error) {
	d.mu.Lock()
	n := d.nodes[name]
	var id string
	err := d.mergeableLocked(caller, name)
	if err == nil {
		id = n.ID
	}
	d.mu.Unlock()
	if errors.Is(err, errGone) {
		return d.dropGone(n)
	}
	if err != nil {
		return "", err
	}
	a, err := d.awaitIdle(ctx, name, id)
	if err != nil {
		return "", err
	}
	if err := d.hold(n, id); err != nil {
		return "", err
	}
	defer d.release(n)
	// A session between turns may begin one, as it does when the user types into it, so it stops before its worktree is judged.
	if a.Status != "" {
		if err := d.claude.Stop(id); err != nil {
			return "", err
		}
		if err := d.awaitExit(ctx, name, a.PID); err != nil {
			return "", err
		}
	}
	d.mu.Lock()
	err = d.childrenLocked(name)
	d.mu.Unlock()
	if err != nil {
		return "", err
	}
	text, err := d.land(n, id)
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dropLocked(name)
	d.saveLocked()
	d.broadcastSessionsLocked()
	log.Printf("merged and removed %s", name)
	return text, nil
}

// errGone marks a worker whose session Claude Code no longer lists.
var errGone = errors.New("gone")

func (d *daemon) mergeableLocked(caller, name string) error {
	n := d.nodes[name]
	switch {
	case n == nil || !isWorker(name):
		return fmt.Errorf("no worker %q", name)
	case caller != n.Parent && caller != masterNode:
		return fmt.Errorf("%s is %s's child, not yours", name, n.Parent)
	case n.Killed:
		return fmt.Errorf("the user stopped %s, so only the user can merge or remove it", name)
	case n.Base == "":
		return fmt.Errorf("bosun recorded no checkout that %s began from", name)
	case n.Status == "exited":
		return errGone
	case n.ID == "" || n.Status == "starting" || n.held:
		return fmt.Errorf("%s is %w", name, errStarting)
	case n.BaseBranch == "":
		return fmt.Errorf("%s began from a detached HEAD in %s, so it has no branch to merge into", name, n.Base)
	}
	return d.childrenLocked(name)
}

// childrenLocked refuses to merge name while a child of name's lives on, whose worktree began from name's.
func (d *daemon) childrenLocked(name string) error {
	for _, c := range d.nodes {
		switch {
		case c.Parent != name || c.Status == "exited":
		case c.Killed:
			return fmt.Errorf("%s has a child the user stopped, %s, which only the user can remove, with `claude rm %s`", name, c.Node, c.ID)
		default:
			return fmt.Errorf("%s still has a child, %s, which must merge into it first", name, c.Node)
		}
	}
	return nil
}

// dropLocked forgets node name and its children, whose sessions are gone.
func (d *daemon) dropLocked(name string) {
	for _, c := range d.nodes {
		if c.Parent == name {
			d.dropLocked(c.Node)
		}
	}
	delete(d.nodes, name)
	delete(d.started, name)
	if err := os.RemoveAll(filepath.Join(d.stateDir, "nodes", name)); err != nil {
		log.Printf("%s: %v", name, err)
	}
}

// dropGone forgets worker n, whose session Claude Code no longer lists, unless its branch holds commits that its base branch lacks.
func (d *daemon) dropGone(n *Session) (string, error) {
	branch := "worktree-" + n.Node
	if tip, err := gitOut(n.Base, "rev-parse", "--verify", "-q", branch+"^{commit}"); err == nil {
		if _, err := gitOut(n.Base, "merge-base", "--is-ancestor", tip, n.BaseBranch); err != nil {
			return "", fmt.Errorf("%s's session is gone, but %s holds commits that %s lacks, so the user must merge or delete it", n.Node, branch, n.BaseBranch)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.childrenLocked(n.Node); err != nil {
		return "", err
	}
	d.dropLocked(n.Node)
	d.saveLocked()
	d.broadcastSessionsLocked()
	return fmt.Sprintf("%s's session was already removed, so bosun forgot it.", n.Node), nil
}

// land fast-forwards n's base branch to n's branch, whose session id has stopped, and then removes the session, its worktree and its branch.
func (d *daemon) land(n *Session, id string) (string, error) {
	branch := "worktree-" + n.Node
	common, err := gitOut(n.Base, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	// A session can point its worktree's .git file at a repository of its own, whose config git would act on.
	if theirs, err := gitOut(n.Dir, "rev-parse", "--path-format=absolute", "--git-common-dir"); err != nil || theirs != common {
		return "", fmt.Errorf("%s's worktree %s is not one of %s's repository, so bosun leaves it to the user: %v", n.Node, n.Dir, n.Base, err)
	}
	if head, err := gitOut(n.Dir, "symbolic-ref", "--short", "HEAD"); err != nil || head != branch {
		return "", fmt.Errorf("%s's worktree has %q checked out rather than %s, so bosun leaves it to the user", n.Node, head, branch)
	}
	tip, err := gitOut(n.Dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	dropStandIn(n.Dir)
	dirty, err := gitOut(n.Dir, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if dirty != "" {
		return "", fmt.Errorf("%s has uncommitted changes, which it must commit or drop before it merges:\n%s", n.Node, dirty)
	}
	if cur, err := gitOut(n.Base, "symbolic-ref", "--short", "HEAD"); err != nil || cur != n.BaseBranch {
		return "", fmt.Errorf("%s began from %s in %s, which now has %q checked out, so the user must switch it back before %s merges", n.Node, n.BaseBranch, n.Base, cur, n.Node)
	}
	merged, err := isAncestor(n.Base, tip, "HEAD")
	if err != nil {
		return "", err
	}
	if !merged {
		if ahead, err := isAncestor(n.Base, "HEAD", tip); err != nil || !ahead {
			return "", fmt.Errorf("%s has fallen behind %s, so it must rebase onto %s before it merges: %v", branch, n.BaseBranch, n.BaseBranch, err)
		}
		// Git would otherwise overwrite the user's ignored files, or stash the user's changes and leave conflicts in them.
		if _, err := gitOut(n.Base, "-c", "merge.autostash=false", "merge", "--ff-only", "--no-overwrite-ignore", tip); err != nil {
			return "", fmt.Errorf("the checkout %s refuses the merge, which only the user can clear: %w", n.Base, err)
		}
	}
	// A worker that moved to a new id left its first id, which owns the worktree, stopped and listed.
	for _, old := range n.Retired {
		if err := d.remove(old, tip); err != nil {
			log.Printf("removing %s's earlier session %s: %v", n.Node, old, err)
		}
	}
	if err := d.remove(id, tip); err != nil {
		return "", fmt.Errorf("merged %s into %s, but removing %s failed: %w", branch, n.BaseBranch, n.Node, err)
	}
	return fmt.Sprintf("Merged %s into %s in %s, up to %.12s, and removed %s with its worktree and branch.", branch, n.BaseBranch, n.Base, tip, n.Node), nil
}

// dropStandIn deletes the empty .gitmodules that standInGitmodules wrote, which claude rm would refuse as an untracked file.
func dropStandIn(dir string) {
	path := filepath.Join(dir, ".gitmodules")
	if fi, err := os.Stat(path); err != nil || fi.Size() != 0 {
		return
	}
	if _, err := gitOut(dir, "ls-files", "--error-unmatch", ".gitmodules"); err != nil {
		_ = os.Remove(path)
	}
}

// remove runs claude rm on the stopped session id, whose worktree holds no commit past tip.
func (d *daemon) remove(id, tip string) error {
	err := d.claude.Remove(id, "")
	if err == nil {
		return nil
	}
	// A branch merged only locally still has commits on no remote, which claude rm discards only when told which.
	m := discardHint.FindStringSubmatch(err.Error())
	if m == nil || m[1] != tip {
		return err
	}
	return d.claude.Remove(id, m[1]+"@"+m[2])
}

// isAncestor reports whether commit a is an ancestor of commit b in dir's repository.
func isAncestor(dir, a, b string) (bool, error) {
	_, err := gitOut(dir, "merge-base", "--is-ancestor", a, b)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return err == nil, err
}

// gitOut runs git in dir with hooks and fsmonitor off, since a worker's commits or worktree could otherwise make the daemon run its code.
func gitOut(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-C", dir}, args...)...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, text)
	}
	return text, nil
}

// efforts are the levels claude --effort takes.
var efforts = []string{"low", "medium", "high", "xhigh", "max"}

// checkEffort accepts an empty effort, the user's default, or a level claude --effort takes.
func checkEffort(effort string) error {
	if effort == "" || slices.Contains(efforts, effort) {
		return nil
	}
	return fmt.Errorf("unknown effort %q: use %s", effort, strings.Join(efforts, ", "))
}

// resume gives a worker that ended its turn a new prompt, on another model or effort when asked.
func (d *daemon) resume(ctx context.Context, caller, name, model, effort, prompt string) error {
	if caller != masterNode {
		return errors.New("only the master resumes a worker")
	}
	if prompt == "" {
		return errors.New("the prompt is empty")
	}
	if !isWorker(name) {
		return fmt.Errorf("no worker %q", name)
	}
	if err := checkEffort(effort); err != nil {
		return err
	}
	return d.carryOnAs(ctx, name, prompt, model, effort)
}

// carryOn gives a session that ended its turn, or stopped, a new prompt, which continues its conversation under the same id with its saved options.
func (d *daemon) carryOn(ctx context.Context, name, prompt string) error {
	return d.carryOnAs(ctx, name, prompt, "", "")
}

// carryOnAs is carryOn on another model or effort, where either differs from the session's. The session then moves to a copy of its conversation under a new id, as switchModel moves the master.
func (d *daemon) carryOnAs(ctx context.Context, name, prompt, model, effort string) error {
	d.mu.Lock()
	n := d.nodes[name]
	var id, dir, status string
	var killed, held bool
	var to *Session
	if n != nil {
		id, dir, status, killed, held = n.ID, n.Dir, n.Status, n.Killed, n.held
		if model != "" && model != n.Model || effort != "" && effort != n.Effort {
			next := *n
			next.Model, next.Effort = cmp.Or(model, n.Model), cmp.Or(effort, n.Effort)
			to = &next
		}
	}
	d.mu.Unlock()
	switch {
	case n == nil:
		return fmt.Errorf("no node %q", name)
	case id == "" || status == "starting" || held:
		// The listing may still show a session it just started as stopped, and a second resume would start a copy.
		return fmt.Errorf("%s is %w", name, errStarting)
	case status == "exited":
		return fmt.Errorf("%s has exited", name)
	case killed:
		return fmt.Errorf("the user stopped %s", name)
	}
	next, err := d.restart(ctx, n, id, dir, prompt, to)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		return err
	}
	if to != nil {
		// The first id of a worker that moved began its worktree, which only that id's removal deletes.
		n.Retired = append(n.Retired, id)
		n.ID, n.SessionID, n.Conversation, n.Model, n.Effort = next, "", "", to.Model, to.Effort
		id = next
	}
	n.Status, n.State, n.pre, n.Paused = "starting", "", nil, false
	d.started[name] = time.Now()
	d.saveLocked()
	d.broadcastSessionsLocked()
	log.Printf("resumed %s (%s)", name, id)
	return nil
}

// restart resumes n, still session id, with prompt once it ends its turn, holding it meanwhile. It returns the id the session has after, which differs from id only when to, the session as it should run, names another model or effort.
// A failed move leaves the session stopped under its own id, resumed idle, since a stop preceded it.
func (d *daemon) restart(ctx context.Context, n *Session, id, dir, prompt string, to *Session) (string, error) {
	a, err := d.awaitIdle(ctx, n.Node, id)
	if err != nil {
		return "", err
	}
	// Claude Code resumes a running session into a copy under a new id, so a running session stops first.
	if err := d.hold(n, id); err != nil {
		return "", err
	}
	defer d.release(n)
	stopped := false
	if a.Status != "" {
		if err := d.claude.Stop(id); err != nil {
			return "", err
		}
		stopped = true
		if err := d.awaitExit(ctx, n.Node, a.PID); err != nil {
			return "", err
		}
		// A running session lists the conversation it is in. Once a clear has begun a new one, only the stopped listing names the id that resumes the session.
		if _, a, err = d.listed(id); err != nil {
			return "", err
		}
	}
	d.mu.Lock()
	n.Socket = ""
	// A pause may have begun since the hold, and would only stop the session again.
	paused := d.paused
	sid := cmp.Or(n.Conversation, a.SessionID)
	d.mu.Unlock()
	if paused {
		return "", errPaused
	}
	if to == nil {
		return id, d.resumeStopped(dir, id, a.SessionID, prompt)
	}
	next, err := d.start(to, dir, []string{"--resume", sid}, workerPrompt(n.Node), prompt)
	if err != nil {
		if stopped {
			if rerr := d.resumeStopped(dir, id, a.SessionID, ""); rerr != nil {
				log.Printf("resuming %s after its switch failed: %v", n.Node, rerr)
			}
		}
		return "", err
	}
	return next, nil
}

// hold claims n, still session id, for resume or merge, as long as the user did not stop it or pause the fleet.
func (d *daemon) hold(n *Session, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case n.ID != id || n.held:
		return fmt.Errorf("%s is %w", n.Node, errStarting)
	case n.Killed:
		return fmt.Errorf("the user stopped %s", n.Node)
	case d.paused:
		return errPaused
	}
	n.held = true
	return nil
}

func (d *daemon) release(n *Session) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n.held = false
}

// attach readies session id for a frontend about to run `claude attach` on it. A stale desk moves to a new conversation first, as clearStaleDesk allows, since the user's next word would read its old one uncached.
// The user then sees whatever the desk replied, which no longer holds off its clear.
func (d *daemon) attach(id string) error {
	d.mu.Lock()
	desk := d.nodes[deskNode]
	isDesk := desk != nil && id != "" && desk.ID == id
	d.mu.Unlock()
	if !isDesk {
		return d.resumeIdle(id)
	}
	d.deskLock.Lock()
	defer d.deskLock.Unlock()
	var err error
	if !d.clearStaleDesk() {
		err = d.resumeIdle(id)
	}
	if err == nil {
		d.mu.Lock()
		desk.Unseen = false
		d.saveLocked()
		d.mu.Unlock()
	}
	return err
}

// attached holds while a frontend runs claude attach on session id, until ctx ends as the frontend's connection closes, however its process ends.
// Only the master's counts: while the user has it open, next_event returns within openReturn.
func (d *daemon) attached(ctx context.Context, id string) {
	d.mu.Lock()
	if m := d.nodes[masterNode]; m == nil || id == "" || m.ID != id {
		d.mu.Unlock()
		return
	}
	if d.masterOpen == 0 {
		d.masterOpenAt = time.Now()
	}
	d.masterOpen++
	// A next_event already waiting takes the shorter bound.
	close(d.wake)
	d.wake = make(chan struct{})
	d.mu.Unlock()
	log.Printf("the user opened the master")
	<-ctx.Done()
	d.mu.Lock()
	d.masterOpen--
	d.mu.Unlock()
	log.Printf("the user left the master")
}

// clearStaleDesk clears the desk once it is stale, and reports whether it did. It spares a desk holding answers that the user has not opened it to read since bosun handed it a tell. Its caller holds deskLock.
func (d *daemon) clearStaleDesk() bool {
	d.mu.Lock()
	desk := d.nodes[deskNode]
	unseen := desk != nil && desk.Unseen
	d.mu.Unlock()
	if unseen || !d.stale(deskNode, time.Now()) {
		return false
	}
	if err := d.clear(context.Background(), deskNode); err != nil {
		log.Printf("clearing the desk's conversation: %v", err)
		return false
	}
	return true
}

// transcript shows the last turns of session name's current conversation.
func (d *daemon) transcript(name string, turns int) (string, error) {
	if turns <= 0 {
		turns = 3
	}
	d.mu.Lock()
	n := d.nodes[name]
	if n == nil {
		d.mu.Unlock()
		return "", fmt.Errorf("no session %q", name)
	}
	sid, state := cmp.Or(n.Conversation, n.SessionID), sessionState(n.public())
	d.mu.Unlock()
	t, err := readTranscript(sid, turns)
	if err != nil {
		return "", err
	}
	text := t.render()
	if text == "" {
		return fmt.Sprintf("%s is %s, and its conversation holds no turn yet.", name, state), nil
	}
	return fmt.Sprintf("%s is %s. Its last turns, oldest first:\n\n%s", name, state, text), nil
}

// resumeIdle resumes session id idle if it stopped, for a frontend about to run `claude attach` on it, or for a message to it.
// claude attach would resume it itself, and with no supervisor running, start Claude Code's supervisor in the frontend's terminal.
func (d *daemon) resumeIdle(id string) error {
	node, a, err := d.listed(id)
	if err != nil || a.Status != "" {
		return err
	}
	if node == masterNode {
		d.masterLock.Lock()
		defer d.masterLock.Unlock()
		if node, a, err = d.listed(id); err != nil || a.Status != "" {
			return err
		}
	}
	d.mu.Lock()
	n := d.nodes[node]
	if n == nil || n.ID != id || n.held {
		d.mu.Unlock()
		return fmt.Errorf("%s is %w", node, errStarting)
	}
	n.held, n.Socket = true, ""
	dir := n.Dir
	d.mu.Unlock()
	err = d.resumeStopped(dir, id, a.SessionID, "")
	d.mu.Lock()
	defer d.mu.Unlock()
	n.held = false
	d.stirred = true
	if err == nil {
		log.Printf("resumed %s (%s) idle", node, id)
	}
	return err
}

// listed returns the node of session id and Claude Code's listing of it.
func (d *daemon) listed(id string) (string, agent, error) {
	d.mu.Lock()
	node := ""
	for _, n := range d.nodes {
		if n.ID == id {
			node = n.Node
		}
	}
	d.mu.Unlock()
	if node == "" {
		return "", agent{}, fmt.Errorf("no session %s", id)
	}
	agents, err := d.claude.Agents()
	if err != nil {
		return "", agent{}, err
	}
	i := slices.IndexFunc(agents, func(a agent) bool { return a.ID == id })
	if i < 0 {
		return "", agent{}, fmt.Errorf("%s has exited", node)
	}
	return node, agents[i], nil
}

// resumeStopped continues the stopped session id, whose Claude Code session id is sid, with prompt, or idle without one.
// Claude Code resumes a session still running into a copy under a new id, without the session's options, so a copy is stopped and reported.
func (d *daemon) resumeStopped(dir, id, sid, prompt string) error {
	args := []string{"--bg", "--resume", sid}
	if prompt != "" {
		args = append(args, "--", prompt)
	}
	next, err := d.claude.Start(dir, args)
	if err != nil || next == id {
		return err
	}
	if err := d.claude.Stop(next); err != nil {
		log.Print(err)
	}
	return fmt.Errorf("Claude Code resumed %s into a copy, %s, which bosun stopped, since %s still ran", id, next, id)
}

// awaitExit waits until process pid, a session claude stop stopped, exits.
// claude stop returns, and Claude Code lists the session as stopped, before the session's process exits, and a resume before then starts a copy.
func (d *daemon) awaitExit(ctx context.Context, name string, pid int) error {
	deadline := time.Now().Add(stopWait)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			return fmt.Errorf("%s still runs %v after claude stop", name, stopWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil
}

// processAlive reports whether process pid runs. A pid of 0 is unknown, and taken for gone.
var processAlive = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// awaitIdle waits until Claude Code lists the session id as idle or stopped, and returns its listing.
func (d *daemon) awaitIdle(ctx context.Context, name, id string) (agent, error) {
	deadline := time.Now().Add(idleWait)
	for {
		agents, err := d.claude.Agents()
		if err != nil {
			return agent{}, err
		}
		i := slices.IndexFunc(agents, func(a agent) bool { return a.ID == id })
		if i >= 0 && (agents[i].Status == "idle" || agents[i].Status == "") {
			return agents[i], nil
		}
		if time.Now().After(deadline) {
			if i < 0 {
				return agent{}, fmt.Errorf("%s has exited", name)
			}
			return agent{}, fmt.Errorf("%s is %w", name, errInTurn)
		}
		select {
		case <-ctx.Done():
			return agent{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// ensureMaster adopts the master of an earlier run while Claude Code still lists it, and starts one otherwise.
// An adopted master that ended its turn, as one does once its calls to a stopped daemon fail, or that stopped, sleeps until the next event, or wakes for the events already waiting.
func (d *daemon) ensureMaster() error {
	agents, err := d.claude.Agents()
	if err != nil {
		return err
	}
	d.mu.Lock()
	if m := d.nodes[masterNode]; m != nil {
		if i := slices.IndexFunc(agents, func(a agent) bool { return a.ID == m.ID }); i >= 0 {
			// A saved "starting" would keep wakeMaster from resuming it until the first poll.
			m.Status, m.State = cmp.Or(agents[i].Status, "stopped"), agents[i].State
			if betweenTurns(m.Status) {
				// Its turn ended while no daemon ran, and the events it took may still wait, as may events the earlier run saved.
				m.Turn = false
				d.requeueLocked()
				d.sleepLocked()
			}
			d.mu.Unlock()
			return nil
		}
	}
	m := &Session{Node: masterNode, Dir: d.masterDir, Model: d.masterModel, Status: "starting"}
	d.nodes[masterNode] = m
	d.started[masterNode] = time.Now()
	d.mu.Unlock()
	// The master starts before the desk, and its sandbox must find the desk's folder to fence, not a name it could create.
	if err := os.MkdirAll(d.deskDir(), 0o700); err != nil {
		return err
	}
	id, err := d.start(m, d.masterDir, nil, masterPrompt, "Call next_event to begin.")
	if err != nil {
		return fmt.Errorf("starting the master in %s, which Claude Code must trust: %w", d.masterDir, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	m.ID = id
	d.saveLocked()
	d.broadcastSessionsLocked()
	log.Printf("started the master (%s) in %s", id, d.masterDir)
	return nil
}

// ensureDesk adopts the desk of an earlier run while Claude Code still lists it, and starts one otherwise, with the tells that waited for the old one or that its last turn took.
// An adopted desk between turns gets what waits for it.
func (d *daemon) ensureDesk() error {
	agents, err := d.claude.Agents()
	if err != nil {
		return err
	}
	d.mu.Lock()
	old := d.nodes[deskNode]
	if old != nil {
		if i := slices.IndexFunc(agents, func(a agent) bool { return a.ID == old.ID }); i >= 0 {
			old.Status, old.State = cmp.Or(agents[i].Status, "stopped"), agents[i].State
			if betweenTurns(old.Status) {
				old.Turn = false
				if len(old.Events) > 0 {
					go d.deliver(deskNode)
				}
			}
			d.mu.Unlock()
			return nil
		}
	}
	n := &Session{Node: deskNode, Dir: d.deskDir(), Model: d.deskModel, Status: "starting"}
	if old != nil {
		n.Taken, n.Events = old.Taken, old.Events
		d.giveBackLocked(n)
	}
	d.nodes[deskNode] = n
	d.started[deskNode] = time.Now()
	d.mu.Unlock()
	err = os.MkdirAll(n.Dir, 0o700)
	var id string
	if err == nil {
		id, err = d.start(n, n.Dir, nil, deskPrompt, deskStart)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		delete(d.nodes, deskNode)
		d.strandLocked(n, "could not start")
		d.saveLocked()
		return fmt.Errorf("starting the desk in %s, which Claude Code must trust: %w", n.Dir, err)
	}
	n.ID = id
	d.saveLocked()
	d.broadcastSessionsLocked()
	log.Printf("started the desk (%s) in %s", id, n.Dir)
	return nil
}

// switchModel moves the master to model, in a copy of its conversation under a new id.
func (d *daemon) switchModel(model string) error {
	if model == "" {
		return errors.New("name a model")
	}
	d.mu.Lock()
	paused := d.paused
	d.mu.Unlock()
	if paused {
		return errPaused
	}
	if !d.masterLock.TryLock() {
		return errors.New("the master is being woken or switched, so try again in a moment")
	}
	defer d.masterLock.Unlock()
	d.mu.Lock()
	m := d.nodes[masterNode]
	old, sid := m.ID, cmp.Or(m.Conversation, m.SessionID)
	next := &Session{Node: masterNode, Dir: m.Dir, Model: model}
	d.mu.Unlock()
	if old == "" || sid == "" {
		return errors.New("the master is still starting")
	}
	pid := d.pidOf(old)
	if err := d.claude.Stop(old); err != nil {
		return err
	}
	id, err := d.start(next, next.Dir, []string{"--resume", sid}, masterPrompt, fmt.Sprintf("The user switched your model to %s. Carry on: call next_event.", model))
	if err != nil {
		d.restoreMaster(old, pid, fmt.Sprintf("Switching your model to %s failed, so you keep the old one. Carry on: call next_event.", model))
		return err
	}

	d.mu.Lock()
	m.ID, m.SessionID, m.Conversation, m.Model = id, "", "", model
	m.Status, m.State, m.pre = "starting", "", nil
	d.asleep = false
	d.started[masterNode] = time.Now()
	// The old master may have taken decisions it never answered.
	queued := map[string]bool{}
	for _, ev := range d.events {
		if ev.Decision != nil {
			queued[ev.Decision.ID] = true
		}
	}
	for _, dec := range d.snapshotLocked().Decisions {
		if dec.Holder == "master" && !queued[dec.ID] {
			d.pushLocked(Event{Kind: "decision", Decision: &dec})
		}
	}
	d.saveLocked()
	d.broadcastSessionsLocked()
	d.mu.Unlock()
	log.Printf("switched the master to %s (%s)", model, id)
	if err := d.claude.Remove(old, ""); err != nil {
		log.Print(err)
	}
	return nil
}

// pidOf returns the process of session id, or 0 when Claude Code lists none.
func (d *daemon) pidOf(id string) int {
	agents, err := d.claude.Agents()
	if err != nil {
		log.Print(err)
	}
	for _, a := range agents {
		if a.ID == id {
			return a.PID
		}
	}
	return 0
}

// restoreMaster resumes the old master, whose process was pid until a switch that failed stopped it, under its own id.
func (d *daemon) restoreMaster(id string, pid int, prompt string) {
	d.mu.Lock()
	m := d.nodes[masterNode]
	dir := m.Dir
	d.mu.Unlock()
	err := d.awaitExit(context.Background(), masterNode, pid)
	var a agent
	if err == nil {
		_, a, err = d.listed(id)
	}
	if err == nil {
		err = d.resumeStopped(dir, id, a.SessionID, prompt)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		log.Printf("resuming the master: %v", err)
		d.sleepLocked()
		return
	}
	m.Status, m.State, m.pre = "starting", "", nil
	d.asleep = false
	d.started[masterNode] = time.Now()
	d.saveLocked()
	d.broadcastSessionsLocked()
}

// pollDue reports whether a poll could find a change: a session is in its turn or starting, something stirred since the last poll, or the last poll is quietPollEvery old.
func (d *daemon) pollDue(now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stirred || now.Sub(d.lastPoll) >= quietPollEvery {
		return true
	}
	for _, n := range d.nodes {
		if n.ID == "" || n.held || n.Turn || !betweenTurns(n.Status) {
			return true
		}
	}
	return false
}

// betweenTurns reports whether a session with status runs no turn: it is idle, stopped or gone.
func betweenTurns(status string) bool {
	return status == "idle" || status == "stopped" || status == "exited"
}

// poll refreshes each session from Claude Code's listing, and tells the master of a worker that stopped in its turn.
func (d *daemon) poll() {
	// A hook during the listing may begin a turn the listing misses, so it stirs the next poll.
	d.mu.Lock()
	d.stirred = false
	d.mu.Unlock()
	listed := time.Now()
	agents, err := d.claude.Agents()
	if err != nil {
		log.Print(err)
		d.mu.Lock()
		d.stirred = true
		d.mu.Unlock()
		return
	}
	byID := map[string]agent{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	if d.refresh(byID, listed) {
		d.checkSupervisor()
	}
}

// cutLocked tells the parent of worker n, which status shows stopped or gone, that it stopped in its turn, and puts back the reports of its children that the turn took.
func (d *daemon) cutLocked(n *Session, status string) {
	text := ""
	if status == "exited" {
		text = "Its session is gone, so nothing resumes it."
	}
	n.Events, n.Taken = append(n.Taken, n.Events...), nil
	d.reportLocked(Event{Kind: "exit", Node: n.Node, Text: text})
	log.Printf("%s %s in its turn", n.Node, status)
}

// turnSettle is how long after a hook of a turn the daemon trusts a listing that shows the session between turns, which Claude Code may update only after the hook.
const turnSettle = 10 * time.Second

// refresh updates each session from its listing, taken at listed, and tells the master of each turn that ended or stopped without its knowing, a turn of the daemon's earlier run included.
// It reports whether the master runs in a new process, which a new supervisor may have started.
func (d *daemon) refresh(byID map[string]agent, listed time.Time) (masterMoved bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastPoll = time.Now()
	changed := false
	for _, n := range d.nodes {
		if n.ID == "" || n.held {
			continue
		}
		status, state := "exited", ""
		a, ok := byID[n.ID]
		if ok {
			// The listing outranks a hook, which a stopped session may still send after a switch.
			n.SessionID = cmp.Or(a.SessionID, n.SessionID)
		}
		switch {
		case ok && a.Status != "":
			status, state = a.Status, a.State
		case n.Status == "starting" && time.Since(d.started[n.Node]) < startGrace:
			continue
		case ok:
			status = "stopped"
		}
		// A deliver that found the session in its turn or held put its reports back, and only this retries it.
		if betweenTurns(status) && status != "exited" && len(n.Events) > 0 {
			go d.deliver(n.Node)
		}
		if betweenTurns(status) {
			d.withdrawLocked(n.Node, func(dec *Decision) bool { return dec.prompt })
		}
		// The turn's hooks ran, but not its Stop: the daemon was away, or the user interrupted the turn.
		missed := n.Turn && betweenTurns(status) && n.turnAt.Before(listed.Add(-turnSettle))
		if !missed && status == n.Status && state == n.State {
			continue
		}
		switch {
		case n.Node == masterNode:
			if missed {
				n.Turn = false
				d.requeueLocked()
			}
			if missed || status == "stopped" && !d.asleep {
				d.sleepLocked()
			}
		case n.Node == deskNode:
			// Nobody hears of the desk's turns, but a turn cut short gives back the tells it took.
			switch {
			case n.Paused:
			case missed && status != "idle", (status == "stopped" || status == "exited") && !betweenTurns(n.Status):
				d.giveBackLocked(n)
			case missed:
				n.Taken = nil
			}
			n.Turn = n.Turn && !missed
		case n.Killed || n.Paused:
			n.Turn = false
		case missed:
			d.heardLocked(n)
			tail, err := readTranscript(n.SessionID, 0)
			switch {
			case status == "idle" || err == nil && tail.ended:
				if err != nil {
					tail.text = fmt.Sprintf("Its turn ended, and bosun could not read its last message: %v", err)
				}
				n.Taken = nil
				d.reportLocked(Event{Kind: "turn_end", Node: n.Node, Text: tail.text})
				log.Printf("%s ended its turn without a Stop that reached bosun", n.Node)
			default:
				d.cutLocked(n, status)
			}
		// Claude Code stops a worker idle for an hour, which has sent its turn_end and still resumes, so only a worker stopped in its turn is news.
		case (status == "stopped" || status == "exited") && !betweenTurns(n.Status):
			d.heardLocked(n)
			d.cutLocked(n, status)
		}
		n.Status, n.State = status, state
		if !ok || a.Status == "" {
			n.Socket = ""
		}
		// A worker the user stopped handed its children over then.
		if status == "exited" && isWorker(n.Node) && !n.Killed {
			d.orphanLocked(n)
		}
		if status == "exited" && n.Node == deskNode {
			d.strandLocked(n, "is gone, so bosun tell goes to the master until a restart of the daemon starts a new desk")
		}
		changed = true
	}
	if changed {
		d.saveLocked()
		d.broadcastSessionsLocked()
	}
	if m := d.nodes[masterNode]; m != nil {
		if pid := byID[m.ID].PID; pid != 0 && pid != d.masterPID {
			d.masterPID = pid
			return true
		}
	}
	return false
}

// cgroupOf returns the cgroup of process pid.
var cgroupOf = func(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	return strings.TrimSpace(string(data)), err
}

// checkSupervisor tells the user when Claude Code's supervisor runs outside the daemon's cgroup, as one a terminal started does.
// Every session takes that supervisor's environment, and the daemon cannot change it.
func (d *daemon) checkSupervisor() {
	pid, err := d.claude.Supervisor()
	if err != nil {
		log.Print(err)
		return
	}
	theirs, err := cgroupOf(pid)
	if err != nil {
		return
	}
	ours, err := cgroupOf(os.Getpid())
	if err != nil || theirs == ours {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if theirs == d.foreign {
		return
	}
	d.foreign = theirs
	d.openLocked(&Decision{Kind: "notice", Node: masterNode, Holder: "user", Note: fmt.Sprintf("Claude Code's supervisor runs outside the bosun daemon, in %s, so every session takes the environment of whatever started it, such as a terminal. `claude daemon stop --any` stops it and every background session, and restarting the daemon then starts a new master under a supervisor of its own.", theirs)})
}
