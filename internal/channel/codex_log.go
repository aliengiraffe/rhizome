package channel

import (
	"encoding/json"
	"strings"
)

// Consume response_item only: event_msg often mirrors the same assistant text.
// Tool arguments, results, reasoning, system prompts and user text are never emitted.
func (c *collector) normalizeCodexLog(line []byte, n *Node, gen uint64) {
	var r struct {
		Type    string `json:"type"`
		Ordinal *int64 `json:"ordinal"`
		Payload struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Role    string `json:"role"`
			Name    string `json:"name"`
			CallID  string `json:"call_id"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &r) != nil {
		c.skip(n, "invalid Codex record")
		return
	}
	p := r.Payload
	if r.Type != "response_item" {
		if r.Type == "event_msg" && p.Type == "task_complete" {
			n.activity(gen, "turn", "Turn completed")
		}
		return
	}
	key := p.ID
	// Older rollouts omit IDs. Byte offsets still prevent rereads during an
	// incarnation; replacement creates a visible gap and a new live baseline.
	if key != "" {
		if c.seen[key] {
			return
		}
		c.seen[key] = true
		c.order = append(c.order, key)
		if len(c.order) > 4096 {
			delete(c.seen, c.order[0])
			c.order = c.order[1:]
		}
	}
	switch p.Type {
	case "message":
		if p.Role == "user" {
			for _, b := range p.Content {
				if strings.TrimSpace(b.Text) != "" {
					c.peerResponse = isPeerInput(b.Text)
				}
			}
			return
		}
		if p.Role != "assistant" || c.peerResponse {
			return
		}
		for _, b := range p.Content {
			if b.Type == "output_text" && strings.TrimSpace(b.Text) != "" && !isPeerInput(b.Text) {
				n.activity(gen, "activity", b.Text)
			}
		}
	case "function_call", "custom_tool_call":
		if p.CallID == "" || c.peerResponse {
			return
		}
		if _, ok := c.tools[p.CallID]; ok {
			return
		}
		c.tools[p.CallID] = p.Name
		c.toolOrder = append(c.toolOrder, p.CallID)
		if len(c.toolOrder) > 4096 {
			delete(c.tools, c.toolOrder[0])
			c.toolOrder = c.toolOrder[1:]
		}
		if !rhizomeTool(p.Name) {
			n.activity(gen, "tool", "Tool started: "+p.Name)
		}
	case "function_call_output", "custom_tool_call_output":
		name, known := c.tools[p.CallID]
		if !known {
			return
		}
		c.tools[p.CallID] = ""
		if name != "" && !rhizomeTool(name) && !c.peerResponse {
			n.activity(gen, "tool", "Tool completed: "+name)
		}
	case "reasoning":
	default:
		c.skip(n, "unsupported Codex response item")
	}
}
func rhizomeTool(name string) bool {
	return strings.Contains(name, "mcp__rhizome__") || strings.HasPrefix(name, "rhizome.")
}
