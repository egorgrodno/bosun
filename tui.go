package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unsafe"
)

// tui is the terminal frontend's state: the daemon's view, the selected line and an answer being typed.
type tui struct {
	sessions  []Session
	decisions []Decision
	log       []Entry
	answered  []Answered
	paused    bool
	away      *Away
	sleepLock *SleepLock
	parked    []Parked
	grants    []Grant
	connected bool
	sel       int
	// answering is the question an answer is being typed for, or switching the master or the desk a model and effort are being typed for, or granting a grant being typed, and answer the text so far.
	answering string
	switching string
	granting  bool
	answer    []rune
	err       string
	// layout is the keyboard layout, qwerty or hallmack, which picks the movement keys.
	layout string
	// armed is the key a first press armed, y, d or p, and armedAt when. A second press of it within armTimeout acts, as in the panel. Any other key or the timeout disarms it.
	armed   string
	armedAt time.Time
	// now is the clock, time.Now unless a test sets it.
	now func() time.Time
}

// armTimeout is how long a first press stays armed.
const armTimeout = 3 * time.Second

func (t *tui) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// armedKey returns the key still armed, or "".
func (t *tui) armedKey() string {
	if t.armed != "" && t.clock().Sub(t.armedAt) < armTimeout {
		return t.armed
	}
	return ""
}

// layoutHallmack is the one layout name that changes the keys. Anything else is qwerty.
const layoutHallmack = "hallmack"

// moveKeys returns the letters that move down and up on the layout. Arrows work on both.
func moveKeys(layout string) (down, up string) {
	if layout == layoutHallmack {
		return "a", "e"
	}
	return "j", "k"
}

// moveHint is the footer's text for the movement keys.
func moveHint(layout string) string {
	down, up := moveKeys(layout)
	return down + "/" + up + " move"
}

// tuiItem is a selectable line: a decision, a standing grant, or a session at its depth in the tree.
type tuiItem struct {
	decision *Decision
	grant    *Grant
	session  *Session
	depth    int
}

// tuiAction is what a key asks of the daemon or the terminal.
type tuiAction struct {
	op, id, name, text string
}

// items lists the decisions waiting on the user, then those waiting on the master, then the standing grants, then the sessions depth first.
func (t *tui) items() []tuiItem {
	var items []tuiItem
	for _, holder := range []string{"user", "master"} {
		for i := range t.decisions {
			if t.decisions[i].Holder == holder {
				items = append(items, tuiItem{decision: &t.decisions[i]})
			}
		}
	}
	for i := range t.grants {
		items = append(items, tuiItem{grant: &t.grants[i]})
	}
	children := map[string][]int{}
	for i, s := range t.sessions {
		if s.Node == deskNode {
			children[""] = append([]int{i}, children[""]...)
			continue
		}
		children[s.Parent] = append(children[s.Parent], i)
	}
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		for _, i := range children[parent] {
			items = append(items, tuiItem{session: &t.sessions[i], depth: depth})
			walk(t.sessions[i].Node, depth+1)
		}
	}
	walk("", 0)
	return items
}

func (t *tui) selected() tuiItem {
	items := t.items()
	if t.sel < 0 || t.sel >= len(items) {
		return tuiItem{}
	}
	return items[t.sel]
}

