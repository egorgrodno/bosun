# bosun

A master Claude Code session that runs a tree of worker sessions. It answers their routine questions and brings you the rest. A Quickshell panel drives it on the NixOS desktops, and a TUI drives it everywhere else.

**Nothing is built yet.** [CLAUDE.md](CLAUDE.md) holds the design.

## Scope

- **Claude Code only.** Every session is a `claude --bg` background session.
- **One machine, one fleet.** The daemon listens on a unix socket and never on the network.
- **No process ownership.** Claude Code starts, attaches, stops and lists sessions. bosun never holds a PTY.

## Development

```sh
direnv allow     # or: nix develop
nix fmt
nix flake check
```
