package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"
)

const (
	pauseResetMaster = "The user paused bosun's sessions during your turn, and has resumed them. Carry on: call next_event."
	pauseReset       = "The user paused bosun's sessions during your turn, and has resumed them. Carry on where you left off."
)

// pause stops every session, and marks those in their turns, which unpause resumes. Until then the daemon resumes no session of its own accord.
// Pausing again stops what the user resumed since, such as by claude attach.
func (d *daemon) pause(ctx context.Context) (string, error) {
	d.pauseLock.Lock()
	defer d.pauseLock.Unlock()
	d.mu.Lock()
	if !d.paused {
		d.paused = true
		d.saveLocked()
		d.broadcastLocked(Event{Kind: "paused"})
		d.logLocked(Entry{Kind: "paused", By: "user"})
		log.Print("pausing")
	}
	d.mu.Unlock()
	// A resume or merge already under way could start a session after its stop.
	if err := d.awaitUnheld(ctx); err != nil {
		return "", err
	}
	agents, err := d.claude.Agents()
	if err != nil {
		return "", err
	}
	byID := map[string]agent{}
	for _, a := range agents {
		byID[a.ID] = a
	}

	d.mu.Lock()
	var stops []*Session
	var turns []string
	for _, n := range d.sortedLocked() {
		a, ok := byID[n.ID]
		if n.ID == "" || n.held || !ok || a.Status == "" {
			continue
		}
		// Marked first, so that a poll that sees the session stopped tells the master nothing of it.
		if !n.Killed && (n.Turn || !betweenTurns(a.Status)) {
			n.Paused, n.Turn = true, false
			turns = append(turns, n.Node)
		}
		n.held = true
		stops = append(stops, n)
	}
	d.asleep = true
	d.saveLocked()
	d.mu.Unlock()

	var failed []string
	for _, n := range stops {
		err := d.claude.Stop(n.ID)
		d.mu.Lock()
		n.held = false
		if err != nil {
			failed = append(failed, err.Error())
			n.Turn, n.Paused = n.Turn || n.Paused, false
		}
		d.mu.Unlock()
	}
	d.mu.Lock()
	// The master may have taken events up to its stop.
	if m := d.nodes[masterNode]; m != nil && m.Paused {
		d.requeueLocked()
	}
	d.stirred = true
	d.saveLocked()
	d.broadcastSessionsLocked()
	d.mu.Unlock()

	text := "Paused bosun's sessions. None was in its turn."
	if len(turns) > 0 {
		text = fmt.Sprintf("Paused bosun's sessions. Unpausing resumes the turns of %s.", strings.Join(turns, ", "))
	}
	if len(failed) > 0 {
		return text, fmt.Errorf("stopping some sessions failed, so they run on: %s", strings.Join(failed, "; "))
	}
	return text, nil
}

// awaitUnheld waits until no resume or merge holds a session, for as long as one can take.
func (d *daemon) awaitUnheld(ctx context.Context) error {
	deadline := time.Now().Add(idleWait + stopWait + time.Minute)
	for {
		d.mu.Lock()
		held := slices.ContainsFunc(d.sortedLocked(), func(n *Session) bool { return n.held })
		d.mu.Unlock()
		if !held {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("a resume or merge still runs, so pause again in a moment")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// unpause ends a pause, and resumes the sessions it stopped in their turns, the master first.
func (d *daemon) unpause() (string, error) {
	d.pauseLock.Lock()
	defer d.pauseLock.Unlock()
	d.mu.Lock()
	if !d.paused {
		d.mu.Unlock()
		return "", errors.New("bosun is not paused")
	}
	d.paused = false
	var due []string
	for _, n := range d.sortedLocked() {
		if n.Paused && n.Node != masterNode {
			due = append(due, n.Node)
		}
	}
	m := d.nodes[masterNode]
	master := m != nil && m.Paused
	if !master {
		d.sleepLocked()
	}
	d.saveLocked()
	d.broadcastLocked(Event{Kind: "unpaused"})
	d.mu.Unlock()
	log.Print("unpausing")

	var resumed, failed []string
	if master {
		if err := d.resumeMaster(pauseResetMaster); err != nil && !errors.Is(err, errInTurn) && !errors.Is(err, errStarting) {
			failed = append(failed, fmt.Sprintf("%s: %v", masterNode, err))
		} else {
			resumed = append(resumed, masterNode)
		}
		d.mu.Lock()
		m.Paused = false
		d.mu.Unlock()
	}
	for _, name := range due {
		err := d.carryOn(context.Background(), name, pauseReset)
		d.mu.Lock()
		n := d.nodes[name]
		switch {
		case n == nil:
		case err == nil, errors.Is(err, errInTurn), errors.Is(err, errStarting):
			// A session in its turn or starting already runs, as one the user resumed does.
			n.Paused = false
			resumed = append(resumed, name)
		default:
			n.Paused = false
			failed = append(failed, fmt.Sprintf("%s: %v", name, err))
			if !n.Killed && isWorker(name) {
				d.heardLocked(n)
				d.reportLocked(Event{Kind: "exit", Node: name, Text: fmt.Sprintf("The user paused bosun during its turn, and resuming it failed: %v", err)})
			}
		}
		d.saveLocked()
		d.broadcastSessionsLocked()
		d.mu.Unlock()
	}
	// The reports of children that came during the pause wait for a parent between turns.
	d.mu.Lock()
	for _, n := range d.sortedLocked() {
		if len(n.Events) > 0 {
			go d.deliver(n.Node)
		}
	}
	d.mu.Unlock()

	text := "Unpaused bosun. No session was paused in its turn."
	if len(resumed) > 0 {
		text = fmt.Sprintf("Unpaused bosun, and resumed the turns of %s.", strings.Join(resumed, ", "))
	}
	var err error
	logged := text
	if len(failed) > 0 {
		err = fmt.Errorf("resuming some sessions failed: %s", strings.Join(failed, "; "))
		logged += " Resuming failed for " + strings.Join(failed, "; ") + "."
	}
	d.mu.Lock()
	d.logLocked(Entry{Kind: "unpaused", By: "user", Text: logged})
	d.mu.Unlock()
	return text, err
}
