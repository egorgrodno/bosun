# CLAUDE.md

bosun orchestrates Claude Code background sessions: one master, a front desk and a tree of workers. [README.md](README.md) holds the scope. This file holds the design. Its Claude Code behaviour and timings were measured against 2.1.310.

## Shape

```text
Quickshell panel ─┐                              ┌─ claude --bg          master
                  ├─ unix socket ─ bosun daemon ─┼─ claude --bg          desk
bosun tui ────────┘                              ├─ claude --bg -w a     worker
                                                 │    └─ claude --bg -w b
                                                 └─ claude --bg -w c
              hook and MCP calls from every session ──▶ bosun daemon
```

One binary carries every part:

| Command | Runs as |
|---|---|
| `bosun daemon` | a systemd user service in the NixOS configuration's `modules/claude/`, which starts the master and the desk |
| `bosun mcp` | the stdio MCP server each session starts, relaying to the daemon |
| `bosun hook` | every hook each session runs |
| `bosun tui` | the terminal frontend |
| `bosun status`, `approve`, `deny`, `reply`, `dismiss`, `tell`, `spawn`, `kill`, `pause`, `unpause`, `model`, `attach`, `watch` | the command-line frontend |

The daemon holds each session's parent and the queue of decisions waiting on an answer. It reads each session's state from `claude agents --json --all`, which also lists sessions bosun did not start.

## Sessions

**Spawning.** `spawn` runs `claude --bg -n <node> -w <node>` with `--settings` and `--mcp-config`. The settings carry the hooks and `worktree.baseRef: head`. Sessions started outside bosun load nothing from it.

**Resuming.** A stopped `--bg` session keeps its options, model included. `claude --bg --resume <session-id>` without flags continues it under the same id. A prompt after `--` starts its next turn. With any flag, the command starts a copy of the conversation under a new id. So does a resume of a session still running. A session still runs for a moment after `claude stop` returns, until its process exits, though `claude agents` already lists it as stopped.

**Clearing.** A resume whose prompt after `--` is `/clear` clears a stopped session in place and runs no turn. The session keeps its short `id` and saved options, and its conversation starts over under a new session id, which the `SessionStart` with `source` `clear` carries. The clear writes the new conversation's transcript at once. While the session runs, the listing's `sessionId` is the new id. Once it stops, the listing shows the original id. Only the original session id resumes the session: a resume of the new id without flags starts a separate session that lacks the saved options. A copy, which a resume with flags makes, gets the conversation of the id it names, so a copy of the conversation since the clear must name the new id.

**Retiring.** Claude Code's supervisor stops a background session that has sat idle for an hour. `claude agents --json --all` keeps listing it without a `pid` or `status` until `claude rm`, the same as a session that `claude stop` stopped. Either resumes like an idle session, so stopped does not mean done. The listing cannot tell a kill from a retirement, so the daemon marks each worker that `bosun kill` stops, and `resume` refuses it.

**Usage limits.** A turn that ends on an API error runs the `StopFailure` hook instead of `Stop`. Its `error` names the kind, such as `rate_limit`, `overloaded` or `server_error`, and its `last_assistant_message` holds Claude Code's message. For a usage limit that message says when the limit resets, as `resets 3pm (Europe/Berlin)`, or past a day `resets Oct 27, 3pm (Europe/Berlin)`. The daemon resumes each session a limit stopped at that time, the master first. Every session shares one account, so a limit usually stops them all, though a limit on one model stops only its sessions. A worker's other errors reach its parent as its `turn_end`.

**Pausing.** `bosun pause` stops every session and marks the ones in their turns. While paused, the daemon resumes no session on its own, the usage-limit resume included, and refuses `spawn`, `resume`, `merge` and `model`. A session you resume yourself through `bosun attach` runs, and its messages resume their recipients. Events still queue for the master. `bosun unpause` resumes each marked session with a prompt that says why, and wakes the master if an event waits for it. The pause outlasts a restart of the daemon.

**Upgrades.** A session reads its settings and MCP config only when its process starts. Both run bosun through `bosun` in the state directory, a link that each daemon start points at its own binary, and each start rewrites every session's settings. The system prompt is another matter. Claude Code snapshots it into the transcript, and every resume reuses the snapshot, even a resume with new flags. Tool descriptions come from the MCP server, which each process start runs from the current build. So the master's system prompt holds only what never changes, and its tools' descriptions hold its rules.

