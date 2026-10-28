package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// progressEvery keeps a blocking call alive: Claude Code aborts a stdio tool call that sends nothing for 30 seconds.
var progressEvery = 10 * time.Second

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func object(required []string, props map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required}
}

var instructionProp = map[string]any{"type": "integer", "description": "The instruction number of the tell you act on, as the tell event gave it. Left out for work no tell asked for."}

func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

const (
	nameHelp   = "Node name: lowercase letters, digits and hyphens, unique in the repository. It names the session, its worktree and its branch worktree-<name>."
	peersHelp  = " Workers message each other by node name. When several share one feature, name in each worker's prompt the other workers' nodes and what each one builds, and start them all before any needs another."
	modelHelp  = "The session's model, picked from the task: haiku for mechanical edits, version bumps and docs; sonnet for ordinary features and fixes; opus or fable, the strongest, for design, concurrency or security-sensitive work. Left out, it is the daemon's default."
	effortHelp = "The session's effort, picked from the task: low for mechanical edits, version bumps and docs; medium for ordinary features; high for design, concurrency or security-sensitive work; xhigh or max only where high failed. Left out, it is the daemon's default."

	pluginsHelp    = "Plugins to start the worker with, by name, such as craft for a task that designs, edits media or documents, or drives a Craft app. A worker starts without the user's plugins except the code-intelligence ones, since each plugin's tools and instructions ride on every request it makes. Add one only when the task needs it, and none for a task that writes code. A child inherits nothing from its parent."
	connectorsHelp = "Whether to start the worker with the user's claude.ai connectors, such as Claude Docs, Todoist and Google Drive. It starts without them, since their tools ride on every request it makes. Turn them on only for a task that needs one, such as writing a Claude Docs document."

	statusHelp = `Show the fleet as bosun status shows it: the decisions waiting on the user and on the master, the tree of sessions with their ids, states and models, and the latest messages between sessions. Each decision and each message then follows in full.

Read it before you answer a question about the run. desk is you, and master is the session that runs the workers.

When the user asks you to approve, deny or answer a decision that waits on them, give the answer with answer. A decision waiting on the master is the master's to settle: tell the master what the user wants done with it, with tell_master. Only the user answers a decision of your own, or one with userOnly set, which marks a reserved action on the user's list. Show such a decision in full, with its id and the whole tool call, and point the user at the bosun panel, bosun tui, or bosun approve ID, bosun deny ID MESSAGE and bosun reply ID ANSWER.`

	answerHelp = `Give the user's answer to a decision that waits on them: allow or deny a permission, or answer a question. Give only the answer the user asks for, never one of your own judgement. It takes two calls, with the user's yes between them.

First call answer with the decision's id and the answer, and leave out words. For a permission, pass allow, and a message when you deny. For a question, pass answer. bosun gives nothing yet: it holds the answer and returns the decision in full. Show the user all of it, with its id, its session and the whole tool call or question, and the answer you will give. Ask whether to give it, and end your turn. When what the user asked could fit more than one decision, show each from status, ask which they mean, and propose only that one.

Only the user's next message, typed to you or sent through bosun tell, can say yes. When it does, call answer again with the same id, with allow, message or answer unchanged, and with words that quote all of that message exactly. bosun gives the answer only then, and bosun tui and bosun status show the user what you gave and on which words. When the user replies with anything but a plain yes to that answer, such as a question or a change, there is no yes: deal with the reply, then call answer without words again before you ask anew.

bosun refuses a notice, a decision that waits on the master, a decision of your own, and one with userOnly set. Pass on what the user wants done with one that waits on the master with tell_master. Show the user any other in full, and point them at the bosun panel or bosun tui, as status says.`

	awayHelp = `Turn the away mode on when the user says they are leaving the fleet to work without them, such as "I'm logging off" or "going to bed", and off when they say they are back. Act on their word alone, never of your own judgement, such as because they have gone quiet.

Turning it on takes two calls, with the user's yes between them, as answer does. First call away with on true, and leave out words. bosun turns nothing on yet: it returns what turning it on would do, the decisions it would park and the standing grants it would set acting. Show the user all of it, ask whether to turn it on, and end your turn. Do this at once, since the user is leaving: tell them it goes on only once they say yes. Only the user's next message can say yes. When it does, call away again with on true and words that quote all of that message exactly. A reply other than a plain yes, such as a question or "not yet", is no yes: deal with it, then call away without words again before you ask anew.

Turning it off takes one call, with on false and the user's words that say they are back, quoted exactly from their latest message.

While it is on, bosun parks what would wait on the user, such as the master's push, a reserved action on the user's list, a decision the master escalates or a notice. It denies each call and answers each question with a note that it waits for the user's return, so the fleet works on without them. bosun status shows it on and lists what it parked.

Turning it off returns the list of what was parked, the one whose approval unblocks the most first. Show it to the user, and pass on what they want done with each item to the master with tell_master. A parked call was denied, so the master runs it again, or resumes the worker that made it. When the user turns it off themselves, as with bosun away off, bosun hands you the same list as a prompt.`

	grantHelp = `Give a standing grant when the user asks ahead of time that bosun settle a class of the master's calls without them, such as "push whatever you merge tonight". Act on their word alone. A grant acts only while away mode is on, and ends at until or when revoked. bosun status lists the grants.

It takes two calls, with the user's yes between them, as answer does. First call grant with the class and until, and leave out words. bosun gives nothing yet: it holds the grant and returns it in full, with its class, when it ends and what it would allow. Show the user all of it, ask whether to give it, and end your turn. Only the user's next message, typed to you or sent through bosun tell, can say yes. When it does, call grant again with the same class and until, and with words that quote all of that message exactly. bosun gives the grant only then, and bosun status, bosun tui and the log show it with those words. When the user replies with anything but a plain yes to that grant, such as a question, a refusal or another end, there is no yes: deal with the reply, then call grant without words again before you ask anew.

No grant covers anything but its class, such as a worker's call, a reserved action on the user's list or a question. Tell the user so when they ask for more. The classes:`

	revokeHelp = "End a standing grant when the user asks, quoting their words exactly from their latest message."

	transcriptHelp = `Show the last turns of a session's conversation: the prompt that began each turn, the session's text, and a line for each tool call, oldest first. Use it to answer what a session is doing or did, after status names the sessions. A turn's tool results do not show.`

	// The read tools ride on every request of every session, so their descriptions stay short. status holds the scope and the reason once.
	readStatusHelp   = "Show the fleet: decisions waiting, the tree of sessions with state, model and activity, and the latest log entries. Read-only, and it works inside the sandbox, so use it, never bosun status. A worker reads itself and its descendants through this and the other read tools; the master and the desk read everything."
	conversationHelp = "Show a session's conversation as JSON entries, oldest first: prompts, text, tool calls with results. Use it, never bosun conversation."
	logHelp          = "Read the event log: decisions and how they were settled, tells, messages, spawns, merges, turn ends. Filter by since, node, kind, who acted (for settled, who settled it) or decisions that reached the user. Use it, never bosun status --json, and for any question about what happened in the run."
	activityHelp     = "Show what a session is doing now: its latest tool call and the calls still open. Without a name, every session you can read."
	deskLogHelp      = " For a question about what happened in the run, such as which decisions reached the user, read log."

	tellMasterHelp = `Pass an instruction of the user's on to the master, the session that runs the fleet: it spawns, resumes, merges and stops workers, and settles their decisions. Call it for everything the user asks that would change the fleet or a repository, and never do any of that yourself, by SendMessage to a session or otherwise. The one thing you do yourself is give the user's answer to a decision that waits on them, with answer.

Write it as a spec the master can act on without asking back: what to do, in which repository, what done looks like, and what the user said that settles open choices. The master takes it as the user's own instruction, so pass on only what the user asked for, and ask the user first when you are unsure they want it done.

The master answers once it has acted on the instruction, such as by spawning a worker for it, and again once it knows how it ended, which may take a while. bosun hands you the answer at the end of your turn, or in a new turn when you have ended yours, as a prompt that begins "bosun: the master answers:". Pass it on to the user. When the user asks for news of an instruction before then, read status and the transcripts of the sessions it concerns.

bosun also hands you what the user sends through bosun tell, such as from the bosun panel, as a prompt that begins "bosun: the user sent this through bosun tell". Treat it as the user's own word, and remember that they may not be watching this session, so your answer waits here for them.

Once you have sat an hour idle, bosun clears your conversation before your next turn. Whenever you learn something that must outlast a clear, such as how the user wants things done, write it to your memory directory then.`

	nextEventHelp = `Wait for events from the workers and the user: up to four minutes, or ten seconds while the user has your session open. Returns them as JSON, the user's tells first, or says there were none.

Work in a loop: call next_event, handle every event it returns, and call it again. When it returns no events, end your turn, and the next event starts a new one. The exception is a subagent or background command of yours that still runs: call next_event again instead, since bosun wakes you by stopping and resuming your session, which ends it.

The user waits on you. A tell waits until your next call to next_event, and what the user types into your session waits until your current tool call returns. So act on a batch's tells first, and get back to next_event quickly. Hand long work, such as reviewing a large branch before a merge, to a subagent in the background, and keep calling next_event while it runs. Claude Code hands you its result once your current next_event call returns. Answer what the user types into your session in the session itself, not with tell_desk.

Once you have merged every worker and an hour has passed since your last turn, bosun clears your conversation before the next event wakes you, so you wake with no earlier conversation. Whenever you learn something that must outlast a clear, such as how the user wants decisions handled, write it to your memory directory then.

When the user leaves the fleet to work without them, as overnight, they turn on the away mode, and each batch next_event returns begins with a line that says so. Until a batch comes without that line:
- escalate and notify reach nobody. bosun parks what they get on a list for the user's return, and denies a parked permission or answers a parked question with a note that it waits for the user. Escalate what needs the user as you would, then move on: the user gets the list once they are back.
- Your own permissions and the reserved actions on the user's list park the same way, such as a push of yours. Do not try them again, nor another way to the same end.
- Carry on with everything else. Decide and reply as you would. Merge on into the local checkout rather than wait on a push, and start the next piece of work on what merged. A worker whose call was parked carries on with the rest of its task, and its turn_end says what waits.
- Answer tells with tell_desk as you would: the desk keeps your answers for the user, and without a desk they park.

The events:
- tell: an instruction from the user. Act on it, usually by starting workers with spawn. With from set to desk, the front desk relays it: the desk is the session the user talks to, which reads the fleet but changes nothing in it, so it passes the user's instructions to you. Act on such a tell as on the user's own. Answer every tell with tell_desk, whatever its from: that is how the user hears of it. Answer once you have acted on it, such as by spawning a worker for it, and again once you know how it ended. When acting on it is all it needs, one answer covers both. A tell from the desk carries instruction, a number: pass it as instruction to the tell_desk, spawn, resume and merge that act on it, so the user can follow the instruction to its outcome.
- decision of kind question: a worker asks something. Settle it with reply or escalate.
- decision of kind permission: a worker wants to run a tool, or bosun holds a reserved action of the worker's. Settle it with decide or escalate.
- turn_end: a worker finished a turn, and its last message says what it did, or that the turn failed on an API error. To give it more to do, such as a fix after review or another try, call resume. Once its work is right, call merge.
- exit: a worker's session stopped. Either it stopped in the middle of a turn, so no turn_end follows, and resume carries it on, or the user stopped it, as the event's text says, and resume refuses it.
- stalled: a worker is in its turn and no hook of it has run for ten minutes by default (the daemon's -stall-after), or a decision has waited on the user that long. It is a report: a long build or a script that polls is normal, so never stop the worker for it. The text says what the worker last did and its open calls. Look with activity or conversation, message the worker with SendMessage if it should change course, or tell the user with notify. With id set, it is the decision of that id, which only the user can settle, such as one of the user's list or one you escalated: tell the user with notify, and do nothing else. It comes once for each stall and decision.
- message: what a session told a worker through Claude Code's SendMessage, such as an interface two workers agreed on or the order their branches merge in. The worker gets it without you: a message to a worker between turns starts a turn, so never resume a worker to pass one on. A message needs nothing from you, so it comes along with the next event that does.

A worker's own children report their turn_end and exit to it, not to you, and it merges them. One reaches you only when the user stopped its parent or the parent is gone, and is then yours to merge.

An event with again set reached you before, in a turn of yours that was cut short, so you may have acted on it already. Act on it as on any other: a tool refuses what is already done, such as a merge of a worker that is gone or an answer to a settled decision, and such a refusal needs nothing more.

Workers message each other, so a turn_end does not always mean done. A worker may end its turn to wait for another worker's message, and a turn that a message started also ends in a turn_end. The turn_end's text says which. Merge workers in the order they agreed on. When workers wait only on each other, none starts again: resume one of them and tell it to send its part first.

A usage limit stops a worker without a turn_end, and bosun resumes it once the limit resets.

These go straight to the user, never to you, and no answer of yours allows one:
- a push that forces, deletes a ref, pushes every branch, carries a git option other than -C, or goes to a URL or to a name that is not a remote of its repository, and a push outside the sandbox from a repository whose git config, hooks or remotes changed since bosun recorded them, or that bosun has no record of
- a write through a file tool to a credential store, a shell startup file, Claude Code's configuration, git hooks or config, a systemd unit, autostart entry or cron table, or bosun's state other than the sessions' temporary folders. In a worker's own worktree, only its .claude settings and hooks, its .mcp.json and its .git count.
- a command outside the sandbox that may write one of those paths, reads a credential, runs in one of those folders other than to read, runs sudo, doas, pkexec, su or run0, may write a path bosun cannot resolve, schedules a job or changes a unit with crontab, at, systemd-run or systemctl enable and the like, writes git config with --global or runs git through a -c alias, runs a bosun subcommand other than status, conversation, log, follow, watch and help or a program a variable names, or activates a NixOS configuration other than as below. bosun trusts that a command only reads a path when it understands the whole command, and otherwise counts every path it names
- every permission of your own or of the desk's

Every other reserved action reaches you as a permission with reserved set: a write outside a worker's own folders, deleting a branch, and any other command outside the sandbox. Two kinds of command outside the sandbox skip that and take the normal flow, each only as a whole command, with nothing chained, piped or redirected:
- nixos-rebuild test or build with --flake on /etc/nixos or a worktree of it, and nxt and nxc, which test and build /etc/nixos. Every nixos-rebuild, nxt and nxc reaches you, inside the sandbox too.
- any other push, from a repository whose git config, hooks and remotes are as bosun recorded them while no worker was in it. Auto mode decides such a push, so it reaches you only when auto mode asks.`
)

