package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// errFrontendOnly refuses a session's request for what only the user may do from a frontend, since it uses Claude Code's control key.
var errFrontendOnly = errors.New("only the user steers a session, from a frontend")

// interruptWait bounds how long an Esc has to end a turn before the interrupt stops and resumes the session instead.
var interruptWait = 10 * time.Second

// refusedCommands change what the daemon records of a session: /model its model, which bosun model and resume switch, and /clear its session id.
var refusedCommands = []string{"/model", "/clear"}

// lockSession takes the lock that keeps the daemon's own resumes of the master or the desk off it, and returns its release.
func (d *daemon) lockSession(name string) func() {
	switch name {
	case masterNode:
		d.masterLock.Lock()
		return d.masterLock.Unlock
	case deskNode:
		d.deskLock.Lock()
		return d.deskLock.Unlock
	}
	return func() {}
}

// steerable returns bosun's session name, and its short id and directory, when the user may steer it now.
func (d *daemon) steerable(name string) (*Session, string, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := d.nodes[name]
	switch {
	case n == nil:
		return nil, "", "", fmt.Errorf("no session %q: bosun steers only its own sessions", name)
	case n.Killed:
		return nil, "", "", fmt.Errorf("the user stopped %s", name)
	case n.Status == "exited":
		return nil, "", "", fmt.Errorf("%s has exited", name)
	case n.ID == "" || n.held || n.Status == "starting":
		return nil, "", "", fmt.Errorf("%s is %w", name, errStarting)
	case d.paused:
		return nil, "", "", errPaused
	}
	return n, n.ID, n.Dir, nil
}