**Supervisor.** A `claude --bg` that finds no supervisor starts Claude Code's supervisor in its own cgroup. The supervisor runs every background session and exits after the last one. Each session takes the supervisor's cgroup and environment, not those of its own `claude --bg`. So when the daemon starts the supervisor, every session runs in the daemon's cgroup. When a terminal started it, every session takes the terminal's environment, and the daemon queues a notice for you. `claude attach` resumes a stopped session itself, so a frontend sends `attach` first, which resumes the session idle, and runs `claude attach` only once that succeeds.

**Models.** The daemon's `-master-model`, `-desk-model` and `-worker-model` pick the models, and `-worker-effort` a worker's effort. `spawn` can pick a model and an effort for a worker, which `start` passes as `--model` and `--effort`. `bosun model` switches the master by resuming it with `--model`, and the daemon follows the new id. Workers block on the daemon, not on the master, so they keep running through the switch.

**Switching a worker.** `resume` with a model or effort that differs from the worker's moves it to a copy of its conversation under a new id, through `start`, since a resume with a flag copies the session. The daemon stops the old session first, and follows the new id like `switchModel`: the worker's `id` changes, its `sessionId` is read from the listing again, and the old id goes to `retired`. A failed start resumes the old session idle under its own id. The old id stays listed, stopped, since it began the worktree and `claude rm` of it may delete the worktree. `merge` removes the retired ids first, then the current one.

`claude agents --json` reports neither a session's model nor its effort, so the daemon keeps both in `state.json`. A start with `--effort` records it in the session's `respawnFlags` in `~/.claude/jobs/<id>/state.json`, which a flagless resume replays.

**The master runs lean.** Each request carries the system prompt, every tool's description and the listings of skills and MCP servers. The desk talks to you, so the master needs little of that. Its settings turn off the claude.ai connectors and each of the user's plugins. They also deny the built-in tools in `masterDenied`: `Skill`, which carries the skill listing, `Artifact`, `Workflow`, `SendFeedback`, `ScheduleWakeup`, `ReportFindings`, `EnterWorktree` and `AskUserQuestion`. Its start adds `--strict-mcp-config`, which keeps out every MCP server but bosun's. The master keeps Bash, the file tools, `Agent` with its built-in agent types, `ToolSearch`, `ListAgents`, `SendMessage`, `TaskStop`, `Monitor`, `WebFetch` and `WebSearch`. The desk keeps everything. A probe measured a fresh session at 62.7k tokens with all of it and 17.7k without. `--disable-slash-commands` would also drop the skill listing, but it makes `/clear` an unknown command.

**Workers run lean too.** A worker's settings and start follow the master's, with `leanDenied`, the master's list less `Skill`, since a worker's prompt sends it to the user's skills. A worker keeps the plugins whose name ends in `-lsp` or whose install folder holds a `.lsp.json`, the code-intelligence ones. It loses every other plugin, the built-in ones the user's settings enable, such as `cc-plugin-you-should-know@builtin`, included: its notes are for a person watching, and nobody watches a worker. The desk keeps that plugin, since the user reads its notes there.

`spawn` takes `plugins`, names of the user's plugins, and `connectors`, which turns the claude.ai connectors on, for a task that needs them. A worker's settings then leave those plugins on, and its start omits `--strict-mcp-config`, which also keeps out the servers of an enabled plugin. A child inherits nothing: its parent names what it needs, and a resume keeps the worker's set. A probe measured a fresh haiku worker's second request, after its MCP servers connected, at 33.8k tokens against 69.4k for a default session. It found the craft plugin's servers absent under `--strict-mcp-config`, with the plugin enabled.

**The lean master takes a new conversation.** A resume continues the old conversation. Its transcript keeps the listings and the snapshot the conversation began with. A clear starts a new conversation from the settings the daemon rewrote, so an existing master turns lean at its next clear. A clear keeps the flags the master started with, so only a new master, or the copy that `bosun model` makes, gets `--strict-mcp-config`. That copy keeps the old conversation. You get a lean master sooner by stopping the daemon while the master sleeps and no worker runs, running `claude rm` on the master's session, and starting the daemon, which starts a new master.

