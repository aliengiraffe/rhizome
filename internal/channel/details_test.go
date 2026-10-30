package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"rhizome/internal/config"
	pb "rhizome/proto"
)

// detailNode is a bound node with [sessions].share_prompts_and_tools set.
func detailNode(t *testing.T, details bool) *Node {
	t.Helper()
	n := node(t)
	n.identity.SessionId, n.identity.Generation = "s", 1
	n.details = details
	return n
}

// published lists the node's retained events as "kind: text".
func published(n *Node) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for _, e := range n.history {
		out = append(out, e.Kind+": "+e.Text)
	}
	return out
}

func claudeDetail(typ, uuid string, content any, meta bool) []byte {
	return []byte(strings.TrimSpace(claudeLine{typ: typ, uuid: uuid, session: "s", cwd: "/p", content: content, meta: meta}.String()))
}

func TestPayloadText(t *testing.T) {
	cases := []struct{ name, raw, want string }{
		{"absent", ``, ""},
		{"null", `null`, ""},
		{"empty object", `{}`, ""},
		{"string", `"plain output"`, "plain output"},
		{"object", `{ "command": "ls -la" }`, `{"command":"ls -la"}`},
		{"text blocks", `[{"type":"text","text":"one"},{"type":"image","source":{"data":"AAAA"}},{"type":"text","text":"two"}]`, "one\ntwo"},
		{"strings", `["a"," ","b"]`, "a\nb"},
		{"only images", `[{"type":"image","source":{"data":"AAAA"}}]`, ""},
		{"malformed", `{"x":`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := payloadText(json.RawMessage(tc.raw)); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClaudeDetails(t *testing.T) {
	bash := []map[string]any{{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{"command": "go test ./..."}}}
	result := []map[string]any{{"type": "tool_result", "tool_use_id": "t1", "content": []map[string]any{{"type": "text", "text": "ok rhizome"}}}}
	cases := []struct {
		name    string
		details bool
		records [][]byte
		want    []string
	}{
		{"human prompt and tool round trip", true, [][]byte{
			claudeDetail("user", "u1", "fix the flaky test", false),
			claudeDetail("assistant", "a1", bash, false),
			claudeDetail("user", "u2", result, false),
		}, []string{"prompt: fix the flaky test", "tool: Tool started: Bash", `tool_input: Bash: {"command":"go test ./..."}`, "tool: Tool completed: Bash", "tool_output: Bash: ok rhizome"}},
		{"multi-block prompt is one event", true, [][]byte{
			claudeDetail("user", "u1", []map[string]any{{"type": "text", "text": "first"}, {"type": "image"}, {"type": "text", "text": "second"}}, false),
		}, []string{"prompt: first\nsecond"}},
		{"disabled keeps the summary-only policy", false, [][]byte{
			claudeDetail("user", "u1", "fix the flaky test", false),
			claudeDetail("assistant", "a1", bash, false),
			claudeDetail("user", "u2", result, false),
		}, []string{"tool: Tool started: Bash", "tool: Tool completed: Bash"}},
		{"meta records are never prompts", true, [][]byte{
			claudeDetail("user", "u1", "<local-command-caveat>caveat</local-command-caveat>", true),
		}, nil},
		{"peer-triggered turn publishes nothing", true, [][]byte{
			claudeDetail("user", "u1", `<channel source="rhizome" session_id="x">peer said hi</channel>`, false),
			claudeDetail("assistant", "a1", bash, false),
			claudeDetail("user", "u2", result, false),
		}, nil},
		{"peer envelope is never a prompt", true, [][]byte{
			claudeDetail("user", "u1", "Rhizome peer activity.\n<rhizome-peer-event>{}</rhizome-peer-event>", false),
		}, nil},
		{"rhizome tools stay excluded", true, [][]byte{
			claudeDetail("user", "u1", "share it", false),
			claudeDetail("assistant", "a1", []map[string]any{{"type": "tool_use", "id": "r1", "name": "mcp__rhizome__send", "input": map[string]any{"text": "hello"}}}, false),
			claudeDetail("user", "u2", []map[string]any{{"type": "tool_result", "tool_use_id": "r1", "content": "queued"}}, false),
		}, []string{"prompt: share it"}},
		{"thinking stays excluded", true, [][]byte{
			claudeDetail("assistant", "a1", []map[string]any{{"type": "thinking", "thinking": "private"}}, false),
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := detailNode(t, tc.details)
			c, err := newCollector(filepath.Join(t.TempDir(), "s.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range tc.records {
				c.normalize(r, n, 1)
			}
			if got := published(n); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestCodexDetails(t *testing.T) {
	exit := 0
	human := codexItem{ID: "u1", Type: "userMessage", Content: json.RawMessage(`[{"type":"text","text":"run the tests"}]`)}
	peer := codexItem{ID: "u2", Type: "userMessage", Content: json.RawMessage(`[{"type":"text","text":"Rhizome peer activity.\n<rhizome-peer-event>{}</rhizome-peer-event>"}]`)}
	running := codexItem{ID: "c1", Type: "commandExecution", Status: "inProgress", Command: "go test ./...", Source: "agent"}
	done := running
	done.Status, done.AggregatedOutput, done.ExitCode = "completed", "ok rhizome", &exit
	mcp := codexItem{ID: "m1", Type: "mcpToolCall", Status: "completed", Server: "docs", Tool: "search", Arguments: json.RawMessage(`{"q":"x"}`), Result: json.RawMessage(`{"content":[{"type":"text","text":"found"}]}`)}
	rhizome := codexItem{ID: "r1", Type: "mcpToolCall", Status: "completed", Server: "rhizome", Tool: "send", Arguments: json.RawMessage(`{"text":"hi"}`)}
	edit := codexItem{ID: "f1", Type: "fileChange", Status: "completed", Changes: json.RawMessage(`[{"path":"/p/a.go","kind":{"type":"update"},"diff":"-a\n+b"}]`)}
	n := detailNode(t, true)
	c := newCodexCollector("s", nil)
	for _, item := range []codexItem{human, peer, running, done, done, mcp, rhizome, edit} {
		c.normalize(n, 1, "turn", item, true)
	}
	want := []string{
		"prompt: run the tests",
		"tool: commandExecution: inProgress",
		"tool_input: commandExecution: go test ./...",
		"tool: commandExecution: completed (exit 0)",
		"tool_output: commandExecution: ok rhizome",
		"tool: docs/search: completed",
		`tool_input: docs/search: {"q":"x"}`,
		"tool_output: docs/search: found",
		"tool: fileChange: completed",
		"tool_input: fileChange: /p/a.go\n-a\n+b",
	}
	if got := published(n); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	// A turn that is not emitted (peer-only or baseline) publishes nothing.
	quiet := detailNode(t, true)
	c = newCodexCollector("s", nil)
	c.normalize(quiet, 1, "turn", human, false)
	c.normalize(quiet, 1, "turn", done, false)
	if got := published(quiet); len(got) != 0 {
		t.Fatalf("suppressed turn published %q", got)
	}
}

func TestCodexDetailsPeerTurnScan(t *testing.T) {
	n := detailNode(t, true)
	reader := &fakeCodex{thread: "s", status: "idle", turns: []codexTurn{{ID: "t1", Status: "completed"}}, items: map[string][]codexItem{"t1": {
		{ID: "u1", Type: "userMessage", Content: json.RawMessage(`[{"type":"text","text":"Rhizome peer activity.\n<rhizome-peer-event>{}</rhizome-peer-event>"}]`)},
		{ID: "c1", Type: "commandExecution", Status: "completed", Command: "ls", AggregatedOutput: "files", Source: "agent"},
	}}}
	c := newCodexCollector("s", reader)
	c.baseline = true
	if !c.scan(context.Background(), n, 1) {
		t.Fatal("scan failed")
	}
	if got := published(n); len(got) != 0 {
		t.Fatalf("peer-only turn published %q", got)
	}
}

func TestCodexLogDetails(t *testing.T) {
	line := func(v map[string]any) []byte { b, _ := json.Marshal(v); return b }
	records := [][]byte{
		line(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "run the tests"}}),
		line(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "<rhizome-peer-event>{}</rhizome-peer-event>"}}),
		line(map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "id": "f1", "call_id": "c1", "name": "shell", "arguments": `{"command":["ls"]}`}}),
		line(map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "c1", "output": "files"}}),
		line(map[string]any{"type": "response_item", "payload": map[string]any{"type": "custom_tool_call", "id": "f2", "call_id": "c2", "name": "apply_patch", "input": "*** Begin Patch"}}),
		line(map[string]any{"type": "response_item", "payload": map[string]any{"type": "custom_tool_call_output", "call_id": "c2", "output": "Done"}}),
		line(map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "id": "f3", "call_id": "c3", "name": "mcp__rhizome__send", "arguments": `{"text":"hi"}`}}),
		line(map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "c3", "output": "queued"}}),
	}
	n := detailNode(t, true)
	c, err := newCollector(filepath.Join(t.TempDir(), "rollout.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	c.agent = "codex"
	for _, r := range records {
		c.normalize(r, n, 1)
	}
	want := []string{
		"prompt: run the tests",
		"tool: Tool started: shell",
		`tool_input: shell: {"command":["ls"]}`,
		"tool: Tool completed: shell",
		"tool_output: shell: files",
		"tool: Tool started: apply_patch",
		"tool_input: apply_patch: *** Begin Patch",
		"tool: Tool completed: apply_patch",
		"tool_output: apply_patch: Done",
	}
	if got := published(n); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestOpenCodeDetails(t *testing.T) {
	n := detailNode(t, true)
	c := newOpenCodeCollector()
	for _, f := range []bridgeFrame{
		{Type: "input", Kind: "user", Text: "fix login"},
		{Type: "tool", CallID: "c1", Name: "bash", Phase: "start", Text: `{"command":"ls"}`},
		{Type: "tool", CallID: "c1", Name: "bash", Phase: "completed", Text: "files"},
		{Type: "tool", CallID: "c2", Name: "rhizome_send", Phase: "start", Text: `{"text":"hi"}`},
		{Type: "tool", CallID: "c3", Name: "execute", Phase: "start", Text: "x"},
		// A peer delivery starts a reaction turn: neither its text nor its tools publish.
		{Type: "input", Kind: "synthetic", Rhizome: true, Text: "<rhizome-peer-event>{}</rhizome-peer-event>"},
		{Type: "tool", CallID: "c4", Name: "bash", Phase: "start", Text: "peer-driven"},
		{Type: "tool", CallID: "c4", Name: "bash", Phase: "completed", Text: "peer-output"},
	} {
		c.handle(n, 1, f)
	}
	want := []string{
		"prompt: fix login",
		"tool: Tool started: bash",
		`tool_input: bash: {"command":"ls"}`,
		"tool: Tool completed: bash",
		"tool_output: bash: files",
	}
	if got := published(n); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestDetailRedactionAndBounds(t *testing.T) {
	n := detailNode(t, true)
	n.activity(1, "prompt", "deploy with "+testGitHubPAT)
	n.activity(1, "tool_input", "Bash: export KEY="+testAWSKey)
	// A secret just past the live clip must not leave a fragment behind, and
	// megabytes of output are bounded before the scan.
	huge := strings.Repeat("y", maxText-10) + testGitHubPAT + strings.Repeat("z", 2*1024*1024)
	n.activity(1, "tool_output", huge)
	got := published(n)
	if len(got) != 3 || got[0] != "prompt: deploy with [REDACTED:github-pat]" || got[1] != "tool_input: Bash: export KEY=[REDACTED:aws-access-token]" {
		t.Fatalf("got %q", got)
	}
	for _, e := range got {
		assertNoSecret(t, e)
	}
	if out := n.history[2].Text; len(out) > maxText || strings.Contains(out, testGitHubPAT[:4]) || !strings.HasSuffix(out, "[truncated]") {
		t.Fatalf("tool output not bounded and redacted: %d bytes", len(out))
	}
	if n.stats.Redacted != 3 {
		t.Fatalf("redacted %d", n.stats.Redacted)
	}
	off := detailNode(t, false)
	for _, kind := range []string{"prompt", "tool_input", "tool_output"} {
		off.activity(1, kind, "content")
	}
	if got := published(off); len(got) != 0 {
		t.Fatalf("disabled node published %q", got)
	}
}

// Older subscribers reject unknown kinds, so publishers send detail kinds only
// to subscribers that set FollowRequest.details, both in replay and live.
func TestDetailKindsOnlyReachCapableSubscribers(t *testing.T) {
	a, b := pair(t)
	a.details = true
	gen := a.local().Generation
	a.activity(gen, "prompt", "replayed prompt")
	a.activity(gen, "activity", "replayed text")
	a.mu.Lock()
	streamID := a.stream
	a.mu.Unlock()
	conn, err := a.dial(a.local())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	follow := func(details bool) []string {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stream, err := pb.NewChannelClient(conn).Subscribe(b.outgoing(ctx), &pb.FollowRequest{Target: a.local(), Subscriber: b.local(), StreamId: streamID, Details: details})
		if err != nil {
			t.Fatal(err)
		}
		var kinds []string
		for {
			e, err := stream.Recv()
			if err != nil {
				t.Fatal(err)
			}
			if e.Kind == "position" {
				break
			}
			kinds = append(kinds, e.Kind)
		}
		a.activity(gen, "tool_output", "live output")
		a.activity(gen, "activity", "live text")
		for {
			e, err := stream.Recv()
			if err != nil {
				t.Fatal(err)
			}
			kinds = append(kinds, e.Kind)
			if e.Kind == "activity" && e.Text == "live text" {
				return kinds
			}
		}
	}
	if got := follow(false); !reflect.DeepEqual(got, []string{"activity", "activity"}) {
		t.Fatalf("legacy subscriber received %q", got)
	}
	if got := follow(true); !reflect.DeepEqual(got, []string{"prompt", "activity", "tool_output", "activity", "tool_output", "activity"}) {
		t.Fatalf("capable subscriber received %q", got)
	}
}

func TestDetailKindsDeliverEndToEnd(t *testing.T) {
	a, b := pair(t)
	a.details = true
	if err := b.subscribe("session-a"); err != nil {
		t.Fatal(err)
	}
	gen := a.local().Generation
	eventually(t, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return len(a.listeners) > 0
	})
	a.activity(gen, "prompt", "fix "+testGitHubPAT)
	eventually(t, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		for _, p := range b.incoming {
			if p.event.Kind == "prompt" {
				return p.event.Text == "fix [REDACTED:github-pat]" && automatic(p.event)
			}
		}
		return false
	})
}

func TestHistoryDetails(t *testing.T) {
	hh, f, dir := openCodeHarness(t)
	hh.h.details = true
	claude := userText("c", "/p", "u1", "2026-10-01T00:00:00Z", "deploy with "+testGitHubPAT) +
		claudeLine{typ: "assistant", uuid: "a1", session: "c", cwd: "/p", ts: "2026-10-01T00:00:01Z", content: []map[string]any{{"type": "tool_use", "id": "t1", "name": "Bash", "input": "echo " + testAWSKey}}}.String() +
		claudeLine{typ: "user", uuid: "u2", session: "c", cwd: "/p", ts: "2026-10-01T00:00:02Z", content: []map[string]any{{"type": "tool_result", "tool_use_id": "t1", "content": "printed " + testAWSKey}}}.String()
	appendLog(t, filepath.Join(hh.o.opts.ClaudeRoot, "c.jsonl"), claude)
	codexDir := filepath.Join(hh.o.opts.CodexRoot, "2026", "10", "01")
	if err := os.MkdirAll(codexDir, 0700); err != nil {
		t.Fatal(err)
	}
	rec := func(ts string, v map[string]any) string {
		v["timestamp"] = ts
		b, _ := json.Marshal(v)
		return string(b) + "\n"
	}
	appendLog(t, filepath.Join(codexDir, "rollout-x.jsonl"), codexMeta("x", "/p", "2026-10-01T00:00:00Z")+
		rec("2026-10-01T00:00:00Z", map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "deploy with " + testGitHubPAT}})+
		rec("2026-10-01T00:00:01Z", map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "id": "f1", "call_id": "c1", "name": "Bash", "arguments": "echo " + testAWSKey}})+
		rec("2026-10-01T00:00:02Z", map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "c1", "output": "printed " + testAWSKey}}))
	f.session("ses_d", dir, "details", "", "2026-10-20T11:00:00Z", "2026-10-20T11:04:00Z")
	f.message("ses_d", 1, "user", "2026-10-20T11:00:01Z", ocPart{"text": "deploy with " + testGitHubPAT})
	f.message("ses_d", 2, "assistant", "2026-10-20T11:00:05Z", ocAssistant(ocPart{"type": "tool", "id": "c1", "name": "Bash", "state": ocPart{"status": "completed", "input": "echo " + testAWSKey, "content": "printed " + testAWSKey}}))
	hh.index()
	want := []string{
		"user_prompt|deploy with [REDACTED:github-pat]|true",
		"tool_summary|Tool started: Bash|false",
		"tool_input|Bash: echo [REDACTED:aws-access-token]|true",
		"tool_summary|Tool completed: Bash|false",
		"tool_output|Bash: printed [REDACTED:aws-access-token]|true",
	}
	check := func(name string, out map[string]any, herr *historyError) {
		t.Helper()
		if herr != nil {
			t.Fatalf("%s: %v", name, herr)
		}
		raw, _ := json.Marshal(out)
		assertNoSecret(t, string(raw))
		var got []string
		for _, e := range eventsOf(out) {
			got = append(got, fmt.Sprintf("%v|%v|%v", e["kind"], e["text"], e["redacted"] == true))
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: got %q\nwant %q", name, got, want)
		}
		if cov := out["coverage"].(map[string]any); cov["content_policy"] != "prompts_and_tools" {
			t.Fatalf("%s: coverage %v", name, cov)
		}
	}
	out, herr := hh.read(`{"session_id":"c","agent_type":"claude","limit":10}`)
	check("claude", out, herr)
	out, herr = hh.read(`{"session_id":"x","agent_type":"codex","limit":10}`)
	check("codex", out, herr)
	out, herr = hh.read(`{"session_id":"ses_d","agent_type":"opencode","limit":10}`)
	check("opencode", out, herr)
	a := &historyAccess{host: peerHostA, grant: peerGrant, policy: peerPolicy(config.AgentHistory{Enabled: true}, config.AgentHistory{Enabled: true}), now: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}
	out, herr = hh.peerRead(a, `{"session_id":"c","agent_type":"claude","limit":10}`)
	check("peer claude", out, herr)
	out, herr = hh.peerRead(a, `{"session_id":"x","agent_type":"codex","limit":10}`)
	check("peer codex", out, herr)
	// The serving host's switch decides: off restores the summary-only policy.
	hh.h.details = false
	out, herr = hh.read(`{"session_id":"c","agent_type":"claude","limit":10}`)
	if herr != nil {
		t.Fatal(herr)
	}
	for _, e := range eventsOf(out) {
		if k := e["kind"]; k != "tool_summary" {
			t.Fatalf("disabled history returned %v", e)
		}
	}
	if cov := out["coverage"].(map[string]any); cov["content_policy"] != "assistant_activity_only" {
		t.Fatalf("coverage %v", cov)
	}
}
