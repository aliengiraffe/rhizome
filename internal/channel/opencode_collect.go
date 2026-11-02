package channel

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// openCodeCollector normalizes plugin events for one bound session. The plugin
// forwards raw allowlisted events; every publication decision is made here.
// Callers serialize handle (openCodeNodes holds frameMu).
type openCodeCollector struct {
	peer  bool // current turn was started only by Rhizome deliveries
	seen  map[string]bool
	order []string
}

func newOpenCodeCollector() *openCodeCollector {
	return &openCodeCollector{seen: map[string]bool{}}
}

// once reports whether key is new within the bounded 4,096-entry window.
func (c *openCodeCollector) once(key string) bool {
	if c.seen[key] {
		return false
	}
	c.seen[key] = true
	c.order = append(c.order, key)
	if len(c.order) > 4096 {
		delete(c.seen, c.order[0])
		c.order = c.order[1:]
	}
	return true
}

func (c *openCodeCollector) handle(n *Node, gen uint64, f bridgeFrame) {
	switch f.Type {
	case "input":
		// Peer deliveries mark the turn as a reaction until a human prompt arrives.
		if f.Rhizome {
			c.peer = true
		} else if f.Kind == "user" {
			c.peer = false
			if strings.TrimSpace(f.Text) != "" && !strings.Contains(f.Text, "<rhizome-peer-event>") {
				n.activity(gen, "prompt", f.Text)
			}
		}
	case "text":
		if c.peer || strings.TrimSpace(f.Text) == "" || strings.Contains(f.Text, "<rhizome-peer-event>") {
			return
		}
		sum := sha256.Sum256([]byte(f.Text))
		if !c.once("text:" + f.MessageID + ":" + hex.EncodeToString(sum[:8])) {
			return
		}
		n.activity(gen, "activity", f.Text)
	case "tool":
		name := f.Name
		if c.peer || name == "" || name == "execute" || strings.HasPrefix(name, "rhizome_") {
			return
		}
		switch f.Phase {
		case "start":
			if c.once("tool:" + f.CallID + ":" + name + ":start") {
				n.activity(gen, "tool", "Tool started: "+name)
				toolDetail(n, gen, "tool_input", name, f.Text)
			}
		case "completed", "failed":
			if c.once("tool:" + f.CallID + ":" + name + ":end") {
				n.activity(gen, "tool", "Tool "+f.Phase+": "+name)
				toolDetail(n, gen, "tool_output", name, f.Text)
			}
		}
	case "turn":
		if !c.peer {
			n.activity(gen, "turn", "Turn completed")
		}
	}
}

// openCodeGap tells subscribers that OpenCode activity may have been missed.
func openCodeGap(n *Node, reason string) {
	n.mu.Lock()
	n.stats.CollectionGaps++
	gen := n.identity.Generation
	n.mu.Unlock()
	n.activity(gen, "gap", "OpenCode activity discontinuity: "+reason)
}
