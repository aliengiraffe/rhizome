package channel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"rhizome/internal/config"
)

var peerPolicies = []config.PeerActivityPolicy{"", config.PeerActivityNone, config.PeerActivityResponses, config.PeerActivityAll}

// Each format supplies the same peer-only turn: reply, ordinary tool round
// trip, excluded reasoning/transport/tool content, and completion.
func peerRecords(agent string) []string {
	if agent == "claude" {
		return []string{
			`{"type":"user","uuid":"u","isMeta":true,"message":{"content":"<channel source=\"rhizome\">input</channel>"}}`,
			`{"type":"assistant","uuid":"a","message":{"content":[{"type":"text","text":"reply"},{"type":"text","text":"<rhizome-peer-event>excluded</rhizome-peer-event>"},{"type":"thinking","thinking":"excluded"},{"type":"tool_use","id":"t","name":"Bash","input":"input"},{"type":"tool_use","id":"r","name":"mcp__rhizome__send","input":"excluded"}]}}`,
			`{"type":"user","uuid":"r","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"output"},{"type":"tool_result","tool_use_id":"r","content":"excluded"}]}}`,
			`{"type":"assistant","uuid":"end","message":{"content":[],"stop_reason":"end_turn"}}`,
		}
	}
	return []string{
		`{"type":"response_item","payload":{"type":"message","id":"u","role":"user","content":[{"type":"input_text","text":"<rhizome-peer-event>input</rhizome-peer-event>"}]}}`,
		`{"type":"response_item","payload":{"type":"message","id":"a","role":"assistant","content":[{"type":"output_text","text":"reply"},{"type":"output_text","text":"<rhizome-peer-event>excluded</rhizome-peer-event>"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call","id":"t","call_id":"t","name":"shell","arguments":"input"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","id":"o","call_id":"t","output":"output"}}`,
		`{"type":"response_item","payload":{"type":"function_call","id":"r","call_id":"r","name":"mcp__rhizome__send","arguments":"excluded"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","id":"ro","call_id":"r","output":"excluded"}}`,
		`{"type":"response_item","payload":{"type":"reasoning","id":"reason","content":"excluded"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
	}
}

func newPeerCollector() *collector {
	return &collector{seen: map[string]bool{}, tools: map[string]string{}}
}
func normalizePeerRecord(c *collector, agent, record string, sink activitySink) {
	if agent == "claude" {
		c.normalize([]byte(record), sink, 1)
	} else {
		c.normalizeCodexLog([]byte(record), sink, 1)
	}
}

func TestPeerActivityLogPolicy(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		for _, policy := range peerPolicies {
			for _, details := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/details=%t", agent, policy, details), func(t *testing.T) {
					n := detailNode(t, details)
					n.peerActivity = policy
					sink := &historySink{identity: n.local(), details: details, peerActivity: policy}
					live, historical := newPeerCollector(), newPeerCollector()
					records := peerRecords(agent)
					for _, record := range records {
						normalizePeerRecord(live, agent, record, n)
						normalizePeerRecord(historical, agent, record, sink)
						// Repeated identified inputs/output cannot emit twice or reset state.
						if !strings.Contains(record, "task_complete") {
							normalizePeerRecord(live, agent, record, n)
						}
					}
					want := 0
					if policy == config.PeerActivityResponses {
						want = 1
					}
					if policy == config.PeerActivityAll {
						want = 4
						if details {
							want = 6
						}
					}
					if len(n.history) != want || len(sink.events) != want {
						t.Fatalf("live=%v history=%v want %d", published(n), sink.events, want)
					}
					if want > 0 && (n.history[0].Text != "reply" || *sink.events[0].Text != "reply") {
						t.Fatal("missing reply")
					}
					for _, event := range n.history {
						if strings.Contains(event.Text, "excluded") {
							t.Fatal("excluded content emitted")
						}
					}
					// Human input resumes ordinary activity in every policy.
					human := `{"type":"user","uuid":"human","message":{"content":"continue"}}`
					reply := `{"type":"assistant","uuid":"human-reply","message":{"content":"human reply"}}`
					if agent == "codex" {
						human = `{"type":"response_item","payload":{"type":"message","id":"human","role":"user","content":[{"type":"input_text","text":"continue"}]}}`
						reply = `{"type":"response_item","payload":{"type":"message","id":"human-reply","role":"assistant","content":[{"type":"output_text","text":"human reply"}]}}`
					}
					normalizePeerRecord(live, agent, human, n)
					normalizePeerRecord(live, agent, reply, n)
					if got := activityTexts(n); got[len(got)-1] != "human reply" {
						t.Fatal(got)
					}
				})
			}
		}
	}
}

func TestPeerActivityCodexDirect(t *testing.T) {
	for _, policy := range peerPolicies {
		for _, mixed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/mixed=%t", policy, mixed), func(t *testing.T) {
				n := detailNode(t, true)
				n.peerActivity = policy
				f := &fakeCodex{thread: "s", status: "idle", items: map[string][]codexItem{}}
				c := newCodexCollector("s", f)
				if !c.scan(n.ctx, n, 1) {
					t.Fatal("baseline")
				}
				f.turns = []codexTurn{{"peer", "completed"}}
				f.items["peer"] = []codexItem{
					{ID: "u", Type: "userMessage", Text: "<rhizome-peer-event>input</rhizome-peer-event>"},
					{ID: "a", Type: "agentMessage", Text: "reply"},
					{ID: "injection", Type: "agentMessage", Text: `<channel source="rhizome">excluded</channel>`},
					{ID: "envelope", Type: "agentMessage", Text: "<rhizome-peer-event>excluded</rhizome-peer-event>"},
					{ID: "t", Type: "commandExecution", Status: "completed", Command: "input", AggregatedOutput: "output"},
					{ID: "r", Type: "mcpToolCall", Server: "rhizome", Tool: "send", Status: "completed"},
					{ID: "reason", Type: "reasoning", Text: "excluded"},
				}
				if mixed {
					f.items["peer"] = append(f.items["peer"], codexItem{ID: "human", Type: "userMessage", Text: "continue"})
				}
				for range 2 {
					if !c.scan(n.ctx, n, 1) {
						t.Fatal("scan")
					}
				}
				want := 0
				if policy == config.PeerActivityResponses {
					want = 1
				}
				if policy == config.PeerActivityAll {
					want = 5
				}
				if mixed {
					want = 6
				}
				if len(n.history) != want {
					t.Fatalf("got %v want %d", published(n), want)
				}
				// A populated initial snapshot establishes cursors, never replays output.
				baseline := newCodexCollector("s", f)
				before := len(n.history)
				for range 2 {
					if !baseline.scan(n.ctx, n, 1) {
						t.Fatal("baseline scan")
					}
				}
				if len(n.history) != before {
					t.Fatal("baseline replay")
				}
			})
		}
	}
}

func TestPeerActivityOpenCode(t *testing.T) {
	for _, policy := range peerPolicies {
		for _, details := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/details=%t", policy, details), func(t *testing.T) {
				n := detailNode(t, details)
				n.peerActivity = policy
				c := newOpenCodeCollector()
				frames := []bridgeFrame{
					{Type: "input", Kind: "synthetic", Rhizome: true, Text: "<rhizome-peer-event>input</rhizome-peer-event>"},
					{Type: "text", MessageID: "a", Text: "reply"},
					{Type: "text", MessageID: "channel", Text: `<channel source="rhizome">excluded</channel>`},
					{Type: "tool", CallID: "t", Name: "bash", Phase: "start", Text: "input"},
					{Type: "tool", CallID: "t", Name: "bash", Phase: "completed", Text: "output"},
					{Type: "tool", CallID: "r", Name: "rhizome_send", Phase: "start", Text: "excluded"},
					{Type: "tool", CallID: "e", Name: "execute", Phase: "start", Text: "excluded"},
					{Type: "text", MessageID: "envelope", Text: "<rhizome-peer-event>excluded</rhizome-peer-event>"},
					{Type: "turn"},
				}
				for _, f := range frames {
					c.handle(n, 1, f)
					if f.Type != "turn" {
						c.handle(n, 1, f)
					}
				}
				sink := &historySink{details: details, peerActivity: policy}
				h := &openCodeCursor{}
				h.normalize("synthetic", `{"text":"<rhizome-peer-event>input</rhizome-peer-event>"}`, sink)
				h.normalize("assistant", `{"content":[{"type":"text","text":"reply"},{"type":"tool","name":"bash","state":{"status":"completed","input":"input","content":"output"}},{"type":"tool","name":"rhizome_send"},{"type":"tool","name":"execute"},{"type":"reasoning","text":"excluded"},{"type":"text","text":"<channel source=\"rhizome\">excluded</channel>"},{"type":"text","text":"<rhizome-peer-event>excluded</rhizome-peer-event>"}]}`, sink)
				h.normalize("idle", `{}`, sink)
				want := 0
				if policy == config.PeerActivityResponses {
					want = 1
				}
				if policy == config.PeerActivityAll {
					want = 4
					if details {
						want = 6
					}
				}
				if len(n.history) != want || len(sink.events) != want {
					t.Fatalf("live=%v history=%v want %d", published(n), sink.events, want)
				}
				c.handle(n, 1, bridgeFrame{Type: "input", Kind: "user", Text: "continue"})
				c.handle(n, 1, bridgeFrame{Type: "text", MessageID: "human", Text: "human reply"})
				if got := activityTexts(n); got[len(got)-1] != "human reply" {
					t.Fatal(got)
				}
			})
		}
	}
}

func TestPeerActivityRedaction(t *testing.T) {
	for _, policy := range []config.PeerActivityPolicy{config.PeerActivityResponses, config.PeerActivityAll} {
		for _, fail := range []bool{false, true} {
			n := detailNode(t, true)
			n.peerActivity = policy
			sink := &historySink{identity: n.local(), details: true, peerActivity: policy}
			if fail {
				n.filter = newRedactor(fakeScanner{err: errors.New("test failure")}, nil, time.Second)
				sink.filter = n.filter
			}
			for _, target := range []activitySink{n, sink} {
				c := newPeerCollector()
				for _, r := range peerRecords("claude") {
					normalizePeerRecord(c, "claude", strings.ReplaceAll(r, "reply", testGitHubPAT), target)
				}
			}
			for _, e := range n.history {
				assertNoSecret(t, e.Text)
			}
			if !sink.events[0].Redacted || !strings.Contains(n.history[0].Text, "[REDACTED:") {
				t.Fatal("redaction bypassed")
			}
			if fail && n.history[0].Text != "[REDACTED:unscanned]" {
				t.Fatal("scan failure not closed")
			}
		}
	}
}

func TestPeerActivityUnknownContextAndMixedBlocks(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		for _, policy := range peerPolicies {
			n := detailNode(t, false)
			n.peerActivity = policy
			c := newPeerCollector()
			c.unknownTurn = true
			for _, r := range peerRecords(agent) {
				normalizePeerRecord(c, agent, r, n)
			}
			if len(n.history) != 0 {
				t.Fatal("unknown context bypassed", published(n))
			}
			human := `{"type":"user","uuid":"mixed","message":{"content":[{"type":"text","text":"continue"},{"type":"text","text":"<rhizome-peer-event>input</rhizome-peer-event>"}]}}`
			reply := `{"type":"assistant","uuid":"mixed-reply","message":{"content":"mixed reply"}}`
			if agent == "codex" {
				human = `{"type":"response_item","payload":{"type":"message","id":"mixed","role":"user","content":[{"type":"input_text","text":"continue"},{"type":"input_text","text":"<rhizome-peer-event>input</rhizome-peer-event>"}]}}`
				reply = `{"type":"response_item","payload":{"type":"message","id":"mixed-reply","role":"assistant","content":[{"type":"output_text","text":"mixed reply"}]}}`
			}
			normalizePeerRecord(c, agent, human, n)
			normalizePeerRecord(c, agent, reply, n)
			if got := activityTexts(n); !reflect.DeepEqual(got, []string{"mixed reply"}) {
				t.Fatal(got)
			}
		}
	}
}

// Real subscriptions carry the three-line scene; synthetic agent-format input
// is appended only after its corresponding notification arrives. No send calls.
func TestPeerActivityMutualExchange(t *testing.T) {
	for _, policy := range []config.PeerActivityPolicy{config.PeerActivityNone, config.PeerActivityResponses} {
		t.Run(string(policy), func(t *testing.T) {
			a, b := pair(t)
			a.peerActivity, b.peerActivity = policy, policy
			if err := a.subscribe("session-b"); err != nil {
				t.Fatal(err)
			}
			if err := b.subscribe("session-a"); err != nil {
				t.Fatal(err)
			}
			eventually(t, func() bool {
				a.mu.Lock()
				defer a.mu.Unlock()
				b.mu.Lock()
				defer b.mu.Unlock()
				return a.feeds["session-b"].status == "following" && b.feeds["session-a"].status == "following"
			})
			ca, cb := newPeerCollector(), newPeerCollector()
			emit := func(c *collector, n *Node, typ, text string) {
				raw, _ := json.Marshal(map[string]any{"type": typ, "uuid": id(), "message": map[string]any{"content": text}})
				c.normalize(raw, n, n.local().Generation)
			}
			emit(ca, a, "user", "Action")
			emit(ca, a, "assistant", "Wake up, Neo.")
			received := func(n *Node, text string) bool {
				n.mu.Lock()
				defer n.mu.Unlock()
				for _, p := range n.incoming {
					if p.event.Text == text {
						return true
					}
				}
				return false
			}
			eventually(t, func() bool { return received(b, "Wake up, Neo.") })
			emit(cb, b, "user", "<rhizome-peer-event>Wake up, Neo.</rhizome-peer-event>")
			emit(cb, b, "assistant", "What?")
			if policy == config.PeerActivityNone {
				if len(activityTexts(b)) != 0 {
					t.Fatal("suppression failed")
				}
				return
			}
			eventually(t, func() bool { return received(a, "What?") })
			emit(ca, a, "user", "<rhizome-peer-event>What?</rhizome-peer-event>")
			emit(ca, a, "assistant", "The Matrix has you.")
			eventually(t, func() bool { return received(b, "The Matrix has you.") })
			if got := activityTexts(a); !reflect.DeepEqual(got, []string{"Wake up, Neo.", "The Matrix has you."}) {
				t.Fatal(got)
			}
		})
	}
}

func TestPeerActivityObserverAndServedHistory(t *testing.T) {
	for _, policy := range []config.PeerActivityPolicy{config.PeerActivityNone, config.PeerActivityResponses, config.PeerActivityAll} {
		t.Run(string(policy), func(t *testing.T) {
			hh := newHistoryHarness(t)
			hh.o.opts.PeerActivity = policy
			hh.h.peerActivity = policy
			path := filepath.Join(hh.o.opts.ClaudeRoot, "s.jsonl")
			if err := os.WriteFile(path, []byte{}, 0600); err != nil {
				t.Fatal(err)
			}
			// New files start from byte zero; a full peer turn is authoritative input.
			var records []string
			for _, r := range peerRecords("claude") {
				var v map[string]any
				if err := json.Unmarshal([]byte(r), &v); err != nil {
					t.Fatal(err)
				}
				v["sessionId"] = "s"
				v["cwd"] = hh.o.opts.ClaudeRoot
				v["timestamp"] = "2026-10-21T12:00:00Z"
				raw, _ := json.Marshal(v)
				records = append(records, string(raw))
			}
			appendLog(t, path, strings.Join(records, "\n")+"\n")
			hh.o.visit(path, false)
			f := hh.o.files[path]
			if f == nil || f.n == nil {
				t.Fatal("observer not bound")
			}
			want := 0
			if policy == config.PeerActivityResponses {
				want = 1
			}
			if policy == config.PeerActivityAll {
				want = 4
			}
			if len(f.n.history) != want {
				t.Fatalf("observer %v want %d", published(f.n), want)
			}
			hh.index()
			access := &historyAccess{host: peerHostA, grant: peerGrant, policy: peerPolicy(config.AgentHistory{Enabled: true}, config.AgentHistory{}), now: peerNow}
			for _, remote := range []bool{false, true} {
				var out map[string]any
				var err *historyError
				if remote {
					out, err = hh.peerRead(access, `{"agent_type":"claude","session_id":"s"}`)
				} else {
					out, err = hh.read(`{"agent_type":"claude","session_id":"s"}`)
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(eventsOf(out)) != want {
					t.Fatalf("remote=%t events=%v want %d", remote, eventsOf(out), want)
				}
			}
		})
	}
}

// Keep the interface exercised by both sink types, including its zero-value policy.
var _ activitySink = (*historySink)(nil)
