# CLAUDE.md

bosun orchestrates Claude Code background sessions: one master and a tree of workers. [README.md](README.md) holds the scope. Nothing is built yet, so this file is the design.

## Shape

```text
Quickshell panel ─┐                              ┌─ claude --bg          master
                  ├─ unix socket ─ bosun daemon ─┼─ claude --bg -w a     worker
bosun tui ────────┘                              │    └─ claude --bg -w b
                                                 └─ claude --bg -w c
              hook and MCP calls from every session ──▶ bosun daemon
```

One binary carries every part:

| Command | Runs as |
|---|---|
| `bosun daemon` | a systemd user service, which starts the master |
| `bosun mcp` | the stdio MCP server each session starts, relaying to the daemon |
| `bosun hook` | every hook each session runs |
| `bosun tui` | the terminal frontend |

The daemon holds each session's parent and the queue of decisions waiting on an answer. It reads which of its sessions run from `claude agents --json`, which also lists sessions bosun did not start.

## Sessions

**Spawning.** `spawn` passes `--session-id`, `--mcp-config` and `--settings` to `claude --bg`. The settings carry the hooks. The MCP config gives `bosun mcp` the session id in its environment. Sessions started outside bosun load nothing from it.

**The tree.** Every session has `spawn`, so a worker can add a level. The daemon caps depth and fan-out.

**Worktrees.** Each worker works in its own worktree. Its branch starts at its parent's `HEAD`, and the parent merges it once the child is done. The master works in no repository.

## Decisions

| Kind | Example | Answered by |
|---|---|---|
| Permission | run `npm test`, edit a file | rules, then auto mode, then the master, then you |
| Question | "X or Y?", "tests fail, keep going?" | the master, then you |

**Permissions reach the master once auto mode passes them up.** A worker's permission prompt goes to the daemon, which hands it to the master as an event. The master allows it, denies it, or escalates it to you.

**Reserved actions skip the master.** Only you can approve a reserved action, such as `git push` or a write outside the session's worktree. A `PreToolUse` hook catches one before Claude Code's allow rules or auto mode can approve it.

**Questions go to the master, whatever the depth.** Workers run without `AskUserQuestion`, which would wait on a person, and ask through `ask` instead.

| MCP tool | Held by | Does |
|---|---|---|
| `spawn` | every session | starts a child session |
| `ask` | workers | sends a question to the master and blocks until the answer |
| `next_event` | master | blocks until a worker asks, needs a permission, ends a turn or exits |
| `reply` | master | answers a question |
| `decide` | master | allows or denies a permission |
| `escalate` | master | queues a question or a permission for you |

**The master loops on `next_event`.** It calls `next_event`, handles the event, and calls it again. A `Stop` hook reports a turn's end, and `claude agents --json` reports an exit.

## Frontends

Both frontends are thin clients of one protocol on the daemon's socket. The daemon sends JSON lines: a snapshot, then events. A frontend sends commands back: `approve`, `deny`, `reply`, `spawn`, `kill`. Every rule lives in the daemon.

Both lead with the queue of decisions waiting on you, above the tree.

- **Quickshell panel.** It lives in the NixOS configuration's `modules/quickshell/`. Opening a session runs `claude attach <id>` in a new kitty window.
- **`bosun tui`.** Opening a session hands the terminal to `claude attach`.

## Open questions

- **Branching from `HEAD`.** `claude -w` branches from `origin/<default-branch>`. A setting, reportedly `worktree.baseRef: head`, branches from the current `HEAD` instead.
- **The permission hook.** It is not yet settled which hook event fires on a permission prompt, or what a session does once the hook's `timeout` passes while the decision waits on you.
- **Typing to the master.** The master spends its life inside one turn, so a message typed after `claude attach` waits until `next_event` returns.
- **Leaving `claude attach`.** It has no plain detach: `←` opens the agent view and `Ctrl+Z` suspends it. `bosun tui` needs a way back.
