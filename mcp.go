package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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

func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

const (
	nameHelp  = "Node name: lowercase letters, digits and hyphens, unique in the repository. It names the session, its worktree and its branch worktree-<name>."
	peersHelp = " Workers message each other by node name. When several share one feature, name in each worker's prompt the other workers' nodes and what each one builds, and start them all before any needs another."
	modelHelp = "The session's model, such as fable, opus, sonnet or haiku. Leave it out unless the user named one for this task: the user sets the default."

	nextEventHelp = `Wait up to four minutes for events from the workers and the user. Returns them as JSON, or says there were none.

Work in a loop: call next_event, handle every event it returns, and call it again. When it returns no events, end your turn: the next event starts a new one.

The events:
- tell: a message from the user. Act on it, usually by starting workers with spawn.
- decision of kind question: a worker asks something. Settle it with reply or escalate.
- decision of kind permission: a worker wants to run a tool. Settle it with decide or escalate.
- turn_end: a worker finished a turn, and its last message says what it did, or that the turn failed on an API error. To give it more to do, such as a fix after review or another try, call resume. Once its work is right, call merge.
- exit: a worker's session stopped. Either it stopped in the middle of a turn, so no turn_end follows, and resume carries it on, or the user stopped it, as the event's text says, and resume refuses it.
- message: what a session told a worker through Claude Code's SendMessage, such as an interface two workers agreed on or the order their branches merge in. The worker gets it without you: a message to a worker between turns starts a turn, so never resume a worker to pass one on. A message needs nothing from you, so it comes along with the next event that does.

Workers message each other, so a turn_end does not always mean done. A worker may end its turn to wait for another worker's message, and a turn that a message started also ends in a turn_end. The turn_end's text says which. Merge workers in the order they agreed on. When workers wait only on each other, none starts again: resume one of them and tell it to send its part first.

A usage limit stops a worker without a turn_end, and bosun resumes it once the limit resets.

bosun sends a git push, deleting a branch, a file write outside a session's own folders and a command outside the sandbox straight to the user, so none of them reaches you, and no answer of yours allows one.`
)

func tools(master bool) []tool {
	if !master {
		return []tool{
			{"spawn", "Start a child session in its own git worktree, branched from your HEAD, with prompt as its task." + peersHelp, object([]string{"name", "prompt"}, map[string]any{
				"name":   str(nameHelp),
				"prompt": str("The child's task. It sees nothing else, so say everything it needs."),
				"model":  str(modelHelp),
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
			"name":   str(nameHelp),
			"repo":   str("Path to a git repository Claude Code trusts."),
			"prompt": str("The worker's task. It sees nothing else, so say everything it needs."),
			"model":  str(modelHelp),
		})},
		{"next_event", nextEventHelp, object([]string{}, map[string]any{})},
		{"reply", "Answer a worker's question. The worker gets the answer as the result of its ask. Answer when the user's instructions or the worker's task settle the question, and escalate it otherwise.", object([]string{"id", "answer"}, map[string]any{
			"id":     str("The decision's id."),
			"answer": str("The answer."),
		})},
		{"decide", "Allow or deny a worker's permission request: a tool call that its permission rules and auto mode did not approve. Allow it when the worker's task needs it and it can be undone. Deny it with a message that tells the worker what to do instead. Escalate what the user should judge, such as a call the task needs that cannot be undone.", object([]string{"id", "allow"}, map[string]any{
			"id":      str("The decision's id."),
			"allow":   map[string]any{"type": "boolean", "description": "true to allow the tool call, false to deny it."},
			"message": str("When denying, why. It reaches the worker verbatim, so tell it what to do instead."),
		})},
		{"merge", "Merge a worker's branch into the branch it began from once you have reviewed it, then remove the worker with its worktree and branch. It only fast-forwards: when the worker's branch has fallen behind, resume the worker to rebase onto it first. When the merge needs the user, as when they switched the checkout to another branch, tell them with notify. Merge with this tool, never with git: your sandbox cannot write to a repository, so git would wait on the user.", object([]string{"name"}, map[string]any{
			"name": str("The worker's node name."),
		})},
		{"resume", "Give a worker that ended its turn, or stopped in the middle of one, a new prompt, such as a fix after review. It carries on in the same conversation and worktree, and its next turn_end comes as an event.", object([]string{"name", "prompt"}, map[string]any{
			"name":   str("The worker's node name."),
			"prompt": str("What to do next. The worker remembers its task, so say only what is new."),
		})},
		{"escalate", "Hand a question or permission request to the user, who answers it from bosun. Its worker keeps waiting, and the decision needs nothing more from you.", object([]string{"id"}, map[string]any{
			"id":   str("The decision's id."),
			"note": str("What the user should know, such as why you did not decide it yourself."),
		})},
		{"notify", "Tell the user about a problem that is not a decision. Call it whenever something fails that you cannot fix, such as a spawn that Claude Code refuses: the user hears of it no other way. It waits in bosun until the user dismisses it, and nobody waits on it.", object([]string{"message"}, map[string]any{
			"message": str("What happened, and what the user can do about it."),
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
			s.send(rpcOut{ID: msg.ID, Result: map[string]any{"tools": tools(s.node == masterNode)}})
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
		Name     string `json:"name"`
		Repo     string `json:"repo"`
		Model    string `json:"model"`
		Prompt   string `json:"prompt"`
		Question string `json:"question"`
		ID       string `json:"id"`
		Answer   string `json:"answer"`
		Allow    bool   `json:"allow"`
		Message  string `json:"message"`
		Note     string `json:"note"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", err
		}
	}
	known := false
	for _, t := range tools(s.node == masterNode) {
		known = known || t.Name == name
	}
	if !known {
		return "", fmt.Errorf("no tool %q", name)
	}
	req := Request{Op: name, Node: s.node, ID: args.ID}
	switch name {
	case "spawn":
		req.Name, req.Repo, req.Model, req.Text = args.Name, args.Repo, args.Model, args.Prompt
	case "resume":
		req.Name, req.Text = args.Name, args.Prompt
	case "merge":
		req.Name = args.Name
	case "ask":
		req.Text = args.Question
	case "reply":
		req.Text = args.Answer
	case "decide":
		req.Op, req.Text = "deny", args.Message
		if args.Allow {
			req.Op = "approve"
		}
	case "escalate":
		req.Text = args.Note
	case "notify":
		req.Text = args.Message
	}
	resp, err := call(ctx, req)
	if err != nil {
		return "", err
	}
	switch name {
	case "spawn":
		return fmt.Sprintf("Started %s, session %s, in %s.", resp.Session.Node, resp.Session.ID, resp.Session.Dir), nil
	case "ask", "merge":
		return resp.Text, nil
	case "next_event":
		if len(resp.Events) == 0 {
			return "No events. End your turn: the next event starts a new one.", nil
		}
		data, err := json.MarshalIndent(resp.Events, "", "  ")
		return string(data), err
	case "escalate":
		return "Escalated to the user.", nil
	case "notify":
		return "The user will see it.", nil
	}
	return "Done.", nil
}
