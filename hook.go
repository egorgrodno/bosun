package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

func runHook() {
	if out := hookOutput(os.Getenv("BOSUN_NODE"), os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET"), os.Stdin); out != nil {
		_ = json.NewEncoder(os.Stdout).Encode(out)
	}
}

// hookOutput relays a hook event to the daemon and returns what the hook prints, or nil to leave the call to Claude Code.
// A PreToolUse that fails or times out lets the call through, so the reserved check denies on any error, an unreachable daemon included.
func hookOutput(node, socket string, stdin io.Reader) any {
	raw, err := io.ReadAll(stdin)
	var in struct {
		Event string `json:"hook_event_name"`
	}
	if err == nil {
		err = json.Unmarshal(raw, &in)
	}
	timeout := 10 * time.Second
	if in.Event == "PreToolUse" || in.Event == "PermissionRequest" {
		timeout = holdTimeout - holdMargin
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var resp Response
	if err == nil {
		resp, err = callAgain(ctx, Request{Op: "hook", Node: node, Socket: socket, Hook: raw})
	}
	switch in.Event {
	case "PreToolUse":
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return preToolUse("deny", fmt.Sprintf("bosun: this needs the user's approval, and none came within %v.", timeout))
		case err != nil:
			return preToolUse("deny", fmt.Sprintf("bosun: checking whether this needs the user's approval failed: %v", err))
		case resp.Behavior != "":
			return preToolUse(resp.Behavior, resp.Text)
		}
	case "PermissionRequest":
		if err == nil && resp.Behavior != "" {
			decision := map[string]string{"behavior": resp.Behavior}
			if resp.Behavior == "deny" {
				decision["message"] = resp.Text
			}
			return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PermissionRequest", "decision": decision}}
		}
	case "Stop":
		if err == nil && resp.Behavior == "block" {
			return map[string]string{"decision": "block", "reason": resp.Text}
		}
	}
	return nil
}

func preToolUse(behavior, reason string) any {
	out := map[string]string{"hookEventName": "PreToolUse", "permissionDecision": behavior}
	if reason != "" {
		out["permissionDecisionReason"] = reason
	}
	return map[string]any{"hookSpecificOutput": out}
}