// jobNeeds reads what the dialog that session id waits in asks of the user, such as `approve Bash: npm test` or `answer: red or blue? (Red · Blue)`, or "" when it waits in none.
func jobNeeds(id string) (string, error) {
	data, err := os.ReadFile(filepath.Join(claudeDir(), "jobs", id, "state.json"))
	if err != nil {
		return "", err
	}
	var st struct {
		Needs string `json:"needs"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return "", err
	}
	return st.Needs, nil
}

// logUseLocked logs an action of the user's on node, which reached it via supervisor or resume, and failed with err when it is not nil.
func (d *daemon) logUseLocked(e Entry, err error) {
	e.By = "user"
	if err != nil {
		e.Error = err.Error()
	}
	d.logLocked(e)
}

// say sends text to session name as the user's own, as typing it into the session's terminal would: a prompt, an answer to its question, or a slash command.
// A running session takes it through the supervisor, and a stopped one in a resume. When the supervisor's protocol is not the one bosun speaks, bosun stops the session and resumes it with text, which cuts its turn short.
func (d *daemon) say(ctx context.Context, caller, name, text string) (string, error) {
	if caller != "" {
		return "", errFrontendOnly
	}
	if strings.TrimSpace(text) == "" {
		return "", errors.New("the text is empty")
	}
	switch f := strings.Fields(text); {
	case f[0] == "/clear":
		return "", errors.New("bosun does not send /clear, which would change the session behind bosun's record of it: bosun clear clears the master's or the desk's conversation")
	case slices.Contains(refusedCommands, f[0]):
		return "", fmt.Errorf("bosun does not send %s, which would change the session behind bosun's record of it: bosun model switches the master's or the desk's model, and the master's resume a worker's", f[0])
	}
	unlock := d.lockSession(name)
	defer unlock()
	n, id, dir, err := d.steerable(name)
	if err != nil {
		return "", err
	}
	_, a, err := d.listed(id)
	if err != nil {
		return "", err
	}
	command := strings.HasPrefix(strings.TrimSpace(text), "/")
	// The Stop hook outranks the listing, which still shows a session busy for a moment after a turn that ended on a backgrounded call.
	d.mu.Lock()
	idle := a.Status == "idle" || n.settled()
	d.mu.Unlock()
	if a.Status == "" {
		err := d.resumeWith(ctx, n, id, dir, text)
		d.mu.Lock()
		d.logUseLocked(Entry{Kind: "say", Node: name, Text: text, Via: "resume"}, err)
		d.mu.Unlock()
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Resumed %s with it.", name), nil
	}
	needs, nerr := jobNeeds(id)
	switch {
	case needOf(needs).Kind == "permission":
		return "", fmt.Errorf("%s waits on its own prompt for permission (%s), which would hold the text and drop it at a stop. Approve or deny the permission first", name, needs)
	case nerr != nil && a.Status == "waiting":
		return "", fmt.Errorf("%s waits on its own prompt, and bosun cannot read whether it asks for a permission, which would hold the text: %v", name, nerr)
	}
	c, err := newControl(ctx)
	if err == nil {
		err = c.reply(id, text)
		d.mu.Lock()
		d.logUseLocked(Entry{Kind: "say", Node: name, Text: text, Via: "supervisor"}, err)
		if err == nil && name == masterNode && !idle {
			d.saidMaster = true
			close(d.wake)
			d.wake = make(chan struct{})
		}
		d.mu.Unlock()
	}
	switch {
	case err == nil && idle && command:
		return fmt.Sprintf("%s runs it.", name), nil
	case err == nil && idle:
		return fmt.Sprintf("%s takes it as a new turn.", name), nil
	case err == nil && needs != "":
		return fmt.Sprintf("%s takes it as its answer if it names an option of: %s", name, needs), nil
	case err == nil && command:
		return fmt.Sprintf("%s runs it once its turn ends.", name), nil
	case err == nil:
		return fmt.Sprintf("%s takes it in once its running tool call returns.", name), nil
	case !errors.Is(err, errNoControl):
		return "", err
	}
	log.Printf("say to %s: %v, so bosun stops and resumes it", name, err)
	inTurn, why := !idle, err
	err = d.resumeWith(ctx, n, id, dir, text)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.logUseLocked(Entry{Kind: "say", Node: name, Text: text, Via: "resume"}, err)
	if err != nil {
		return "", err
	}
	if !inTurn {
		return fmt.Sprintf("Resumed %s with it, since %v.", name, why), nil
	}
	d.interruptedLocked(n, "The user cut its turn short with a message of their own, which began a new turn.", true)
	return fmt.Sprintf("Cut %s's turn short and resumed it with the text, since %v.", name, why), nil
}

// interrupt ends session name's turn, as Esc in its terminal does, and leaves it idle. Without the supervisor, or when the Esc ends no turn, bosun stops the session and resumes it idle.
// No Stop runs either way, so the session's parent hears of the turn's end from bosun.
func (d *daemon) interrupt(ctx context.Context, caller, name string) (string, error) {
	if caller != "" {
		return "", errFrontendOnly
	}
	unlock := d.lockSession(name)
	defer unlock()
	n, id, dir, err := d.steerable(name)
	if err != nil {
		return "", err
	}
	_, a, err := d.listed(id)
	d.mu.Lock()
	settled := n.settled()
	d.mu.Unlock()
	switch {
	case err != nil:
		return "", err
	case a.Status == "":
		return "", fmt.Errorf("%s has stopped, so it runs no turn", name)
	case a.Status == "idle" || settled:
		return "", fmt.Errorf("%s is not in its turn", name)
	}
	// Held, the session is off the poll, which would report the turn's end too.
	if err := d.hold(n, id); err != nil {
		return "", err
	}
	defer d.release(n)
	c, err := newControl(ctx)
	if err == nil {
		err = c.keys(id, "\x1b")
		if err == nil {
			err = d.awaitStatus(ctx, id, "idle", interruptWait)
		}
		d.mu.Lock()
		d.logUseLocked(Entry{Kind: "interrupt", Node: name, Via: "supervisor"}, err)
		if err == nil {
			d.interruptedLocked(n, "The user interrupted its turn.", false)
		}
		d.mu.Unlock()
		if err == nil {
			return fmt.Sprintf("Interrupted %s's turn.", name), nil
		}
	}
	if !errors.Is(err, errNoControl) {
		return "", err
	}
	log.Printf("interrupting %s: %v, so bosun stops and resumes it", name, err)
	why := err
	err = d.resumeHeld(ctx, n, id, dir, "")
	d.mu.Lock()
	defer d.mu.Unlock()
	d.logUseLocked(Entry{Kind: "interrupt", Node: name, Via: "resume"}, err)
	if err != nil {
		return "", err
	}
	d.interruptedLocked(n, "The user interrupted its turn.", false)
	return fmt.Sprintf("Interrupted %s's turn by stopping and resuming it, since %v.", name, why), nil
}

// awaitStatus waits up to wait for Claude Code to list session id with status.
func (d *daemon) awaitStatus(ctx context.Context, id, status string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		agents, err := d.claude.Agents()
		if err != nil {
			return err
		}
		if i := slices.IndexFunc(agents, func(a agent) bool { return a.ID == id }); i >= 0 && agents[i].Status == status {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: the session is not %s %v after an Esc", errNoControl, status, wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// resumeWith holds n, still session id, for resumeHeld.
func (d *daemon) resumeWith(ctx context.Context, n *Session, id, dir, prompt string) error {
	if err := d.hold(n, id); err != nil {
		return err
	}
	defer d.release(n)
	return d.resumeHeld(ctx, n, id, dir, prompt)
}

// resumeHeld stops n, session id, if it runs, and resumes it under its own id with prompt, or idle without one. Its caller holds n.
func (d *daemon) resumeHeld(ctx context.Context, n *Session, id, dir, prompt string) error {
	_, a, err := d.listed(id)
	if err != nil {
		return err
	}
	if a.Status != "" {
		if err := d.claude.Stop(id); err != nil {
			return err
		}
		if err := d.awaitExit(ctx, n.Node, a.PID); err != nil {
			return err
		}
		if _, a, err = d.listed(id); err != nil {
			return err
		}
	}
	d.mu.Lock()
	n.Socket = ""
	d.mu.Unlock()
	if err := d.resumeStopped(dir, id, a.SessionID, prompt); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	n.Status, n.State, n.pre = "starting", "", nil
	d.started[n.Node] = time.Now()
	d.saveLocked()
	d.broadcastSessionsLocked()
	return nil
}

// interruptedLocked ends n's turn, which the user cut short with no Stop to report it: its parent hears why, the master's events wait again, and the master sleeps unless it runs on in a new turn.
// The reports and tells the turn took stay taken, since the user cut it short knowing what it did.
func (d *daemon) interruptedLocked(n *Session, why string, runsOn bool) {
	n.pre = nil
	d.withdrawLocked(n.Node, func(*Decision) bool { return true }, outcome{by: "bosun", cause: "the user interrupted " + n.Node + "'s turn"})
	switch n.Node {
	case masterNode:
		n.Turn = false
		d.requeueLocked()
		d.asleep = !runsOn
	case deskNode:
		n.Turn, n.Taken = false, nil
	default:
		n.Taken = nil
		d.heardLocked(n)
		d.reportLocked(Event{Kind: "turn_end", Node: n.Node, Text: why})
	}
	d.saveLocked()
}

// promptNotice reports whether decision id is a notice of a permission waiting in its session's own prompt.
func (d *daemon) promptNotice(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	dec := d.decisions[id]
	return dec != nil && dec.Kind == "notice" && dec.Prompt
}

// answerPrompt gives the user's answer to the permission that notice id says waits in its session's own prompt, by pressing the dialog's keys through the supervisor: Enter on its first option, Yes, or Esc, No. A message with a denial follows as the user's text.
func (d *daemon) answerPrompt(ctx context.Context, caller, id string, a answer) (string, error) {
	if caller != "" {
		return "", errFrontendOnly
	}
	d.mu.Lock()
	dec := d.decisions[id]
	if dec == nil || !dec.Prompt {
		d.mu.Unlock()
		return "", fmt.Errorf("no open decision %s", id)
	}
	name, tool := dec.Node, dec.Tool
	d.mu.Unlock()
	_, sid, _, err := d.steerable(name)
	if err != nil {
		return "", err
	}
	needs, err := jobNeeds(sid)
	switch {
	case err != nil:
		return "", fmt.Errorf("bosun cannot read what %s's own prompt asks: %v", name, err)
	case needOf(needs).Kind != "permission":
		return "", fmt.Errorf("%s's own prompt asks for no permission now", name)
	case tool != "" && needOf(needs).Tool != tool:
		return "", fmt.Errorf("%s's own prompt asks about another call now: %s", name, needs)
	}
	c, err := newControl(ctx)
	if err != nil {
		return "", fmt.Errorf("bosun cannot answer %s's own prompt, %v, so answer it in the session: bosun attach %s", name, err, sid)
	}
	keys := "\r"
	if a.behavior == "deny" {
		keys = "\x1b"
	}
	err = c.keys(sid, keys)
	if err == nil && a.behavior == "deny" && a.text != "" {
		err = c.reply(sid, a.text)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.logUseLocked(Entry{Kind: "prompt", Node: name, Behavior: a.behavior, Text: a.text, Via: "supervisor"}, err)
	if err != nil {
		return "", err
	}
	if d.decisions[id] == dec {
		d.settleLocked(dec, a, outcome{by: "user", behavior: a.behavior})
	}
	return "", nil
}

// needOf is the dialog that needs names, and the zero Need for none.
func needOf(needs string) Need {
	if n := parseNeeds(needs); n != nil {
		return *n
	}
	return Need{}
}
