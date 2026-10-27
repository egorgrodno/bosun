package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"time"
)

// compactWait bounds how long compact waits for the session to compact, which one in its turn does only once the turn ends.
var compactWait = 5 * time.Minute

// conversationOf refuses name unless it is the master or the desk, the sessions whose conversation the user compacts or clears from a frontend.
func conversationOf(verb, name string) error {
	if name != masterNode && name != deskNode {
		return fmt.Errorf("bosun %s only the master and the desk, not %q", verb, name)
	}
	return nil
}

// compactConversation runs /compact on name, the master or the desk, and waits until it has compacted. A running session takes it through the supervisor, and runs it once its turn ends, and a stopped one in a resume.
// It holds the session's lock throughout, so that waking the master or handing the desk its tells does not stop the session while it compacts.
func (d *daemon) compactConversation(ctx context.Context, caller, name string) (string, error) {
	if caller != "" {
		return "", errFrontendOnly
	}
	if err := conversationOf("compacts", name); err != nil {
		return "", err
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
	if a.Status != "" {
		needs, nerr := jobNeeds(id)
		switch {
		case needOf(needs).Kind == "permission":
			return "", fmt.Errorf("%s waits on its own prompt for permission (%s), which would hold /compact. Approve or deny the permission first", name, needs)
		case nerr != nil && a.Status == "waiting":
			return "", fmt.Errorf("%s waits on its own prompt, and bosun cannot read whether it asks for a permission, which would hold /compact: %v", name, nerr)
		}
	}
	compacted := make(chan struct{})
	d.mu.Lock()
	inTurn := a.Status != "" && a.Status != "idle" && !n.settled()
	d.compacted[name] = compacted
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		if d.compacted[name] == compacted {
			delete(d.compacted, name)
		}
		d.mu.Unlock()
	}()
	via := "resume"
	if a.Status != "" {
		var c *control
		if c, err = newControl(ctx); err == nil {
			via = "supervisor"
			err = c.reply(id, "/compact")
		}
	}
	if a.Status == "" || errors.Is(err, errNoControl) {
		if err != nil {
			log.Printf("compacting %s: %v, so bosun resumes it with /compact once its turn ends", name, err)
		}
		via = "resume"
		if name == masterNode && inTurn {
			d.endNextEvent()
		}
		_, err = d.restart(ctx, n, id, dir, "/compact", nil)
	}
	d.mu.Lock()
	d.logUseLocked(Entry{Kind: "compact", Node: name, Via: via}, err)
	d.mu.Unlock()
	if err != nil {
		return "", err
	}
	// /compact waits for the turn's Stop, and the master's turn goes on until next_event returns.
	if name == masterNode && inTurn && via == "supervisor" {
		d.endNextEvent()
	}
	timer := time.NewTimer(compactWait)
	defer timer.Stop()
	select {
	case <-compacted:
		return fmt.Sprintf("Compacted %s's conversation.", name), nil
	case <-timer.C:
		return fmt.Sprintf("Sent /compact to %s, which has not finished compacting within %v: it compacts once its turn ends.", name, compactWait), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// endNextEvent makes the master's next_event return now, events or not, so that its turn ends.
func (d *daemon) endNextEvent() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.saidMaster = true
	close(d.wake)
	d.wake = make(chan struct{})
}

// clearConversation moves name, the master or the desk, to a new conversation, as the daemon's own clear of a stale one does, and cuts short a turn it is in first.
// The master's events and the desk's tells that the cut turn took wait again, for the new conversation.
func (d *daemon) clearConversation(ctx context.Context, caller, name string) (string, error) {
	if caller != "" {
		return "", errFrontendOnly
	}
	if err := conversationOf("clears", name); err != nil {
		return "", err
	}
	unlock := d.lockSession(name)
	defer unlock()
	n, id, _, err := d.steerable(name)
	if err != nil {
		return "", err
	}
	if name == masterNode {
		d.mu.Lock()
		workers := len(slices.DeleteFunc(slices.Collect(maps.Keys(d.nodes)), func(node string) bool { return !isWorker(node) }))
		d.mu.Unlock()
		if workers > 0 {
			return "", fmt.Errorf("bosun clears the master only once no worker is left, since its conversation keeps track of them: %d left", workers)
		}
	}
	_, a, err := d.listed(id)
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	inTurn := a.Status != "" && a.Status != "idle" && !n.settled()
	d.mu.Unlock()
	if inTurn {
		if err := d.cutForClear(ctx, n, id, a.PID); err != nil {
			return "", err
		}
	}
	err = d.clear(ctx, name)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.logUseLocked(Entry{Kind: "clear", Node: name, Via: "resume"}, err)
	if err != nil {
		return "", err
	}
	if name == masterNode {
		d.sleepLocked()
	} else {
		n.Unseen = false
		if len(n.Events) > 0 {
			go d.deliver(deskNode)
		}
	}
	d.saveLocked()
	return fmt.Sprintf("Cleared %s's conversation.", name), nil
}

// cutForClear stops n, session id in its turn in process pid, ahead of a clear, which would otherwise wait for the turn to end.
// No Stop runs, so its open decisions go, and what the turn took waits again: the master's events, over which it sleeps until the clear ends, or the desk's tells.
func (d *daemon) cutForClear(ctx context.Context, n *Session, id string, pid int) error {
	if err := d.hold(n, id); err != nil {
		return err
	}
	defer d.release(n)
	if err := d.claude.Stop(id); err != nil {
		return err
	}
	if err := d.awaitExit(ctx, n.Node, pid); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	n.pre = nil
	d.withdrawLocked(n.Node, func(*Decision) bool { return true }, outcome{by: "bosun", cause: "the user cleared " + n.Node + "'s conversation"})
	n.Turn = false
	if n.Node == masterNode {
		d.requeueLocked()
		d.asleep = true
	} else {
		d.giveBackLocked(n)
	}
	d.saveLocked()
	return nil
}
