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
	"slices"
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
	case "preToolUse":
		// The IDE (3.27) skips beforeMCPExecution and reports MCP calls here as
		// "MCP:<tool>" without the server name, so only Rhizome's tool names
		// qualify for a ticket.
		name, ok := strings.CutPrefix(p.ToolName, "MCP:")
		if !ok || !slices.Contains(rhizomeTools, name) {
			return msg, false
		}
		msg.ToolName = name
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

// canonicalPath cleans an absolute path and resolves symlinks when it exists,
// so a workspace reported as /var/... and a process started in /private/var/...
// (macOS) compare equal. Paths that do not exist are only cleaned.
func canonicalPath(p string) string {
	p = filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(p); err == nil && filepath.IsAbs(resolved) {
		return resolved
	}
	return p
}

// broadRoot reports a registered root too wide to claim nested workspaces:
// the filesystem root or the user's home directory (the IDE helper starts MCP
// servers there). Such a root still matches exactly.
func broadRoot(path, home string) bool {
	return path == string(os.PathSeparator) || path == filepath.VolumeName(path)+string(os.PathSeparator) || (home != "" && path == filepath.Clean(home))
}

// rankRegistration scores how well a registration's roots (client-reported
// roots plus its working directory) fit the payload's workspace roots. Tier 1
// is an exact match, tier 2 a payload root inside a registered root, tier 3 a
// registered root inside a payload root; specificity is the matched registered
// root's length. Tier 0 means no relation; tier 4 a registration that reported
// nothing at all. reported says the best match came from client-reported roots
// rather than the working directory.
func rankRegistration(reg *cursorRegistration, roots []string, home string) (tier, specificity int, reported bool) {
	var have []string
	for _, r := range cleanRoots(reg.Roots) {
		have = append(have, canonicalPath(r))
	}
	nReported := len(have)
	if filepath.IsAbs(reg.Cwd) {
		have = append(have, canonicalPath(reg.Cwd))
	}
	if len(have) == 0 {
		return 4, 0, false
	}
	for i, h := range have {
		for _, r := range roots {
			want := canonicalPath(r)
			t := 0
			switch {
			case h == want:
				t = 1
			case strings.HasPrefix(want, h+string(os.PathSeparator)) && !broadRoot(h, home):
				t = 2
			case strings.HasPrefix(h, want+string(os.PathSeparator)):
				t = 3
			}
			if t == 0 {
				continue
			}
			if tier == 0 || t < tier || (t == tier && len(h) > specificity) || (t == tier && len(h) == specificity && i < nReported) {
				tier, specificity, reported = t, len(h), i < nReported
			}
		}
	}
	return tier, specificity, reported
}

// selectCursorRegistration picks the one MCP process serving these workspace
// roots: the best tier wins, the most specific root within it, and exactly one
// candidate must remain. The IDE also starts a process from its agent worker
// in the workspace; it is marked and never qualifies. As a second guard, among
// equal matches one made through client-reported roots (the IDE chat's own
// process) beats one made only through a working directory. Zero or several
// candidates fail closed: the hook then changes nothing. Without payload roots
// only a single live process qualifies.
func selectCursorRegistration(dir string, roots []string, alive func(int) bool, home string) (*cursorRegistration, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var live []*cursorRegistration
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var reg cursorRegistration
		if json.Unmarshal(raw, &reg) != nil || reg.Socket == "" || reg.Worker || !alive(reg.PID) {
			continue
		}
		r := reg
		live = append(live, &r)
	}
	candidates := live
	if len(roots) > 0 {
		bestTier, bestSpec := 0, 0
		for _, r := range live {
			tier, spec, _ := rankRegistration(r, roots, home)
			if tier == 0 {
				continue
			}
			if bestTier == 0 || tier < bestTier || (tier == bestTier && spec > bestSpec) {
				bestTier, bestSpec = tier, spec
			}
		}
		candidates = nil
		for _, r := range live {
			if tier, spec, _ := rankRegistration(r, roots, home); tier == bestTier && tier != 0 && (tier == 1 || tier == 4 || spec == bestSpec) {
				candidates = append(candidates, r)
			}
		}
		var reported []*cursorRegistration
		for _, r := range candidates {
			if _, _, ok := rankRegistration(r, roots, home); ok {
				reported = append(reported, r)
			}
		}
		if len(reported) > 0 {
			candidates = reported
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
	home, _ := os.UserHomeDir()
	reg, err := selectCursorRegistration(cursorRegistry(runtime), msg.WorkspaceRoots, processAlive, home)
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