**Identity.** `--bg` ignores `--session-id` and picks its own. bosun knows a session by its node name, set in the environment of its hook commands and its MCP server. Each hook event carries Claude Code's `session_id`, which `claude agents --json` reports as `sessionId`, beside a short `id`.

**Trust.** `claude --bg` refuses a directory the user has not trusted. A first-level worker starts only in a trusted repository, and the master's own directory needs trust too. Every directory inside a trusted repository, worktrees included, shares its trust. A repository nested inside it needs trust of its own. Outside a repository, trust in a directory covers each directory inside it, so the master's trust covers the desk's.

**Sandbox.** Every session runs its Bash commands in Claude Code's sandbox, so a command writes only inside the session's directory, its repository's `.git`, its own temporary folder, `~/.cache/go-build`, `~/.cache/nix` and `~/.cache/treefmt`. `sudo` fails inside it. The sandbox allows every unix socket, since nix needs its daemon's. bosun hides by path the sockets that reach outside the sandbox, its own included. A socket missing from that list stays open. Claude Code's system prompt sends scratch files to `$CLAUDE_JOB_DIR/tmp`, which is read-only inside the sandbox. bosun's prompt sends them to the session's temporary folder instead. The sandbox refuses an unlisted host. The model then retries with the host in the Bash call's `allowed_domains`, and auto mode decides that retry.

**The tree.** Every session has `spawn`, so a worker can add a level. The daemon's `-max-depth` and `-max-children` cap depth and fan-out.

**Worktrees.** `spawn` runs a child from its parent's worktree, so the child's branch `worktree-<node>` begins at the parent's `HEAD`. Every worktree sits directly in the repository's `.claude/worktrees/`, whatever its depth, so a node name must be unique within its repository. The master and the desk work in no repository.

**Cleanup.** A child's parent, or the master, lands the child with `merge`. The daemon fast-forwards the branch the child began from, and then removes the child, its worktree and its branch with `claude rm`. For the master's children that branch is checked out in a repository the master's sandbox cannot write. A session deleting a branch would also trip the reserved check. `claude rm` refuses a worktree whose branch has unpushed commits, and a branch merged locally still has them, so the daemon passes `--discard-unpushed` with the `<commit>@<worktree-id>` that the refusing `claude rm` printed. That flag also discards uncommitted changes, so `merge` refuses a child whose worktree has any. `claude rm` also refuses the empty `.gitmodules` the daemon writes on `SessionStart`, so the daemon deletes it first. `claude stop` and `claude rm` take the short `id`, not the `-n` name.

## Decisions

| Kind | Example | Answered by |
|---|---|---|
| Permission | run `npm test`, edit a file | rules, then auto mode, then the master, then you |
| Reserved action | write `/tmp/x`, a command outside the sandbox | the master, then you, or you alone for the user's list |
| Question | "X or Y?", "tests fail, keep going?" | the master, then you |
| Notice | "spawn failed: the repository is not trusted" | you, by dismissing it |

**The permission hook is `PermissionRequest`.** It receives `session_id`, `tool_name` and `tool_input`. It answers with `hookSpecificOutput.decision.behavior` set to `allow` or `deny`, and a deny's `message` reaches the model verbatim. Auto mode approves a call without running the hook, unless an `ask` rule matches the call.

**Permissions reach the master once auto mode passes them up.** The daemon hands the permission to the master as an event. The master allows it, denies it, or escalates it to you. The master's and the desk's own permissions go straight to you.

**Auto mode refuses some calls and asks about others.** A call that auto mode's classifier refuses runs `PermissionDenied`, not `PermissionRequest`, and the session gets the classifier's reason. After a run of refusals, auto mode asks about a call instead of refusing it, so `PermissionRequest` runs and `PermissionDenied` does not. In the probe, that run was a hook's denial followed by two refusals. Such a request carries nothing of the refusals, so the daemon keeps the reason of each session's last refusal until one of its calls runs, and sends it with the permission as `refused`.

