# bosun

A master Claude Code session that runs a tree of worker sessions. It answers their routine questions and brings you the rest. A front desk session talks to you: it answers questions about the fleet at once, passes your instructions to the master, and settles a decision for you once you say yes. A Quickshell panel drives bosun on the NixOS desktops, and a TUI drives it everywhere else.

[CLAUDE.md](CLAUDE.md) holds the design. The daemon and its frontends work: the command line, `bosun tui` and the Quickshell panel.

## Scope

- **Claude Code only.** Every session is a `claude --bg` background session.
- **One machine, one fleet.** The daemon listens on a unix socket and never on the network.
- **No process ownership.** Claude Code starts, attaches, stops and lists sessions. bosun never holds a PTY.

## Usage

By default the daemon runs the master in `~/.local/state/bosun/master`, and the front desk in its `desk` folder. Claude Code must trust the master's directory, which covers the desk's, and every repository a worker starts in. Run `claude` in each once and accept the prompt. Sessions run their commands in Claude Code's sandbox, which on Linux needs `bwrap` and `socat` on the `PATH`.

```sh
bosun daemon &
bosun tell 'Fix the flaky test in ~/Projects/app'
bosun status      # the decisions waiting, the tree, then the log
bosun tui         # the same, live, to answer them and attach to a session
bosun approve 3
bosun attach <id>
```

`bosun` alone lists every command. `bosun pause` stops every session, such as before a reboot, and `bosun unpause` resumes the turns it cut. `bosun model -effort low desk sonnet` moves the desk to another model and effort, keeping its conversation, and `m` in `bosun tui` does the same for the master or the desk.

`bosun follow <node>` streams a session's conversation as JSON lines for a frontend, and `bosun conversation -before <id> <node>` pages back through it.

Talk to the desk: type into it through `bosun attach`, or send it a line with `bosun tell`. The master and the desk refuse messages from your other Claude Code sessions, though the sender's `SendMessage` reports success.

The flake's default package builds `bosun`, so `nix run github:egorgrodno/bosun -- daemon` runs the daemon. As a systemd user service, the daemon needs `KillMode=process`, so that stopping the daemon leaves the sessions running.

## Development

```sh
direnv allow     # or: nix develop
git config core.hooksPath .githooks
nix fmt
nix flake check
go test ./...
```