// readTools are the read-only tools every session has, served by the daemon for the session's node: a worker's reads reach its subtree alone. The desk has its own status.
func readTools(node string) []tool {
	nameArg := str("The session's node name, as status shows it.")
	t := []tool{
		{"conversation", conversationHelp, object([]string{"name"}, map[string]any{
			"name":   nameArg,
			"limit":  map[string]any{"type": "integer", "description": "How many entries, 20 when left out."},
			"before": str("An entry's id: show the entries before it."),
		})},
		{"log", logHelp, object([]string{}, map[string]any{
			"since":        str("RFC 3339 time, or a duration back from now such as 2h."),
			"node":         str("Only entries about or by this session."),
			"kind":         str("Only this kind, such as settled, escalated, decision, say, tell, message, spawned, merged, turn_end."),
			"by":           str("Only this actor: user, master, desk, worker, session or bosun."),
			"reached_user": map[string]any{"type": "boolean", "description": "Only entries of decisions that reached the user."},
			"limit":        map[string]any{"type": "integer", "description": "The latest this many, 50 when left out."},
			"full":         map[string]any{"type": "boolean", "description": "Show each entry as JSON, with its decision and routing."},
		})},
		{"activity", activityHelp, object([]string{}, map[string]any{"name": nameArg})},
	}
	status := readStatusHelp
	if node == deskNode {
		status = statusHelp + deskLogHelp
	}
	return append([]tool{{"status", status, object([]string{}, map[string]any{})}}, t...)
}