**Waiting on you.** A hook's `timeout` may exceed the 600-second default, and the hook decides the call whenever it answers within it. Six seconds after the hook starts, the session also shows its own prompt, and `claude agents --json` reports it as `"status":"waiting","state":"blocked"`. A minute before the `timeout`, the daemon stops holding the call. From then on only the session's own prompt settles it, and a notice for you replaces the decision. The notice goes once the session moves past the call.

**An answer in the session wins.** Answering the session's own prompt through `claude attach` settles the call at once, and the hook keeps running. `PermissionRequest` carries no call id, so the daemon pairs it with the `PreToolUse` event before it, which carries `tool_use_id`. A `PostToolUse` or `PostToolUseFailure` for that id withdraws the decision. A denial in the session runs no tool, so the session's next `PreToolUse` or `Stop` withdraws it instead.

**A worker's reserved action reaches the master first, unless it is on the user's list.** A `PreToolUse` hook holds a reserved action before Claude Code's allow rules and auto mode see it. Auto mode would approve even a force push that its task asks for. The master gets the action as a permission whose `reserved` says why, with `refused` as for any permission. It allows the action, denies it, or escalates it to you. The master's and the desk's own go straight to you. The reserved actions:

- a push that forces, deletes a ref or pushes every branch
- a push to a URL, or to a name that `git remote` does not list in the repository the command runs in
- a push with a git option other than `-C`, or with a push option the check does not know
- deleting a branch
- a write through a file tool outside the session's worktree, memory directory, temporary folder and `$CLAUDE_JOB_DIR/tmp`, or by the master into the desk's directory
- a Bash command outside the sandbox, through `dangerouslyDisableSandbox`, unless it is on the allowlist

The check holds the call until the answer and returns `allow` or `deny`. A `PreToolUse` hook that times out or fails lets the call through to the normal flow. So the check denies shortly before its `timeout`, and on any error, an unreachable daemon included. Bash writes inside the sandbox are left to the sandbox.

**The user's list stays with you alone.** It holds two kinds of action: those that change code that runs later where no bosun session sees it, and those that reach credentials for other machines. An action on it never reaches the master:

- the pushes above, and a push outside the sandbox from a repository whose git setup does not match its record, or has none
- a write through a file tool to a credential store, a shell startup file, Claude Code's configuration, git hooks or config, a systemd unit or autostart entry, or bosun's state directory
- a Bash command outside the sandbox that names such a path or runs in a folder of the list
- a Bash command outside the sandbox that runs as root, as through `sudo`, or activates a NixOS configuration other than through the allowlist

`userList` in `reserved.go` holds the exact paths. In a worker's own worktree, only what its next start runs outside the sandbox counts, such as `.claude/settings.json`. The merge's review sees the rest.

**The list cannot bind a command outside the sandbox.** Such a command can write anywhere. The check reads two things: the paths a command's words name, and whether a word runs as root. A path built at run time, as by a script or an unknown variable, escapes it. So the `decide` description tells the master to read the command whole, and to escalate anything it cannot follow or that touches the list.

**The allowlist lets a few commands outside the sandbox take the normal flow.** Each must be the whole command: plain words, with nothing chained, piped, substituted, redirected or assigned before it.

- `nixos-rebuild test` or `build` with `--flake` on `/etc/nixos` or a worktree in `/etc/nixos/.claude/worktrees/`, optionally with a `#host` attribute and `--sudo`
- `nxt` and `nxc`
- a push that no reserved rule catches, from a repository whose git setup matches its record

In the normal flow, auto mode decides a call outside the sandbox alone. The settings' `ask` rules send every `nixos-rebuild`, `nxt` and `nxc` to the master instead, inside the sandbox too. A push has no such rule, so auto mode decides it, and it reaches the master only when auto mode asks.

**A push must run outside the sandbox.** Inside it, ssh refuses `~/.ssh/config`. That file is a home-manager link into the nix store, and the sandbox shows the store owned by nobody. Outside it, a push runs the repository's hooks and any command its config names, such as `core.sshCommand`. Its config also decides where the push goes. A worker's sandbox can write all of that in `.git`.

