package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// cursorHookPayload is the subset of a Cursor hook stdin payload Rhizome inspects.
// Everything else, including attachments, tool output and the user's email, is
// never read into a named field.
type cursorHookPayload struct {
	Event              string          `json:"hook_event_name"`
	ConversationID     string          `json:"conversation_id"`
	WorkspaceRoots     []string        `json:"workspace_roots"`
	Cwd                string          `json:"cwd"`
	ToolName           string          `json:"tool_name"`
	ToolInput          json.RawMessage `json:"tool_input"`
	MCPServer          string          `json:"mcp_server_name"`
	Text               string          `json:"text"`
	Status             string          `json:"status"`
	LoopCount          int             `json:"loop_count"`
	Prompt             string          `json:"prompt"`
	SubagentID         string          `json:"subagent_id"`
	ParentConversation string          `json:"parent_conversation_id"`
	Background         bool            `json:"is_background_agent"`
}

// defaultCursorResponse is the answer that leaves Cursor's own behavior
// unchanged when Rhizome has nothing to add or cannot be reached.
func defaultCursorResponse(event string) map[string]any {
	switch event {
	case "beforeSubmitPrompt":
		return map[string]any{"continue": true}
	case "beforeMCPExecution":
		return map[string]any{"permission": "ask"}
	}
	return map[string]any{}
}

// minimizeCursorPayload decides whether an event reaches the MCP process and
// strips it to the fields the node needs. ok is false for events Rhizome
// ignores: subagents, cloud/background agents, other MCP servers and unknown
// hooks. The prompt is reduced to one boolean and never forwarded.
func minimizeCursorPayload(p cursorHookPayload) (msg cursorMessage, ok bool) {
	if p.SubagentID != "" || p.ParentConversation != "" || p.Background || p.ConversationID == "" {
		return msg, false
	}
	msg = cursorMessage{Event: p.Event, ConversationID: p.ConversationID, WorkspaceRoots: cleanRoots(p.WorkspaceRoots)}
	if filepath.IsAbs(p.Cwd) {
		msg.Cwd = filepath.Clean(p.Cwd)
	}
	switch p.Event {
	case "sessionStart", "sessionEnd":
	case "beforeSubmitPrompt":
		msg.PeerInput = isPeerInput(p.Prompt)
	case "postToolUse", "postToolUseFailure":
		msg.ToolName = clip(p.ToolName, 128)
	case "afterAgentResponse":
		msg.Text = clip(p.Text, maxText)
	case "stop":
		msg.Status, msg.LoopCount = p.Status, p.LoopCount
	case "beforeMCPExecution":
		if p.MCPServer != "rhizome" {
			return msg, false
		}
		msg.ToolName = clip(p.ToolName, 128)
		msg.InputHash = canonicalHash(p.ToolInput)
	default:
		return msg, false
	}
	return msg, true
}

func cleanRoots(roots []string) []string {
	var out []string
	for _, r := range roots {
		if filepath.IsAbs(r) {
			out = append(out, filepath.Clean(r))
		}
	}
	return out
}

// sameRoot reports whether two workspace roots describe the same tree: equal,
// or one inside the other (multi-root workspaces).
func sameRoot(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+string(os.PathSeparator)) || strings.HasPrefix(b, a+string(os.PathSeparator))
}

// selectCursorRegistration picks the one MCP process serving these workspace
// roots. Zero or several candidates fail closed: the hook then changes nothing.
func selectCursorRegistration(dir string, roots []string, alive func(int) bool) (*cursorRegistration, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var live, matched, unknown []*cursorRegistration
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var reg cursorRegistration
		if json.Unmarshal(raw, &reg) != nil || reg.Socket == "" || !alive(reg.PID) {
			continue
		}
		r := reg
		live = append(live, &r)
		if len(r.Roots) == 0 {
			unknown = append(unknown, &r)
			continue
		}
		for _, have := range r.Roots {
			for _, want := range roots {
				if sameRoot(filepath.Clean(have), want) {
					matched = append(matched, &r)
					have = ""
					break
				}
			}
			if have == "" {
				break
			}
		}
	}
	candidates := live
	if len(roots) > 0 {
		candidates = matched
		if len(candidates) == 0 {
			candidates = unknown
		}
	}
	switch len(candidates) {
	case 0:
		return nil, fmt.Errorf("no live Cursor MCP process registered for this workspace (%d live)", len(live))
	case 1:
		return candidates[0], nil
	}
	return nil, fmt.Errorf("%d Cursor MCP processes match this workspace; refusing to guess", len(candidates))
}

// CursorHook implements `rhizome hook --agent cursor`: it reads one hook
// payload from stdin, forwards the minimized event to the matching MCP process
// and prints that process's response. Every failure prints a response that
// leaves Cursor's behavior unchanged, so a missing or slow Rhizome never blocks
// the user. runtime may be empty when configuration could not be resolved.
func CursorHook(ctx context.Context, runtime string, stdin io.Reader, stdout io.Writer) error {
	resp := cursorHookResponse(ctx, runtime, stdin)
	raw, err := json.Marshal(resp)
	if err != nil {
		raw = []byte("{}")
	}
	_, err = fmt.Fprintln(stdout, string(raw))
	return err
}

func cursorHookResponse(ctx context.Context, runtime string, stdin io.Reader) map[string]any {
	raw, err := io.ReadAll(io.LimitReader(stdin, maxCursorMessage+1))
	if err != nil || len(raw) > maxCursorMessage {
		slog.Warn("cursor_hook_skip", "reason", "payload unreadable or oversized")
		return map[string]any{}
	}
	var p cursorHookPayload
	if json.Unmarshal(raw, &p) != nil {
		slog.Warn("cursor_hook_skip", "reason", "payload is not JSON")
		return map[string]any{}
	}
	fallback := defaultCursorResponse(p.Event)
	msg, ok := minimizeCursorPayload(p)
	if !ok {
		return fallback
	}
	if runtime == "" {
		slog.Warn("cursor_hook_skip", "event", p.Event, "reason", "configuration unavailable")
		return fallback
	}
	reg, err := selectCursorRegistration(cursorRegistry(runtime), msg.WorkspaceRoots, processAlive)
	if err != nil {
		slog.Warn("cursor_hook_skip", "event", p.Event, "reason", err.Error())
		return fallback
	}
	resp, err := cursorHookCall(ctx, reg.Socket, msg)
	if err != nil {
		slog.Warn("cursor_hook_skip", "event", p.Event, "reason", err.Error())
		return fallback
	}
	return resp
}

// cursorHookCall performs one request over the MCP process's private socket.
func cursorHookCall(ctx context.Context, socket string, msg cursorMessage) (map[string]any, error) {
	deadline := time.Now().Add(cursorSocketTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn, err := (&net.Dialer{Deadline: deadline}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)
	raw, _ := json.Marshal(msg)
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return nil, err
	}
	line, err := readLine(conn, maxCursorResponse)
	if err != nil {
		return nil, err
	}
	var resp map[string]any
	if json.Unmarshal(line, &resp) != nil || resp == nil {
		return nil, errors.New("invalid response from MCP process")
	}
	return resp, nil
}