// tools are the tools node's MCP server offers: its role's own, then the read tools.
func tools(node string) []tool {
	return append(roleTools(node), readTools(node)...)
}

func roleTools(node string) []tool {
	if node == deskNode {
		return []tool{
			{"tell_master", tellMasterHelp, object([]string{"text"}, map[string]any{
				"text": str("The instruction, as a spec the master can act on alone."),
			})},
			{"transcript", transcriptHelp, object([]string{"name"}, map[string]any{
				"name":  str("The session's node name, as status shows it."),
				"turns": map[string]any{"type": "integer", "description": "How many of its last turns to show. 3 when left out."},
			})},
			{"answer", answerHelp, object([]string{"id"}, map[string]any{
				"id":      str("The decision's id, as status shows it."),
				"allow":   map[string]any{"type": "boolean", "description": "For a permission: true to allow the tool call, false to deny it."},
				"message": str("For a permission you deny, why. It reaches the session verbatim, so say what to do instead, as the user put it."),
				"answer":  str("For a question, the user's answer."),
				"words":   str("Leave it out on the first call. On the second, the user's yes: all of their latest message, quoted exactly."),
			})},
			{"away", awayHelp, object([]string{"on"}, map[string]any{
				"on":    map[string]any{"type": "boolean", "description": "true when the user leaves, false when they are back."},
				"words": str("Turning it on, leave it out on the first call, and on the second give the user's yes: all of their latest message, quoted exactly. Turning it off, the user's words that say they are back, quoted exactly from their latest message."),
			})},
			{"grant", grantHelp + classList(), object([]string{"class", "until"}, map[string]any{
				"class": str("push or rebuild."),
				"until": str("When it ends, at most a day away: a time of day such as 09:00, which means its next coming, or a duration such as 8h. Pass the same on both calls: the grant ends when the first call showed."),
				"words": str("Leave it out on the first call. On the second, the user's yes: all of their latest message, quoted exactly."),
			})},
			{"revoke", revokeHelp, object([]string{"id", "words"}, map[string]any{
				"id":    str("The grant's id, such as g1, as status shows it."),
				"words": str("The user's words that ask for it, quoted exactly from their latest message."),
			})},
		}
	}
	if node != masterNode {
		return []tool{
			{"spawn", "Start a child session in its own git worktree, branched from your HEAD, with prompt as its task. Each time it ends its turn or stops in the middle of one, bosun tells you: when you next end your turn, or in a new turn once you have ended yours. So to wait for your children, end your turn." + peersHelp, object([]string{"name", "prompt"}, map[string]any{
				"name":       str(nameHelp),
				"prompt":     str("The child's task. It sees nothing else, so say everything it needs."),
				"model":      str(modelHelp),
				"effort":     str(effortHelp),
				"plugins":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": pluginsHelp},
				"connectors": map[string]any{"type": "boolean", "description": connectorsHelp},
			})},
			{"ask", "Ask the master a question and wait for the answer. Use it for any decision you would otherwise put to the user. The master may pass it on to the user, so the wait can be long.", object([]string{"question"}, map[string]any{
				"question": str("The question, with what the answer depends on."),
			})},
			{"merge", "Merge a child's branch into yours once its work is right, then remove the child with its worktree and branch. It only fast-forwards, so a child whose branch has fallen behind yours must rebase onto it first.", object([]string{"name"}, map[string]any{
				"name": str("The child's node name."),
			})},
		}
	}
	return []tool{
		{"spawn", "Start a worker session in its own git worktree, branched from the repository's checked-out HEAD, with prompt as its task." + peersHelp, object([]string{"name", "repo", "prompt"}, map[string]any{
			"name":        str(nameHelp),
			"repo":        str("Path to a git repository Claude Code trusts."),
			"prompt":      str("The worker's task. It sees nothing else, so say everything it needs."),
			"model":       str(modelHelp),
			"effort":      str(effortHelp),
			"plugins":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": pluginsHelp},
			"connectors":  map[string]any{"type": "boolean", "description": connectorsHelp},
			"instruction": instructionProp,
		})},
		{"next_event", nextEventHelp, object([]string{}, map[string]any{})},
		{"reply", "Answer a worker's question. The worker gets the answer as the result of its ask. Answer when the user's instructions or the worker's task settle the question, and escalate it otherwise.", object([]string{"id", "answer"}, map[string]any{
			"id":     str("The decision's id."),
			"answer": str("The answer."),
		})},
		{"decide", "Allow or deny a worker's permission request: a tool call that its permission rules and auto mode did not approve, or a reserved action. Allow it when the worker's task needs it and it can be undone. Deny it with a message that tells the worker what to do instead. Escalate what the user should judge, such as a call the task needs that cannot be undone. While away mode is on, judge each request as you would, and escalate what the user should judge all the same: bosun parks it for their return and denies the call, as next_event says. A request with reserved set is a reserved action, which bosun held before the sandbox and auto mode could judge it, and reserved says why. Allowing one lets it past both: a command outside the sandbox runs with all of the user's access, and a write outside the worker's folders lands where neither the sandbox nor your review of its branch reaches. So allow one only when the user's instructions or the worker's task plainly cover it, and escalate the rest. Deny with what to do instead, such as \"write it under $TMPDIR\" for a throwaway file, or \"run it in the sandbox\" when nothing in it needs to leave. Read a command outside the sandbox whole: it can write anywhere, and bosun checked only the paths it names, which of them it may write, and whether it runs as root. A command that only reads, such as bosun status or cat of bosun's log, needs no escalation. Escalate it when you cannot follow all it does, such as a script you have not read, or when it may write what the user keeps for themselves: credentials such as ~/.ssh, shell startup files, Claude Code's configuration, git hooks and config, systemd units, autostart entries and scheduled jobs such as crontab or systemd-run, and bosun's state. Deleting a branch can lose commits that nothing else holds: allow it only for a branch that is merged or that the worker made for itself. /etc/nixos outside its worktrees is the configuration that nxt activates as root: deny a write there, and send the worker to its worktree. A request with refused set comes after auto mode refused the worker's last call, which says why: after a run of refusals, auto mode asks about a call instead of refusing it, so it likely judged this one risky too. Escalate it unless the user's instructions plainly cover it. A push publishes its commits, and only a force push, which is the user's, takes them back: allow one only when the user's instructions or the worker's task ask for it. nixos-rebuild test, or nxt, runs the activation scripts of the configuration it names as root, and a reboot does not undo what they did. On /etc/nixos that configuration is what is checked out there. On a worktree under /etc/nixos/.claude/worktrees/ it is that worktree's files, whichever worker asks: first read git -C <worktree> diff <the branch it began from>, which includes uncommitted changes, and escalate if the change does anything its task does not need. nixos-rebuild build, or nxc, activates nothing.", object([]string{"id", "allow"}, map[string]any{
			"id":      str("The decision's id."),
			"allow":   map[string]any{"type": "boolean", "description": "true to allow the tool call, false to deny it."},
			"message": str("When denying, why. It reaches the worker verbatim, so tell it what to do instead."),
		})},
		{"merge", "Merge a worker's branch into the branch it began from once you have reviewed it, then remove the worker with its worktree and branch. It only fast-forwards: when the worker's branch has fallen behind, resume the worker to rebase onto it first. When the merge needs the user, as when they switched the checkout to another branch, tell them with notify. Merge with this tool, never with git: your sandbox cannot write to a repository, so git would wait on the user.", object([]string{"name"}, map[string]any{
			"name":        str("The worker's node name."),
			"instruction": instructionProp,
		})},
		{"resume", "Give a worker that ended its turn, or stopped in the middle of one, a new prompt, such as a fix after review. It carries on in the same conversation and worktree, and its next turn_end comes as an event. Give a model or effort to switch it for this turn on, such as a higher effort after a failed attempt, or a lower one once the hard part is done. The switch moves the worker to a new session id, which the daemon follows.", object([]string{"name", "prompt"}, map[string]any{
			"name":        str("The worker's node name."),
			"prompt":      str("What to do next. The worker remembers its task, so say only what is new."),
			"model":       str("The model to switch to, as for spawn. Left out, it keeps its own."),
			"effort":      str("The effort to switch to, as for spawn. Left out, it keeps its own."),
			"instruction": instructionProp,
		})},
		{"escalate", "Hand a question or permission request to the user, who answers it from bosun. Its worker keeps waiting, and the decision needs nothing more from you. While away mode is on, it reaches nobody: bosun parks the decision on the list the user gets when they come back, and denies the call or answers the question with a note that it waits for them, so the worker carries on.", object([]string{"id"}, map[string]any{
			"id":   str("The decision's id."),
			"note": str("What the user should know, such as why you did not decide it yourself."),
		})},
		{"notify", "Tell the user about a problem that is not a decision. Call it whenever something fails that you cannot fix, such as a spawn that Claude Code refuses: the user hears of it no other way. It waits in bosun until the user dismisses it, and nobody waits on it. While away mode is on, bosun parks it instead, on the list the user gets when they come back.", object([]string{"message"}, map[string]any{
			"message": str("What happened, and what the user can do about it."),
		})},
		{"tell_desk", "Answer a tell: tell the front desk, the session the user talks to, what became of an instruction, such as which worker took it, what merged, or what failed and waits on the user. The desk passes it on to the user, and when there is no desk, the user gets it as a notice. Keep it short, and say which instruction it answers. The desk takes no messages, so answer it only with this.", object([]string{"text"}, map[string]any{
			"text":        str("The answer."),
			"instruction": instructionProp,
		})},
	}
}

