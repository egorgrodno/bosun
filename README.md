# bosun

A master Claude Code session that runs a tree of worker sessions. It answers their routine questions and brings you the rest. A Quickshell panel drives it on the NixOS desktops, and a TUI drives it everywhere else.

[CLAUDE.md](CLAUDE.md) holds the design. The daemon and its frontends work: the command line, `bosun tui` and the Quickshell panel.

## Scope

- **Claude Code only.** Every session is a `claude --bg` background session.
- **One machine, one fleet.** The daemon listens on a unix socket and never on the network.
- **No process ownership.** Claude Code starts, attaches, stops and lists sessions. bosun never holds a PTY.

## Usage

By default the daemon runs the master in `~/.local/state/bosun/master`. Claude Code must trust that directory, and every repository a worker starts in. Run `claude` in each once and accept the prompt. Sessions run their commands in Claude Code's sandbox, which on Linux needs `bwrap` and `socat` on the `PATH`.

```sh
bosun daemon &
bosun tell 'Fix the flaky test in ~/Projects/app'
bosun status      # the decisions waiting, then the tree
bosun tui         # the same, live, to answer them and attach to a session
bosun approve 3
claude attach <id>
```

`bosun` alone lists every command.

The flake's default package builds `bosun`, so `nix run github:egorgrodno/bosun -- daemon` runs the daemon. As a systemd user service, the daemon needs `KillMode=process`, so that stopping the daemon leaves the sessions running.

## Development

```sh
direnv allow     # or: nix develop
git config core.hooksPath .githooks
nix fmt
nix flake check
go test ./...
```
