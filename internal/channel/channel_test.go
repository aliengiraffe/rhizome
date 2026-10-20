package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "rhizome/proto"
)

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	end := time.Now().Add(8 * time.Second)
	for time.Now().Before(end) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition timed out")
}
func bind(t *testing.T, n *Node, session, dir string) {
	t.Helper()
	err := n.observe(Hook{SessionID: session, TranscriptPath: filepath.Join(dir, session+".jsonl"), Cwd: dir, Event: "UserPromptSubmit"})
	if err != nil {
		t.Fatal(err)
	}
}
func node(t *testing.T) *Node {
	t.Helper()
	n := newNode(context.Background())
	t.Cleanup(n.close)
	return n
}
func pair(t *testing.T) (*Node, *Node) {
	t.Helper()
	a, b := node(t), node(t)
	dir := t.TempDir()
	if err := a.start(filepath.Join(dir, "runtime")); err != nil {
		t.Fatal(err)
	}
	if err := b.start(filepath.Join(dir, "runtime")); err != nil {
		t.Fatal(err)
	}
	bind(t, a, "session-a", dir)
	bind(t, b, "session-b", dir)
	eventually(t, func() bool { _, err := b.resolve(context.Background(), "session-a"); return err == nil })
	return a, b
}
func TestBindingFences(t *testing.T) {
	n := node(t)
	dir := t.TempDir()
	if n.bound() == nil {
		t.Fatal("started bound")
	}
	bind(t, n, "one", dir)
	first := n.local()
	bind(t, n, "one", dir)
	if n.local().Generation != first.Generation {
		t.Fatal("refresh incremented")
	}
	for _, h := range []Hook{{SessionID: "two", Event: "Stop"}, {SessionID: "two", Event: "PreToolUse"}} {
		h.Cwd = dir
		h.TranscriptPath = filepath.Join(dir, "two.jsonl")
		if n.observe(h) == nil {
			t.Fatal("tool rebound")
		}
	}
	if err := n.observe(Hook{SessionID: "child", AgentID: "child", Event: "UserPromptSubmit"}); err != nil {
		t.Fatal(err)
	}
	if err := n.subscribe("remote"); err != nil {
		t.Fatal(err)
	}
	n.mu.Lock()
	n.incoming = append(n.incoming, pending{generation: first.Generation})
	n.mu.Unlock()
	bind(t, n, "two", dir)
	n.activity(first.Generation, "activity", "old data")
	n.mu.Lock()
	if len(n.feeds) != 0 || len(n.incoming) != 0 || len(n.history) != 0 {
		t.Fatal("old state survived")
	}
	n.mu.Unlock()
	if n.observe(Hook{SessionID: "one", Event: "SessionStart", Cwd: dir, TranscriptPath: filepath.Join(dir, "one.jsonl")}) == nil {
		t.Fatal("delayed lifecycle rebound retired session")
	}
}
func TestConcurrentBootstrapAndAmbiguity(t *testing.T) {
	dir := t.TempDir()
	nodes := []*Node{node(t), node(t), node(t)}
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for _, n := range nodes {
		wg.Add(1)
		go func(n *Node) { defer wg.Done(); errs <- n.start(filepath.Join(dir, "runtime")) }(n)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	endpoints := map[string]bool{}
	for idx, n := range nodes {
		ep := n.local().Endpoint
		if endpoints[ep] {
			t.Fatal("port collision")
		}
		endpoints[ep] = true
		if idx == 0 {
			bind(t, n, "same", dir)
		} else {
			if err := n.observe(Hook{SessionID: "same", TranscriptPath: filepath.Join(dir, "same.jsonl"), Cwd: dir, Event: "UserPromptSubmit"}); err == nil {
				t.Fatal("duplicate collector acquired ownership")
			}
			// Simulate a legacy peer without collection fencing: routing must
			// still fail closed when multiple direct identities are advertised.
			n.mu.Lock()
			n.identity.SessionId = "same"
			n.identity.Generation = 1
			n.mu.Unlock()
			_ = n.members.UpdateNode(time.Second) // eventual routing below is authoritative
		}
	}
	eventually(t, func() bool {
		_, err := nodes[0].resolve(context.Background(), "same")
		return err != nil && strings.Contains(err.Error(), "ambiguous")
	})
	conn, err := dial(nodes[0].local().Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = pb.NewChannelClient(conn).Info(context.Background(), &pb.InfoRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unauthenticated probe: %v", err)
	}
}
func TestFollowReplayAndDirect(t *testing.T) {
	a, b := pair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := dial(a.local().Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := pb.NewChannelClient(conn)
	a.activity(a.local().Generation, "activity", "historical")
	streamCtx, stop := context.WithCancel(ctx)
	stream, err := client.Subscribe(b.outgoing(streamCtx), &pb.FollowRequest{Target: a.local(), Subscriber: b.local()})
	if err != nil {
		t.Fatal(err)
	}
	position, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if position.Kind != "position" || position.Sequence != 1 {
		t.Fatalf("new subscription replayed history: %v", position)
	}
	a.activity(a.local().Generation, "activity", "live")
	live, err := stream.Recv()
	if err != nil || live.Text != "live" {
		t.Fatalf("live: %v %v", live, err)
	}
	stop()
	for i := 0; i < 5; i++ {
		a.activity(a.local().Generation, "activity", fmt.Sprint(i))
	}
	stream, err = client.Subscribe(b.outgoing(ctx), &pb.FollowRequest{Target: a.local(), Subscriber: b.local(), StreamId: live.StreamId, AfterSequence: live.Sequence})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		e, err := stream.Recv()
		if err != nil || e.Text != fmt.Sprint(i) {
			t.Fatalf("replay %d: %v %v", i, e, err)
		}
	}
	position, err = stream.Recv()
	if err != nil || position.Kind != "position" {
		t.Fatalf("boundary %v %v", position, err)
	}
	accepted, err := a.send(ctx, "session-b", "direct message", "")
	if err != nil || !accepted.Accepted {
		t.Fatalf("send %v %v", accepted, err)
	}
	b.mu.Lock()
	if len(b.incoming) != 1 || b.incoming[0].event.Text != "direct message" {
		t.Fatal("direct not queued")
	}
	b.mu.Unlock()
	// Expired and previous-incarnation cursors visibly signal a gap.
	for i := 0; i < 1005; i++ {
		a.activity(a.local().Generation, "activity", "eviction")
	}
	for _, sid := range []string{live.StreamId, "old-incarnation"} {
		s, err := client.Subscribe(b.outgoing(ctx), &pb.FollowRequest{Target: a.local(), Subscriber: b.local(), StreamId: sid, AfterSequence: 1})
		if err != nil {
			t.Fatal(err)
		}
		e, err := s.Recv()
		if err != nil || e.Kind != "gap" {
			t.Fatalf("expected gap %v %v", e, err)
		}
	}
}
func TestSubscriptionUnsubscribeAndRestart(t *testing.T) {
	a, b := pair(t)
	if err := b.subscribe("session-a"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return len(a.listeners) == 1 })
	if err := a.announce("hello", ""); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return len(b.incoming) == 1 })
	if err := b.unsubscribe("session-a"); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	if len(b.incoming) != 0 {
		t.Fatal("queued activity survived unsubscribe")
	}
	b.mu.Unlock()
	if err := b.subscribe("session-a"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.feeds["session-a"].status == "following" })
	// A fresh publisher stream under the same bound incarnation exercises restart cursor handling.
	a.mu.Lock()
	a.stream = id()
	a.history = nil
	a.sequence = 0
	a.historyBytes = 0
	for l := range a.listeners {
		close(l.done)
		delete(a.listeners, l)
	}
	a.mu.Unlock()
	eventually(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.feeds["session-a"].gaps > 0 })
}
func TestTranscriptPartialDedupAndTruncation(t *testing.T) {
	n := node(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	old := `{"type":"assistant","uuid":"old","message":{"content":[{"type":"text","text":"historical"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := newCollector(path)
	if err != nil {
		t.Fatal(err)
	}
	n.identity.SessionId = "session"
	n.identity.Generation = 1
	appendFile := func(s string) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString(s)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		c.scan(n, 1)
	}
	record := `{"type":"assistant","uuid":"new","message":{"content":[{"type":"thinking","text":"secret"},{"type":"text","text":"visible"},{"type":"tool_use","id":"t","name":"Bash"},{"type":"tool_use","id":"r","name":"mcp__rhizome__send"}],"stop_reason":"end_turn"}}`
	appendFile(record[:30])
	if len(n.history) != 0 {
		t.Fatal("partial emitted")
	}
	appendFile(record[30:] + "\n")
	appendFile(record + "\n")
	appendFile(`{"type":"user","uuid":"result","message":{"content":[{"type":"text","text":"<channel source=\"rhizome\">echo</channel>"},{"type":"tool_result","tool_use_id":"t","is_error":true,"content":"sensitive body"},{"type":"tool_result","tool_use_id":"r","content":"secret"}]}}` + "\n")
	if len(n.history) != 4 {
		t.Fatalf("history count %d", len(n.history))
	}
	for _, e := range n.history {
		if strings.Contains(e.Text, "secret") || strings.Contains(e.Text, "echo") || strings.Contains(e.Text, "historical") {
			t.Fatal("excluded data emitted")
		}
	}
	if err := os.WriteFile(path, []byte(`{"type":"assistant","uuid":"replacement","message":{"content":"replacement"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c.scan(n, 1)
	if n.history[4].Kind != "gap" || n.history[5].Text != "replacement" {
		t.Fatal("missing truncation signal")
	}
}
func TestBackpressureRetentionAndRate(t *testing.T) {
	n := node(t)
	bind(t, n, "local", t.TempDir())
	n.mu.Lock()
	slow := &listener{queue: make(chan *pb.Event, 1), done: make(chan struct{})}
	n.listeners[slow] = true
	n.mu.Unlock()
	for i := 0; i < 1010; i++ {
		n.activity(1, "activity", strings.Repeat("x", maxText))
	}
	select {
	case <-slow.done:
	default:
		t.Fatal("slow stream not disconnected")
	}
	n.mu.Lock()
	if len(n.history) != 1000 || n.historyBytes > 8*1024*1024 {
		t.Fatal("retention limit")
	}
	n.evictLocked(time.Now().Add(6 * time.Minute))
	if len(n.history) != 0 {
		t.Fatal("age retention")
	}
	n.mu.Unlock()
	for i := 0; i < 10; i++ {
		if err := n.announce("test", ""); err != nil {
			t.Fatal(err)
		}
	}
	if n.announce("over limit", "") == nil {
		t.Fatal("rate limit missing")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for i := 0; i < maxQueue; i++ {
		if !n.enqueueLocked(&pb.Event{NodeId: "peer", SessionId: "peer", MessageId: fmt.Sprint(i)}, "", "", 1) {
			t.Fatal("queue prematurely full")
		}
	}
	if n.enqueueLocked(&pb.Event{NodeId: "peer", MessageId: "overflow"}, "", "", 1) {
		t.Fatal("queue unbounded")
	}
	if !n.enqueueLocked(&pb.Event{NodeId: "peer", MessageId: "0"}, "", "", 1) {
		t.Fatal("retry not deduplicated")
	}
}
func TestCircuitBreakerAndRearm(t *testing.T) {
	n := node(t)
	bind(t, n, "local", t.TempDir())
	var mu sync.Mutex
	var contents []string
	n.mu.Lock()
	n.ready = true
	n.notify = func(ctx context.Context, text string, meta map[string]string) error {
		mu.Lock()
		defer mu.Unlock()
		contents = append(contents, text)
		return nil
	}
	for i := 0; i < 35; i++ {
		n.enqueueLocked(&pb.Event{NodeId: "peer", SessionId: "remote", Kind: "direct", Text: "hello", MessageId: fmt.Sprint(i)}, "", "", 1)
	}
	n.mu.Unlock()
	n.wg.Add(1)
	go func() { defer n.wg.Done(); n.deliver(n.ctx) }()
	eventually(t, func() bool { n.mu.Lock(); defer n.mu.Unlock(); return n.stats.Pauses == 1 })
	mu.Lock()
	if len(contents) != 31 {
		t.Fatalf("notifications %d", len(contents))
	}
	mu.Unlock()
	if err := n.subscribe("remote"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { n.mu.Lock(); defer n.mu.Unlock(); return len(n.incoming) == 0 })
}
func TestMCPHandshakeAndTools(t *testing.T) {
	n := node(t)
	if err := n.start(filepath.Join(t.TempDir(), "runtime")); err != nil {
		t.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	adapter := &channelTransport{base: serverTransport}
	ss, err := n.mcpServer().Connect(n.ctx, adapter, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(n.ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if cs.InitializeResult().ProtocolVersion != "2025-11-25" {
		t.Fatal("channel-incompatible protocol revision")
	}
	if _, ok := cs.InitializeResult().Capabilities.Experimental["claude/channel"]; !ok {
		t.Fatal("channel capability missing")
	}
	tools, err := cs.ListTools(n.ctx, nil)
	if err != nil || len(tools.Tools) != 7 {
		t.Fatalf("tools %v %v", tools, err)
	}
	r, err := cs.CallTool(n.ctx, &mcp.CallToolParams{Name: "publish", Arguments: map[string]any{"text": "unbound"}})
	if err != nil || !r.IsError {
		t.Fatalf("unbound publish %v %v", r, err)
	}
	r, err = cs.CallTool(n.ctx, &mcp.CallToolParams{Name: "set_session_name", Arguments: map[string]any{"name": "unbound"}})
	if err != nil || !r.IsError {
		t.Fatalf("unbound set_session_name %v %v", r, err)
	}
	dir := t.TempDir()
	r, err = cs.CallTool(n.ctx, &mcp.CallToolParams{Name: "observe_session", Arguments: Hook{SessionID: "hook-session", Cwd: dir, TranscriptPath: filepath.Join(dir, "s.jsonl"), Event: "UserPromptSubmit"}})
	if err != nil || r.IsError {
		t.Fatalf("hook %v %v", r, err)
	}
	b, _ := json.Marshal(n.status())
	if !strings.Contains(string(b), "hook-session") {
		t.Fatal("hook didn't bind")
	}
	r, err = cs.CallTool(n.ctx, &mcp.CallToolParams{Name: "set_session_name", Arguments: map[string]any{"name": "  Rhizome: naming test  "}})
	if err != nil || r.IsError {
		t.Fatalf("set_session_name %v %v", r, err)
	}
	var named map[string]string
	if json.Unmarshal([]byte(r.Content[0].(*mcp.TextContent).Text), &named) != nil || named["session_name"] != "Rhizome: naming test" || named["source"] != "explicit" {
		t.Fatalf("unexpected naming result %v", named)
	}
	r, err = cs.CallTool(n.ctx, &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
	if err != nil || r.IsError || !strings.Contains(r.Content[0].(*mcp.TextContent).Text, `"session_name":"Rhizome: naming test"`) {
		t.Fatalf("list_sessions missing name %v %v", r, err)
	}
}

func TestPollingFallbackAndHistoricalPartialLine(t *testing.T) {
	n := node(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "later", "session.jsonl")
	// Missing parent prevents installing a watch: the periodic scan must recover.
	if err := n.observe(Hook{SessionID: "poll", TranscriptPath: path, Cwd: dir, Event: "UserPromptSubmit"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"assistant","uuid":"poll","message":{"content":"poll recovered"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return len(n.history) == 1 && n.history[0].Text == "poll recovered"
	})
	other := filepath.Join(dir, "partial.jsonl")
	if err := os.WriteFile(other, []byte(`{"type":"assistant"`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := newCollector(other)
	if err != nil {
		t.Fatal(err)
	}
	c.consume([]byte(`,"message":{"content":"historical fragment"}}`+"\n"), n, 1)
	n.mu.Lock()
	if len(n.history) != 1 {
		t.Fatal("historical partial line leaked")
	}
	n.mu.Unlock()
}

func TestReplayLiveBoundaryConcurrent(t *testing.T) {
	a, b := pair(t)
	gen := a.local().Generation
	for i := 0; i < 20; i++ {
		a.activity(gen, "activity", fmt.Sprint(i))
	}
	a.mu.Lock()
	streamID := a.stream
	a.mu.Unlock()
	conn, err := dial(a.local().Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := pb.NewChannelClient(conn).Subscribe(b.outgoing(ctx), &pb.FollowRequest{Target: a.local(), Subscriber: b.local(), StreamId: streamID, AfterSequence: 0})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for i := 20; i < 60; i++ {
			a.activity(gen, "activity", fmt.Sprint(i))
			time.Sleep(time.Millisecond)
		}
	}()
	var sequence uint64
	for sequence < 60 {
		event, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind == "position" {
			continue
		}
		if event.Sequence != sequence+1 {
			t.Fatalf("replay/live discontinuity: %d after %d", event.Sequence, sequence)
		}
		sequence = event.Sequence
	}
}

func TestBatchingAndPromptExplicitDelivery(t *testing.T) {
	n := node(t)
	bind(t, n, "local", t.TempDir())
	type notification struct {
		content string
		when    time.Time
	}
	delivered := make(chan notification, 8)
	n.mu.Lock()
	n.ready = true
	n.notify = func(ctx context.Context, text string, meta map[string]string) error {
		delivered <- notification{text, time.Now()}
		return nil
	}
	for i := 0; i < 3; i++ {
		n.enqueueLocked(&pb.Event{NodeId: "peer", SessionId: "remote", Kind: "activity", Text: strings.Repeat("a", 5000), MessageId: fmt.Sprint(i)}, "", "", 1)
	}
	n.enqueueLocked(&pb.Event{NodeId: "peer", SessionId: "remote", Kind: "announcement", Text: "prompt", MessageId: "explicit"}, "", "", 1)
	n.mu.Unlock()
	n.wg.Add(1)
	go func() { defer n.wg.Done(); n.deliver(n.ctx) }()
	select {
	case d := <-delivered:
		if !strings.Contains(d.content, "prompt") {
			t.Fatal("explicit delivery waited for activity")
		}
	case <-time.After(time.Second):
		t.Fatal("explicit delayed")
	}
	var previous time.Time
	for i := 0; i < 2; i++ {
		select {
		case d := <-delivered:
			if len(d.content) > 8192 {
				t.Fatal("oversized batch")
			}
			if !previous.IsZero() && d.when.Sub(previous) < 2*time.Second {
				t.Fatal("batch rate exceeded")
			}
			previous = d.when
		case <-time.After(4 * time.Second):
			t.Fatal("automatic batch missing")
		}
	}
}
