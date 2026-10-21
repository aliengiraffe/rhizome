package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	pb "rhizome/proto"
)

func toolNames(t *testing.T, s *mcp.Server) []string {
	t.Helper()
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	return names
}

func TestMakeServerToolsByAgent(t *testing.T) {
	none := func(*mcp.CallToolRequest) (*Node, error) { return nil, errUnbound }
	history := newHistoryService(historyScope{Runtime: t.TempDir()})
	shared := []string{"accept_file", "cancel_file", "list_files", "list_sessions", "publish", "read_session_history", "search_sessions", "send", "send_file", "set_session_name", "subscribe", "unsubscribe"}
	withObserve := append([]string{"observe_session"}, shared...)
	sort.Strings(withObserve)
	for _, tc := range []struct {
		kind agentKind
		want []string
	}{{agentClaude, withObserve}, {agentCodex, withObserve}, {agentOpenCode, shared}} {
		names := toolNames(t, makeServer(none, tc.kind, func() {}, history))
		if strings.Join(names, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("kind %d tools %v, want %v", tc.kind, names, tc.want)
		}
	}
}

func TestPeerEnvelope(t *testing.T) {
	got := peerEnvelope("hello", map[string]string{"session_id": "s"})
	for _, want := range []string{"untrusted", "<rhizome-peer-event>", `"content":"hello"`, `"session_id":"s"`, "</rhizome-peer-event>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("envelope %q missing %q", got, want)
		}
	}
}

func callerReq(meta mcp.Meta) *mcp.CallToolRequest {
	return &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Meta: meta}}
}

func TestOpenCodeCaller(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  *mcp.CallToolRequest
		ok   bool
	}{
		{"nil", nil, false},
		{"no meta", callerReq(nil), false},
		{"wrong type", callerReq(mcp.Meta{openCodeMetaKey: 7}), false},
		{"bad shape", callerReq(mcp.Meta{openCodeMetaKey: "../ses"}), false},
		{"too long", callerReq(mcp.Meta{openCodeMetaKey: "ses_" + strings.Repeat("a", 65)}), false},
		{"valid", callerReq(mcp.Meta{openCodeMetaKey: "ses_ea97c3b87ffePshOUQn4PDOCU5"}), true},
	} {
		got, err := openCodeCaller(tc.req)
		if (err == nil) != tc.ok || (tc.ok && got != "ses_ea97c3b87ffePshOUQn4PDOCU5") {
			t.Fatalf("%s: %q %v", tc.name, got, err)
		}
	}
}

func openCodeManager(t *testing.T) (*openCodeNodes, string) {
	t.Helper()
	runtime := filepath.Join(shortDir(t), "run")
	if err := os.MkdirAll(runtime, 0o700); err != nil {
		t.Fatal(err)
	}
	m := newOpenCodeNodes(context.Background(), runtime)
	t.Cleanup(m.close)
	return m, runtime
}

func bindFrame(session, parent, dir, title string) bridgeFrame {
	return bridgeFrame{Type: "bind", SessionID: session, ParentID: parent, Directory: dir, Title: title}
}

