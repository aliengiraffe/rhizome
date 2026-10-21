package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "rhizome/proto"
)

// shortTempDir keeps unix socket paths under the platform limit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rhz")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func cursorManager(t *testing.T) *cursorNodes {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	runtime := filepath.Join(shortTempDir(t), "run")
	if err := secureDir(runtime); err != nil {
		t.Fatal(err)
	}
	m := newCursorNodes(ctx, runtime, Options{Runtime: runtime}, false)
	t.Cleanup(func() { cancel(); m.close() })
	return m
}

func TestMinimizeCursorPayload(t *testing.T) {
	root := t.TempDir()
	base := cursorHookPayload{ConversationID: "conv-a", WorkspaceRoots: []string{root, "relative"}}
	with := func(f func(p *cursorHookPayload)) cursorHookPayload { p := base; f(&p); return p }
	cases := []struct {
		name  string
		p     cursorHookPayload
		ok    bool
		check func(cursorMessage) bool
	}{
		{"subagent ignored", with(func(p *cursorHookPayload) { p.Event = "afterAgentResponse"; p.SubagentID = "sub" }), false, nil},
		{"child conversation ignored", with(func(p *cursorHookPayload) { p.Event = "stop"; p.ParentConversation = "parent" }), false, nil},
		{"background agent ignored", with(func(p *cursorHookPayload) { p.Event = "sessionStart"; p.Background = true }), false, nil},
		{"missing conversation", with(func(p *cursorHookPayload) { p.Event = "sessionStart"; p.ConversationID = "" }), false, nil},
		{"unknown event", with(func(p *cursorHookPayload) { p.Event = "afterAgentThought"; p.Text = "secret reasoning" }), false, nil},
		{"session start keeps roots", with(func(p *cursorHookPayload) { p.Event = "sessionStart" }), true, func(m cursorMessage) bool {
			return len(m.WorkspaceRoots) == 1 && m.WorkspaceRoots[0] == root && m.Cwd == ""
		}},
		{"prompt reduced to envelope flag", with(func(p *cursorHookPayload) {
			p.Event = "beforeSubmitPrompt"
			p.Prompt = "x\n<rhizome-peer-event>\n{}\n</rhizome-peer-event>"
		}), true, func(m cursorMessage) bool {
			raw, _ := json.Marshal(m)
			return m.PeerInput && !bytes.Contains(raw, []byte("peer-event")) && !bytes.Contains(raw, []byte("prompt"))
		}},
		{"plain prompt", with(func(p *cursorHookPayload) { p.Event = "beforeSubmitPrompt"; p.Prompt = "please mention rhizome" }), true, func(m cursorMessage) bool { return !m.PeerInput }},
		{"tool failure keeps name only", with(func(p *cursorHookPayload) {
			p.Event = "postToolUseFailure"
			p.ToolName = "Shell"
			p.Cwd = root
			p.ToolInput = json.RawMessage(`{"command":"rm"}`)
		}), true, func(m cursorMessage) bool {
			raw, _ := json.Marshal(m)
			return m.ToolName == "Shell" && m.Cwd == root && !bytes.Contains(raw, []byte("rm"))
		}},
		{"assistant text bounded", with(func(p *cursorHookPayload) { p.Event = "afterAgentResponse"; p.Text = strings.Repeat("x", maxText+100) }), true, func(m cursorMessage) bool {
			return len(m.Text) <= maxText && strings.HasSuffix(m.Text, "[truncated]")
		}},
		{"stop", with(func(p *cursorHookPayload) { p.Event = "stop"; p.Status = "completed"; p.LoopCount = 2 }), true, func(m cursorMessage) bool { return m.Status == "completed" && m.LoopCount == 2 }},
		{"other MCP server ignored", with(func(p *cursorHookPayload) {
			p.Event = "beforeMCPExecution"
			p.MCPServer = "github"
			p.ToolName = "send"
		}), false, nil},
		{"rhizome ticket hashes input", with(func(p *cursorHookPayload) {
			p.Event = "beforeMCPExecution"
			p.MCPServer = "rhizome"
			p.ToolName = "send"
			p.ToolInput = json.RawMessage(`"{\"text\":\"hi\",\"session_id\":\"s\"}"`)
		}), true, func(m cursorMessage) bool {
			raw, _ := json.Marshal(m)
			return m.InputHash == canonicalHash([]byte(`{"session_id":"s","text":"hi"}`)) && !bytes.Contains(raw, []byte("hi"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := minimizeCursorPayload(tc.p)
			if ok != tc.ok {
				t.Fatalf("ok=%v want %v (%+v)", ok, tc.ok, m)
			}
			if ok && !tc.check(m) {
				t.Fatalf("unexpected message %+v", m)
			}
		})
	}
}

func TestCanonicalHash(t *testing.T) {
	a := canonicalHash([]byte(`{"b":1,"a":"x"}`))
	if a != canonicalHash([]byte(`{"a":"x","b":1}`)) || a != canonicalHash([]byte(`"{\"a\":\"x\",\"b\":1}"`)) {
		t.Fatal("hash depends on key order or string wrapping")
	}
	if a == canonicalHash([]byte(`{"a":"x","b":2}`)) {
		t.Fatal("different inputs collide")
	}
	if canonicalHash(nil) != canonicalHash([]byte(`{}`)) || canonicalHash(nil) != canonicalHash([]byte(`null`)) {
		t.Fatal("absent arguments must equal empty arguments")
	}
}

func TestSelectCursorRegistration(t *testing.T) {
	dir := t.TempDir()
	write := func(id string, pid int, roots ...string) {
		raw, _ := json.Marshal(cursorRegistration{ID: id, PID: pid, Socket: filepath.Join(dir, id+".sock"), Roots: roots})
		if err := os.WriteFile(filepath.Join(dir, id+".json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	alive := func(pid int) bool { return pid != 99 }
	write("dead", 99, "/w/a")
	write("a", 1, "/w/a")
	write("b", 2, "/w/b", "/w/c")
	if err := os.WriteFile(filepath.Join(dir, "junk.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		roots []string
		want  string
		err   string
	}{
		{"exact", []string{"/w/a"}, "a", ""},
		{"second root of a multi-root process", []string{"/w/c"}, "b", ""},
		{"nested workspace matches its parent", []string{"/w/b/sub"}, "b", ""},
		{"dead process ignored", []string{"/w/a"}, "a", ""},
		{"no match", []string{"/elsewhere"}, "", "no live Cursor MCP process"},
		{"ambiguous without roots", nil, "", "refusing to guess"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg, err := selectCursorRegistration(dir, tc.roots, alive)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("got %v %v, want error %q", reg, err, tc.err)
				}
				return
			}
			if err != nil || reg.ID != tc.want {
				t.Fatalf("got %v %v, want %s", reg, err, tc.want)
			}
		})
	}
	// Roots are unknown until roots/list answers: a lone process still matches.
	write("unknown", 3)
	if reg, err := selectCursorRegistration(dir, []string{"/elsewhere"}, alive); err != nil || reg.ID != "unknown" {
		t.Fatalf("unknown-roots fallback: %v %v", reg, err)
	}
	os.Remove(filepath.Join(dir, "a.json"))
	os.Remove(filepath.Join(dir, "b.json"))
	os.Remove(filepath.Join(dir, "dead.json"))
	if reg, err := selectCursorRegistration(dir, nil, alive); err != nil || reg.ID != "unknown" {
		t.Fatalf("single live process without roots: %v %v", reg, err)
	}
}

func TestCursorHandleBindsCollectsAndSuppresses(t *testing.T) {
	m := cursorManager(t)
	root := filepath.Join(shortTempDir(t), "proj")
	msg := func(event string) cursorMessage {
		return cursorMessage{Event: event, ConversationID: "conv-a", WorkspaceRoots: []string{root}}
	}
	if resp := m.handle(msg("sessionStart")); resp["additional_context"] != cursorInstructions {
		t.Fatalf("session start: %v", resp)
	}
	n := m.nodes["conv-a"]
	if n == nil {
		t.Fatal("no node")
	}
	st := n.status()
	id := st["identity"].(*pb.Identity)
	if st["binding"] != "BOUND" || id.AgentType != "cursor" || id.SessionId != "conv-a" || id.Project != root || id.DisplayName != "proj" || id.SessionName != "proj · cursor · conv-a" || st["collection_state"] != "hooks" || st["delivery_mode"] != "hook_pull" {
		t.Fatalf("status %+v identity %+v", st, id)
	}
	last := func() *pb.Event {
		n.mu.Lock()
		defer n.mu.Unlock()
		if len(n.history) == 0 {
			return nil
		}
		return n.history[len(n.history)-1]
	}
	// Relative-only roots cannot bind, and no absolute root means no project.
	if m.handle(cursorMessage{Event: "sessionStart", ConversationID: "conv-rel", WorkspaceRoots: []string{"relative"}})["additional_context"] == nil || m.nodes["conv-rel"] != nil {
		t.Fatal("relative root created a node")
	}
	if m.handle(cursorMessage{Event: "afterAgentResponse", ConversationID: strings.Repeat("x", 129), Text: "t"})["additional_context"] != nil || len(m.nodes) != 1 {
		t.Fatal("oversized conversation id accepted")
	}
	text := msg("afterAgentResponse")
	text.Text = "assistant said"
	m.handle(text)
	if e := last(); e == nil || e.Kind != "activity" || e.Text != "assistant said" {
		t.Fatalf("activity not published: %+v", e)
	}
	tool := msg("postToolUse")
	tool.ToolName = "Shell"
	tool.Cwd = filepath.Join(root, "sub")
	if resp := m.handle(tool); len(resp) != 0 {
		t.Fatalf("empty outbox injected context: %v", resp)
	}
	if e := last(); e.Kind != "tool" || e.Text != "Tool completed: Shell" {
		t.Fatalf("tool marker: %+v", e)
	}
	if n.local().Project != filepath.Join(root, "sub") {
		t.Fatal("cwd did not refresh the project label")
	}
	fail := msg("postToolUseFailure")
	fail.ToolName = "Write"
	m.handle(fail)
	if e := last(); e.Text != "Tool failed: Write" {
		t.Fatalf("failure marker: %+v", e)
	}
	own := msg("postToolUse")
	own.ToolName = "mcp_rhizome_list_sessions"
	before := last().Sequence
	m.handle(own)
	if last().Sequence != before {
		t.Fatal("Rhizome tool call was collected")
	}
	stop := msg("stop")
	stop.Status = "completed"
	m.handle(stop)
	if e := last(); e.Kind != "turn" {
		t.Fatalf("turn marker: %+v", e)
	}
	aborted := msg("stop")
	aborted.Status = "aborted"
	before = last().Sequence
	m.handle(aborted)
	if last().Sequence != before {
		t.Fatal("aborted turn published a marker")
	}
	// A turn started by a peer envelope is suppressed until the next real prompt.
	peer := msg("beforeSubmitPrompt")
	peer.PeerInput = true
	if resp := m.handle(peer); resp["continue"] != true {
		t.Fatalf("prompt hook must continue: %v", resp)
	}
	m.handle(text)
	m.handle(tool)
	m.handle(stop)
	if last().Sequence != before || n.status()["counters"].(counters).Suppressed != 3 {
		t.Fatalf("peer turn not suppressed: %+v %+v", last(), n.status()["counters"])
	}
	m.handle(msg("beforeSubmitPrompt"))
	m.handle(text)
	if last().Sequence != before+1 {
		t.Fatal("suppression did not end with the next prompt")
	}
	m.handle(msg("sessionEnd"))
	if m.nodes["conv-a"] != nil {
		t.Fatal("sessionEnd kept the node")
	}
	if _, err := m.resolve(&mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "list_sessions"}}); !errors.Is(err, errCursorUnbound) {
		t.Fatalf("retired process still resolves: %v", err)
	}
	m.handle(msg("beforeSubmitPrompt"))
	if n2 := m.nodes["conv-a"]; n2 == nil || n2 == n || n2.local().SessionId != "conv-a" {
		t.Fatal("conversation did not rebind after sessionEnd")
	}
}

func TestCursorDeliveryOutboxAndFollowup(t *testing.T) {
	m := cursorManager(t)
	root := filepath.Join(shortTempDir(t), "proj")
	msg := func(event string) cursorMessage {
		return cursorMessage{Event: event, ConversationID: "conv-a", WorkspaceRoots: []string{root}}
	}
	m.handle(msg("sessionStart"))
	n := m.nodes["conv-a"]
	queue := func(text string) {
		n.mu.Lock()
		defer n.mu.Unlock()
		e := &pb.Event{SessionId: "peer", NodeId: id(), Generation: 1, StreamId: "s/direct", Sequence: 1, Kind: "direct", UnixMillis: time.Now().UnixMilli(), Text: text, MessageId: id()}
		if !n.enqueueLocked(e, "", "", n.identity.Generation) {
			t.Fatal("enqueue refused")
		}
	}
	queue("hello from peer")
	eventually(t, func() bool { return n.status()["outbox_depth"].(int) == 1 })
	tool := msg("postToolUse")
	tool.ToolName = "Shell"
	resp := m.handle(tool)
	injected, _ := resp["additional_context"].(string)
	if !strings.Contains(injected, "<rhizome-peer-event>") || !strings.Contains(injected, "hello from peer") || !strings.Contains(injected, `"session_id":"peer"`) || !strings.Contains(injected, "untrusted") {
		t.Fatalf("additional_context: %q", injected)
	}
	if n.status()["outbox_depth"].(int) != 0 || n.status()["counters"].(counters).QueueHandoffs != 1 {
		t.Fatalf("outbox not drained: %+v", n.status())
	}
	// Followup delivery is off by default: the stop hook never auto-submits.
	queue("second")
	eventually(t, func() bool { return n.status()["outbox_depth"].(int) == 1 })
	stop := msg("stop")
	stop.Status = "completed"
	if resp := m.handle(stop); resp["followup_message"] != nil {
		t.Fatalf("followup delivered while disabled: %v", resp)
	}
	m.followup = true
	loop := stop
	loop.LoopCount = 1
	if resp := m.handle(loop); resp["followup_message"] != nil {
		t.Fatal("followup delivered on a looped turn")
	}
	aborted := stop
	aborted.Status = "aborted"
	if resp := m.handle(aborted); resp["followup_message"] != nil {
		t.Fatal("followup delivered on an aborted turn")
	}
	follow, _ := m.handle(stop)["followup_message"].(string)
	if !strings.HasPrefix(follow, cursorFollowupPrefix) || !strings.Contains(follow, "second") {
		t.Fatalf("followup: %q", follow)
	}
	n.mu.Lock()
	peer := n.peerTurn
	n.mu.Unlock()
	if !peer {
		t.Fatal("followup turn not marked as peer-only")
	}
	if resp := m.handle(stop); resp["followup_message"] != nil {
		t.Fatal("empty outbox produced a followup")
	}
	// A full outbox retains events in the delivery queue instead of dropping them.
	n.mu.Lock()
	for i := 0; i < maxCursorOutbox; i++ {
		n.outbox = append(n.outbox, outboxEntry{content: "x"})
	}
	n.mu.Unlock()
	if err := n.outboxNotify(context.Background(), "overflow", nil); !errors.Is(err, errDestinationUnavailable) {
		t.Fatalf("full outbox: %v", err)
	}
	queue("retained")
	time.Sleep(150 * time.Millisecond)
	st := n.status()
	if st["queue_depth"].(int) != 1 || st["counters"].(counters).Dropped != 0 {
		t.Fatalf("retained event was dropped: %+v", st)
	}
	drained := n.drainOutbox()
	if drained == "" || !n.deliveryReady {
		t.Fatal("drain did not re-arm delivery")
	}
	eventually(t, func() bool { return strings.Contains(n.drainOutbox(), "retained") })
}

func TestCursorResolveTickets(t *testing.T) {
	m := cursorManager(t)
	root := filepath.Join(shortTempDir(t), "proj")
	call := func(name, args string) *mcp.CallToolRequest {
		return &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: name, Arguments: json.RawMessage(args)}}
	}
	if _, err := m.resolve(call("list_sessions", "{}")); !errors.Is(err, errCursorUnbound) {
		t.Fatalf("no conversations: %v", err)
	}
	m.handle(cursorMessage{Event: "sessionStart", ConversationID: "conv-a", WorkspaceRoots: []string{root}})
	a := m.nodes["conv-a"]
	if n, err := m.resolve(call("list_sessions", "{}")); err != nil || n != a {
		t.Fatalf("single conversation: %v %v", n, err)
	}
	// A ticket for an unbound conversation binds it first, so the single-node
	// shortcut never attributes another chat's call to this one.
	ticket := cursorMessage{Event: "beforeMCPExecution", ConversationID: "conv-b", WorkspaceRoots: []string{root}, ToolName: "rhizome_send", InputHash: canonicalHash([]byte(`{"session_id":"x","text":"hi"}`))}
	if resp := m.handle(ticket); resp["permission"] != "allow" {
		t.Fatalf("ticket response: %v", resp)
	}
	b := m.nodes["conv-b"]
	if b == nil || b == a {
		t.Fatal("ticket did not bind conv-b")
	}
	if _, err := m.resolve(call("list_sessions", "{}")); !errors.Is(err, errCursorAmbiguous) {
		t.Fatalf("two conversations without ticket: %v", err)
	}
	if _, err := m.resolve(call("send", `{"text":"other","session_id":"x"}`)); !errors.Is(err, errCursorAmbiguous) {
		t.Fatalf("hash mismatch must not match: %v", err)
	}
	if n, err := m.resolve(call("send", `{"text":"hi","session_id":"x"}`)); err != nil || n != b {
		t.Fatalf("ticket match: %v %v", n, err)
	}
	if _, err := m.resolve(call("send", `{"text":"hi","session_id":"x"}`)); !errors.Is(err, errCursorAmbiguous) {
		t.Fatal("ticket was reusable")
	}
	// Two live tickets for the same call are ambiguous; nothing is consumed.
	ticket.ConversationID = "conv-a"
	m.handle(ticket)
	ticket.ConversationID = "conv-b"
	m.handle(ticket)
	if _, err := m.resolve(call("send", `{"text":"hi","session_id":"x"}`)); !errors.Is(err, errCursorAmbiguous) || len(m.tickets) != 2 {
		t.Fatalf("duplicate tickets: %v %d", err, len(m.tickets))
	}
	m.mu.Lock()
	for i := range m.tickets {
		m.tickets[i].expires = time.Now().Add(-time.Second)
	}
	m.mu.Unlock()
	if _, err := m.resolve(call("send", `{"text":"hi","session_id":"x"}`)); !errors.Is(err, errCursorAmbiguous) || len(m.tickets) != 0 {
		t.Fatal("expired tickets were honored")
	}
	// A hook without tool input matches on the tool name alone.
	m.handle(cursorMessage{Event: "beforeMCPExecution", ConversationID: "conv-a", WorkspaceRoots: []string{root}, ToolName: "list_sessions"})
	if n, err := m.resolve(call("list_sessions", "")); err != nil || n != a {
		t.Fatalf("name-only ticket: %v %v", n, err)
	}
	// A ticket whose conversation ended fails closed.
	m.handle(cursorMessage{Event: "beforeMCPExecution", ConversationID: "conv-b", WorkspaceRoots: []string{root}, ToolName: "publish"})
	m.handle(cursorMessage{Event: "sessionEnd", ConversationID: "conv-b"})
	if _, err := m.resolve(call("publish", "")); !errors.Is(err, errCursorUnbound) {
		t.Fatalf("retired ticket: %v", err)
	}
	if len(m.tickets) != 0 {
		t.Fatal("ticket cap bookkeeping")
	}
	for i := 0; i < maxCursorTickets+5; i++ {
		m.ticket(cursorMessage{ConversationID: "conv-a", ToolName: "publish"})
	}
	if len(m.tickets) != maxCursorTickets {
		t.Fatalf("ticket cap: %d", len(m.tickets))
	}
}