**The daemon records each repository's git setup.** When a spawn finds no worker in a repository and no record of it, the daemon records the repository's own config, its hooks and its remotes' push URLs in `repos/` of the state directory. A push outside the sandbox passes only while the setup matches the record. A record stays until you delete it, and the next spawn into the repository without workers records it anew.

**Questions go to the master, whatever the depth.** Workers run without `AskUserQuestion`, which would wait on a person, and ask through `ask` instead.

| MCP tool | Held by | Does |
|---|---|---|
| `spawn` | the master and workers | starts a child session |
| `merge` | the master and workers | lands a finished worker's branch and removes the worker |
| `ask` | workers | sends a question to the master and blocks until the answer |
| `tell_master` | desk | queues the user's instruction for the master |
| `status` | desk | shows what `bosun status` shows, with each decision and message in full |
| `transcript` | desk | shows a session's last turns: each prompt, its text and a line per tool call |
| `answer` | desk | gives your answer to a decision that waits on you, once you have said yes to it |
| `tell_desk` | master | answers the desk, or the user when there is no desk |
| `next_event` | master | blocks until a worker asks or needs a permission, a child of the master ends a turn or exits, or a `tell` comes from the desk, or from a frontend when there is no desk |
| `reply` | master | answers a question |
| `decide` | master | allows or denies a permission |
| `resume` | master | gives a worker that ended its turn, or stopped in it, a new prompt, and optionally a new model or effort |
| `escalate` | master | queues a question or a permission for you |
| `notify` | master | queues a notice for you, about a problem that is not a decision |

**The master loops on `next_event`.** It calls `next_event`, handles the event, and calls it again. A `Stop` hook reports a worker's turn end, and the session listing reports a worker that stops in its turn.

**A parent hears of its children.** A worker's `turn_end` and `exit` go to its parent. The master gets them only for its own children, and for the children of a worker that `bosun kill` stopped or that has exited. A worker runs no `next_event`, so the daemon queues the reports. A parent in its turn gets them at its `Stop`, which the daemon blocks with the reports as its reason. The daemon resumes a parent between turns with the reports as its prompt. `SendMessage`'s `notify_when_idle` cannot replace this queue: the child's process holds the subscription, so a stop or resume of the child voids it, and it fires only once.

**An idle master sleeps.** Once `next_event` returns empty, the master ends its turn, and the next event resumes it. So an idle master spends no turns. The daemon blocks a `Stop` that comes while events wait, which sends the master back to `next_event`. If the master stops again before calling `next_event`, the daemon lets it, queues a notice for you, and the next event resumes it. On start, the daemon adopts an idle or stopped master as asleep, unless an event waits for it. A wake stops and resumes the master, which ends a subagent or background command still running. So the master hands long work, such as a review before a merge, to a subagent in the background, and keeps calling `next_event` while it runs.

**A stale master wakes cleared.** A wake past the prompt cache's hour re-reads the master's whole transcript uncached. So once the tree holds no worker and the master's transcript has gone an hour unwritten, the daemon resumes the master with `/clear` before it wakes it. What the master must keep across a clear lives in its memory directory.

**Blocking tools send progress.** Claude Code aborts a stdio tool call that sends nothing for 30 seconds. While `next_event` or `ask` blocks, it sends `notifications/progress` on the call's `progressToken` every 10 seconds. Claude Code also moves a call still running after 120 seconds to the background, unless the settings' `env` sets `CLAUDE_CODE_MCP_AUTO_BACKGROUND_MS` to `0`.

**Typing to the master.** Text typed into a sleeping master starts its turn. Text typed while a tool call runs waits until the call returns, so `next_event` must return empty within five minutes. While `bosun attach` or `bosun tui` runs `claude attach` on the master, it holds an `attached` request on the daemon, and `next_event` then returns within seconds. A bare `claude attach` holds nothing, so text typed there waits until `next_event` returns empty.

## Messages

**Workers talk through Claude Code.** `SendMessage` reaches a running session by its `-n` name, so workers address each other by node name. Auto mode runs it without a permission request, so the master never decides on a message. A message arrives between tool calls, so one to a worker blocked in `ask` waits until the call returns. A message to an idle session starts a turn, and that turn's `Stop` reaches its parent as a `turn_end`.