type mcpServer struct {
	node   string
	mu     sync.Mutex
	enc    *json.Encoder
	cancel map[string]context.CancelFunc
}

func runMCP() error {
	node := os.Getenv("BOSUN_NODE")
	if node == "" {
		return errors.New("BOSUN_NODE is not set")
	}
	return newMCPServer(node, os.Stdout).serve(os.Stdin)
}

func newMCPServer(node string, w io.Writer) *mcpServer {
	return &mcpServer{node: node, enc: json.NewEncoder(w), cancel: map[string]context.CancelFunc{}}
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcOut struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (s *mcpServer) send(m rpcOut) {
	m.JSONRPC = "2.0"
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.enc.Encode(m)
}

// serve reads JSON-RPC messages until stdin closes, running each tool call concurrently so a blocked one does not hold up the rest.
func (s *mcpServer) serve(r io.Reader) error {
	dec := json.NewDecoder(r)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer s.cancelAll()
	for {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch msg.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			s.send(rpcOut{ID: msg.ID, Result: map[string]any{
				"protocolVersion": p.ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "bosun", "version": "0.1.0"},
			}})
		case "ping":
			s.send(rpcOut{ID: msg.ID, Result: map[string]any{}})
		case "tools/list":
			s.send(rpcOut{ID: msg.ID, Result: map[string]any{"tools": tools(s.node)}})
		case "tools/call":
			ctx, cancel := context.WithCancel(context.Background())
			s.mu.Lock()
			s.cancel[string(msg.ID)] = cancel
			s.mu.Unlock()
			wg.Go(func() { s.toolCall(ctx, msg.ID, msg.Params) })
		case "notifications/cancelled":
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			s.mu.Lock()
			if cancel := s.cancel[string(p.RequestID)]; cancel != nil {
				cancel()
			}
			s.mu.Unlock()
		default:
			if len(msg.ID) > 0 && msg.Method != "" {
				s.send(rpcOut{ID: msg.ID, Error: &rpcError{Code: -32601, Message: "method not found: " + msg.Method}})
			}
		}
	}
}

