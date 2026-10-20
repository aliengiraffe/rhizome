package channel

import (
	"encoding/json"
	pb "rhizome/proto"
	"testing"
)

func TestClaudePeerResponseSuppressedUntilHumanPrompt(t *testing.T) {
	n := node(t)
	n.identity.SessionId, n.identity.Generation = "local", 1
	c, err := newCollector(t.TempDir() + "/transcript")
	if err != nil {
		t.Fatal(err)
	}
	records := []string{
		`{"type":"user","isMeta":true,"message":{"content":"<channel source=\"rhizome\">[turn] Turn completed</channel>"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Still waiting"},{"type":"tool_use","id":"echo","name":"Bash"}],"stop_reason":"end_turn"}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"echo"}]}}`,
	}
	for _, r := range records {
		c.normalize([]byte(r), n, 1)
	}
	if len(n.history) != 0 {
		t.Fatalf("peer response leaked: %v", n.history)
	}
	c.normalize([]byte(`{"type":"user","message":{"content":"Please continue"}}`), n, 1)
	c.normalize([]byte(`{"type":"assistant","message":{"content":"Real work","stop_reason":"end_turn"}}`), n, 1)
	if len(n.history) != 2 || n.history[0].Text != "Real work" {
		t.Fatalf("human work suppressed: %v", n.history)
	}
}

func TestCodexPeerOnlyTurnSuppressed(t *testing.T) {
	for _, structured := range []bool{false, true} {
		n := node(t)
		n.identity.SessionId, n.identity.Generation = threadA, 1
		f := &fakeCodex{thread: threadA, status: "idle", items: map[string][]codexItem{}}
		c := newCodexCollector(threadA, f)
		if !c.scan(n.ctx, n, 1) {
			t.Fatal("baseline failed")
		}
		input := codexItem{ID: "u", Type: "userMessage", Text: "<rhizome-peer-event>activity</rhizome-peer-event>"}
		if structured {
			input.Text = ""
			input.Content = json.RawMessage(`[{"type":"text","text":"<rhizome-peer-event>activity</rhizome-peer-event>"}]`)
		}
		f.turns = []codexTurn{{"peer", "completed"}}
		f.items["peer"] = []codexItem{input, {ID: "a", Type: "agentMessage", Text: "Still waiting"}, {ID: "tool", Type: "commandExecution", Status: "completed"}}
		if !c.scan(n.ctx, n, 1) || len(n.history) != 0 {
			t.Fatalf("peer response leaked: %v", n.history)
		}
		f.turns = []codexTurn{{"human", "completed"}, {"peer", "completed"}}
		f.items["human"] = []codexItem{{ID: "u2", Type: "userMessage", Text: "Continue"}, {ID: "a2", Type: "agentMessage", Text: "Real work"}}
		if !c.scan(n.ctx, n, 1) || len(n.history) != 2 || n.history[0].Text != "Real work" {
			t.Fatalf("human work suppressed: %v", n.history)
		}
	}
}

func TestTurnMarkersCannotWakeSubscribers(t *testing.T) {
	n := node(t)
	n.identity.SessionId, n.identity.Generation = "local", 1
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.enqueueLocked(&pb.Event{NodeId: "peer", Kind: "turn", MessageId: "turn"}, "", "", 1) || len(n.incoming) != 0 {
		t.Fatal("turn marker queued")
	}
	if !n.enqueueLocked(&pb.Event{NodeId: "peer", Kind: "activity", MessageId: "work", Text: "Real work"}, "", "", 1) {
		t.Fatal("activity rejected")
	}
	if !n.enqueueLocked(&pb.Event{NodeId: "peer", Kind: "announcement", MessageId: "explicit", Text: "Intentional response"}, "", "", 1) {
		t.Fatal("explicit publish rejected")
	}
}

// A followed turn marker is consumed, not replayed: activity after it still arrives
// on the same stream.
func TestTurnMarkerDoesNotStallSubscription(t *testing.T) {
	a, b := pair(t)
	if err := b.subscribe("session-a"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.feeds["session-a"].status == "following" })
	gen := a.local().Generation
	a.activity(gen, "turn", "Turn completed")
	a.activity(gen, "activity", "after turn")
	eventually(t, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.incoming) == 1 && b.incoming[0].event.Text == "after turn"
	})
	b.mu.Lock()
	defer b.mu.Unlock()
	if f := b.feeds["session-a"]; f.reconnects != 0 || f.status != "following" {
		t.Fatalf("turn marker broke the stream: reconnects=%d status=%s", f.reconnects, f.status)
	}
}
