package channel

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// openCodeCollector normalizes plugin events for one bound session. Pi's
// extension relays the same frame shapes, so piSession reuses it; Rhizome's own
// tools are `rhizome_*` in OpenCode and `mcp__rhizome__*` in Pi. The plugin
// forwards raw allowlisted events; every publication decision is made here.
// Callers serialize handle (openCodeNodes holds frameMu).
type openCodeCollector struct {
	privacyRevision string
	privateTurn     bool
	peer            bool // current turn was started only by Rhizome deliveries
	seen            map[string]bool
	order           []string
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
	n.collectionGate.Lock()
	defer n.collectionGate.Unlock()
	policy, err := loadVisibility(n.runtime, n.local().SessionId)
	if err != nil || policy.Visibility != "public" {
		c.privateTurn = true
		c.privacyRevision = "private"
		return
	}
	if c.privacyRevision != policy.Revision {
		c.privateTurn = true
		c.privacyRevision = policy.Revision
	}
	if policy.Revision != "" && (f.EmittedAt <= policy.PublicAfter || f.EmittedAt > time.Now().Add(5*time.Second).UnixMilli()) {
		return
	}
	if c.privateTurn {
		if f.Type != "input" || f.Kind != "user" || f.Rhizome || isPeerInput(f.Text) {
			return
		}
		c.privateTurn = false
	}
	switch f.Type {
	case "input":
		// Peer deliveries mark the turn as a reaction until a human prompt arrives.
		if f.Rhizome || isPeerInput(f.Text) {
			c.peer = true
		} else if f.Kind == "user" {
			c.peer = false
			if strings.TrimSpace(f.Text) != "" && !strings.Contains(f.Text, "<channel ") {
				n.activity(gen, "prompt", f.Text)
			}
		}
	case "text":
		if !n.peerActivity.Allows(c.peer, "activity") || strings.TrimSpace(f.Text) == "" || isPeerInput(f.Text) || strings.Contains(f.Text, "<channel ") {
			return
		}
		sum := sha256.Sum256([]byte(f.Text))
		if !c.once("text:" + f.MessageID + ":" + hex.EncodeToString(sum[:8])) {
			return
		}
		n.activity(gen, "activity", f.Text)
	case "tool":
		name := f.Name
		if !n.peerActivity.Allows(c.peer, "tool") || name == "" || name == "execute" || strings.HasPrefix(name, "rhizome_") || strings.HasPrefix(name, "mcp__rhizome__") {
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
		if n.peerActivity.Allows(c.peer, "turn") {
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