**Messages stay in the tree.** The user's other sessions act on a peer's message in auto mode, without bosun's reserved check. So every session's `PreToolUse` checks the `to` of a `SendMessage`:

| `to` | `SendMessage` reaches | The hook |
|---|---|---|
| a worker's name | the worker; while it has no inbox, a session whose name starts with it | resumes a stopped worker idle and allows it; denies a killed, exited or starting one |
| a name and `[ref]` | one of several sessions of that name | denies it, since bosun cannot tell which |
| `uds:` and a path, or a path ending in `.sock` | the session listening there | allows a worker's socket while its process runs |
| `main`, a subagent's id | the sender's own session | allows it |
| any other name, a prefix of a name, `bridge:`, `did:`, a Claude Desktop session id | a session bosun cannot match to a worker | denies it |

The check covers only what a session sends: a message from outside bosun reaches a worker like typed text.

**The daemon reads every message.** Each delivered message runs `UserPromptSubmit`, with the prompt in a `<cross-session-message>` tag that names the sender. The daemon shows it to the frontends. Unless the master sent it, the daemon also queues it as a `message` event. That event wakes no master and blocks no `Stop`, and reaches the master along with the next event the master must act on.

**A stopped worker resumes for a message.** A stopped session has no inbox, so a `SendMessage` to it reports `No agent named '<name>' is reachable`. The sender's `PreToolUse` therefore resumes a stopped worker idle first, unless `bosun kill` stopped it. That is in time, since `SendMessage` looks up its recipient only after `PreToolUse` returns, and `claude --bg --resume` returns once the inbox exists. Only a node name resumes a stopped worker: the `uds:` address a message came from names the old process.

**The master and the desk take no messages.** Their settings set `crossSessionInbound` to `refuse`. The master acts on what the desk relays as the user's word, so a session that could message the desk could pose as the user. Every session's `PreToolUse` denies a `SendMessage` to either. A worker's denial points it to `ask`, and the master's, for the desk, to `tell_desk`. The desk's own `PreToolUse` denies its `SendMessage` to any bosun session, since a message starts a turn.

## The front desk

**The desk is the session the user talks to.** The daemon starts it beside the master as node `desk`. It reads the fleet, files issues and reads repositories. It passes the user's instructions to the master with `tell_master`, and the master answers with `tell_desk`. Only the master changes the fleet. The desk also gives your answer to a decision that waits on you, once you say yes.

**It shares the master's memory, not its transcripts.** It runs in `desk` inside the master's directory, so Claude Code files its transcripts apart from the master's. Its settings point `autoMemoryDirectory` at the master's memory directory.

**The master may not write the desk's directory.** The desk would run hooks written in its `.claude` outside any sandbox. Claude Code's sandbox guards the `.claude` and `.mcp.json` of a session's own directory alone, so the master's settings add the desk's directory to `sandbox.filesystem.denyWrite`, and the reserved check holds a file tool's write there for you. Claude Code's file tools count a write into any `.claude` folder as sensitive, which covers the desk's own.

**The daemon carries tells both ways.** A frontend's `tell` and the master's `tell_desk` queue text for the desk. The desk gets its queue the way a worker parent gets its children's reports. In a turn, the daemon blocks its `Stop` with the queue as the reason. Between turns, the daemon resumes it with the queue as the prompt, and a stopped desk resumes like an idle one. A turn cut short gives back the tells it took, marked `again`. Tells cut short twice go on to the master, so a desk whose every turn fails cannot loop on them. The desk's `tell_master` reaches the master as a `tell` event with `from` set to `desk`.

**Without a desk, tells go to the master.** When the desk could not start, its session is gone or a usage limit stopped it, a `tell` goes to the master and a `tell_desk` reaches you as a notice. The master may run on a model the limit spares. A desk that the daemon fails to resume, or that a limit stops, hands on its queue the same way.

**The desk answers a decision only after your yes to it.** Its `answer` takes two calls. The first proposes an answer to one decision. The daemon holds it and returns the decision in full, for the desk to show you. The second gives the answer, under three conditions:

- it repeats the proposed answer
- you have sent the desk exactly one message since the proposal
- its `words` appear in that message

