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
	masterNode  = "master"
	maxDepth    = 3
	maxChildren = 4

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
)

// idleWait bounds how long resume waits for a worker to be listed idle, which it is not until its Stop hook returns.
var idleWait = 30 * time.Second

// stopWait bounds how long a resume waits for the process of the session it stopped to exit.
const stopWait = 10 * time.Second

var (
	errInTurn   = errors.New("still in its turn")
	errStarting = errors.New("still starting")
)

// eventsWaiting is the prompt that wakes the master, and the reason a block of its Stop gives.
const eventsWaiting = "Events are waiting: call next_event."

var nodeName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

type daemon struct {
	exe         string
	socket      string
	stateDir    string
	masterDir   string
	masterModel string
	workerModel string
	claude      claudeCLI

	// masterLock is held while the master is woken or switched, so that neither resumes it beside the other.
	masterLock sync.Mutex
	mu         sync.Mutex
	nodes      map[string]*Session
	started    map[string]time.Time
	decisions  map[string]*Decision
	nextID     int
	events     []Event
	wake       chan struct{}
	watchers   map[*watcher]bool
	// stopBlocked holds from a block of the master's Stop until its next call to next_event.
	// Claude Code's stop_hook_active cannot stand in: it stays true for the rest of the turn a block continued.
	stopBlocked bool
	// asleep holds from a Stop that ends the master's turn with no events waiting until the next event resumes it.
	asleep bool
	// recovering holds while the daemon resumes the sessions that a usage limit stopped, and limitNotice is the last notice of a limit.
	recovering  bool
	limitNotice string
	// lastPoll and stirred decide whether the next tick polls: stirred holds from a hook or a kill until the poll after it.
	lastPoll time.Time
	stirred  bool
	// masterPID is the master's process when the daemon last checked the supervisor, and foreign the cgroup of a supervisor it last told the user of.
	masterPID int
	foreign   string
}

type watcher struct {
	ch     chan Event
	cancel context.CancelFunc
}

// preCall is a PreToolUse not yet followed by its PostToolUse.
type preCall struct {
	tool  string
	input string
}

type savedState struct {
	NextID   int        `json:"nextId"`
	Sessions []*Session `json:"sessions"`
}

func newDaemon(exe, socket, stateDir, masterDir string, claude claudeCLI) *daemon {
	return &daemon{
		exe:       exe,
		socket:    socket,
		stateDir:  stateDir,
		masterDir: masterDir,
		claude:    claude,
		nodes:     map[string]*Session{},
		started:   map[string]time.Time{},
		decisions: map[string]*Decision{},
		wake:      make(chan struct{}),
		watchers:  map[*watcher]bool{},
	}
}

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
	workerModel := flags.String("worker-model", "", "model of a worker started without one; the user's default when empty")
	_ = flags.Parse(args)
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	d := newDaemon(exe, socketPath(), stateDir(), *masterDir, claudeExec{})
	d.masterModel, d.workerModel = *masterModel, *workerModel
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
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if d.pollDue(time.Now()) {
				d.poll()
			}
			d.recoverLimited(time.Now())
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

// load adopts the sessions of an earlier run, and rewrites their settings for this build, which each reads when its process next starts.
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
	for _, s := range st.Sessions {
		d.nodes[s.Node] = s
		if _, _, err := d.writeConfig(s); err != nil {
			log.Printf("%s: %v", s.Node, err)
		}
	}
	return nil
}

func (d *daemon) saveLocked() {
	st := savedState{NextID: d.nextID}
	for _, n := range d.sortedLocked() {
		st.Sessions = append(st.Sessions, n)
	}
	data, _ := json.MarshalIndent(st, "", "  ")
	path := filepath.Join(d.stateDir, "state.json")
	err := os.MkdirAll(d.stateDir, 0o700)
	if err == nil {
		err = os.WriteFile(path+".tmp", data, 0o600)
	}
	if err == nil {
		err = os.Rename(path+".tmp", path)
	}
	if err != nil {
		log.Printf("saving state: %v", err)
	}
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
	c.pre = nil
	return c
}

var notAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

// memoryDir is the auto memory directory Claude Code picks for the session's repository, or for its directory outside one, which the user's own sessions there share.
// Claude Code adds a hash to a name over 200 characters and this does not, so such a repository's sessions keep memory apart from the user's.
func (s *Session) memoryDir() string {
	return filepath.Join(claudeDir(), "projects", notAlnum.ReplaceAllString(cmp.Or(s.Repo, s.Dir), "-"), "memory")
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

// writable lists where the session's file tools write without the user: its worktree, its scratch folder, and the memory and job directories Claude Code's system prompt sends it to.
func (d *daemon) writable(s *Session) []string {
	dirs := []string{s.Dir, d.scratchDir(s.Node), s.memoryDir()}
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
		resp, err = d.hook(ctx, r.Node, r.Hook)
	case "ask":
		resp.Text, err = d.ask(ctx, r.Node, r.Text)
	case "next_event":
		if r.Node != masterNode {
			err = errors.New("only the master waits for events")
		} else {
			resp.Events = d.nextEvent(ctx)
		}
	case "approve":
		err = d.answer(r.Node, r.ID, "permission", answer{behavior: "allow"})
	case "deny":
		err = d.answer(r.Node, r.ID, "permission", answer{behavior: "deny", text: r.Text})
	case "reply":
		err = d.answer(r.Node, r.ID, "question", answer{text: r.Text})
	case "escalate":
		err = d.escalate(r.Node, r.ID, r.Text)
	case "notify":
		err = d.notify(r.Node, r.Text)
	case "dismiss":
		err = d.answer(r.Node, r.ID, "notice", answer{})
	case "tell":
		err = d.tell(r.Text)
	case "spawn":
		resp.Session, err = d.spawn(r.Node, r.Name, r.Repo, r.Model, r.Text)
	case "resume":
		err = d.resume(ctx, r.Node, r.Name, r.Text)
	case "kill":
		err = d.kill(r.Name)
	case "merge":
		resp.Text, err = d.merge(ctx, r.Node, r.Name)
	case "model":
		err = d.switchModel(r.Model)
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
}

// hook handles one hook event of a session. It blocks while a decision it opened waits, and answers with no behavior when the decision is withdrawn.
func (d *daemon) hook(ctx context.Context, node string, raw json.RawMessage) (Response, error) {
	resp := Response{Type: "result"}
	var in hookInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return resp, fmt.Errorf("hook input: %w", err)
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
	d.stirred = true
	var dec *Decision
	var worktree string
	switch in.Event {
	case "SessionStart":
		if n.Repo != "" {
			worktree = n.Dir
		}
	case "PreToolUse":
		// A permission still open from an earlier call was denied in the session's own prompt, which runs no tool.
		d.withdrawLocked(node, func(dec *Decision) bool { return dec.toolUseID != in.ToolUseID })
		if n.pre == nil {
			n.pre = map[string]preCall{}
		}
		n.pre[in.ToolUseID] = preCall{in.ToolName, compact(in.ToolInput)}
		if reason := reservedAction(in.ToolName, in.ToolInput, d.writable(n)...); reason != "" {
			dec = &Decision{Kind: "permission", Node: node, Holder: "user", Reserved: reason, Tool: in.ToolName, Input: in.ToolInput, toolUseID: in.ToolUseID}
		}
	case "PermissionRequest":
		holder := "master"
		if node == masterNode {
			holder = "user"
		}
		dec = &Decision{Kind: "permission", Node: node, Holder: holder, Tool: in.ToolName, Input: in.ToolInput, toolUseID: n.match(in.ToolName, in.ToolInput)}
	case "PostToolUse", "PostToolUseFailure":
		delete(n.pre, in.ToolUseID)
		d.withdrawLocked(node, func(dec *Decision) bool { return dec.toolUseID == in.ToolUseID })
	case "StopFailure":
		// The turn ended on an API error, and Stop does not run.
		n.pre = nil
		d.withdrawLocked(node, func(*Decision) bool { return true })
		switch {
		case in.Error == "rate_limit":
			d.limitLocked(n, in.LastMessage)
			if node == masterNode {
				d.asleep = true
			}
		case node != masterNode:
			d.pushLocked(Event{Kind: "turn_end", Node: node, Text: fmt.Sprintf("Its turn failed on an API error, %s: %s", in.Error, in.LastMessage)})
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
		case node != masterNode:
			d.pushLocked(Event{Kind: "turn_end", Node: node, Text: in.LastMessage})
		case len(d.pendingLocked()) == 0:
			d.asleep = true
			log.Print("the master sleeps until the next event")
		case !d.stopBlocked:
			resp.Behavior, resp.Text = "block", eventsWaiting
			d.stopBlocked = true
		default:
			// The block did not send the master back to next_event, and blocking it again might never end.
			d.asleep = true
			d.openLocked(&Decision{Kind: "notice", Node: masterNode, Holder: "user", Note: fmt.Sprintf("The master ended its turn twice in a row while events waited, so it has not read them. claude attach %s shows why, and the next event resumes it.", n.ID)})
		}
	}
	if dec != nil {
		d.openLocked(dec)
	}
	d.mu.Unlock()
	if worktree != "" {
		if err := standInGitmodules(worktree); err != nil {
			log.Printf("%s: .gitmodules: %v", node, err)
		}
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

func compact(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}

func (d *daemon) ask(ctx context.Context, node, question string) (string, error) {
	if question == "" {
		return "", errors.New("the question is empty")
	}
	d.mu.Lock()
	if d.nodes[node] == nil || node == masterNode {
		d.mu.Unlock()
		return "", errors.New("only a worker asks")
	}
	dec := &Decision{Kind: "question", Node: node, Holder: "master", Question: question}
	d.openLocked(dec)
	d.mu.Unlock()
	a, ok := d.wait(ctx, dec)
	if !ok {
		return "", errors.New("the question was withdrawn")
	}
	return a.text, nil
}

func (d *daemon) openLocked(dec *Decision) {
	d.nextID++
	dec.ID = strconv.Itoa(d.nextID)
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
	select {
	case a := <-dec.answer:
		return a, !a.withdrawn
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
	dec.answer <- a
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

// withdrawLocked settles the node's open permissions that match, except reserved ones, whose hook still blocks the call.
func (d *daemon) withdrawLocked(node string, match func(*Decision) bool) {
	for _, dec := range d.decisions {
		if dec.Node == node && dec.Kind == "permission" && dec.Reserved == "" && match(dec) {
			d.settleLocked(dec, answer{withdrawn: true})
		}
	}
}

// answer settles a decision for caller: the master for a decision it holds, or "" for the user, who may answer any.
func (d *daemon) answer(caller, id, kind string, a answer) error {
	if caller != "" && caller != masterNode {
		return errors.New("only the master or the user answers")
	}
	if kind == "question" && a.text == "" {
		return errors.New("the answer is empty")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	dec := d.decisions[id]
	switch {
	case dec == nil:
		return fmt.Errorf("no open decision %s", id)
	case dec.Kind != kind:
		return fmt.Errorf("decision %s is a %s", id, dec.Kind)
	case caller == masterNode && dec.Holder != "master":
		return fmt.Errorf("decision %s waits on the user", id)
	}
	if a.behavior == "deny" && a.text == "" {
		a.text = "The user denied this."
		if caller == masterNode {
			a.text = "The bosun master denied this."
		}
	}
	d.settleLocked(dec, a)
	return nil
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

func (d *daemon) tell(text string) error {
	if text == "" {
		return errors.New("nothing to tell")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pushLocked(Event{Kind: "tell", Text: text})
	return nil
}

func (d *daemon) pushLocked(ev Event) {
	d.events = append(d.events, ev)
	close(d.wake)
	d.wake = make(chan struct{})
	// A master that a usage limit stopped wakes once the limit resets.
	if m := d.nodes[masterNode]; d.asleep && (m == nil || m.LimitedUntil == nil) {
		d.asleep = false
		go d.wakeMaster()
	}
}

// wakeMaster resumes the master asleep. A master found in a turn, as one is once the user types into it, or starting needs no waking: it reads the events before its turn ends.
func (d *daemon) wakeMaster() {
	d.masterLock.Lock()
	defer d.masterLock.Unlock()
	err := d.carryOn(context.Background(), masterNode, eventsWaiting)
	if err == nil || errors.Is(err, errInTurn) || errors.Is(err, errStarting) {
		return
	}
	log.Printf("waking the master: %v", err)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.asleep = true
	id := d.nodes[masterNode].ID
	d.openLocked(&Decision{Kind: "notice", Node: masterNode, Holder: "user", Note: fmt.Sprintf("Waking the master failed, so its events wait unread: %v. The next event tries again. If it keeps failing, `claude rm %s` and a restart of the daemon start a new master, without the old one's conversation.", err, id)})
}

// sleepLocked lets the master sleep until the next event, or wakes it now when events already wait, since only a new event wakes it.
func (d *daemon) sleepLocked() {
	d.asleep = len(d.pendingLocked()) == 0
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

// nextEvent waits for the master's events and returns them all.
func (d *daemon) nextEvent(ctx context.Context) []Event {
	timer := time.NewTimer(idleReturn)
	defer timer.Stop()
	d.mu.Lock()
	d.stopBlocked, d.asleep = false, false
	if m := d.nodes[masterNode]; m != nil {
		m.LimitedUntil = nil
	}
	d.mu.Unlock()
	for {
		d.mu.Lock()
		evs := d.pendingLocked()
		d.events = nil
		wake := d.wake
		d.mu.Unlock()
		if len(evs) > 0 {
			return evs
		}
		select {
		case <-wake:
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
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
	resp := Response{Type: "snapshot", Sessions: []Session{}, Decisions: []Decision{}}
	for _, n := range d.sortedLocked() {
		resp.Sessions = append(resp.Sessions, n.public())
	}
	for _, dec := range d.decisions {
		resp.Decisions = append(resp.Decisions, *dec)
	}
	slices.SortFunc(resp.Decisions, func(a, b Decision) int {
		x, _ := strconv.Atoi(a.ID)
		y, _ := strconv.Atoi(b.ID)
		return x - y
	})
	return resp
}

// spawn starts a child of caller, or of the master when a frontend asks. A child of the master needs repo; a deeper one branches from its parent's worktree.
func (d *daemon) spawn(caller, name, repo, model, prompt string) (*Session, error) {
	caller = cmp.Or(caller, masterNode)
	if !nodeName.MatchString(name) || name == masterNode {
		return nil, fmt.Errorf("invalid node name %q: use lowercase letters, digits and hyphens", name)
	}
	if err := d.fitsSockets(name); err != nil {
		return nil, fmt.Errorf("node name %q is too long: %w", name, err)
	}
	if prompt == "" {
		return nil, errors.New("the prompt is empty")
	}
	d.mu.Lock()
	parent := d.nodes[caller]
	if parent == nil {
		d.mu.Unlock()
		return nil, fmt.Errorf("unknown node %q", caller)
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
	if err == nil {
		d.nodes[name] = &Session{Node: name, Parent: caller, Repo: root, Dir: worktree, Base: dir, BaseBranch: branch, Model: cmp.Or(model, d.workerModel), Depth: d.nodes[caller].Depth + 1, Status: "starting"}
		d.started[name] = time.Now()
	}
	n := d.nodes[name]
	d.mu.Unlock()
	if err != nil {
		return nil, err
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
	if d.nodes[parent].Depth >= maxDepth {
		return fmt.Errorf("%s is at the depth cap of %d", parent, maxDepth)
	}
	live := 0
	for _, n := range d.nodes {
		if n.Parent == parent && n.Status != "exited" && !n.Killed {
			live++
		}
	}
	if live >= maxChildren {
		return fmt.Errorf("%s already runs %d children", parent, maxChildren)
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
	permissions := map[string][]string{"allow": {"mcp__bosun"}}
	if node != masterNode {
		permissions["deny"] = []string{"AskUserQuestion"}
	}
	excludes := filepath.Join(dir, "gitignore")
	if err := writeExcludes(excludes); err != nil {
		return "", "", err
	}
	settings := map[string]any{
		"worktree": map[string]string{"baseRef": "head"},
		// Claude Code's default, set so that the reserved check spares exactly the directory the session writes.
		"autoMemoryDirectory": n.memoryDir(),
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
			"network": map[string]any{"allowAllUnixSockets": true},
			"filesystem": map[string]any{
				"allowWrite": []string{"~/.cache/go-build", "~/.cache/nix", "~/.cache/treefmt"},
				"denyRead":   d.hiddenSockets(),
			},
		},
		"hooks": map[string]any{
			"SessionStart":       quick,
			"UserPromptSubmit":   quick,
			"PreToolUse":         hold,
			"PermissionRequest":  hold,
			"PostToolUse":        quick,
			"PostToolUseFailure": quick,
			"Stop":               quick,
			"StopFailure":        quick,
		},
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
// The master stays stopped only until it has events to read, as poll finds.
func (d *daemon) kill(name string) error {
	d.mu.Lock()
	n := d.nodes[name]
	var id string
	var killed, held bool
	if n != nil {
		id, killed, held = n.ID, n.Killed, n.held
		// Marked first, so that a poll that sees the worker stopped tells the master nothing of its own.
		n.Killed = killed || (id != "" && !held && name != masterNode)
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
		d.pushLocked(Event{Kind: "exit", Node: name, Text: "The user stopped it."})
		d.saveLocked()
		d.broadcastSessionsLocked()
	}
	return err
}

// merge fast-forwards the branch that worker name began from to the worker's branch, then removes the worker: its session, worktree and branch.
// Its parent or the master may merge it, since the master hears every worker's turn end.
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
	case n == nil || name == masterNode:
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

// resume gives a worker that ended its turn a new prompt.
func (d *daemon) resume(ctx context.Context, caller, name, prompt string) error {
	if caller != masterNode {
		return errors.New("only the master resumes a worker")
	}
	if prompt == "" {
		return errors.New("the prompt is empty")
	}
	if name == masterNode {
		return fmt.Errorf("no worker %q", name)
	}
	return d.carryOn(ctx, name, prompt)
}

// carryOn gives a session that ended its turn, or stopped, a new prompt, which continues its conversation under the same id with its saved options.
func (d *daemon) carryOn(ctx context.Context, name, prompt string) error {
	d.mu.Lock()
	n := d.nodes[name]
	var id, dir, status string
	var killed, held bool
	if n != nil {
		id, dir, status, killed, held = n.ID, n.Dir, n.Status, n.Killed, n.held
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
	a, err := d.awaitIdle(ctx, name, id)
	if err != nil {
		return err
	}

	// Claude Code resumes a running session into a copy under a new id, so a running session stops first.
	if err := d.hold(n, id); err != nil {
		return err
	}
	defer d.release(n)
	if a.Status != "" {
		err = d.claude.Stop(id)
		if err == nil {
			err = d.awaitExit(ctx, name, a.PID)
		}
	}
	if err == nil {
		err = d.resumeStopped(dir, id, a.SessionID, prompt)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		return err
	}
	n.Status, n.State, n.pre = "starting", "", nil
	d.started[name] = time.Now()
	d.saveLocked()
	d.broadcastSessionsLocked()
	log.Printf("resumed %s (%s)", name, id)
	return nil
}

// hold claims n, still session id, for resume or merge, as long as the user did not stop it.
func (d *daemon) hold(n *Session, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case n.ID != id || n.held:
		return fmt.Errorf("%s is %w", n.Node, errStarting)
	case n.Killed:
		return fmt.Errorf("the user stopped %s", n.Node)
	}
	n.held = true
	return nil
}

func (d *daemon) release(n *Session) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n.held = false
}

// resumeStopped continues the stopped session id, whose Claude Code session id is sid, with prompt.
// Claude Code resumes a session still running into a copy under a new id, without the session's options, so a copy is stopped and reported.
func (d *daemon) resumeStopped(dir, id, sid, prompt string) error {
	next, err := d.claude.Start(dir, []string{"--bg", "--resume", sid, "--", prompt})
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
// An adopted master that ended its turn, as one does once its calls to a stopped daemon fail, or that stopped, sleeps until the next event.
func (d *daemon) ensureMaster() error {
	agents, err := d.claude.Agents()
	if err != nil {
		return err
	}
	d.mu.Lock()
	if m := d.nodes[masterNode]; m != nil {
		if i := slices.IndexFunc(agents, func(a agent) bool { return a.ID == m.ID }); i >= 0 {
			d.asleep = agents[i].Status == "idle" || agents[i].Status == ""
			// A saved "starting" would keep wakeMaster from resuming it until the first poll.
			m.Status, m.State = cmp.Or(agents[i].Status, "stopped"), agents[i].State
			d.mu.Unlock()
			return nil
		}
	}
	m := &Session{Node: masterNode, Dir: d.masterDir, Model: d.masterModel, Status: "starting"}
	d.nodes[masterNode] = m
	d.started[masterNode] = time.Now()
	d.mu.Unlock()
	if err := os.MkdirAll(d.masterDir, 0o700); err != nil {
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

// switchModel moves the master to model, in a copy of its conversation under a new id.
func (d *daemon) switchModel(model string) error {
	if model == "" {
		return errors.New("name a model")
	}
	if !d.masterLock.TryLock() {
		return errors.New("the master is being woken or switched, so try again in a moment")
	}
	defer d.masterLock.Unlock()
	d.mu.Lock()
	m := d.nodes[masterNode]
	old, sid := m.ID, m.SessionID
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
		d.restoreMaster(old, sid, pid, fmt.Sprintf("Switching your model to %s failed, so you keep the old one. Carry on: call next_event.", model))
		return err
	}

	d.mu.Lock()
	m.ID, m.SessionID, m.Model = id, "", model
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
func (d *daemon) restoreMaster(id, sid string, pid int, prompt string) {
	d.mu.Lock()
	m := d.nodes[masterNode]
	dir := m.Dir
	d.mu.Unlock()
	err := d.awaitExit(context.Background(), masterNode, pid)
	if err == nil {
		err = d.resumeStopped(dir, id, sid, prompt)
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
		if n.ID == "" || n.held || !betweenTurns(n.Status) {
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
	if d.refresh(byID) {
		d.checkSupervisor()
	}
}

// refresh updates each session from its listing, and reports whether the master runs in a new process, which a new supervisor may have started.
func (d *daemon) refresh(byID map[string]agent) (masterMoved bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastPoll = time.Now()
	changed := false
	for _, n := range d.nodes {
		if n.ID == "" || n.held {
			continue
		}
		status, state := "exited", ""
		a, listed := byID[n.ID]
		if listed {
			// The listing outranks a hook, which a stopped session may still send after a switch.
			n.SessionID = cmp.Or(a.SessionID, n.SessionID)
		}
		switch {
		case listed && a.Status != "":
			status, state = a.Status, a.State
		case n.Status == "starting" && time.Since(d.started[n.Node]) < startGrace:
			continue
		case listed:
			status = "stopped"
		}
		if status == n.Status && state == n.State {
			continue
		}
		switch {
		case n.Node == masterNode && status == "stopped" && !d.asleep:
			d.sleepLocked()
		// Claude Code stops a worker idle for an hour, which has sent its turn_end and still resumes, so only a worker stopped in its turn is news.
		case (status == "stopped" || status == "exited") && !betweenTurns(n.Status) && n.Node != masterNode && !n.Killed:
			d.pushLocked(Event{Kind: "exit", Node: n.Node})
			log.Printf("%s %s in its turn", n.Node, status)
		}
		n.Status, n.State = status, state
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