// apply takes a line of the watch stream.
func (t *tui) apply(r Response) {
	switch {
	case r.Type == "snapshot":
		t.sessions, t.decisions, t.log, t.answered, t.paused, t.connected = r.Sessions, r.Decisions, r.Log, r.Answered, r.Paused, true
		t.away, t.parked = r.Away, r.Parked
		t.grants = r.Grants
		t.sleepLock = r.SleepLock
	case r.Event == nil:
	case r.Event.Kind == "paused", r.Event.Kind == "unpaused":
		t.paused = r.Event.Kind == "paused"
	case r.Event.Kind == "sleeplock":
		t.sleepLock = r.Event.SleepLock
	case r.Event.Kind == "away":
		t.away, t.parked = r.Event.Away, nil
	case r.Event.Kind == "back":
		t.away, t.parked = nil, nil
	case r.Event.Kind == "grants":
		t.grants = r.Event.Grants
	case r.Event.Kind == "parked" && r.Event.Parked != nil:
		t.parked = append(t.parked, *r.Event.Parked)
	case r.Event.Kind == "sessions":
		t.sessions = r.Event.Sessions
	case r.Event.Kind == "activity":
		for i := range t.sessions {
			if t.sessions[i].Node == r.Event.Node {
				t.sessions[i].Activity = r.Event.Activity
			}
		}
	case r.Event.Kind == "context":
		for i := range t.sessions {
			if t.sessions[i].Node == r.Event.Node {
				t.sessions[i].Context = r.Event.Context
			}
		}
	case r.Event.Kind == "log" && r.Event.Entry != nil:
		t.log = append(t.log, *r.Event.Entry)
		t.log = t.log[max(0, len(t.log)-shownLog):]
	case r.Event.Kind == "answered" && r.Event.Answered != nil:
		t.answered = append(t.answered, *r.Event.Answered)
		t.answered = t.answered[max(0, len(t.answered)-keptMessages):]
	case r.Event.Kind == "decision" && r.Event.Decision != nil:
		d := *r.Event.Decision
		t.decisions = append(slices.DeleteFunc(t.decisions, func(o Decision) bool { return o.ID == d.ID }), d)
		slices.SortFunc(t.decisions, func(a, b Decision) int { return byID(a.ID, b.ID) })
	case r.Event.Kind == "settled":
		t.decisions = slices.DeleteFunc(t.decisions, func(o Decision) bool { return o.ID == r.Event.ID })
		if t.answering == r.Event.ID {
			t.answering, t.answer = "", nil
		}
	}
	t.sel = max(0, min(t.sel, len(t.items())-1))
}

// key handles one key, and returns what it asks for, if anything.
func (t *tui) key(k string) (tuiAction, bool) {
	was := t.armedKey()
	t.armed = ""
	// twice acts on the second press of k, and otherwise arms it.
	twice := func(a tuiAction) (tuiAction, bool) {
		if was == k {
			return a, true
		}
		t.armed, t.armedAt = k, t.clock()
		return tuiAction{}, false
	}
	if t.answering != "" || t.switching != "" || t.granting {
		switch k {
		case "\x1b":
			t.answering, t.switching, t.granting, t.answer = "", "", false, nil
		case "\r":
			text := strings.TrimSpace(string(t.answer))
			a := tuiAction{op: "reply", id: t.answering, text: text}
			if t.switching != "" {
				a = tuiAction{op: "model", name: t.switching, text: text}
			}
			if t.granting {
				a = tuiAction{op: "grant", text: text}
			}
			if text != "" {
				t.answering, t.switching, t.granting, t.answer = "", "", false, nil
				return a, true
			}
		case "\x7f", "\b":
			if len(t.answer) > 0 {
				t.answer = t.answer[:len(t.answer)-1]
			}
		default:
			if r := []rune(k); len(r) == 1 && unicode.IsPrint(r[0]) {
				t.answer = append(t.answer, r[0])
			}
		}
		return tuiAction{}, false
	}
	it := t.selected()
	kind := ""
	if it.decision != nil {
		kind = it.decision.Kind
	}
	down, up := moveKeys(t.layout)
	switch {
	case k == "q" || k == "\x03":
		return tuiAction{op: "quit"}, true
	case k == "p" && t.paused:
		return twice(tuiAction{op: "unpause"})
	case k == "p":
		return twice(tuiAction{op: "pause"})
	case k == "w" && t.away != nil:
		return twice(tuiAction{op: "away", name: "off"})
	case k == "w":
		return twice(tuiAction{op: "away", name: "on"})
	case k == down || k == "\x1b[B":
		t.sel = min(t.sel+1, len(t.items())-1)
	case k == up || k == "\x1b[A":
		t.sel = max(t.sel-1, 0)
	case k == "y" && kind == "permission":
		return twice(tuiAction{op: "approve", id: it.decision.ID})
	case k == "d" && kind == "permission":
		return twice(tuiAction{op: "deny", id: it.decision.ID})
	case k == "x" && kind == "notice":
		return tuiAction{op: "dismiss", id: it.decision.ID}, true
	case k == "x" && it.grant != nil:
		return tuiAction{op: "revoke", id: it.grant.ID}, true
	case k == "n":
		t.granting = true
	case (k == "r" || k == "\r") && kind == "question":
		t.answering = it.decision.ID
	case k == "m" && it.session != nil && (it.session.Node == masterNode || it.session.Node == deskNode):
		t.switching = it.session.Node
	case k == "\r" && it.session != nil && it.session.ID != "" && it.session.Status != "exited":
		return tuiAction{op: "attach", id: it.session.ID}, true
	}
	return tuiAction{}, false
}

const (
	reverse = "\x1b[7m"
	dim     = "\x1b[2m"
	yellow  = "\x1b[33m"
	plain   = "\x1b[0m"
)

