package channel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

const maxRecord = 2 * 1024 * 1024

// Only this collector's goroutine accesses offset, tail and normalization state.
type collector struct {
	path         string
	offset       int64
	info         os.FileInfo
	tail         []byte
	dropping     bool
	wake         chan struct{}
	seen         map[string]bool
	order        []string
	tools        map[string]string
	toolOrder    []string
	anchor       []byte
	peerResponse bool
	baseline     int64 // bytes present at binding; scanned once for title records only
	titled       bool
}

func newCollector(path string) (*collector, error) {
	c := &collector{path: path, wake: make(chan struct{}, 1), seen: map[string]bool{}, tools: map[string]string{}}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	c.info = st
	c.offset = st.Size()
	c.baseline = c.offset
	if c.offset > 0 {
		b := []byte{0}
		_, _ = f.ReadAt(b, c.offset-1)
		c.dropping = b[0] != '\n'
		c.setAnchor(f)
	}
	return c, nil
}
func (c *collector) setAnchor(f *os.File) {
	size := int64(128)
	if c.offset < size {
		size = c.offset
	}
	c.anchor = make([]byte, size)
	_, _ = f.ReadAt(c.anchor, c.offset-size)
}
func (c *collector) run(ctx context.Context, n *Node, gen uint64) {
	watcher, err := fsnotify.NewWatcher()
	var events <-chan fsnotify.Event
	var failures <-chan error
	if err == nil {
		defer watcher.Close()
		if err = watcher.Add(filepath.Dir(c.path)); err == nil {
			events = watcher.Events
			failures = watcher.Errors
		}
	}
	if err != nil {
		slog.Warn("transcript_watch_polling", "error", err)
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-c.wake:
		case _, ok := <-events:
			if !ok {
				events = nil
			}
		case err, ok := <-failures:
			if !ok {
				failures = nil
			} else {
				slog.Warn("transcript_watch", "error", err)
			}
		}
		c.scan(n, gen)
	}
}
func (c *collector) scan(n *Node, gen uint64) {
	f, err := os.Open(c.path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("transcript_open", "error", err)
		}
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	changed := c.info != nil && (!os.SameFile(c.info, st) || st.Size() < c.offset)
	if !changed && len(c.anchor) > 0 {
		b := make([]byte, len(c.anchor))
		_, err = f.ReadAt(b, c.offset-int64(len(b)))
		changed = err != nil || !bytes.Equal(b, c.anchor)
	}
	if changed {
		c.offset = 0
		c.peerResponse = false
		c.tail = nil
		c.dropping = false
		c.anchor = nil
		n.activity(gen, "gap", "Transcript replaced or truncated; collection resumed from new contents")
	}
	c.info = st
	if !c.titled {
		c.titled = true
		if !changed && c.baseline > 0 {
			c.scanTitles(f, n, gen)
		}
	}
	if _, err = f.Seek(c.offset, io.SeekStart); err != nil {
		return
	}
	// Bound work per scan, including pathological lines; polling resumes the rest.
	buf := make([]byte, 64*1024)
	remaining := 8 * 1024 * 1024
	for remaining > 0 {
		size, err := f.Read(buf)
		if size > 0 {
			c.offset += int64(size)
			remaining -= size
			c.consume(buf[:size], n, gen)
		}
		if err != nil {
			break
		}
	}
	c.setAnchor(f)
}
func (c *collector) consume(b []byte, n *Node, gen uint64) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		var piece []byte
		if i < 0 {
			piece = b
			b = nil
		} else {
			piece = b[:i]
			b = b[i+1:]
		}
		if !c.dropping {
			if len(c.tail)+len(piece) > maxRecord {
				c.tail = nil
				c.dropping = true
				c.skip(n, "oversized record")
			} else {
				c.tail = append(c.tail, piece...)
			}
		}
		if i >= 0 {
			if !c.dropping && len(c.tail) > 0 {
				c.normalize(c.tail, n, gen)
			}
			c.tail = nil
			c.dropping = false
		}
	}
}

// scanTitles reads the pre-binding transcript once, looking only at title
// records so a session named before Rhizome bound still exposes its native
// title. No activity is published and no conversation content is retained.
func (c *collector) scanTitles(f *os.File, n *Node, gen uint64) {
	const maxTitleScan = 64 * 1024 * 1024
	limit := c.baseline
	if limit > maxTitleScan {
		limit = maxTitleScan
	}
	r := bufio.NewReaderSize(io.NewSectionReader(f, 0, limit), 64*1024)
	session := n.local().SessionId
	var custom, auto *string
	for {
		line, err := r.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// Oversized line: discard the rest; title records are tiny.
			for err == bufio.ErrBufferFull {
				_, err = r.ReadSlice('\n')
			}
			if err != nil {
				break
			}
			continue
		}
		if kind, title, ok := titleRecord(line, session); ok {
			if kind == "custom-title" {
				custom = &title
			} else {
				auto = &title
			}
		}
		if err != nil {
			break
		}
	}
	if auto != nil {
		n.setNativeTitle(gen, false, *auto)
	}
	if custom != nil {
		n.setNativeTitle(gen, true, *custom)
	}
}

