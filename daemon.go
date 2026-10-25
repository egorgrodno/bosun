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
	// startGrace covers a session `claude --bg` started that `claude agents --json` does not list yet.
	startGrace = 30 * time.Second
)

var nodeName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

type daemon struct {
	exe         string
	socket      string
	stateDir    string
	masterDir   string
	masterModel string
	workerModel string
	claude      claudeCLI

	switching sync.Mutex
	mu        sync.Mutex
	nodes     map[string]*Session
	started   map[string]time.Time
	decisions map[string]*Decision
	nextID    int
	events    []Event
	wake      chan struct{}
	watchers  map[*watcher]bool
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
	if err := d.load(); err != nil {
		return err
	}
	ln, err := listen(d.socket)
	if err != nil {
		return err
	}
	defer ln.Close()
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
			d.poll()
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
	case "tell":
		err = d.tell(r.Text)
	case "spawn":
		resp.Session, err = d.spawn(r.Node, r.Name, r.Repo, r.Model, r.Text)
	case "kill":
		err = d.kill(r.Name)
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
	case "Stop":
		n.pre = nil
		d.withdrawLocked(node, func(*Decision) bool { return true })
		if node != masterNode {
			d.pushLocked(Event{Kind: "turn_end", Node: node, Text: in.LastMessage})
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
	if a.withdrawn {
		log.Printf("decision %s: withdrawn", dec.ID)
	} else {
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
}

// nextEvent waits for the master's events and returns them all, leaving out decisions settled or escalated since.
func (d *daemon) nextEvent(ctx context.Context) []Event {
	timer := time.NewTimer(idleReturn)
	defer timer.Stop()
	for {
		d.mu.Lock()
		var evs []Event
		for _, ev := range d.events {
			if ev.Decision != nil {
				if cur := d.decisions[ev.Decision.ID]; cur == nil || cur.Holder != "master" {
					continue
				}
			}
			evs = append(evs, ev)
		}
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

	d.mu.Lock()
	err := d.admitLocked(caller, name)
	if err == nil {
		d.nodes[name] = &Session{Node: name, Parent: caller, Repo: root, Dir: worktree, Model: cmp.Or(model, d.workerModel), Depth: d.nodes[caller].Depth + 1, Status: "starting"}
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
		if n.Parent == parent && n.Status != "exited" {
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
				"allowWrite": []string{"~/.cache/go-build", "~/.cache/nix"},
				"denyRead":   d.hiddenSockets(),
			},
		},
		"hooks": map[string]any{
			"SessionStart":       quick,
			"PreToolUse":         hold,
			"PermissionRequest":  hold,
			"PostToolUse":        quick,
			"PostToolUseFailure": quick,
			"Stop":               quick,
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

func (d *daemon) kill(name string) error {
	d.mu.Lock()
	n := d.nodes[name]
	var id string
	if n != nil {
		id = n.ID
	}
	d.mu.Unlock()
	switch {
	case n == nil:
		return fmt.Errorf("unknown node %q", name)
	case id == "":
		return fmt.Errorf("%s is still starting", name)
	}
	return d.claude.Stop(id)
}

// ensureMaster adopts the master of an earlier run while Claude Code still lists it, and starts one otherwise.
func (d *daemon) ensureMaster() error {
	agents, err := d.claude.Agents()
	if err != nil {
		return err
	}
	d.mu.Lock()
	if m := d.nodes[masterNode]; m != nil && slices.ContainsFunc(agents, func(a agent) bool { return a.ID == m.ID }) {
		d.mu.Unlock()
		return nil
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
	if !d.switching.TryLock() {
		return errors.New("the master is already switching")
	}
	defer d.switching.Unlock()
	d.mu.Lock()
	m := d.nodes[masterNode]
	old, sid := m.ID, m.SessionID
	next := &Session{Node: masterNode, Dir: m.Dir, Model: model}
	d.mu.Unlock()
	if old == "" || sid == "" {
		return errors.New("the master is still starting")
	}
	if err := d.claude.Stop(old); err != nil {
		return err
	}
	id, err := d.start(next, next.Dir, []string{"--resume", sid}, masterPrompt, fmt.Sprintf("The user switched your model to %s. Carry on: call next_event.", model))
	if err != nil {
		// Without flags, the resume continues the old master under its own id.
		if _, rerr := d.claude.Start(next.Dir, []string{"--bg", "--resume", sid}); rerr != nil {
			log.Printf("resuming the master: %v", rerr)
		}
		return err
	}

	d.mu.Lock()
	m.ID, m.SessionID, m.Model = id, "", model
	m.Status, m.State, m.pre = "starting", "", nil
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
	if err := d.claude.Remove(old); err != nil {
		log.Print(err)
	}
	return nil
}

// poll refreshes each session from `claude agents --json`, and tells the master of a worker that stopped being listed.
func (d *daemon) poll() {
	agents, err := d.claude.Agents()
	if err != nil {
		log.Print(err)
		return
	}
	byID := map[string]agent{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	changed := false
	for _, n := range d.nodes {
		if n.ID == "" {
			continue
		}
		status, state := "exited", ""
		a, listed := byID[n.ID]
		switch {
		case listed:
			status, state = a.Status, a.State
			// The listing outranks a hook, which a stopped session may still send after a switch.
			n.SessionID = cmp.Or(a.SessionID, n.SessionID)
		case n.Status == "starting" && time.Since(d.started[n.Node]) < startGrace:
			continue
		}
		if status == n.Status && state == n.State {
			continue
		}
		if status == "exited" && n.Node != masterNode {
			d.pushLocked(Event{Kind: "exit", Node: n.Node})
			log.Printf("%s exited", n.Node)
		}
		n.Status, n.State = status, state
		changed = true
	}
	if changed {
		d.saveLocked()
		d.broadcastSessionsLocked()
	}
}
