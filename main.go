// Command bosun runs a master Claude Code session over a tree of worker sessions.
package main

import (
	"fmt"
	"os"
)

const usage = `usage:
  bosun daemon [-master-dir DIR] [-master-model M] [-worker-model M]
               [-max-depth N] [-max-children N]
                                   run the daemon, which starts the master
  bosun mcp                        the MCP server each session starts
  bosun hook                       every hook each session runs

  bosun tui                        the decisions waiting and the tree, in the terminal
  bosun status [--json]            the decisions waiting, and the tree
  bosun watch                      stream the daemon's snapshot and events
  bosun approve ID                 allow a permission
  bosun deny ID [MESSAGE]          deny a permission
  bosun reply ID ANSWER            answer a question
  bosun dismiss ID                 clear a notice from the master
  bosun tell TEXT                  send the master a message
  bosun spawn [-model M] NAME REPO PROMPT
                                   start a worker under the master
  bosun kill NAME                  stop a session
  bosun pause                      stop every session, and resume none until unpause
  bosun unpause                    resume the turns that pause stopped
  bosun attach ID                  claude attach to a session, which the daemon resumes if it stopped
  bosun model M                    switch the master to model M, keeping its conversation

A model is an alias such as fable, opus, sonnet or haiku, or a full model name.`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "daemon":
		err = runDaemon(args)
	case "mcp":
		err = runMCP()
	case "hook":
		runHook()
	case "status":
		err = runStatus(args)
	case "watch":
		err = runWatch()
	case "tui":
		err = runTUI()
	case "attach":
		err = runAttach(args)
	case "approve", "deny", "reply", "dismiss", "tell", "spawn", "kill", "model", "pause", "unpause":
		err = runCommand(cmd, args)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bosun:", err)
		os.Exit(1)
	}
}
