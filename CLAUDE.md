# CLAUDE.md

bosun orchestrates Claude Code background sessions: one master and a tree of workers. [README.md](README.md) holds the scope. This file holds the design, of which the TUI, the panel, the systemd service and cleanup are not built yet. Its Claude Code behaviour and timings were measured against 2.1.310.

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
| `bosun status`, `approve`, `deny`, `reply`, `tell`, `spawn`, `kill`, `model`, `watch` | the command-line frontend |

The daemon holds each session's parent and the queue of decisions waiting on an answer. It reads which of its sessions run from `claude agents --json`, which also lists sessions bosun did not start.

## Sessions

**Spawning.** `spawn` runs `claude --bg -n <node> -w <node>` with `--settings` and `--mcp-config`. The settings carry the hooks and `worktree.baseRef: head`. Sessions started outside bosun load nothing from it.

**Resuming.** A stopped `--bg` session keeps its options, model included. `claude --bg --resume <session-id>` without flags continues it under the same id. A prompt after `--` starts its next turn. With any flag, the command starts a copy of the conversation under a new id.

**Models.** The daemon's `-master-model` and `-worker-model` pick the models, and `spawn` can pick one for a worker. `bosun model` switches the master by resuming it with `--model`, and the daemon follows the new id. Workers block on the daemon, not on the master, so they keep running through the switch.

**Identity.** `--bg` ignores `--session-id` and picks its own. bosun knows a session by its node name, set in the environment of its hook commands and its MCP server. Each hook event carries Claude Code's `session_id`, which `claude agents --json` reports as `sessionId`, beside a short `id`.

**Trust.** `claude --bg` refuses a directory the user has not trusted. A first-level worker starts only in a trusted repository, and the master's own directory needs trust too. Every directory inside a trusted repository, worktrees included, shares its trust. A repository nested inside it needs trust of its own.

**Sandbox.** Every session runs its Bash commands in Claude Code's sandbox, so a command writes only inside the session's directory, its repository's `.git`, its own temporary folder, `~/.cache/go-build` and `~/.cache/nix`. `sudo` fails inside it. The sandbox allows every unix socket, since nix needs its daemon's. bosun hides by path the sockets that reach outside the sandbox, its own included. A socket missing from that list stays open. Claude Code's system prompt sends scratch files to `$CLAUDE_JOB_DIR/tmp`, which is read-only inside the sandbox. bosun's prompt sends them to the session's temporary folder instead. The sandbox refuses an unlisted host. The model then retries with the host in the Bash call's `allowed_domains`, and auto mode decides that retry.

**The tree.** Every session has `spawn`, so a worker can add a level. The daemon caps depth and fan-out.

**Worktrees.** `spawn` runs a child from its parent's worktree, so the child's branch `worktree-<node>` begins at the parent's `HEAD`. Every worktree sits directly in the repository's `.claude/worktrees/`, whatever its depth, so a node name must be unique within its repository. The master works in no repository.

**Cleanup.** A parent merges a child's branch once the child is done. The daemon then removes the child with `claude rm`, since a session doing so would trip the reserved check on deleting a branch. `claude rm` refuses a worktree whose branch has unpushed commits, and a branch merged locally still has them, so the daemon passes `--discard-unpushed` with the `<commit>@<worktree-id>` that the refusing `claude rm` printed. `claude rm` also refuses the empty `.gitmodules` the daemon writes on `SessionStart`, so the daemon deletes it first. `claude stop` and `claude rm` take the short `id`, not the `-n` name.

## Decisions

| Kind | Example | Answered by |
|---|---|---|
| Permission | run `npm test`, edit a file | rules, then auto mode, then the master, then you |
| Question | "X or Y?", "tests fail, keep going?" | the master, then you |

**The permission hook is `PermissionRequest`.** It receives `session_id`, `tool_name` and `tool_input`. It answers with `hookSpecificOutput.decision.behavior` set to `allow` or `deny`, and a deny's `message` reaches the model verbatim. Auto mode approves a call without running the hook, unless an `ask` rule matches the call.

