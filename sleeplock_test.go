package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type fakeInhibitor struct {
	fail  error
	taken int
	last  *fakeHold
}

type fakeHold struct {
	released bool
	done     chan struct{}
	err      error
}

func (f *fakeInhibitor) Inhibit(string) (sleepHold, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	f.taken++
	f.last = &fakeHold{done: make(chan struct{})}
	return f.last, nil
}

func (h *fakeHold) Release()              { h.released = true }
func (h *fakeHold) Done() <-chan struct{} { return h.done }
func (h *fakeHold) Err() error            { return h.err }

// sleepDaemon is a daemon whose sleep lock is a fake, with worker a in a turn.
func sleepDaemon(t *testing.T) (*daemon, *fakeInhibitor) {
	d, _ := testDaemon(t)
	f := &fakeInhibitor{}
	d.sleep = sleepLock{mode: "on", quiet: 10 * time.Minute, inhibitor: f, onBattery: func() bool { return true }}
	d.mu.Lock()
	d.nodes["a"].Turn = true
	d.mu.Unlock()
	return d, f
}

func sleepEntries(d *daemon) []Entry {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Entry
	for _, e := range d.logged {
		if e.Kind == "sleeplock" {
			out = append(out, e)
		}
	}
	return out
}

func endTurn(d *daemon, node string) {
	d.mu.Lock()
	d.nodes[node].Turn = false
	d.mu.Unlock()
}

func TestSleepLockFollowsTheFleet(t *testing.T) {
	d, f := sleepDaemon(t)
	at := time.Now()

	d.checkSleepLock(at)
	if f.taken != 1 {
		t.Fatalf("a worker works: %d locks taken", f.taken)
	}
	d.mu.Lock()
	shown := d.snapshotLocked().SleepLock
	d.mu.Unlock()
	if shown == nil || !shown.Until.IsZero() {
		t.Fatalf("the snapshot while a session works: %+v", shown)
	}
	d.checkSleepLock(at.Add(time.Minute))
	if f.taken != 1 {
		t.Fatalf("held already, yet %d taken", f.taken)
	}

	// The gap before the next spawn keeps the lock.
	endTurn(d, "a")
	d.checkSleepLock(at.Add(2 * time.Minute))
	d.checkSleepLock(at.Add(10 * time.Minute))
	if f.last.released {
		t.Fatal("released before the quiet spell was over")
	}
	d.mu.Lock()
	shown = d.snapshotLocked().SleepLock
	d.mu.Unlock()
	if shown == nil || !shown.Until.Equal(at.Add(time.Minute+10*time.Minute)) {
		t.Fatalf("the snapshot while the fleet is quiet: %+v", shown)
	}

	// New work inside the spell holds on.
	d.mu.Lock()
	d.nodes["a"].Turn = true
	d.mu.Unlock()
	d.checkSleepLock(at.Add(10*time.Minute + 30*time.Second))
	endTurn(d, "a")
	d.checkSleepLock(at.Add(20 * time.Minute))
	if f.last.released || f.taken != 1 {
		t.Fatalf("work restarted the spell: released %v, taken %d", f.last.released, f.taken)
	}
	d.checkSleepLock(at.Add(21 * time.Minute))
	if !f.last.released {
		t.Fatal("still held after the quiet spell")
	}
	d.mu.Lock()
	shown = d.snapshotLocked().SleepLock
	d.mu.Unlock()
	if shown != nil {
		t.Fatalf("the snapshot once released: %+v", shown)
	}
	entries := sleepEntries(d)
	if len(entries) != 2 || !strings.Contains(entries[0].Text, "took") || !strings.Contains(entries[1].Text, "let go") {
		t.Fatalf("log: %+v", entries)
	}

	// Quiet since before the release, nothing takes it again.
	d.checkSleepLock(at.Add(30 * time.Minute))
	if f.taken != 1 {
		t.Fatalf("taken again with no work: %d", f.taken)
	}
}

func TestSleepLockIgnoresWhatWaitsOnTheUser(t *testing.T) {
	d, f := sleepDaemon(t)
	d.mu.Lock()
	d.decisions["7"] = &Decision{ID: "7", Kind: "permission", Node: "a", Holder: "user"}
	d.decisions["8"] = &Decision{ID: "8", Kind: "notice", Node: "a", Holder: "user", Note: "just a notice"}
	d.mu.Unlock()
	d.checkSleepLock(time.Now())
	if f.taken != 0 {
		t.Fatal("a worker waiting on the user holds the machine awake")
	}
	d.mu.Lock()
	delete(d.decisions, "7")
	d.decisions["9"] = &Decision{ID: "9", Kind: "permission", Node: "a", Holder: "master"}
	d.mu.Unlock()
	d.checkSleepLock(time.Now())
	if f.taken != 1 {
		t.Fatal("a worker waiting on the master is working")
	}
}

func TestSleepLockCountsNoDeskAndNoSleepingMaster(t *testing.T) {
	d, f := sleepDaemon(t)
	endTurn(d, "a")
	d.mu.Lock()
	d.nodes[masterNode].Turn, d.asleep = true, true
	d.nodes[deskNode] = &Session{Node: deskNode, Turn: true, Status: "busy"}
	d.mu.Unlock()
	d.checkSleepLock(time.Now())
	if f.taken != 0 {
		t.Fatal("the desk or a sleeping master counted as working")
	}
	d.mu.Lock()
	d.asleep = false
	d.mu.Unlock()
	d.checkSleepLock(time.Now())
	if f.taken != 1 {
		t.Fatal("the master in its turn did not count")
	}
}

