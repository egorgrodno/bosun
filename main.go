// Command bosun runs a master Claude Code session over a tree of worker sessions.
package main

import (
	"fmt"
	"os"
)

const usage = `usage:
  bosun daemon [-master-dir DIR]   run the daemon, which starts the master
  bosun mcp                        the MCP server each session starts
  bosun hook                       every hook each session runs

  bosun status [--json]            the decisions waiting, and the tree
  bosun watch                      stream the daemon's snapshot and events
  bosun approve ID                 allow a permission
  bosun deny ID [MESSAGE]          deny a permission
  bosun reply ID ANSWER            answer a question
  bosun tell TEXT                  send the master a message
  bosun spawn NAME REPO PROMPT     start a worker under the master
  bosun kill NAME                  stop a session`

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
	case "approve", "deny", "reply", "tell", "spawn", "kill":
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