func TestCursorSocketAndHookProcess(t *testing.T) {
	m := cursorManager(t)
	if err := m.start(); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(shortTempDir(t), "proj")
	registry := cursorRegistry(m.runtime)
	reg, err := selectCursorRegistration(registry, []string{root}, processAlive)
	if err != nil || reg.PID != os.Getpid() || len(reg.Roots) != 0 {
		t.Fatalf("registration: %+v %v", reg, err)
	}
	if st, err := os.Stat(reg.Socket); err != nil || st.Mode().Perm()&0077 != 0 {
		t.Fatalf("socket not private: %v %v", st, err)
	}
	m.mu.Lock()
	m.roots = []string{root}
	m.mu.Unlock()
	if err := m.register(); err != nil {
		t.Fatal(err)
	}
	if _, err := selectCursorRegistration(registry, []string{"/other"}, processAlive); err == nil {
		t.Fatal("foreign workspace matched a process with known roots")
	}
	ctx := context.Background()
	resp, err := cursorHookCall(ctx, reg.Socket, cursorMessage{Event: "sessionStart", ConversationID: "conv-a", WorkspaceRoots: []string{root}})
	if err != nil || resp["additional_context"] != cursorInstructions {
		t.Fatalf("socket round trip: %v %v", resp, err)
	}
	if m.nodes["conv-a"] == nil {
		t.Fatal("socket request did not bind")
	}
	// The hook entry point: stdin payload in, one JSON line out, nothing else.
	payload := map[string]any{"hook_event_name": "postToolUse", "conversation_id": "conv-a", "workspace_roots": []string{root}, "cwd": root, "tool_name": "Shell", "tool_input": map[string]any{"command": "ls"}, "tool_output": "secret output", "user_email": "someone@example.com"}
	raw, _ := json.Marshal(payload)
	var out bytes.Buffer
	if err := CursorHook(ctx, m.runtime, bytes.NewReader(raw), &out); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if json.Unmarshal(out.Bytes(), &decoded) != nil || strings.Count(out.String(), "\n") != 1 || len(decoded) != 0 {
		t.Fatalf("hook stdout: %q", out.String())
	}
	n := m.nodes["conv-a"]
	n.mu.Lock()
	marker := n.history[len(n.history)-1].Text
	n.mu.Unlock()
	if marker != "Tool completed: Shell" {
		t.Fatalf("hook did not collect: %q", marker)
	}
	// Failures answer with Cursor's own defaults: unreachable runtime, no
	// registration, oversized or invalid payloads.
	out.Reset()
	_ = CursorHook(ctx, "", strings.NewReader(`{"hook_event_name":"beforeSubmitPrompt","conversation_id":"c","prompt":"x"}`), &out)
	if strings.TrimSpace(out.String()) != `{"continue":true}` {
		t.Fatalf("default without runtime: %q", out.String())
	}
	out.Reset()
	_ = CursorHook(ctx, filepath.Join(m.runtime, "missing"), strings.NewReader(`{"hook_event_name":"beforeMCPExecution","conversation_id":"c","mcp_server_name":"rhizome","tool_name":"send"}`), &out)
	if strings.TrimSpace(out.String()) != `{"permission":"ask"}` {
		t.Fatalf("default without registration: %q", out.String())
	}
	out.Reset()
	_ = CursorHook(ctx, m.runtime, strings.NewReader(`{"hook_event_name":"beforeMCPExecution","conversation_id":"c","mcp_server_name":"github","tool_name":"x"}`), &out)
	if strings.TrimSpace(out.String()) != `{"permission":"ask"}` || len(m.tickets) != 0 {
		t.Fatalf("other server: %q", out.String())
	}
	out.Reset()
	_ = CursorHook(ctx, m.runtime, strings.NewReader("not json"), &out)
	if strings.TrimSpace(out.String()) != `{}` {
		t.Fatalf("invalid payload: %q", out.String())
	}
	out.Reset()
	_ = CursorHook(ctx, m.runtime, bytes.NewReader(bytes.Repeat([]byte("a"), maxCursorMessage+1)), &out)
	if strings.TrimSpace(out.String()) != `{}` {
		t.Fatalf("oversized payload: %q", out.String())
	}
	if _, err := cursorHookCall(ctx, reg.Socket, cursorMessage{Event: "afterAgentResponse", ConversationID: "conv-a", Text: strings.Repeat("x", maxCursorMessage)}); err == nil {
		t.Fatal("oversized socket line accepted")
	}
	m.close()
	if _, err := os.Stat(reg.Socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("socket left behind")
	}
	if files, _ := filepath.Glob(filepath.Join(registry, "*.json")); len(files) != 0 {
		t.Fatalf("registration left behind: %v", files)
	}
	// Stale registrations from dead processes are pruned at the next start.
	stale, _ := json.Marshal(cursorRegistration{ID: "stale", PID: 1 << 30, Socket: filepath.Join(registry, "stale.sock")})
	_ = os.WriteFile(filepath.Join(registry, "stale.json"), stale, 0600)
	_ = os.WriteFile(filepath.Join(registry, "stale.sock"), nil, 0600)
	pruneCursorRegistrations(registry)
	if files, _ := filepath.Glob(filepath.Join(registry, "stale.*")); len(files) != 0 {
		t.Fatalf("stale registration kept: %v", files)
	}
}