// render lays out the screen in at most height lines of width columns.
func (t *tui) render(width, height int) []string {
	var lines []string
	selLine := 0
	add := func(style, text string, selected bool) {
		text = clip(text, width)
		if selected {
			selLine = len(lines)
			style = reverse
			text += strings.Repeat(" ", max(0, width-len([]rune(text))))
		}
		if style != "" {
			text = style + text + plain
		}
		lines = append(lines, text)
	}
	if !t.connected {
		add(dim, "Waiting for the bosun daemon...", false)
	}
	if t.paused {
		add(yellow, "Paused: p resumes the turns it stopped", false)
	}
	if t.sleepLock != nil {
		add(dim, sleepLockLine(*t.sleepLock), false)
	}
	if t.away != nil {
		add(yellow, awayHeader(*t.away)+" w comes back", false)
	}
	if len(t.parked) > 0 {
		add(dim, "Parked while you are away", false)
		for _, p := range t.parked {
			add("", fmt.Sprintf("  %-4s %-14s %s", p.Decision.ID, p.Decision.Node, parkedLine(p)), false)
			add(dim, "       because "+p.Why, false)
		}
		add("", "", false)
	}
	items := t.items()
	section := ""
	for i, it := range items {
		title := "Sessions"
		if it.decision != nil {
			title = "Waiting on the " + it.decision.Holder
			if it.decision.Holder == "user" {
				title = "Waiting on you"
			}
		}
		if it.grant != nil {
			title = grantsHeader(t.away != nil)
		}
		if title != section {
			if section != "" {
				add("", "", false)
			}
			add(dim, title, false)
			section = title
		}
		if d := it.decision; d != nil {
			add("", fmt.Sprintf("  %-4s %-14s %s", d.ID, d.Node, describe(*d)), i == t.sel)
			if d.Note != "" && d.Kind != "notice" {
				add(dim, "       master: "+d.Note, false)
			}
			if d.Kind == "notice" {
				add("", "       "+d.Note, false)
			}
			continue
		}
		if g := it.grant; g != nil {
			add("", "  "+grantLine(*g, t.clock()), i == t.sel)
			add(dim, "       "+classWhat(g.Class), false)
			continue
		}
		s := it.session
		add("", fmt.Sprintf("  %s%-*s %-8s %-15s %s", strings.Repeat("  ", it.depth), max(1, 14-2*it.depth), s.Node, s.ID, sessionState(*s), strings.TrimSpace(modelLabel(*s)+" "+contextLabel(s.Context))), i == t.sel)
		if line := activityLine(s.Activity); line != "" {
			add(dim, "    "+strings.Repeat("  ", it.depth)+line, false)
		}
	}
	if len(t.answered) > 0 {
		add("", "", false)
		add(dim, "Answered by the desk on your word", false)
	}
	for _, a := range t.answered {
		add("", fmt.Sprintf("  %-4s %-14s %s", a.Decision.ID, a.Decision.Node, answeredLine(a)), false)
	}
	if len(t.log) > 0 {
		add("", "", false)
		add(dim, "Log", false)
	}
	for _, e := range t.log[max(0, len(t.log)-shownLog):] {
		add("", "  "+logLine(e), false)
	}

	var foot []string
	switch it := t.selected(); {
	case t.armedKey() != "":
		hint := map[string]string{"y": "approve", "d": "deny", "p": "pause", "w": "turn away mode on"}[t.armed]
		switch {
		case t.armed == "p" && t.paused:
			hint = "unpause"
		case t.armed == "w" && t.away != nil:
			hint = "come back"
		}
		foot = append(foot, yellow+"Press "+t.armed+" again to "+hint+plain)
	case t.answering != "":
		foot = append(foot, fmt.Sprintf("answer %s: %s_", t.answering, string(t.answer)), dim+clip("enter send · esc cancel", width)+plain)
	case t.granting:
		foot = append(foot, fmt.Sprintf("grant: %s_", string(t.answer)), dim+clip("a class ("+classNames()+"), when it ends (09:00 or 8h) and your words · enter grant · esc cancel", width)+plain)
	case it.decision != nil && it.decision.Kind == "permission":
		foot = append(foot, dim+clip("y approve · d deny · "+moveHint(t.layout)+" · q quit", width)+plain)
	case it.decision != nil && it.decision.Kind == "question":
		foot = append(foot, dim+clip("r answer · "+moveHint(t.layout)+" · q quit", width)+plain)
	case t.switching != "":
		foot = append(foot, fmt.Sprintf("switch %s to: %s_", t.switching, string(t.answer)), dim+clip("a model, an effort ("+strings.Join(efforts, ", ")+") or both · enter switch · esc cancel", width)+plain)
	case it.decision != nil:
		foot = append(foot, dim+clip("x dismiss · "+moveHint(t.layout)+" · q quit", width)+plain)
	case it.grant != nil:
		foot = append(foot, dim+clip("x revoke · n grant · "+moveHint(t.layout)+" · q quit", width)+plain)
	case it.session != nil:
		keys := "enter attach, then ctrl+z to come back · "
		if it.session.Node == masterNode || it.session.Node == deskNode {
			keys += "m switch model · "
		}
		if t.paused {
			keys += "p unpause · "
		} else {
			keys += "p pause all · "
		}
		if t.away != nil {
			keys += "w come back · "
		} else {
			keys += "w away · "
		}
		keys += "n grant · "
		foot = append(foot, dim+clip(keys+moveHint(t.layout)+" · q quit", width)+plain)
	default:
		foot = append(foot, dim+"q quit"+plain)
	}
	if t.err != "" {
		foot = append(foot, yellow+clip(t.err, width)+plain)
	}

	// The body scrolls to keep the selected line in view.
	room := max(1, height-len(foot)-1)
	start := 0
	if selLine >= room {
		start = selLine - room + 1
	}
	end := min(len(lines), start+room)
	out := append([]string{}, lines[start:end]...)
	for len(out) < room {
		out = append(out, "")
	}
	return append(append(out, ""), foot...)
}

