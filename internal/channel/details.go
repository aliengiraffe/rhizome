package channel

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Detail events (prompt, tool_input, tool_output) are produced by every
// normalizer but published only with [sessions].share_prompts_and_tools; the
// gate, size bound and secret filter live in the sinks (Node.activity and
// historySink.activity), never here.

// payloadText renders a tool input or output: a JSON string as itself, an
// array as the joined text of its strings and text blocks (images and other
// binary blocks are dropped), and any other value as compact JSON. Empty and
// null payloads yield "".
func payloadText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return ""
		}
		return s
	case '[':
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return ""
		}
		var parts []string
		for _, item := range items {
			item = bytes.TrimSpace(item)
			if len(item) > 0 && item[0] == '"' {
				var s string
				if json.Unmarshal(item, &s) == nil && strings.TrimSpace(s) != "" {
					parts = append(parts, s)
				}
				continue
			}
			var block struct {
				Text *string `json:"text"`
			}
			if json.Unmarshal(item, &block) == nil && block.Text != nil && strings.TrimSpace(*block.Text) != "" {
				parts = append(parts, *block.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil || b.String() == "{}" {
		return ""
	}
	return b.String()
}

// toolDetail prefixes a rendered payload with its tool label; an empty payload
// produces no event.
func toolDetail(n activitySink, gen uint64, kind, label, payload string) {
	if strings.TrimSpace(payload) == "" {
		return
	}
	n.activity(gen, kind, label+": "+payload)
}
