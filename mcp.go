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

const nameHelp = "Node name: lowercase letters, digits and hyphens, unique in the repository. It names the session, its worktree and its branch worktree-<name>."

func tools(master bool) []tool {
	if !master {
		return []tool{
			{"spawn", "Start a child session in its own git worktree, branched from your HEAD, with prompt as its task.", object([]string{"name", "prompt"}, map[string]any{
				"name":   str(nameHelp),
				"prompt": str("The child's task. It sees nothing else, so say everything it needs."),
			})},
			{"ask", "Ask the master a question and wait for the answer. Use it for any decision you would otherwise put to the user. The master may pass it on to the user, so the wait can be long.", object([]string{"question"}, map[string]any{
				"question": str("The question, with what the answer depends on."),
			})},
		}
	}
	return []tool{
		{"spawn", "Start a worker session in its own git worktree, branched from the repository's checked-out HEAD, with prompt as its task.", object([]string{"name", "repo", "prompt"}, map[string]any{
			"name":   str(nameHelp),
			"repo":   str("Path to a git repository Claude Code trusts."),
			"prompt": str("The worker's task. It sees nothing else, so say everything it needs."),
		})},
		{"next_event", "Wait up to four minutes for events from the workers and the user. Returns them as JSON, or says there were none. Call it again either way.", object([]string{}, map[string]any{})},
		{"reply", "Answer a worker's question. The worker gets the answer as the result of its ask.", object([]string{"id", "answer"}, map[string]any{
			"id":     str("The decision's id."),
			"answer": str("The answer."),
		})},
		{"decide", "Allow or deny a worker's permission request.", object([]string{"id", "allow"}, map[string]any{
			"id":      str("The decision's id."),
			"allow":   map[string]any{"type": "boolean", "description": "true to allow the tool call, false to deny it."},
			"message": str("When denying, why. It reaches the worker verbatim, so tell it what to do instead."),
		})},
		{"escalate", "Hand a question or permission request to the user, who answers it from bosun. Its worker keeps waiting.", object([]string{"id"}, map[string]any{
			"id":   str("The decision's id."),
			"note": str("What the user should know, such as why you did not decide it yourself."),
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
		req.Name, req.Repo, req.Text = args.Name, args.Repo, args.Prompt
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
	}
	resp, err := call(ctx, req)
	if err != nil {
		return "", err
	}
	switch name {
	case "spawn":
		return fmt.Sprintf("Started %s, session %s, in %s.", resp.Session.Node, resp.Session.ID, resp.Session.Dir), nil
	case "ask":
		return resp.Text, nil
	case "next_event":
		if len(resp.Events) == 0 {
			return "No events.", nil
		}
		data, err := json.MarshalIndent(resp.Events, "", "  ")
		return string(data), err
	case "escalate":
		return "Escalated to the user.", nil
	}
	return "Done.", nil
}