So a decision that opened after your message can never be answered by it. A message is a prompt you type into the desk, read from its `UserPromptSubmit`, or a `tell` of yours, which counts from when you sent it. The master's answers, a tell handed over again after a cut turn, and bosun's own prompts do not count. Every prompt bosun sends the desk must begin with `bosun:` and a space, or be listed in `deskPromptFromBosun`. Any other prompt counts as your message. A clear or a restart drops what the desk proposed.

**The daemon checks order and the quote, not the yes.** Whether your words say yes to that answer, and whether the desk showed you the decision, rest on the desk. The `answered` record shows you what it gave.

**Some decisions the desk may not answer.** `answer` refuses a notice, a decision the master holds and a decision of the desk's own. It also refuses a reserved action on your list, marked `userOnly`, which only your own answer in a frontend settles. It may answer the master's own permissions, which wait on you.

**A stale desk wakes cleared.** The daemon clears the desk as it clears the master: before it resumes the desk with its queue, and before a frontend's `attach` readies it. The desk is stale once its transcript has gone an hour unwritten, whatever the workers do. A clear would hide a reply you have not read, so a desk handed a tell since you last opened it through `attach` stays uncleared. A `claude attach` that skips `bosun attach` finds it uncleared.

## Restarts

A worker's parent must hear of every turn of it that ends or stops, unless a pause stopped it.

**The daemon's state survives a power loss.** `state.json` holds the sessions, the master's queue and the open decisions, and each save syncs it to disk.

**The desk outlasts a restart.** On start the daemon adopts the desk while Claude Code lists it, and otherwise starts a new one that gets the old one's queue. A desk that cannot start leaves a notice.

**Blocked calls outlast a restart.** While the daemon is away, a hook, `ask`, `next_event` and `attached` send their request again under the same `key`. A hook does so within its own timeout, and the others for 30 seconds after they last reached the daemon. The restarted daemon hands each the decision or batch it waited on, and an escalated decision stays with you. A decision whose request has not come back 30 seconds after the start is settled. If its session still waits in its own prompt, a permission then becomes a notice, as it does a minute before the `timeout`. A call that gives up falls back: a `PreToolUse` denies its call, a `PermissionRequest` leaves it to the session's own prompt, and `ask` and `next_event` fail.

**Missed turn ends.** Each hook of a session's turn marks it in a turn, and the daemon saves the mark. `Stop` and `StopFailure` clear it. A marked worker still listed between turns ten seconds after its last hook ended its turn unheard. Either the daemon was away, or the user interrupted the turn. An idle one reaches its parent as a `turn_end` with the last message of its transcript. A stopped one reaches it as an `exit`, or as a `turn_end` if its transcript shows the turn finished. On start, a worker counts as marked when its transcript holds a message newer than the last turn end its parent heard. That covers a turn that ran while no daemon did.

**The master's batch.** The events `next_event` returns stay with the daemon until the master's next `next_event`, or a `Stop` that ends its turn. When its turn ends any other way, such as by a crash, a kill, an API error, a pause or a restart, the daemon puts the batch back in front of the queue with `again` set. A worker that stops in its turn likewise gets back the reports of its children that the turn took.

## Frontends

Every frontend is a thin client of one protocol on the daemon's socket. A frontend that sends `watch` gets JSON lines: a snapshot, then events. A frontend sends commands back: `approve`, `deny`, `reply`, `dismiss`, `tell`, `spawn`, `kill`, `pause`, `unpause`, `model`, `attach`, `attached`, `transcript`. Every rule lives in the daemon.

A decision's `reserved` holds the reason it is a reserved action, such as `a force push`. Its `userOnly` marks an action on your list.

A frontend should show the `answered` event and the snapshot's `answered`: each decision the desk answered on your word, with the answer given and the words the desk quoted.

The snapshot's `paused`, and the `paused` and `unpaused` events, show a pause. A session's `paused` marks a turn that `unpause` resumes.

In the snapshot and the `sessions` event, the desk is the session whose `node` is `desk`. Like the master, it has no `parent`.

Each session carries `model` and `effort`, both omitted where the session runs at the user's default. `effort` is one of `low`, `medium`, `high`, `xhigh` and `max`, and only workers have one. A session's `retired` lists the ids it had before a switch, and a frontend ignores it. `spawn` takes `model` and `effort`, and a frontend shows each as `model effort`.

