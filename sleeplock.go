package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// defaultSleepLockQuiet is how long the fleet may stay quiet before the daemon lets go of the sleep lock, so that a gap between a merge and the next spawn does not let the machine sleep.
const defaultSleepLockQuiet = 10 * time.Minute

// sleepRetry is how long the daemon waits after a lock failed to start or ended of itself before it takes one again.
const sleepRetry = time.Minute

// powerSupplyDir is where the kernel lists the machine's batteries and power supplies.
const powerSupplyDir = "/sys/class/power_supply"

// sleepInhibitor takes a block-mode logind lock on sleep, which the tests fake since they cannot reach D-Bus.
type sleepInhibitor interface {
	Inhibit(why string) (sleepHold, error)
}

// sleepHold is a lock taken. Release lets go of it. Done closes once it is gone, whoever ended it, and Err then says why if it was not Release.
type sleepHold interface {
	Release()
	Done() <-chan struct{}
	Err() error
}

// sleepLock is the daemon's hold on the machine's sleep, guarded by the daemon's mu.
type sleepLock struct {
	// mode is on, off, or battery for a hold only while the machine runs on its battery.
	mode      string
	quiet     time.Duration
	inhibitor sleepInhibitor
	onBattery func() bool

	hold sleepHold
	// working is when a session other than the desk last worked, as of a tick.
	working time.Time
	// retryAt is when a lock that failed may be taken again, and failing marks a failure the log has told of.
	retryAt time.Time
	failing bool
	// shown is what the frontends last heard.
	shown *SleepLock
}

// SleepLock is the sleep lock the daemon holds, as the frontends see it. Until is when it lets go if the fleet stays quiet, and is left out while a session works.
type SleepLock struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until,omitzero"`
}

func checkSleepLockMode(mode string) error {
	if mode != "on" && mode != "off" && mode != "battery" {
		return fmt.Errorf("%q is not on, off or battery", mode)
	}
	return nil
}

// workingLocked counts the sessions other than the desk that work: those in a turn that does not wait on the user, since a decision left with the user could otherwise hold the machine awake all night.
func (d *daemon) workingLocked() int {
	waiting := map[string]bool{}
	for _, dec := range d.decisions {
		// A notice is for the user to dismiss, except one that says a session waits in its own prompt.
		if dec.Holder == "user" && (dec.Kind != "notice" || dec.Prompt) {
			waiting[dec.Node] = true
		}
	}
	working := 0
	for _, n := range d.nodes {
		if n.Node == deskNode || !n.Turn || n.Killed || n.Paused || n.LimitedUntil != nil || waiting[n.Node] || betweenTurns(n.Status) || n.Node == masterNode && d.asleep {
			continue
		}
		working++
	}
	return working
}

// checkSleepLock takes the sleep lock while a session works and, with the machine on the battery if the mode says so, holds it until the fleet has been quiet for the quiet spell.
// It never suspends the machine: after the release the user's own idle step does.
func (d *daemon) checkSleepLock(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := &d.sleep
	if s.mode == "off" || d.stopping {
		return
	}
	if s.hold != nil {
		select {
		case <-s.hold.Done():
			d.sleepFailedLocked(now, fmt.Errorf("the lock ended of itself: %w", s.hold.Err()))
			s.hold = nil
		default:
		}
	}
	allowed := s.mode == "on" || s.onBattery()
	n := d.workingLocked()
	if allowed && n > 0 {
		s.working = now
	}
	hold := allowed && !s.working.IsZero() && (n > 0 || now.Sub(s.working) < s.quiet)
	switch {
	case hold && s.hold == nil && !now.Before(s.retryAt):
		h, err := s.inhibitor.Inhibit("bosun: the fleet is working")
		if err != nil {
			d.sleepFailedLocked(now, err)
			break
		}
		s.hold = h
		d.logLocked(Entry{Kind: "sleeplock", By: "bosun", Text: fmt.Sprintf("took the sleep lock: %d sessions work", n)})
		log.Printf("sleep lock taken, since %d sessions work", n)
	case !hold && s.hold != nil:
		s.hold.Release()
		s.hold, s.failing = nil, false
		why := fmt.Sprintf("the fleet has been quiet for %s", since(now, s.working))
		if !allowed {
			why = "the machine runs on mains power"
		}
		d.logLocked(Entry{Kind: "sleeplock", By: "bosun", Text: "let go of the sleep lock: " + why})
		log.Printf("sleep lock released: %s", why)
	}
	d.publishSleepLockLocked(n)
}