// titleRecord decodes a Claude title record. Claude Code writes
// {"type":"ai-title","aiTitle":...} when it names a session automatically and
// {"type":"custom-title","customTitle":...} when the user renames it; both
// carry sessionId. Records for another session are ignored.
func titleRecord(line []byte, session string) (kind, title string, ok bool) {
	if !bytes.Contains(line, []byte(`-title"`)) {
		return "", "", false
	}
	var r struct {
		Type        string `json:"type"`
		SessionID   string `json:"sessionId"`
		AITitle     string `json:"aiTitle"`
		CustomTitle string `json:"customTitle"`
	}
	if json.Unmarshal(line, &r) != nil || (r.SessionID != "" && r.SessionID != session) {
		return "", "", false
	}
	switch r.Type {
	case "ai-title":
		return r.Type, r.AITitle, true
	case "custom-title":
		return r.Type, r.CustomTitle, true
	}
	return "", "", false
}
func (c *collector) skip(n *Node, why string) {
	n.mu.Lock()
	n.stats.ParserSkips++
	n.mu.Unlock()
	slog.Debug("parser_skip", "reason", why)
}

type block struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	ToolUseID string `json:"tool_use_id"`
	IsError   bool   `json:"is_error"`
}

func (c *collector) normalize(line []byte, n *Node, gen uint64) {
	var r struct {
		Type      string `json:"type"`
		UUID      string `json:"uuid"`
		SessionID string `json:"sessionId"`
		Sidechain bool   `json:"isSidechain"`
		IsMeta    bool   `json:"isMeta"`
		Message   struct {
			ID         string          `json:"id"`
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			StopReason string          `json:"stop_reason"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &r); err != nil {
		c.skip(n, "invalid JSON")
		return
	}
	if r.Sidechain {
		return
	}
	local := n.local()
	if local.Generation != gen || (r.SessionID != "" && r.SessionID != local.SessionId) {
		return
	}
	if r.Type != "assistant" && r.Type != "user" {
		switch r.Type {
		case "ai-title", "custom-title":
			if kind, title, ok := titleRecord(line, local.SessionId); ok {
				n.setNativeTitle(gen, kind == "custom-title", title)
			}
		case "progress", "system", "file-history-snapshot", "attachment", "api-request-blob", "api-request", "api-request-shape", "last-prompt", "mode", "permission-mode", "atis-latch", "bridge-session":
		default:
			c.skip(n, "unknown type")
		}
		return
	}
	key := r.UUID
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
	var blocks []block
	if json.Unmarshal(r.Message.Content, &blocks) != nil {
		var text string
		if json.Unmarshal(r.Message.Content, &text) == nil {
			blocks = []block{{Type: "text", Text: text}}
		} else {
			c.skip(n, "unknown content")
			return
		}
	}
	// Meta channel records must be inspected before ignoring them: their
	// assistant responses otherwise look like fresh locally authored activity.
	if r.Type == "user" {
		for _, b := range blocks {
			if b.Type != "text" || strings.TrimSpace(b.Text) == "" {
				continue
			}
			if isPeerInput(b.Text) {
				c.peerResponse = true
			} else if !r.IsMeta {
				c.peerResponse = false
			}
		}
	}
	if r.IsMeta || (r.Type == "assistant" && c.peerResponse) {
		return
	}
	// Channel injection is a user record. Never publish user text, even if it resembles assistant output.
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if r.Type == "assistant" && strings.TrimSpace(b.Text) != "" && !strings.Contains(b.Text, "<channel ") {
				n.activity(gen, "activity", b.Text)
			}
		case "tool_use":
			if r.Type != "assistant" {
				continue
			}
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
			}
		case "tool_result":
			if r.Type != "user" {
				continue
			}
			name, ok := c.tools[b.ToolUseID]
			if !ok || name == "" {
				continue
			}
			c.tools[b.ToolUseID] = ""
			if strings.HasPrefix(name, "mcp__rhizome__") {
				continue
			}
			result := "completed"
			if b.IsError {
				result = "failed"
			}
			n.activity(gen, "tool", "Tool "+result+": "+name)
		case "thinking", "redacted_thinking":
		default:
			c.skip(n, "unknown content block")
		}
	}
	if r.Type == "assistant" && r.Message.StopReason == "end_turn" {
		n.activity(gen, "turn", "Turn completed")
	}
}

// Match transport envelopes, not natural-language mentions of Rhizome.
func isPeerInput(text string) bool {
	return strings.Contains(text, "<rhizome-peer-event>") ||
		(strings.Contains(text, "<channel ") &&
			(strings.Contains(text, `source="rhizome"`) || strings.Contains(text, `source="mcp__rhizome"`)))
}