func TestCursorMCPToolsWithoutObserveSession(t *testing.T) {
	m := cursorManager(t)
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := makeServer(m.resolve, agentCursor, func() {}, nil).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "cursor-test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if cs.InitializeResult().Capabilities.Experimental["claude/channel"] != nil {
		t.Fatal("Cursor advertised Claude channels")
	}
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "observe_session" {
			t.Fatal("Cursor mode offers observe_session")
		}
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
	if err != nil || !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "UNBOUND") {
		t.Fatalf("unbound call: %+v %v", res, err)
	}
	root := filepath.Join(shortTempDir(t), "proj")
	m.handle(cursorMessage{Event: "sessionStart", ConversationID: "conv-a", WorkspaceRoots: []string{root}})
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
	if err != nil || res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, `"binding":"BOUND"`) {
		t.Fatalf("bound call: %+v %v", res, err)
	}
	m.handle(cursorMessage{Event: "sessionStart", ConversationID: "conv-b", WorkspaceRoots: []string{root}})
	res, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "set_session_name", Arguments: map[string]any{"name": "mine"}})
	if !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "identity_ambiguous") {
		t.Fatalf("ambiguous call: %+v", res)
	}
	m.handle(cursorMessage{Event: "beforeMCPExecution", ConversationID: "conv-b", WorkspaceRoots: []string{root}, ToolName: "set_session_name", InputHash: canonicalHash([]byte(`{"name":"mine"}`))})
	res, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "set_session_name", Arguments: map[string]any{"name": "mine"}})
	if res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, `"session_id":"conv-b"`) {
		t.Fatalf("ticketed call: %+v", res)
	}
}
