package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"time"
)

// limitProbe spaces the resumes of a session that a usage limit stopped, when bosun cannot read when the limit resets.
const limitProbe = 30 * time.Minute

const (
	limitResetMaster = "The usage limit that stopped your last turn has reset. Carry on: call next_event."
	limitReset       = "The usage limit that stopped your last turn has reset. Carry on where you left off."
)

// resetsAt matches when a limit resets in Claude Code 2.1.310's message: "resets 3pm (Europe/Berlin)", or past a day "resets Oct 27, 3:30pm (Europe/Berlin)".
var resetsAt = regexp.MustCompile(`resets (?:([A-Z][a-z]{2}) (\d{1,2}), (?:(\d{4}), )?)?(\d{1,2})(?::(\d{2}))?([ap]m)(?: \(([^)]+)\))?`)

// resetTime reads when a usage limit resets from Claude Code's message about it.
func resetTime(text string, now time.Time) (time.Time, bool) {
	m := resetsAt.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	loc := now.Location()
	if l, err := time.LoadLocation(m[7]); m[7] != "" && err == nil {
		loc = l
	}
	now = now.In(loc)
	hour, _ := strconv.Atoi(m[4])
	minute, _ := strconv.Atoi(m[5])
	hour %= 12
	if m[6] == "pm" {
		hour += 12
	}
	year, month, day := now.Date()
	if m[1] != "" {
		mon, err := time.Parse("Jan", m[1])
		if err != nil {
			return time.Time{}, false
		}
		month = mon.Month()
		day, _ = strconv.Atoi(m[2])
		if m[3] != "" {
			year, _ = strconv.Atoi(m[3])
		}
	}
	t := time.Date(year, month, day, hour, minute, 0, 0, loc)
	switch {
	case m[1] == "" && t.Before(now):
		t = t.AddDate(0, 0, 1)
	case m[1] != "" && m[3] == "" && t.Before(now.AddDate(0, 0, -1)):
		t = t.AddDate(1, 0, 0)
	}
	return t, true
}

// limitLocked marks n stopped by the usage limit that text describes, until the limit resets, and tells the user unless an earlier notice of a limit still waits.
func (d *daemon) limitLocked(n *Session, text string) {
	now := time.Now()
	until, ok := resetTime(text, now)
	if !ok {
		until = now.Add(limitProbe)
	}
	// The message gives the minute, and a resume within it may still meet the limit.
	until = until.Add(time.Minute)
	n.LimitedUntil = &until
	d.saveLocked()
	d.broadcastSessionsLocked()
	log.Printf("%s stopped at a usage limit until %s", n.Node, until.Format(time.RFC3339))
	if d.decisions[d.limitNotice] == nil {
		dec := &Decision{Kind: "notice", Node: masterNode, Holder: "user", Note: fmt.Sprintf("Claude Code stopped %s at a usage limit: %q. bosun resumes each session the limit stops once it resets, %s for this one.", n.Node, text, until.Format("Jan 2 15:04"))}
		d.openLocked(dec)
		d.limitNotice = dec.ID
	}
}

// recoverLimited resumes each session that a usage limit stopped once the limit has reset, the master first, since it settles what the workers need.
func (d *daemon) recoverLimited(now time.Time) {
	d.mu.Lock()
	if d.recovering || d.paused {
		d.mu.Unlock()
		return
	}
	var due []string
	for _, n := range d.sortedLocked() {
		if n.LimitedUntil == nil || now.Before(*n.LimitedUntil) {
			continue
		}
		if n.Node == masterNode {
			due = append([]string{n.Node}, due...)
		} else {
			due = append(due, n.Node)
		}
	}
	if len(due) == 0 {
		d.mu.Unlock()
		return
	}
	d.recovering = true
	d.mu.Unlock()
	go func() {
		for _, name := range due {
			if err := d.recover(name); err != nil && !errors.Is(err, errInTurn) && !errors.Is(err, errStarting) {
				log.Printf("resuming %s after the usage limit: %v", name, err)
			}
		}
		d.mu.Lock()
		d.recovering = false
		d.mu.Unlock()
	}()
}

// recover resumes name, which a usage limit stopped. Should the limit hold, its next StopFailure marks it again.
func (d *daemon) recover(name string) error {
	d.mu.Lock()
	n := d.nodes[name]
	if n == nil {
		d.mu.Unlock()
		return nil
	}
	until := n.LimitedUntil
	n.LimitedUntil = nil
	d.saveLocked()
	d.mu.Unlock()
	var err error
	if name != masterNode {
		err = d.carryOn(context.Background(), name, limitReset)
	} else {
		err = d.resumeMaster(limitResetMaster)
	}
	if errors.Is(err, errPaused) {
		// The pause that began meanwhile stopped it out of its turn, so unpausing would not resume it.
		d.mu.Lock()
		n.LimitedUntil = until
		d.saveLocked()
		d.mu.Unlock()
	}
	return err
}

// resumeMaster resumes the master with prompt, and leaves it asleep, for the next event to try again, when that fails.
func (d *daemon) resumeMaster(prompt string) error {
	d.masterLock.Lock()
	defer d.masterLock.Unlock()
	d.mu.Lock()
	d.asleep = false
	d.mu.Unlock()
	err := d.carryOn(context.Background(), masterNode, prompt)
	if err != nil && !errors.Is(err, errInTurn) && !errors.Is(err, errStarting) {
		d.mu.Lock()
		d.asleep = true
		d.mu.Unlock()
	}
	return err
}
