// Command bosun runs a master Claude Code session over a tree of worker sessions.
package main

import (
	"fmt"
	"os"
)

const usage = `usage:
  bosun daemon [-master-dir DIR] [-master-model M] [-master-effort E]
               [-desk-model M] [-desk-effort E] [-worker-model M] [-worker-effort E]
               [-max-depth N] [-max-children N]
                                   run the daemon, which starts the master and the front desk
  bosun mcp                        the MCP server each session starts
  bosun hook                       every hook each session runs

  bosun tui [-layout qwerty|hallmack]
                                   the decisions waiting and the tree, in the terminal
  bosun status [--json]            the decisions waiting, and the tree
  bosun watch                      stream the daemon's snapshot and events
  bosun conversation [-before ID] [-limit N] NAME
                                   print a session's latest conversation entries, or those before entry ID, as JSON
  bosun follow [-limit N] NAME     stream a session's conversation as JSON lines: its latest entries, each new one, and its turn in progress
  bosun approve ID                 allow a permission, in the session's own prompt for a notice that waits there
  bosun deny ID [MESSAGE]          deny a permission, the same way
  bosun reply ID ANSWER            answer a question
  bosun dismiss ID                 clear a notice from the master
  bosun tell TEXT                  send the front desk a message, or the master when there is no desk
  bosun say NODE TEXT              type TEXT into a session as yours, a slash command too
  bosun interrupt NODE             end a session's turn, as Esc in its terminal does
  bosun compact master|desk        compact the master's or the desk's conversation, as /compact does
  bosun clear master|desk          start the master or the desk in a new conversation, as /clear does
  bosun spawn [-model M] [-effort E] NAME REPO PROMPT
                                   start a worker under the master
  bosun kill NAME                  stop a session
  bosun pause                      stop every session, and resume none until unpause
  bosun unpause                    resume the turns that pause stopped
  bosun away on [WORDS]            park what would wait on you, so the fleet works on without you
  bosun away off                   hand you what was parked, as one list through the desk
  bosun attach ID                  claude attach to a session, which the daemon resumes if it stopped
  bosun model [-effort E] [master|desk] [M]
                                   switch the master, or the desk, to model M or effort E, keeping its conversation

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
	case "conversation", "follow":
		err = runConversation(cmd, args)
	case "tui":
		err = runTUI(args)
	case "attach":
		err = runAttach(args)
	case "model":
		err = runModel(args)
	case "approve", "deny", "reply", "dismiss", "tell", "say", "interrupt", "compact", "clear", "spawn", "kill", "pause", "unpause", "away":
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