func TestOpenCodeBinding(t *testing.T) {
	m, _ := openCodeManager(t)
	m.limit = 2
	dir := shortDir(t)
	m.handle(bindFrame("ses_child", "ses_parent", dir, "child"))
	m.handle(bindFrame("bad id", "", dir, "x"))
	m.handle(bindFrame("ses_rel", "", "relative/dir", "x"))
	if len(m.sessions) != 0 {
		t.Fatalf("invalid binds accepted: %v", m.sessions)
	}
	m.handle(bindFrame("ses_a", "", dir, "Fix login"))
	n, err := m.resolve(callerReq(mcp.Meta{openCodeMetaKey: "ses_a"}))
	if err != nil {
		t.Fatal(err)
	}
	id := n.local()
	if id.SessionId != "ses_a" || id.AgentType != "opencode" || id.SessionName != "Fix login" || id.DisplayName != filepath.Base(dir) || !n.external {
		t.Fatalf("identity %+v", id)
	}
	m.handle(bindFrame("ses_a", "", dir, "Renamed"))
	m.handle(bridgeFrame{Type: "title", SessionID: "ses_a", Title: "Retitled"})
	if again, _ := m.resolve(callerReq(mcp.Meta{openCodeMetaKey: "ses_a"})); again != n || n.local().SessionName != "Retitled" || n.local().Generation != 1 {
		t.Fatalf("rebind changed node or generation: %+v", n.local())
	}
	m.handle(bindFrame("ses_b", "", dir, ""))
	m.handle(bindFrame("ses_c", "", dir, ""))
	if len(m.sessions) != 2 {
		t.Fatalf("limit not enforced: %d", len(m.sessions))
	}
	if _, err := m.resolve(callerReq(mcp.Meta{openCodeMetaKey: "ses_c"})); err == nil || !strings.Contains(err.Error(), "UNBOUND") {
		t.Fatalf("unbound session resolved: %v", err)
	}
}

func TestOpenCodeCollectionLease(t *testing.T) {
	m, runtime := openCodeManager(t)
	dir := shortDir(t)
	m.handle(bindFrame("ses_a", "", dir, ""))
	if m.sessions["ses_a"] == nil || m.sessions["ses_a"].node.collectionLease == nil {
		t.Fatal("bound session holds no collection lease")
	}
	// A second process on the same runtime must not collect the same session.
	other := newOpenCodeNodes(context.Background(), runtime)
	t.Cleanup(other.close)
	other.handle(bindFrame("ses_a", "", dir, ""))
	if other.sessions["ses_a"] != nil {
		t.Fatal("second collector bound a leased session")
	}
	m.handle(bridgeFrame{Type: "forget", SessionID: "ses_a"})
	other.handle(bindFrame("ses_a", "", dir, ""))
	if other.sessions["ses_a"] == nil {
		t.Fatal("lease not released when the session was retired")
	}
}

func TestOpenCodeRetirement(t *testing.T) {
	m, _ := openCodeManager(t)
	dir := shortDir(t)
	now := time.Unix(1_000_000, 0)
	m.now = func() time.Time { return now }
	m.handle(bindFrame("ses_a", "", dir, ""))
	m.handle(bindFrame("ses_b", "", dir, ""))
	a := m.sessions["ses_a"].node
	reg := a.registration
	m.handle(bridgeFrame{Type: "forget", SessionID: "ses_a"})
	if _, ok := m.sessions["ses_a"]; ok {
		t.Fatal("forget kept session")
	}
	if _, err := os.Stat(reg); !os.IsNotExist(err) {
		t.Fatalf("registration not removed: %v", err)
	}
	now = now.Add(openCodeIdleTTL - time.Minute)
	m.sweep()
	if _, ok := m.sessions["ses_b"]; !ok {
		t.Fatal("evicted before TTL")
	}
	now = now.Add(2 * time.Minute)
	m.sweep()
	if len(m.sessions) != 0 {
		t.Fatal("idle session not evicted")
	}
}