**Permissions reach the master once auto mode passes them up.** The daemon hands the permission to the master as an event. The master allows it, denies it, or escalates it to you. The master's own permissions go straight to you.

**Waiting on you.** A hook's `timeout` may exceed the 600-second default, and the hook decides the call whenever it answers within it. Six seconds after the hook starts, the session also shows its own prompt, and `claude agents --json` reports it as `"status":"waiting","state":"blocked"`. After the `timeout`, only that prompt settles the call. The daemon then moves the decision out of its queue and shows the session as blocked in its own prompt.

**An answer in the session wins.** Answering the session's own prompt through `claude attach` settles the call at once, and the hook keeps running. `PermissionRequest` carries no call id, so the daemon pairs it with the `PreToolUse` event before it, which carries `tool_use_id`. A `PostToolUse` or `PostToolUseFailure` for that id withdraws the decision. A denial in the session runs no tool, so the session's next `PreToolUse` or `Stop` withdraws it instead.

**Reserved actions skip the master.** Only you can approve one:

- any `git push`, force pushes included
- deleting a branch
- a write through a file tool outside the session's worktree, memory directory, temporary folder and `$CLAUDE_JOB_DIR/tmp`
- a Bash command outside the sandbox, through `dangerouslyDisableSandbox`

Bash writes are left to the sandbox. A `PreToolUse` hook catches a reserved action before Claude Code's allow rules and auto mode. Auto mode approves even a force push when its task asks for one. The check waits for your answer and returns `allow` or `deny`. A `PreToolUse` hook that times out or fails lets the call through to the normal flow. So the check denies shortly before its `timeout`, and on any error, an unreachable daemon included.

**Questions go to the master, whatever the depth.** Workers run without `AskUserQuestion`, which would wait on a person, and ask through `ask` instead.

| MCP tool | Held by | Does |
|---|---|---|
| `spawn` | every session | starts a child session |
| `ask` | workers | sends a question to the master and blocks until the answer |
| `next_event` | master | blocks until a worker asks, needs a permission, ends a turn or exits, or a frontend sends `tell` |
| `reply` | master | answers a question |
| `decide` | master | allows or denies a permission |
| `resume` | master | gives a worker that ended its turn a new prompt |
| `escalate` | master | queues a question or a permission for you |

**The master loops on `next_event`.** It calls `next_event`, handles the event, and calls it again. A `Stop` hook reports a turn's end, and `claude agents --json` reports an exit.

**Blocking tools send progress.** Claude Code aborts a stdio tool call that sends nothing for 30 seconds. While `next_event` or `ask` blocks, it sends `notifications/progress` on the call's `progressToken` every 10 seconds. Claude Code also moves a call still running after 120 seconds to the background, unless the settings' `env` sets `CLAUDE_CODE_MCP_AUTO_BACKGROUND_MS` to `0`.

**Typing to the master.** Text typed into the master's session waits until the current tool call returns. So `next_event` must return empty within five minutes. A frontend's `tell` reaches the master as an event straight away.

## Frontends

Every frontend is a thin client of one protocol on the daemon's socket. A frontend that sends `watch` gets JSON lines: a snapshot, then events. A frontend sends commands back: `approve`, `deny`, `reply`, `tell`, `spawn`, `kill`, `model`. Every rule lives in the daemon.

Each leads with the queue of decisions waiting on you, above the tree.

- **Quickshell panel.** It lives in the NixOS configuration's `modules/quickshell/`. Opening a session runs `claude attach <id>` in a new kitty window.
- **`bosun tui`.** Opening a session runs `claude attach` in the TUI's terminal. `Ctrl+Z` exits `claude attach` with status 0, leaving the session running.

## Open questions

- **Auto mode refusals.** It is untested whether an action the classifier refuses in a `--bg` session reaches `PermissionRequest`, or only `PermissionDenied` (#5).
