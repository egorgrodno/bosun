package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// runAttach runs claude attach on a session, and exits non-zero when either the daemon or claude attach fails, so that a terminal opened for it can stay open on the error.
func runAttach(args []string) error {
	if len(args) != 1 {
		return errors.New(usage)
	}
	err := attachSession(args[0])
	if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
		// claude attach printed why.
		os.Exit(max(exit.ExitCode(), 1))
	}
	return err
}

// attachSession hands the terminal to claude attach on session id until it exits, as Ctrl+Z makes it do, once the daemon has readied the session.
// It stops at a daemon error: claude attach would resume a stopped session itself, starting Claude Code's supervisor in this terminal.
func attachSession(id string) error {
	// Readying a stale desk clears it, which can take as long as the stop, the resume and its new conversation's SessionStart together.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := call(ctx, Request{Op: "attach", ID: id}); err != nil {
		if op := (*net.OpError)(nil); errors.As(err, &op) {
			return fmt.Errorf("the daemon is not running: %w", err)
		}
		return err
	}
	// The terminal's signals are claude's while it runs: caught here, they reset to the default in the child.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTSTP, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	// The daemon counts the session open while this request's connection lasts, which closes with the process however it ends.
	held, release := context.WithCancel(context.Background())
	released := make(chan struct{})
	go func() {
		defer close(released)
		_, _ = callAgain(held, Request{Op: "attached", ID: id})
	}()
	defer func() {
		release()
		<-released
	}()
	cmd := exec.Command("claude", "attach", id)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude attach %s: %w", id, err)
	}
	return nil
}

func runCommand(cmd string, args []string) error {
	req := Request{Op: cmd}
	if cmd == "spawn" {
		flags := flag.NewFlagSet("spawn", flag.ExitOnError)
		model := flags.String("model", "", "the worker's model, in place of the daemon's -worker-model")
		_ = flags.Parse(args)
		req.Model, args = *model, flags.Args()
	}
	need := map[string]int{"approve": 1, "dismiss": 1, "deny": 1, "reply": 2, "tell": 1, "spawn": 3, "kill": 1, "model": 1}[cmd]
	if len(args) < need {
		return errors.New(usage)
	}
	switch cmd {
	case "approve", "dismiss":
		req.ID = args[0]
	case "deny", "reply":
		req.ID, req.Text = args[0], strings.Join(args[1:], " ")
	case "tell":
		req.Text = strings.Join(args, " ")
	case "spawn":
		req.Name, req.Repo, req.Text = args[0], args[1], strings.Join(args[2:], " ")
	case "kill":
		req.Name = args[0]
	case "model":
		req.Model = args[0]
	}
	resp, err := call(context.Background(), req)
	if resp.Text != "" {
		fmt.Println(resp.Text)
	}
	if err != nil {
		return err
	}
	if resp.Session != nil {
		fmt.Printf("%s %s %s\n", resp.Session.Node, resp.Session.ID, resp.Session.Dir)
	}
	return nil
}

func runStatus(args []string) error {
	resp, err := call(context.Background(), Request{Op: "status"})
	if err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "--json" {
		return json.NewEncoder(os.Stdout).Encode(resp)
	}
	printStatus(os.Stdout, resp)
	return nil
}

// runWatch prints the daemon's snapshot and events as JSON lines until the daemon goes away.
func runWatch() error {
	conn, err := net.Dial("unix", socketPath())
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(Request{Op: "watch"}); err != nil {
		return err
	}
	_, err = io.Copy(os.Stdout, bufio.NewReader(conn))
	return err
}

func printStatus(w io.Writer, s Response) {
	if s.Paused {
		fmt.Fprintln(w, "Paused: bosun unpause resumes the turns it stopped.")
	}
	for _, holder := range []string{"user", "master"} {
		first := true
		for _, dec := range s.Decisions {
			if dec.Holder != holder {
				continue
			}
			if first {
				fmt.Fprintf(w, "Waiting on the %s:\n", holder)
				first = false
			}
			fmt.Fprintf(w, "  %-4s %-12s %s\n", dec.ID, dec.Node, describe(dec))
			if dec.Note != "" {
				fmt.Fprintf(w, "       master: %s\n", dec.Note)
			}
		}
	}
	children := map[string][]Session{}
	for _, n := range s.Sessions {
		if n.Node == deskNode {
			children[""] = append([]Session{n}, children[""]...)
			continue
		}
		children[n.Parent] = append(children[n.Parent], n)
	}
	fmt.Fprintln(w, "Sessions:")
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		for _, n := range children[parent] {
			fmt.Fprintf(w, "  %s%-*s %-8s %-15s %s\n", strings.Repeat("  ", depth), 14-2*depth, n.Node, n.ID, sessionState(n), n.Model)
			walk(n.Node, depth+1)
		}
	}
	walk("", 0)
	if len(s.Messages) > 0 {
		fmt.Fprintln(w, "Messages:")
	}
	for _, m := range s.Messages {
		fmt.Fprintf(w, "  %s\n", clip(messageLine(m), 120))
	}
}

// sessionState is how a frontend shows the state of session n.
func sessionState(n Session) string {
	switch {
	case n.Killed:
		return "killed"
	case n.Paused:
		return "paused"
	case n.LimitedUntil != nil:
		return "limited until " + n.LimitedUntil.Local().Format("15:04")
	case n.State != "":
		return n.Status + "/" + n.State
	}
	return n.Status
}

// messageLine shows the first line of a message between sessions.
func messageLine(m Event) string {
	text, _, _ := strings.Cut(m.Text, "\n")
	return fmt.Sprintf("%s → %s: %s", m.From, m.Node, text)
}

func describe(dec Decision) string {
	switch dec.Kind {
	case "question":
		return "asks: " + dec.Question
	case "notice":
		return "notice"
	}
	s := fmt.Sprintf("%s: %s", dec.Tool, inputDetail(dec.Input))
	if dec.Reserved != "" {
		s += fmt.Sprintf("  [%s]", dec.Reserved)
	}
	if dec.Refused != "" {
		s += "  [auto mode refused the last call: " + dec.Refused + "]"
	}
	return s
}

// inputDetail is the part of a tool call's input that says what it does, such as a command or a file, cut to 120 bytes.
func inputDetail(input json.RawMessage) string {
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	detail := ""
	for _, key := range []string{"command", "file_path", "notebook_path", "url", "pattern"} {
		if v, ok := in[key].(string); ok {
			detail = v
			break
		}
	}
	if detail == "" {
		detail = compact(input)
	}
	if len(detail) > 120 {
		detail = detail[:117] + "..."
	}
	return detail
}
