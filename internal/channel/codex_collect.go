package channel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"
)

type codexItem struct {
	Content   json.RawMessage `json:"content"`
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Status    string          `json:"status"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Namespace string          `json:"namespace"`
	Source    string          `json:"source"`
	ExitCode  *int            `json:"exitCode"`
	// Tool inputs and outputs, published only as detail events.
	Command          string          `json:"command"`
	AggregatedOutput string          `json:"aggregatedOutput"`
	Changes          json.RawMessage `json:"changes"`
	Arguments        json.RawMessage `json:"arguments"`
	Result           json.RawMessage `json:"result"`
	Error            json.RawMessage `json:"error"`
	ContentItems     json.RawMessage `json:"contentItems"`
	Query            string          `json:"query"`
	RevisedPrompt    string          `json:"revisedPrompt"`
}

// size approximates the item's decoded footprint for the snapshot budget.
func (i codexItem) size() int {
	return len(i.Content) + len(i.Text) + len(i.ID) + len(i.Tool) + len(i.Server) + len(i.Namespace) + len(i.Source) + len(i.Type) + len(i.Status) +
		len(i.Command) + len(i.AggregatedOutput) + len(i.Changes) + len(i.Arguments) + len(i.Result) + len(i.Error) + len(i.ContentItems) + len(i.Query) + len(i.RevisedPrompt) + 128
}

// userText joins a userMessage item's text. Peer-queue deliveries carry the
// envelope; such an item is never a human prompt.
func (i codexItem) userText() string {
	text := i.Text
	var content []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(i.Content, &content)
	for _, part := range content {
		text += part.Text
	}
	return text
}

// toolPayloads renders a tool item's input and, once it has finished, its
// output. Image generation publishes only its revised prompt.
func (i codexItem) toolPayloads() (input, output string) {
	switch i.Type {
	case "commandExecution":
		return i.Command, i.AggregatedOutput
	case "fileChange":
		var changes []struct {
			Path string `json:"path"`
			Kind any    `json:"kind"`
			Diff string `json:"diff"`
		}
		_ = json.Unmarshal(i.Changes, &changes)
		var parts []string
		for _, c := range changes {
			parts = append(parts, c.Path+"\n"+c.Diff)
		}
		return strings.Join(parts, "\n"), ""
	case "mcpToolCall":
		var result struct {
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(i.Result, &result)
		output = payloadText(result.Content)
		if output == "" {
			var failure struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(i.Error, &failure)
			output = failure.Message
		}
		return payloadText(i.Arguments), output
	case "dynamicToolCall":
		return payloadText(i.Arguments), payloadText(i.ContentItems)
	case "webSearch":
		return i.Query, ""
	case "imageGeneration":
		return i.RevisedPrompt, ""
	}
	return "", ""
}

type codexTurn struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}
type turnPage struct {
	Data []codexTurn `json:"data"`
	Next *string     `json:"nextCursor"`
}
type itemEntry struct {
	TurnID string    `json:"turnId"`
	Item   codexItem `json:"item"`
}
type itemPage struct {
	Data []itemEntry `json:"data"`
	Next *string     `json:"nextCursor"`
}
type itemState struct {
	size   int
	hash   [32]byte
	status string
}
type codexCollector struct {
	thread   string
	reader   codexReader
	wake     chan struct{}
	baseline bool
	lastTurn string
	items    map[string]itemState
	turns    map[string]string
	order    []string
}

func newCodexCollector(thread string, reader codexReader) *codexCollector {
	return &codexCollector{thread: thread, reader: reader, wake: make(chan struct{}, 1), items: map[string]itemState{}, turns: map[string]string{}}
}
func (c *codexCollector) run(ctx context.Context, n *Node, gen uint64) {
	defer c.reader.close()
	delay := time.Second
	for {
		wait := delay
		if delay > time.Second {
			wait += time.Duration(rand.IntN(250)) * time.Millisecond
		}
		timer := time.NewTimer(wait)
		if delay > time.Second {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		} else {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			case <-c.wake:
				timer.Stop()
			}
		}
		if c.scan(ctx, n, gen) {
			delay = time.Second
		} else {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}
func (c *codexCollector) state(n *Node, state string, ready bool) {
	n.mu.Lock()
	changed := n.collectionState != state
	n.collectionState = state
	n.deliveryReady = ready
	n.mu.Unlock()
	if changed {
		slog.Info("codex_collection", "session_id", c.thread, "state", state, "delivery_ready", ready)
	}
}
func (c *codexCollector) noticeGap(n *Node, gen uint64, text string) {
	n.mu.Lock()
	n.stats.CollectionGaps++
	n.mu.Unlock()
	n.activity(gen, "gap", text)
}
func (c *codexCollector) gap(n *Node, gen uint64, reason string) {
	c.noticeGap(n, gen, "Codex activity discontinuity: "+reason+"; live baseline reset")
	c.baseline = false
	c.lastTurn = ""
	c.items = map[string]itemState{}
	c.turns = map[string]string{}
	c.order = nil
}
func (c *codexCollector) scan(ctx context.Context, n *Node, gen uint64) bool {
	var read struct {
		Thread struct {
			ID     string  `json:"id"`
			Parent *string `json:"parentThreadId"`
			Name   *string `json:"name"`
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
		} `json:"thread"`
	}
	if err := c.reader.call(ctx, "thread/read", map[string]any{"threadId": c.thread, "includeTurns": false}, &read); err != nil {
		c.state(n, "unavailable: "+err.Error(), false)
		return false
	}
	if read.Thread.ID != c.thread || read.Thread.Parent != nil {
		c.state(n, "unsupported child or mismatched thread", false)
		return false
	}
	// A verified read is the only native title source: null is a confirmed
	// removal, while the failures above leave the last known title in place.
	var title string
	if read.Thread.Name != nil {
		title = *read.Thread.Name
	}
	n.setNativeTitle(gen, true, title)
	status := read.Thread.Status.Type
	if status != "idle" && status != "active" {
		c.state(n, status, false)
		return false
	}
	// Walk newest turns only until the previously observed boundary. Page and
	// item caps limit work; exceeding a cap is visible, never silent history loss.
	var turns []codexTurn
	var cursor *string
	found := c.lastTurn == ""
	for page := 0; page < 4; page++ {
		var p turnPage
		err := c.reader.call(ctx, "thread/turns/list", map[string]any{"threadId": c.thread, "limit": 50, "itemsView": "notLoaded", "sortDirection": "desc", "cursor": cursor}, &p)
		if err != nil {
			var rpcErr *appRPCError
			if errors.As(err, &rpcErr) && rpcErr.code == -32600 && strings.Contains(rpcErr.message, "not materialized yet") && c.lastTurn == "" {
				c.baseline = true
				c.state(n, "watching (empty thread)", true)
				return true
			}
			c.state(n, "history unavailable: "+err.Error(), false)
			return false
		}
		for _, t := range p.Data {
			turns = append(turns, t)
			if t.ID == c.lastTurn {
				found = true
				break
			}
			if !c.baseline {
				found = true
				break
			}
		}
		if found || p.Next == nil {
			break
		}
		cursor = p.Next
	}
	if !found {
		c.gap(n, gen, "turn cursor disappeared or fell beyond pagination limit")
		if len(turns) > 1 {
			turns = turns[:1]
		}
	}
	if len(turns) == 0 && c.lastTurn != "" {
		c.gap(n, gen, "history rolled back")
	}
	// Fetch a complete bounded snapshot before advancing any cursor or publishing.
	entries := make(map[string][]codexItem)
	totalItems, totalBytes := 0, 0
	for _, t := range turns {
		cursor = nil
		complete := false
		for page := 0; page < 4; page++ {
			var p itemPage
			err := c.reader.call(ctx, "thread/items/list", map[string]any{"threadId": c.thread, "turnId": t.ID, "limit": 128, "sortDirection": "asc", "cursor": cursor}, &p)
			if err != nil {
				c.state(n, "items unavailable: "+err.Error(), false)
				return false
			}
			for _, e := range p.Data {
				totalItems++
				totalBytes += e.Item.size()
				if totalItems > 2048 || totalBytes > 8*1024*1024 {
					c.gap(n, gen, "snapshot limit exceeded")
					c.state(n, "snapshot limit exceeded", false)
					return false
				}
				if e.TurnID != t.ID {
					c.state(n, "mismatched item turn", false)
					return false
				}
				entries[t.ID] = append(entries[t.ID], e.Item)
			}
			if p.Next == nil {
				complete = true
				break
			}
			cursor = p.Next
		}
		if !complete {
			c.gap(n, gen, "item pagination limit exceeded")
			c.state(n, "item limit exceeded; awaiting next turn", false)
			return false
		}
	}
	if c.baseline && c.lastTurn != "" {
		visible := map[string]bool{}
		for _, item := range entries[c.lastTurn] {
			visible[c.lastTurn+":"+item.ID] = true
		}
		for key := range c.items {
			if strings.HasPrefix(key, c.lastTurn+":") && !visible[key] {
				c.gap(n, gen, "visible items rolled back")
				break
			}
		}
	}
	for i := len(turns) - 1; i >= 0; i-- {
		t := turns[i]
		// A peer-only turn is a reaction, not a new source of automatic activity.
		peer, human := false, false
		for _, item := range entries[t.ID] {
			if item.Type == "userMessage" {
				text := item.userText()
				if isPeerInput(text) {
					peer = true
				} else if strings.TrimSpace(text) != "" {
					human = true
				}
			}
		}
		emit := c.baseline && !(peer && !human)
		for _, item := range entries[t.ID] {
			c.normalize(n, gen, t.ID, item, emit)
		}
		previous, seen := c.turns[t.ID]
		if emit && t.Status != "inProgress" && t.Status != "" && (!seen || previous != t.Status) {
			n.activity(gen, "turn", "Turn "+t.Status)
		}
		c.turns[t.ID] = t.Status
	}
	if len(turns) > 0 {
		c.lastTurn = turns[0].ID
	}
	// Only the most recent turn can still change after this snapshot.
	for key := range c.turns {
		if key != c.lastTurn {
			delete(c.turns, key)
		}
	}
	c.baseline = true
	c.state(n, "watching", true)
	return true
}
func (c *codexCollector) normalize(n *Node, gen uint64, turn string, item codexItem, emit bool) {
	if item.ID == "" {
		n.mu.Lock()
		n.stats.ParserSkips++
		n.mu.Unlock()
		return
	}
	key := turn + ":" + item.ID
	prev, seen := c.items[key]
	state := itemState{size: len(item.Text), hash: sha256.Sum256([]byte(item.Text)), status: item.Status}
	if !seen {
		c.order = append(c.order, key)
	}
	c.items[key] = state
	if len(c.order) > 2048 {
		delete(c.items, c.order[0])
		c.order = c.order[1:]
	}
	if !emit {
		return
	}
	if seen && prev == state {
		return
	}
	switch item.Type {
	case "agentMessage":
		text := item.Text
		if seen {
			if len(text) < prev.size || sha256.Sum256([]byte(text[:prev.size])) != prev.hash {
				c.noticeGap(n, gen, "Codex assistant item was replaced; skipping revised history")
				return
			}
			text = text[prev.size:]
		}
		if strings.TrimSpace(text) != "" && !strings.Contains(text, "<rhizome-peer-event>") {
			n.activity(gen, "activity", text)
		}
	case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall", "webSearch", "imageGeneration":
		if item.Server == "rhizome" || strings.Contains(item.Tool, "mcp__rhizome__") || item.Namespace == "rhizome" || (item.Type == "commandExecution" && item.Source != "" && item.Source != "agent") {
			return
		}
		label := item.Type
		if item.Type == "mcpToolCall" {
			label = item.Server + "/" + item.Tool
		}
		if item.Type == "dynamicToolCall" {
			label = item.Namespace + "/" + item.Tool
		}
		status := item.Status
		if status == "" {
			status = "observed"
		}
		if item.ExitCode != nil {
			status += fmt.Sprintf(" (exit %d)", *item.ExitCode)
		}
		n.activity(gen, "tool", label+": "+status)
		input, output := item.toolPayloads()
		if !seen {
			toolDetail(n, gen, "tool_input", label, input)
		}
		if item.Status != "" && item.Status != "inProgress" && (!seen || prev.status != item.Status) {
			toolDetail(n, gen, "tool_output", label, output)
		}
	case "userMessage":
		// A human prompt, once; peer-queue deliveries carry the envelope.
		if text := item.userText(); !seen && strings.TrimSpace(text) != "" && !isPeerInput(text) {
			n.activity(gen, "prompt", text)
		}
	case "contextCompaction":
		if !seen {
			c.noticeGap(n, gen, "Codex context compacted; collection continues from visible item IDs")
		}
	case "reasoning", "hookPrompt", "functionCallOutput", "plan", "collabAgentToolCall", "subAgentActivity", "imageView", "sleep", "enteredReviewMode", "exitedReviewMode":
	default:
		n.mu.Lock()
		n.stats.ParserSkips++
		n.mu.Unlock()
		slog.Debug("codex_parser_skip", "type", item.Type)
	}
}
