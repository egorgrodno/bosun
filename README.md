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
bosun say worker-a 'also run the linter'
bosun interrupt worker-a
bosun attach <id>
```

`bosun` alone lists every command. `bosun pause` stops every session, such as before a reboot, and `bosun unpause` resumes the turns it cut. `bosun away on 'off to bed'` keeps the fleet working while you are gone: bosun parks what would wait on you, and `bosun away off` hands you the list through the desk. Telling the desk you are leaving, or back, does the same. `bosun grant push 09:00` lets bosun allow the master's pushes while you are away, until 09:00, and `bosun revoke g1` takes it back. `bosun model -effort low desk sonnet` moves the desk to another model and effort, keeping its conversation, and `m` in `bosun tui` does the same for the master or the desk.

While a session other than the desk works, the daemon holds a logind sleep lock, so the machine does not sleep mid-run. It lets go after 10 quiet minutes, and your own idle step suspends the machine then. `-sleep-lock off|battery` and `-sleep-lock-quiet` tune it.

`bosun tui` takes its movement keys from the keyboard layout: `j`/`k` on qwerty, the default, and `a`/`e` on hallmack. Arrows work on both, and every other key is the same on both. Pick the layout with `bosun tui -layout hallmack` or `BOSUN_LAYOUT=hallmack`. Any other value means qwerty.

`bosun follow <node>` streams a session's conversation and its turn in progress as JSON lines for a frontend, and `bosun conversation -before <id> <node>` pages back through it.

`bosun say` types into one of bosun's sessions as you, a slash command such as `/compact` included, and `bosun interrupt` ends its turn. The daemon does both through Claude Code's supervisor with its control key, which nothing else in bosun reads. When the supervisor's protocol has changed, it stops the session and resumes it instead.

`bosun compact master` runs `/compact` on the master, and `bosun clear desk` starts the desk in a new conversation. Each takes `master` or `desk`. The master clears only once no worker is left, and both keep what they must remember in their memory directory.

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