func (s *mcpServer) cancelAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cancel := range s.cancel {
		cancel()
	}
}

func (s *mcpServer) toolCall(ctx context.Context, id, params json.RawMessage) {
	defer func() {
		s.mu.Lock()
		s.cancel[string(id)]()
		delete(s.cancel, string(id))
		s.mu.Unlock()
	}()
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	_ = json.Unmarshal(params, &p)
	if len(p.Meta.ProgressToken) > 0 {
		go s.progress(ctx, p.Meta.ProgressToken)
	}
	text, err := s.run(ctx, p.Name, p.Arguments)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		text = "bosun: " + err.Error()
	}
	s.send(rpcOut{ID: id, Result: map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
		"isError": err != nil,
	}})
}

func (s *mcpServer) progress(ctx context.Context, token json.RawMessage) {
	tick := time.NewTicker(progressEvery)
	defer tick.Stop()
	for n := 1; ; n++ {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.send(rpcOut{Method: "notifications/progress", Params: map[string]any{"progressToken": token, "progress": n, "message": "waiting"}})
		}
	}
}

func (s *mcpServer) run(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	var args struct {
		Name        string   `json:"name"`
		Repo        string   `json:"repo"`
		Model       string   `json:"model"`
		Effort      string   `json:"effort"`
		Plugins     []string `json:"plugins"`
		Connectors  bool     `json:"connectors"`
		Prompt      string   `json:"prompt"`
		Question    string   `json:"question"`
		ID          string   `json:"id"`
		Answer      string   `json:"answer"`
		Allow       *bool    `json:"allow"`
		On          *bool    `json:"on"`
		Class       string   `json:"class"`
		Until       string   `json:"until"`
		Message     string   `json:"message"`
		Note        string   `json:"note"`
		Text        string   `json:"text"`
		Instruction int      `json:"instruction"`
		Turns       int      `json:"turns"`
		Words       string   `json:"words"`
		Before      string   `json:"before"`
		Limit       int      `json:"limit"`
		Since       string   `json:"since"`
		Node        string   `json:"node"`
		Kind        string   `json:"kind"`
		By          string   `json:"by"`
		ReachedUser bool     `json:"reached_user"`
		Full        bool     `json:"full"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", err
		}
	}
	known := false
	for _, t := range tools(s.node) {
		known = known || t.Name == name
	}
	if !known {
		return "", fmt.Errorf("no tool %q", name)
	}
	req := Request{Op: name, Node: s.node, ID: args.ID}
	switch name {
	case "spawn":
		req.Name, req.Repo, req.Model, req.Effort, req.Text = args.Name, args.Repo, args.Model, args.Effort, args.Prompt
		req.Plugins, req.Connectors = args.Plugins, args.Connectors
		req.Instruction = args.Instruction
	case "resume":
		req.Name, req.Model, req.Effort, req.Text = args.Name, args.Model, args.Effort, args.Prompt
		req.Instruction = args.Instruction
	case "merge":
		req.Name = args.Name
		req.Instruction = args.Instruction
	case "ask":
		req.Text = args.Question
	case "reply":
		req.Text = args.Answer
	case "decide":
		req.Op, req.Text = "deny", args.Message
		if args.Allow != nil && *args.Allow {
			req.Op = "approve"
		}
	case "answer":
		// A permission takes allow, and a question an answer.
		switch {
		case args.Allow == nil:
			req.Op, req.Text = "reply", args.Answer
		case *args.Allow:
			req.Op = "approve"
		default:
			req.Op, req.Text = "deny", args.Message
		}
		req.Words = args.Words
	case "escalate":
		req.Text = args.Note
	case "notify":
		req.Text = args.Message
	case "away":
		if args.On == nil {
			return "", errors.New("say whether the user leaves, with on")
		}
		req.Name, req.Words = "off", args.Words
		if *args.On {
			req.Name = "on"
		}
	case "grant":
		req.Name, req.Text, req.Words = args.Class, args.Until, args.Words
	case "revoke":
		req.Words = args.Words
	case "tell_master", "tell_desk":
		// The daemon passes a tell on by who sends it.
		req.Op, req.Text, req.Instruction = "tell", args.Text, args.Instruction
	case "transcript":
		req.Name, req.Turns = args.Name, args.Turns
	case "activity":
		req.Name = args.Name
	case "log":
		req.Log = &LogQuery{Since: args.Since, Node: args.Node, Kind: args.Kind, By: args.By, ReachedUser: args.ReachedUser, Limit: args.Limit}
	case "conversation":
		req.Name, req.Before, req.Limit = args.Name, args.Before, cmp.Or(args.Limit, mcpConvLimit)
		return s.conversation(ctx, req)
	}
	if s.node != masterNode {
		// Only the master acts on the desk's instructions.
		req.Instruction = 0
	}
	send := call
	if name == "ask" || name == "next_event" {
		// Each may block across a restart of the daemon, which loads its decision or its batch of events again.
		send = callAgain
	}
	resp, err := send(ctx, req)
	if err != nil {
		return "", err
	}
	switch name {
	case "spawn":
		return fmt.Sprintf("Started %s, session %s, in %s.", resp.Session.Node, resp.Session.ID, resp.Session.Dir), nil
	case "ask", "merge", "transcript", "answer", "activity":
		return resp.Text, nil
	case "log":
		return logText(resp.Log, args.Full), nil
	case "status":
		return fleetStatus(resp), nil
	case "tell_master":
		return fmt.Sprintf("The master has it as instruction %d. Its answer comes to you through bosun.", resp.Instruction), nil
	case "tell_desk":
		return "The user will hear of it through the desk.", nil
	case "next_event":
		if len(resp.Events) == 0 {
			return "No events. If a subagent or background command of yours still runs, call next_event again. Otherwise end your turn: the next event starts a new one.", nil
		}
		data, err := json.MarshalIndent(resp.Events, "", "  ")
		if resp.Away != nil {
			return awayLine(resp.Away, resp.Grants) + "\n\n" + string(data), err
		}
		return string(data), err
	case "escalate", "notify", "away", "grant", "revoke":
		return resp.Text, nil
	}
	return "Done.", nil
}

// fleetStatus is what the desk's status tool shows: bosun status, then each decision and message in full.
func fleetStatus(s Response) string {
	var b strings.Builder
	messages := s.Messages
	s.Messages = nil
	printFleet(&b, s, true)
	if len(s.Decisions) > 0 {
		data, _ := json.MarshalIndent(s.Decisions, "", "  ")
		fmt.Fprintf(&b, "\nThe decisions in full:\n%s\n", data)
	}
	if len(messages) > 0 {
		b.WriteString("\nThe latest messages between sessions, oldest first:\n")
	}
	for _, m := range messages {
		fmt.Fprintf(&b, "\n%s → %s:\n%s\n", m.From, m.Node, m.Text)
	}
	return b.String()
}
