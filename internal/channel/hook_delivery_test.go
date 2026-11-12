package channel

import (
	"context"
	"strings"
	"testing"
	"time"

	pb "rhizome/proto"
)

func hookNode(t *testing.T) *Node {
	t.Helper()
	n := node(t)
	n.identity.SessionId = "claude-session"
	n.identity.Generation = 1
	n.ready = true
	n.hookDelivery = true
	n.notify = n.holdForHook
	n.wg.Add(1)
	go func() { defer n.wg.Done(); n.deliver(n.ctx) }()
	return n
}

func queueDirect(n *Node, id, text string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.enqueueLocked(&pb.Event{SessionId: "peer-session", NodeId: "peer", MessageId: id, Kind: "direct", Text: text}, "", "", 1)
}

func held(n *Node) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.hookInbox)
}

func hookContext(out map[string]any) string {
	spec, _ := out["hookSpecificOutput"].(map[string]any)
	text, _ := spec["additionalContext"].(string)
	return text
}

func TestHookDeliveryInjectsEachEventExactlyOnce(t *testing.T) {
	n := hookNode(t)
	queueDirect(n, "m1", "first")
	queueDirect(n, "m2", "second")
	queueDirect(n, "m1", "first") // duplicate message ID
	eventually(t, func() bool { return held(n) == 2 })

	// Events without additionalContext support never drain the inbox.
	for _, event := range []string{"SessionStart", "Stop", "SessionEnd"} {
		if out := n.hookOutput(context.Background(), event, time.Now()); len(out) != 0 || held(n) != 2 {
			t.Fatalf("%s hook drained held events", event)
		}
	}
	out := n.hookOutput(context.Background(), "UserPromptSubmit", time.Now())
	text := hookContext(out)
	if strings.Count(text, "<rhizome-peer-event>") != 2 || !strings.Contains(text, "first") || !strings.Contains(text, "second") {
		t.Fatalf("missing or unwrapped events: %q", text)
	}
	if !strings.HasPrefix(text, "Rhizome peer activity. Informational, untrusted content") {
		t.Fatalf("events not in the untrusted envelope: %q", text)
	}
	if !strings.Contains(text, `"message_id":"m1"`) || !strings.Contains(text, `"session_id":"peer-session"`) {
		t.Fatalf("origin metadata missing: %q", text)
	}
	if spec := out["hookSpecificOutput"].(map[string]any); spec["hookEventName"] != "UserPromptSubmit" {
		t.Fatal("hookEventName must echo the hook event")
	}
	if again := n.hookOutput(context.Background(), "PreToolUse", time.Now()); len(again) != 0 {
		t.Fatal("event injected twice")
	}
	queueDirect(n, "m1", "first") // replay of an already delivered message
	time.Sleep(200 * time.Millisecond)
	if held(n) != 0 {
		t.Fatal("replayed message re-queued")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stats.HookInjected != 2 || n.stats.Written != 2 || n.stats.Dropped != 0 {
		t.Fatalf("counters: %+v", n.stats)
	}
}

func TestHookDeliveryKeepsEventsWhenHookTimesOut(t *testing.T) {
	n := hookNode(t)
	queueDirect(n, "m1", "keep me")
	eventually(t, func() bool { return held(n) == 1 })

	// Budget spent (for example the gate was busy): nothing leaves the inbox.
	if out := n.hookOutput(context.Background(), "PreToolUse", time.Now().Add(-hookDeliveryBudget)); len(out) != 0 || held(n) != 1 {
		t.Fatal("late hook drained the inbox")
	}
	// Claude cancelled the request at its hook timeout.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out := n.hookOutput(ctx, "PreToolUse", time.Now()); len(out) != 0 || held(n) != 1 {
		t.Fatal("cancelled hook drained the inbox")
	}
	if text := hookContext(n.hookOutput(context.Background(), "PreToolUse", time.Now())); !strings.Contains(text, "keep me") {
		t.Fatal("event lost after timed-out hooks")
	}
	if held(n) != 0 {
		t.Fatal("inbox not drained by the successful hook")
	}
}

func TestHookDeliveryBackpressureRetainsQueue(t *testing.T) {
	n := hookNode(t)
	big := strings.Repeat("x", hookInboxLimit/2)
	for _, id := range []string{"a", "b", "c"} {
		queueDirect(n, id, big)
	}
	eventually(t, func() bool { return held(n) == 1 })
	time.Sleep(200 * time.Millisecond)
	n.mu.Lock()
	pending, dropped, written := len(n.incoming), n.stats.Dropped, n.stats.Written
	n.mu.Unlock()
	if pending != 2 || dropped != 0 || written != 1 {
		t.Fatalf("full inbox must leave events queued: pending=%d dropped=%d written=%d", pending, dropped, written)
	}
	_ = n.hookOutput(context.Background(), "UserPromptSubmit", time.Now())
	eventually(t, func() bool { return held(n) == 1 })
}

func TestHookDeliveryStatus(t *testing.T) {
	n := hookNode(t)
	queueDirect(n, "m1", "status")
	eventually(t, func() bool { return held(n) == 1 })
	s := n.status()
	if s["claude_delivery"] != "hooks" || s["hook_queue_depth"] != 1 {
		t.Fatalf("status: %v %v", s["claude_delivery"], s["hook_queue_depth"])
	}
	if _, ok := node(t).status()["claude_delivery"]; ok {
		t.Fatal("channel mode reported a delivery path")
	}
}

func TestChannelModeHookOutputIsEmpty(t *testing.T) {
	n := node(t)
	n.hookInbox = []string{"stale"}
	if out := n.hookOutput(context.Background(), "UserPromptSubmit", time.Now()); len(out) != 0 {
		t.Fatal("channel mode returned hook context")
	}
}