func clip(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width < 1 {
		return ""
	}
	return string(r[:width-1]) + "…"
}

// splitKeys cuts terminal input into keys: an escape sequence such as an arrow is one key, as is each rune.
func splitKeys(b []byte) []string {
	var keys []string
	s := string(b)
	for len(s) > 0 {
		if strings.HasPrefix(s, "\x1b[") && len(s) >= 3 {
			end := strings.IndexFunc(s[2:], func(r rune) bool { return r >= '@' && r <= '~' })
			if end >= 0 {
				keys = append(keys, s[:end+3])
				s = s[end+3:]
				continue
			}
		}
		r := []rune(s)[0]
		keys = append(keys, string(r))
		s = s[len(string(r)):]
	}
	return keys
}

// tuiMessage is what a goroutine hands the main loop: a line of the watch stream, the stream's end, a resize, or a request that failed.
type tuiMessage struct {
	resp    *Response
	gone    bool
	resized bool
	failed  string
}

// layoutFor picks the layout from the flag, else from BOSUN_LAYOUT.
func layoutFor(flagValue string) string {
	if flagValue == "" {
		flagValue = os.Getenv("BOSUN_LAYOUT")
	}
	if flagValue == layoutHallmack {
		return layoutHallmack
	}
	return "qwerty"
}

