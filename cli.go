package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
)

func runCommand(cmd string, args []string) error {
	req := Request{Op: cmd}
	need := map[string]int{"approve": 1, "deny": 1, "reply": 2, "tell": 1, "spawn": 3, "kill": 1}[cmd]
	if len(args) < need {
		return errors.New(usage)
	}
	switch cmd {
	case "approve":
		req.ID = args[0]
	case "deny", "reply":
		req.ID, req.Text = args[0], strings.Join(args[1:], " ")
	case "tell":
		req.Text = strings.Join(args, " ")
	case "spawn":
		req.Name, req.Repo, req.Text = args[0], args[1], strings.Join(args[2:], " ")
	case "kill":
		req.Name = args[0]
	}
	resp, err := call(context.Background(), req)
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
		children[n.Parent] = append(children[n.Parent], n)
	}
	fmt.Fprintln(w, "Sessions:")
	var walk func(parent string, depth int)
	walk = func(parent string, depth int) {
		for _, n := range children[parent] {
			state := n.Status
			if n.State != "" {
				state += "/" + n.State
			}
			fmt.Fprintf(w, "  %s%-*s %-8s %s\n", strings.Repeat("  ", depth), 14-2*depth, n.Node, n.ID, state)
			walk(n.Node, depth+1)
		}
	}
	walk("", 0)
}

func describe(dec Decision) string {
	if dec.Kind == "question" {
		return "asks: " + dec.Question
	}
	var in map[string]any
	_ = json.Unmarshal(dec.Input, &in)
	detail := ""
	for _, key := range []string{"command", "file_path", "notebook_path", "url", "pattern"} {
		if v, ok := in[key].(string); ok {
			detail = v
			break
		}
	}
	if detail == "" {
		detail = compact(dec.Input)
	}
	if len(detail) > 120 {
		detail = detail[:117] + "..."
	}
	s := fmt.Sprintf("%s: %s", dec.Tool, detail)
	if dec.Reserved != "" {
		s += fmt.Sprintf("  [%s]", dec.Reserved)
	}
	return s
}