func TestOpenCodeMCPIdentity(t *testing.T) {
	m, _ := openCodeManager(t)
	dir := shortDir(t)
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := makeServer(m.resolve, agentOpenCode, func() {}, nil).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	call := func(meta mcp.Meta, name string, args any) (*mcp.CallToolResult, map[string]any) {
		r, err := cs.CallTool(ctx, &mcp.CallToolParams{Meta: meta, Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if !r.IsError {
			json.Unmarshal([]byte(r.Content[0].(*mcp.TextContent).Text), &out)
		}
		return r, out
	}
	if r, _ := call(nil, "list_sessions", map[string]any{}); !r.IsError {
		t.Fatal("missing _meta accepted")
	}
	a := mcp.Meta{openCodeMetaKey: "ses_a"}
	if r, _ := call(a, "list_sessions", map[string]any{}); !r.IsError || !strings.Contains(r.Content[0].(*mcp.TextContent).Text, "rhizome setup --agents opencode") {
		t.Fatalf("unbound error lacks setup hint: %+v", r)
	}
	m.handle(bindFrame("ses_a", "", dir, ""))
	m.handle(bindFrame("ses_b", "", dir, ""))
	_, out := call(a, "list_sessions", map[string]any{})
	if out["identity"].(map[string]any)["session_id"] != "ses_a" {
		t.Fatalf("identity %v", out["identity"])
	}
	_, out = call(mcp.Meta{openCodeMetaKey: "ses_b"}, "list_sessions", map[string]any{})
	if out["identity"].(map[string]any)["session_id"] != "ses_b" {
		t.Fatal("callers not isolated")
	}
	if r, _ := call(a, "publish", map[string]any{"text": "hi"}); r.IsError {
		t.Fatalf("publish failed: %+v", r)
	}
	if h := m.sessions["ses_a"].node.history; len(h) != 1 || h[0].Kind != "announcement" {
		t.Fatalf("publish went to wrong node: %v", h)
	}
}

func TestOpenCodeDeliveryThroughBridge(t *testing.T) {
	m, runtime := openCodeManager(t)
	path := filepath.Join(runtime, "p.sock")
	b, err := listenBridge(path, m)
	if err != nil {
		t.Fatal(err)
	}
	b.ackTimeout = time.Second
	m.bridge = b
	b.start()
	t.Cleanup(b.close)
	p := dialPlugin(t, path, true)
	p.send(t, bindFrame("ses_a", "", shortDir(t), ""))
	eventually(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.sessions["ses_a"] != nil })
	n := m.sessions["ses_a"].node
	eventually(t, func() bool { n.mu.Lock(); defer n.mu.Unlock(); return n.deliveryReady })
	enqueue := func(text string) {
		n.mu.Lock()
		n.enqueueLocked(&pb.Event{SessionId: "peer", NodeId: "peer-node", Generation: 1, StreamId: "s/direct", Sequence: 1, Kind: "direct", Text: text, MessageId: id()}, "", "", n.identity.Generation)
		n.mu.Unlock()
	}
	enqueue("first")
	f, ok := p.read(t)
	if !ok || f.Type != "deliver" || f.SessionID != "ses_a" || !strings.Contains(f.Text, "<rhizome-peer-event>") || !strings.Contains(f.Text, "first") {
		t.Fatalf("deliver %+v", f)
	}
	p.send(t, bridgeFrame{Type: "ack", ID: f.ID})
	eventually(t, func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.stats.Written == 1 && n.stats.QueueHandoffs == 1
	})
	p.c.Close()
	eventually(t, func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return !n.deliveryReady && n.collectionState == "plugin disconnected"
	})
	enqueue("second")
	p2 := dialPlugin(t, path, true)
	f, ok = p2.read(t)
	if !ok || !strings.Contains(f.Text, "second") {
		t.Fatalf("retained delivery not sent after reconnect: %+v", f)
	}
	p2.send(t, bridgeFrame{Type: "ack", ID: f.ID})
	eventually(t, func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		for _, e := range n.history {
			if e.Kind == "gap" && strings.Contains(e.Text, "reconnected") {
				return true
			}
		}
		return false
	})
}

