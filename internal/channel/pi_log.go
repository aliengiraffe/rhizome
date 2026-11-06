package channel

import (
	"encoding/json"
	"strings"
)

// normalizePiLog turns one record of a Pi session file into activity. Pi writes
// entries of every branch in append order, and history reads them in that order.
// The header carries identity and is not activity; extension-delivered peer text
// is a `custom_message` entry, so it can never be mistaken for a human prompt.
func (c *collector) normalizePiLog(line []byte, n activitySink, gen uint64) {
	var r struct {
		Type       string          `json:"type"`
		ID         string          `json:"id"`
		Name       string          `json:"name"`
		CustomType string          `json:"customType"`
		Content    json.RawMessage `json:"content"`
		Message    struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			StopReason string          `json:"stopReason"`
			ToolCallID string          `json:"toolCallId"`
			ToolName   string          `json:"toolName"`
			IsError    bool            `json:"isError"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &r); err != nil {
		c.skip(n, "invalid JSON")
		return
	}
	if n.local().Generation != gen {
		return
	}
	switch r.Type {
	case "session", "model_change", "thinking_level_change", "usage", "compaction", "context_edit", "branch_summary", "custom", "label":
		return
	case "session_info":
		if title := cleanName(r.Name); title != "" {
			n.setNativeTitle(gen, true, title)
		}
		return
	case "custom_message":
		if r.CustomType == "rhizome" || isPeerInput(piText(r.Content)) {
			c.peerResponse = true // the turn this delivery causes is a reaction
		}
		return
	case "message":
	default:
		c.skip(n, "unknown type")
		return
	}
	if r.ID != "" {
		if c.seen[r.ID] {
			return
		}
		c.seen[r.ID] = true
		c.order = append(c.order, r.ID)
		if len(c.order) > 4096 {
			delete(c.seen, c.order[0])
			c.order = c.order[1:]
		}
	}
	switch r.Message.Role {
	case "user":
		text := piText(r.Message.Content)
		if isPeerInput(text) {
			c.peerResponse = true
			return
		}
		c.peerResponse = false
		if strings.TrimSpace(text) != "" {
			n.activity(gen, "prompt", text)
		}
	case "assistant":
		var blocks []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(r.Message.Content, &blocks) != nil {
			c.skip(n, "unknown content")
			return
		}
		if c.peerResponse {
			return
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" && !strings.Contains(b.Text, "<rhizome-peer-event>") {
					n.activity(gen, "activity", b.Text)
				}
			case "toolCall":
				if b.ID == "" {
					c.skip(n, "tool missing ID")
					continue
				}
				if _, exists := c.tools[b.ID]; exists {
					continue
				}
				c.tools[b.ID] = b.Name
				c.toolOrder = append(c.toolOrder, b.ID)
				if len(c.toolOrder) > 4096 {
					delete(c.tools, c.toolOrder[0])
					c.toolOrder = c.toolOrder[1:]
				}
				if !strings.HasPrefix(b.Name, "mcp__rhizome__") {
					n.activity(gen, "tool", "Tool started: "+b.Name)
					toolDetail(n, gen, "tool_input", b.Name, payloadText(b.Arguments))
				}
			case "thinking":
			default:
				c.skip(n, "unknown content block")
			}
		}
		if r.Message.StopReason == "stop" {
			n.activity(gen, "turn", "Turn completed")
		}
	case "toolResult":
		name, ok := c.tools[r.Message.ToolCallID]
		if !ok || name == "" || c.peerResponse {
			return
		}
		c.tools[r.Message.ToolCallID] = ""
		if strings.HasPrefix(name, "mcp__rhizome__") {
			return
		}
		result := "completed"
		if r.Message.IsError {
			result = "failed"
		}
		n.activity(gen, "tool", "Tool "+result+": "+name)
		toolDetail(n, gen, "tool_output", name, payloadText(r.Message.Content))
	case "system", "bashExecution", "custom", "branchSummary", "compactionSummary":
	default:
		c.skip(n, "unknown role")
	}
}

// piText joins the text blocks of a Pi content value: a string, or an array of
// text and image blocks.
func piText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