func runTUI(args []string) error {
	flags := flag.NewFlagSet("tui", flag.ExitOnError)
	layout := flags.String("layout", "", "keyboard layout, qwerty or hallmack (default $BOSUN_LAYOUT, else qwerty)")
	_ = flags.Parse(args)
	in := int(os.Stdin.Fd())
	old, err := makeRaw(in)
	if err != nil {
		return fmt.Errorf("bosun tui needs a terminal: %w", err)
	}
	enter := func() { fmt.Print("\x1b[?1049h\x1b[?25l") }
	leave := func() { fmt.Print("\x1b[?25h\x1b[?1049l") }
	enter()
	defer func() {
		leave()
		_ = setTermios(in, old)
	}()

	// Goroutines wake the main loop through the pipe, so that only the loop reads the terminal, which claude attach then has to itself.
	wakeR, wakeW, err := os.Pipe()
	if err != nil {
		return err
	}
	msgs := make(chan tuiMessage, 256)
	post := func(m tuiMessage) {
		msgs <- m
		_, _ = wakeW.Write([]byte{0})
	}
	go watchStream(post)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			post(tuiMessage{resized: true})
		}
	}()

	t := &tui{layout: layoutFor(*layout)}
	draw := func() {
		w, h := termSize(in)
		var b strings.Builder
		b.WriteString("\x1b[H")
		for i, line := range t.render(w, h) {
			if i > 0 {
				b.WriteString("\r\n")
			}
			b.WriteString(line + "\x1b[K")
		}
		b.WriteString("\x1b[J")
		fmt.Print(b.String())
	}
	draw()
	buf := make([]byte, 256)
	for {
		readable, err := waitReadable(in, int(wakeR.Fd()), t.armedKey() != "")
		if err != nil {
			return err
		}
		if readable[1] {
			_, _ = wakeR.Read(buf)
		drain:
			for {
				select {
				case m := <-msgs:
					switch {
					case m.resp != nil:
						t.apply(*m.resp)
					case m.gone:
						t.connected = false
					case m.failed != "":
						t.err = m.failed
					}
				default:
					break drain
				}
			}
		}
		if readable[0] {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				return err
			}
			for _, k := range splitKeys(buf[:n]) {
				a, ok := t.key(k)
				if !ok {
					continue
				}
				switch a.op {
				case "quit":
					return nil
				case "attach":
					leave()
					_ = setTermios(in, old)
					t.err = attach(a.id)
					_, _ = makeRaw(in)
					enter()
				case "grant":
					t.err = ""
					req, err := grantRequest(strings.Fields(a.text))
					if err == nil {
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						_, err = call(ctx, req)
						cancel()
					}
					if err != nil {
						t.err = err.Error()
					}
				case "pause", "unpause", "model", "away":
					// Each stops or resumes sessions, which takes a while, and the watch stream shows the progress. A switch of the desk waits for its turn to end.
					t.err = ""
					req := Request{Op: a.op, Name: a.name}
					if a.op == "model" {
						var err error
						if req, err = modelRequest(a.name, strings.Fields(a.text)); err != nil {
							t.err = err.Error()
							continue
						}
					}
					go func(req Request) {
						if _, err := call(context.Background(), req); err != nil {
							post(tuiMessage{failed: err.Error()})
						}
					}(req)
				default:
					t.err = ""
					req := Request{Op: a.op, ID: a.id, Text: a.text}
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					if _, err := call(ctx, req); err != nil {
						t.err = err.Error()
					}
					cancel()
				}
			}
		}
		draw()
	}
}

// attach hands the terminal to claude attach, and returns why it failed, if it did.
func attach(id string) string {
	if err := attachSession(id); err != nil {
		return err.Error()
	}
	return ""
}

// watchStream posts the daemon's snapshot and events, and connects again whenever the daemon goes away.
func watchStream(post func(tuiMessage)) {
	for {
		if conn, err := net.Dial("unix", socketPath()); err == nil {
			if json.NewEncoder(conn).Encode(Request{Op: "watch"}) == nil {
				sc := bufio.NewScanner(conn)
				sc.Buffer(make([]byte, 1<<20), 1<<24)
				for sc.Scan() {
					var r Response
					if json.Unmarshal(sc.Bytes(), &r) == nil {
						post(tuiMessage{resp: &r})
					}
				}
			}
			conn.Close()
			post(tuiMessage{gone: true})
		}
		time.Sleep(2 * time.Second)
	}
}

// waitReadable blocks until fd a or fd b has input, and reports which.
func waitReadable(a, b int, armed bool) ([2]bool, error) {
	for {
		// An armed key times out, so the hint goes without a key press.
		var timeout *syscall.Timeval
		if armed {
			tv := syscall.NsecToTimeval(int64(armTimeout))
			timeout = &tv
		}
		var set syscall.FdSet
		set.Bits[a/64] |= 1 << (uint(a) % 64)
		set.Bits[b/64] |= 1 << (uint(b) % 64)
		_, err := syscall.Select(max(a, b)+1, &set, nil, nil, timeout)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return [2]bool{}, err
		}
		return [2]bool{set.Bits[a/64]&(1<<(uint(a)%64)) != 0, set.Bits[b/64]&(1<<(uint(b)%64)) != 0}, nil
	}
}

func getTermios(fd int) (*syscall.Termios, error) {
	var t syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCGETS, uintptr(unsafe.Pointer(&t))); e != 0 {
		return nil, e
	}
	return &t, nil
}

func setTermios(fd int, t *syscall.Termios) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TCSETS, uintptr(unsafe.Pointer(t))); e != 0 {
		return e
	}
	return nil
}

// makeRaw puts the terminal in raw mode, as golang.org/x/term does, but keeps output processing, so a newline still returns the carriage, and returns the mode it replaced.
func makeRaw(fd int) (*syscall.Termios, error) {
	old, err := getTermios(fd)
	if err != nil {
		return nil, err
	}
	raw := *old
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	return old, setTermios(fd, &raw)
}

func termSize(fd int) (width, height int) {
	var ws struct{ Row, Col, X, Y uint16 }
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws))); e != 0 || ws.Col == 0 {
		return 80, 24
	}
	return int(ws.Col), int(ws.Row)
}