func TestSleepLockModes(t *testing.T) {
	d, f := sleepDaemon(t)
	d.sleep.mode = "off"
	d.checkSleepLock(time.Now())
	if f.taken != 0 {
		t.Fatal("off takes no lock")
	}

	d.sleep.mode = "battery"
	onBattery := false
	d.sleep.onBattery = func() bool { return onBattery }
	d.checkSleepLock(time.Now())
	if f.taken != 0 {
		t.Fatal("battery mode took the lock on mains power")
	}
	onBattery = true
	d.checkSleepLock(time.Now())
	if f.taken != 1 {
		t.Fatal("battery mode took no lock on the battery")
	}
	onBattery = false
	d.checkSleepLock(time.Now())
	if !f.last.released {
		t.Fatal("battery mode kept the lock on mains power")
	}
}

func TestSleepLockFailureAndLoss(t *testing.T) {
	d, f := sleepDaemon(t)
	at := time.Now()
	f.fail = errors.New("no D-Bus")
	d.checkSleepLock(at)
	d.checkSleepLock(at.Add(time.Second))
	d.checkSleepLock(at.Add(2 * sleepRetry))
	entries := sleepEntries(d)
	if len(entries) != 1 || entries[0].Error != "no D-Bus" {
		t.Fatalf("a failing lock is logged once: %+v", entries)
	}

	f.fail = nil
	d.checkSleepLock(at.Add(4 * sleepRetry))
	if f.taken != 1 {
		t.Fatalf("not retaken: %d", f.taken)
	}
	// systemd-inhibit dying leaves the machine unprotected until the retry.
	f.last.err = errors.New("killed")
	close(f.last.done)
	lostAt := at.Add(5 * sleepRetry)
	d.checkSleepLock(lostAt)
	d.checkSleepLock(lostAt.Add(time.Second))
	if f.taken != 1 {
		t.Fatalf("retaken before the wait: %d", f.taken)
	}
	d.checkSleepLock(lostAt.Add(sleepRetry))
	if f.taken != 2 {
		t.Fatalf("not retaken after the wait: %d", f.taken)
	}
}

func TestReleaseSleepLockOnExit(t *testing.T) {
	d, f := sleepDaemon(t)
	d.checkSleepLock(time.Now())
	d.releaseSleepLock()
	if !f.last.released {
		t.Fatal("the lock outlived the daemon's loop")
	}
}

func TestOnBatteryAt(t *testing.T) {
	dir := t.TempDir()
	supply := func(name, kind, status string) {
		p := filepath.Join(dir, name)
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
		for file, text := range map[string]string{"type": kind, "status": status} {
			if err := os.WriteFile(filepath.Join(p, file), []byte(text+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if onBatteryAt(dir) {
		t.Error("a machine with no battery runs on one")
	}
	supply("AC", "Mains", "")
	supply("BAT0", "Battery", "Charging")
	if onBatteryAt(dir) {
		t.Error("a charging battery counts as discharging")
	}
	supply("BAT1", "Battery", "Discharging")
	if !onBatteryAt(dir) {
		t.Error("a discharging battery was missed")
	}
}

// fakeSystemdInhibit puts a systemd-inhibit on PATH that records its process id and runs the command it is given, as the real one does.
func fakeSystemdInhibit(t *testing.T) (pidFile string) {
	dir := t.TempDir()
	pidFile = filepath.Join(dir, "pid")
	script := "#!/bin/sh\necho $$ > " + pidFile + "\nwhile case $1 in --*) true;; *) false;; esac; do shift; done\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "systemd-inhibit"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return pidFile
}

func TestSystemdInhibitorReleaseKillsTheCommand(t *testing.T) {
	pidFile := fakeSystemdInhibit(t)
	h, err := systemdInhibitor{}.Inhibit("test")
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	for range 100 {
		if data, err := os.ReadFile(pidFile); err == nil && strings.HasSuffix(string(data), "\n") {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("the command never started")
	}
	select {
	case <-h.Done():
		t.Fatal("the lock ended on its own")
	default:
	}
	h.Release()
	h.Release()
	if err := syscall.Kill(pid, 0); err == nil {
		t.Errorf("process %d outlived Release", pid)
	}
	select {
	case <-h.Done():
	default:
		t.Error("Done is open after Release")
	}
}

func TestSystemdInhibitorReportsAFailure(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'Failed to connect to bus' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "systemd-inhibit"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	h, err := systemdInhibitor{}.Inhibit("test")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a command that exits leaves the lock open")
	}
	if h.Err() == nil || !strings.Contains(h.Err().Error(), "Failed to connect to bus") {
		t.Errorf("error: %v", h.Err())
	}
}

func TestPrintStatusShowsTheSleepLock(t *testing.T) {
	var b strings.Builder
	printStatus(&b, Response{SleepLock: &SleepLock{Since: time.Now()}})
	if !strings.Contains(b.String(), "Holding a sleep lock") || !strings.Contains(b.String(), "the fleet works") {
		t.Errorf("status:\n%s", b.String())
	}
	b.Reset()
	printStatus(&b, Response{SleepLock: &SleepLock{Since: time.Now(), Until: time.Now()}})
	if !strings.Contains(b.String(), "lets go at") {
		t.Errorf("status:\n%s", b.String())
	}
}