// sleepFailedLocked notes that no lock is held although one is wanted, tells the log of the first failure of a run, and waits before it tries again.
func (d *daemon) sleepFailedLocked(now time.Time, err error) {
	s := &d.sleep
	s.retryAt = now.Add(sleepRetry)
	log.Printf("sleep lock: %v", err)
	if !s.failing {
		s.failing = true
		d.logLocked(Entry{Kind: "sleeplock", By: "bosun", Text: "could not hold the sleep lock", Error: err.Error()})
	}
}

// publishSleepLockLocked tells the frontends when the lock was taken or released, or the fleet went quiet or began to work again.
func (d *daemon) publishSleepLockLocked(working int) {
	s := &d.sleep
	var now *SleepLock
	if s.hold != nil {
		now = &SleepLock{Since: s.shown.sinceOr(time.Now())}
		if working == 0 {
			now.Until = s.working.Add(s.quiet).Round(0)
		}
	}
	if now == nil && s.shown == nil || now != nil && s.shown != nil && *now == *s.shown {
		return
	}
	s.shown = now
	d.broadcastLocked(Event{Kind: "sleeplock", SleepLock: now})
}

// sinceOr is when the lock shown began, or t for one just taken.
func (l *SleepLock) sinceOr(t time.Time) time.Time {
	if l == nil {
		return t.Round(0)
	}
	return l.Since
}

// releaseSleepLock lets go of the lock as the daemon exits.
func (d *daemon) releaseSleepLock() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sleep.hold != nil {
		d.sleep.hold.Release()
		d.sleep.hold = nil
	}
}

// sleepLockLine is how a frontend shows that the daemon holds the sleep lock.
func sleepLockLine(l SleepLock) string {
	s := "Holding a sleep lock since " + l.Since.Local().Format("15:04")
	if l.Until.IsZero() {
		return s + ": the fleet works."
	}
	return s + ": the fleet is quiet, so it lets go at " + l.Until.Local().Format("15:04") + " unless a session works again."
}

// onBatteryAt reports whether a battery under dir, laid out as the kernel's power_supply directory is, discharges.
func onBatteryAt(dir string) bool {
	supplies, _ := filepath.Glob(filepath.Join(dir, "*"))
	for _, p := range supplies {
		kind, _ := os.ReadFile(filepath.Join(p, "type"))
		status, _ := os.ReadFile(filepath.Join(p, "status"))
		if strings.TrimSpace(string(kind)) == "Battery" && strings.TrimSpace(string(status)) == "Discharging" {
			return true
		}
	}
	return false
}

// systemdInhibitor holds the lock with systemd-inhibit around a sleep that never ends, so that the daemon needs no D-Bus library.
type systemdInhibitor struct{}

func (systemdInhibitor) Inhibit(why string) (sleepHold, error) {
	h := &systemdHold{release: make(chan struct{}), done: make(chan struct{})}
	started := make(chan error, 1)
	go h.run(why, started)
	if err := <-started; err != nil {
		return nil, err
	}
	return h, nil
}

type systemdHold struct {
	release chan struct{}
	done    chan struct{}
	err     error
}

func (h *systemdHold) Release() {
	select {
	case <-h.release:
	default:
		close(h.release)
	}
	<-h.done
}

func (h *systemdHold) Done() <-chan struct{} { return h.done }

// Err is read only after Done closes.
func (h *systemdHold) Err() error { return h.err }

func (h *systemdHold) run(why string, started chan<- error) {
	defer close(h.done)
	// The kernel signals a Pdeathsig child when the thread that forked it exits, not the process, so this thread stays put until the lock ends.
	runtime.LockOSThread()
	var stderr bytes.Buffer
	cmd := exec.Command("systemd-inhibit", "--what=sleep", "--mode=block", "--who=bosun", "--why="+why, "sleep", "infinity")
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		started <- err
		return
	}
	started <- nil
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case <-h.release:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
	case err := <-exited:
		h.err = err
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			h.err = fmt.Errorf("%v: %s", err, msg)
		}
	}
}