- `tell` goes to the desk, or to the master when there is no desk, and its result does not say which. During a pause it waits for `unpause`.
- `transcript` takes a node `name` and an optional `turns`, 3 by default, and returns that session's last turns in `text`.
- `attach` clears a stale desk first, which can take most of a minute. A frontend must wait that long for the answer before it runs `claude attach`.
- `attached` takes the `id` that `claude attach` runs on. A frontend sends it on a connection of its own and closes that connection once `claude attach` exits. The master's id holds without an answer until then, and any other gets an answer at once.

Each leads with the queue of decisions waiting on you, above the tree. The desk is its own row above the master's tree.

- **Quickshell panel.** It lives in the NixOS configuration's `modules/quickshell/`, as the `Agents` bar module and `BosunPanel`, and reads the protocol through `bosun watch`. Opening a session runs `bosun attach <id>` in a new kitty window. `quickshell ipc call bosun toggle` opens it.
- **`bosun tui`.** Opening a session runs `claude attach` in the TUI's terminal. `Ctrl+Z` exits `claude attach` with status 0, leaving the session running.

## Open questions

- **Effort.** That `--effort` lands in `respawnFlags` and survives a flagless resume is inferred from `--model` doing so, and unmeasured. So is whether `claude rm` of a switched worker's first id removes the worktree its copy runs in, and whether `claude rm` of the copy alone leaves it.
- **Usage limits.** The `StopFailure` input and the reset's format come from the 2.1.310 binary. No real usage limit has been measured.
- **Restarts and pauses.** Only a fake `claude` has run the restart handling and the pause. No real reboot, crash or pause has been measured. The daemon reads transcripts in the 2.1.310 format. Whether `claude stop` runs the `Stop` hook is unmeasured, so the daemon ignores a `Stop` from a session that a pause stopped.
- **The desk.** No real desk has run. A probe session inside the master's directory measured the desk's trust, `autoMemoryDirectory` from `--settings` and the clear's transcript, and nothing more. Handing an idle desk its queue stops and resumes it, as waking the master does. Whether a `claude attach` open on it survives that is unmeasured. That a file tool's sensitive write asks even in auto mode comes from the 2.1.310 binary. The desk's answers assume that `UserPromptSubmit` carries each prompt you type, verbatim. No real desk has shown that. When it runs for a prompt typed while the desk is in its turn is unmeasured, and the daemon times the prompt by it.
- **The open master and its subagents.** No real `claude attach` has run under the `attached` hold. While you have the master open, it sleeps ten seconds after its last event, so a wake stops and resumes it under your `claude attach`. Whether the attach survives that is unmeasured. That a stop ends a background subagent, and that its result waits for the running tool call to return, are assumed, not measured.
- **Reserved actions at the master.** Only the tests' fake hooks have sent one. Whether a file tool writes through a symlink, as the check assumes, is unmeasured.
- **The allowlist.** No real session has run it. Two things are unmeasured: whether an `ask` rule sends a call outside the sandbox to `PermissionRequest` in auto mode, and whether `Bash(nixos-rebuild:*)` matches a flake with a `#host` attribute. The gate finds a push's repository from the hook's `cwd`. In the 2.1.310 binary that is the session's working directory, which a `cd` in an earlier Bash call moves, but no real session has shown it.
- **The lean worker.** The probe ran without bosun's MCP server, in a scratch directory, and its Bash call stopped at a permission prompt after its second request. Whether a worker given a plugin or connectors gets the servers once `--strict-mcp-config` is off, and whether the user's other MCP servers then join it, is unmeasured. So is the effect of a `.lsp.json` or `-lsp` plugin on a worker's size.
- **The lean master.** Only haiku probe sessions measured it, in a scratch directory with the same flags and settings. No real master has run lean. The snapshot in a probe's transcript records the built-in tools' descriptions too, so a resume without a clear may send the old ones. That is unmeasured, as is whether `--strict-mcp-config` outlasts a resume. No plugin that claude.ai syncs has been seen, so whether `userPlugins` finds one is unknown.