func TestOpenCodeShutdownRacingBind(t *testing.T) {
	m, runtime := openCodeManager(t)
	dir := shortDir(t)
	m.handle(bindFrame("ses_first", "", dir, ""))
	first := m.sessions["ses_first"].node
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			if i == 1 {
				close(started)
			}
			m.handle(bindFrame(fmt.Sprintf("ses_race%d", i), "", dir, ""))
		}
	}()
	<-started
	m.close()
	<-done
	m.mu.Lock()
	left := len(m.sessions)
	m.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d sessions survived close", left)
	}
	m.handle(bindFrame("ses_late", "", dir, ""))
	if len(m.sessions) != 0 {
		t.Fatal("bind accepted after close")
	}
	if _, err := os.Stat(first.registration); !os.IsNotExist(err) {
		t.Fatalf("registration left behind: %v", err)
	}
	entries, _ := os.ReadDir(runtime)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && strings.Contains(e.Name(), "ses_") {
			t.Fatalf("leftover runtime file %s", e.Name())
		}
	}
}

func TestOpenCodeBindWhileDisconnectedBecomesReady(t *testing.T) {
	m, _ := openCodeManager(t)
	m.handle(bindFrame("ses_a", "", shortDir(t), ""))
	n := m.sessions["ses_a"].node
	n.mu.Lock()
	ready, state := n.deliveryReady, n.collectionState
	n.mu.Unlock()
	if ready || state != "plugin disconnected" {
		t.Fatalf("bound while disconnected: %v %q", ready, state)
	}
	m.connected(true)
	n.mu.Lock()
	ready, state = n.deliveryReady, n.collectionState
	n.mu.Unlock()
	if !ready || state != "watching" {
		t.Fatalf("not ready after connect: %v %q", ready, state)
	}
	m.handle(bindFrame("ses_b", "", shortDir(t), ""))
	b := m.sessions["ses_b"].node
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.deliveryReady {
		t.Fatal("node bound while connected is not ready")
	}
}

func TestOpenCodeFrameDoesNotBlockOnWorker(t *testing.T) {
	m, _ := openCodeManager(t)
	m.handle(bindFrame("ses_a", "", shortDir(t), ""))
	n := m.sessions["ses_a"].node
	m.frameMu.Lock() // the worker blocks on its first item
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		for i := 0; i < openCodeQueueSize+100; i++ {
			m.frame(bridgeFrame{Type: "overflow"})
		}
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("frame blocked while the worker was busy")
	}
	if m.dropped.Load() == 0 {
		t.Fatal("overflow not counted")
	}
	m.frameMu.Unlock()
	eventually(t, func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		for _, e := range n.history {
			if e.Kind == "gap" && strings.Contains(e.Text, "rhizome dropped") {
				return true
			}
		}
		return false
	})
}

func TestOpenCodeAckNotStarvedByBusyWorker(t *testing.T) {
	m, runtime := openCodeManager(t)
	path := filepath.Join(runtime, "p.sock")
	b, err := listenBridge(path, m)
	if err != nil {
		t.Fatal(err)
	}
	b.ackTimeout = 5 * time.Second
	m.bridge = b
	b.start()
	t.Cleanup(b.close)
	p := dialPlugin(t, path, true)
	m.handle(bindFrame("ses_a", "", shortDir(t), ""))
	n := m.sessions["ses_a"].node
	eventually(t, func() bool { n.mu.Lock(); defer n.mu.Unlock(); return n.deliveryReady })
	m.frameMu.Lock() // simulate a slow bind/forget occupying the worker
	released := false
	release := func() {
		if !released {
			released = true
			m.frameMu.Unlock()
		}
	}
	defer release()
	p.send(t, bindFrame("ses_b", "", shortDir(t), ""))
	n.mu.Lock()
	n.enqueueLocked(&pb.Event{SessionId: "peer", NodeId: "peer-node", Generation: 1, StreamId: "s/direct", Sequence: 1, Kind: "direct", Text: "x", MessageId: id()}, "", "", n.identity.Generation)
	n.mu.Unlock()
	f, ok := p.read(t)
	if !ok || f.Type != "deliver" {
		t.Fatalf("deliver %+v", f)
	}
	p.send(t, bridgeFrame{Type: "ack", ID: f.ID})
	eventually(t, func() bool { n.mu.Lock(); defer n.mu.Unlock(); return n.stats.Written == 1 })
	release()
}
