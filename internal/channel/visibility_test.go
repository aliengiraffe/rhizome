package channel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"rhizome/internal/localstate"
	pb "rhizome/proto"
)

func visibilitySet(t *testing.T, runtime, session, value string) VisibilityStatus {
	t.Helper()
	out, err := SessionVisibility(context.Background(), runtime, session, value)
	if err != nil || !out.Applied || out.Visibility != value {
		t.Fatalf("visibility: %+v %v", out, err)
	}
	return out
}

func TestVisibilityPersistenceAndFailures(t *testing.T) {
	runtime := t.TempDir()
	unbound := node(t)
	unbound.runtime = runtime
	if _, err := unbound.setSessionVisibility(context.Background(), "private"); err != errUnbound {
		t.Fatal("unbound control", err)
	}
	unbound.identity.SessionId = "a"
	if _, err := unbound.setSessionVisibility(context.Background(), ""); err == nil {
		t.Fatal("empty visibility accepted")
	}
	if !sessionPublic(runtime, "a") {
		t.Fatal("default must be public")
	}
	for _, bad := range []string{"", "PRIVATE", "hidden"} {
		if bad == "" {
			continue
		}
		if _, err := SessionVisibility(context.Background(), runtime, "a", bad); err == nil {
			t.Fatal("invalid value")
		}
	}
	visibilitySet(t, runtime, "../exact", "private")
	if sessionPublic(runtime, "../exact") || !sessionPublic(runtime, "exact") {
		t.Fatal("policy key is not exact")
	}
	st, err := os.Stat(visibilityPath(runtime, "../exact"))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("private policy permissions", err)
	}
	got, err := SessionVisibility(context.Background(), runtime, "../exact", "")
	if err != nil || !got.Applied || got.Visibility != "private" {
		t.Fatal(got, err)
	}
	visibilitySet(t, runtime, "../exact", "public")
	if !sessionPublic(runtime, "../exact") {
		t.Fatal("public transition")
	}
	for _, raw := range []string{"{", strings.Repeat("x", 1025), `{"session_id":"different","visibility":"public"}`} {
		if err := os.WriteFile(visibilityPath(runtime, "../exact"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if sessionPublic(runtime, "../exact") {
			t.Fatal("malformed policy became public")
		}
	}
	if err := os.Remove(visibilityPath(runtime, "../exact")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(visibilityPath(runtime, "../exact"), 0700); err != nil {
		t.Fatal(err)
	}
	if sessionPublic(runtime, "../exact") {
		t.Fatal("unreadable policy became public")
	}
}

func TestVisibilityPartialAcknowledgement(t *testing.T) {
	runtime := t.TempDir()
	nodeID := id()
	lock, err := localstate.Lock(registrationLockPath(runtime, nodeID), false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	out, err := SessionVisibility(ctx, runtime, "a", "public")
	if err == nil || out.Applied || out.Visibility != "private" || sessionPublic(runtime, "a") {
		t.Fatal("unacknowledged process did not fail closed", out, err)
	}
}

func TestVisibilityActiveRevocation(t *testing.T) {
	a, b := pair(t)
	if err := b.subscribe("session-a"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return len(a.listeners) == 1 })
	a.activity(a.local().Generation, "activity", "retained")
	if _, err := a.send(context.Background(), "session-b", "queued", ""); err != nil {
		t.Fatal(err)
	}
	cached := a.local()
	a.mu.Lock()
	oldStream := a.stream
	a.mu.Unlock()
	visibilitySet(t, a.runtime, "session-a", "private")
	a.mu.Lock()
	if len(a.listeners)+len(a.feeds)+len(a.history)+len(a.incoming) != 0 {
		t.Fatal("own content retained")
	}
	a.mu.Unlock()
	b.mu.Lock()
	if len(b.feeds)+len(b.incoming) != 0 {
		t.Fatal("peer queues or subscriptions retained")
	}
	b.mu.Unlock()
	if i, _ := a.Info(context.Background(), &pb.InfoRequest{}); i.SessionId != "" || i.Project != "" || i.SessionName != "" {
		t.Fatal("Info disclosed identity")
	}
	if strings.Contains(string(a.NodeMeta(512)), "session-a") {
		t.Fatal("gossip disclosed identity")
	}
	if _, err := a.send(context.Background(), "session-b", "private", ""); err == nil {
		t.Fatal("private sender")
	}
	if err := a.subscribe("session-b"); err == nil {
		t.Fatal("private subscriber")
	}
	if err := a.announce("private", ""); err == nil {
		t.Fatal("private publication")
	}
	conn, err := b.dial(cached)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := pb.NewChannelClient(conn)
	sender := b.local()
	direct := &pb.Event{Kind: "direct", MessageId: id(), Text: "blocked", SessionId: sender.SessionId, NodeId: sender.NodeId, Generation: sender.Generation, StreamId: "test/direct"}
	if got, err := client.Info(b.outgoing(ctx), &pb.InfoRequest{}); err != nil || got.SessionId != "" {
		t.Fatal("cached endpoint disclosed identity", got, err)
	}
	if _, err := client.Send(b.outgoing(ctx), &pb.SendRequest{Target: cached, Sender: sender, Event: direct}); err == nil {
		t.Fatal("cached endpoint accepted send")
	}
	visibilitySet(t, a.runtime, "session-a", "public")
	stale, err := client.Subscribe(b.outgoing(ctx), &pb.FollowRequest{Target: a.local(), Subscriber: b.local(), StreamId: oldStream})
	if err == nil {
		_, err = stale.Recv()
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatal("revoked replay cursor accepted", err)
	}

	a.mu.Lock()
	sameStream := oldStream == a.stream
	a.mu.Unlock()
	if sameStream {
		t.Fatal("old stream survived revocation")
	}
	b.mu.Lock()
	if len(b.feeds) != 0 {
		t.Fatal("subscription restored")
	}
	b.mu.Unlock()
	if err := a.announce("new", ""); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(activityTexts(a), ","); strings.Contains(got, "retained") {
		t.Fatal("old replay survived")
	}
}

func TestVisibilityPersistedStartupAndConcurrentTransitions(t *testing.T) {
	runtime := filepath.Join(t.TempDir(), "runtime")
	visibilitySet(t, runtime, "session-a", "private")
	n := node(t)
	n.identity.SessionId = "session-a"
	n.identity.Project = "/private/project"
	n.identity.SessionName = "private name"
	if err := n.start(Options{Runtime: runtime}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(n.NodeMeta(512)), "private") || n.exposed().SessionId != "" {
		t.Fatal("startup exposure")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := SessionVisibility(context.Background(), runtime, "session-a", "private")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if sessionPublic(runtime, "session-a") {
		t.Fatal("concurrent transition republished")
	}
}

func TestVisibilityHistoryStaleCatalogAndCursor(t *testing.T) {
	hh := newHistoryHarness(t)
	path := filepath.Join(hh.scope.ClaudeRoot, "session.jsonl")
	text := userText("hidden", "/p", "u", "2026-01-01T00:00:00Z", "human") + assistantText("hidden", "/p", "a", "2026-01-01T00:00:01Z", "first") + assistantText("hidden", "/p", "b", "2026-01-01T00:00:02Z", "second")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	hh.index()
	page, err := hh.read(`{"session_id":"hidden","agent_type":"claude","limit":1}`)
	if err != nil {
		t.Fatal(err)
	}
	cursor := cursorOf(page)
	if cursor == "" {
		t.Fatal("missing cursor")
	}
	visibilitySet(t, hh.scope.Runtime, "hidden", "private")
	search, err := hh.search(`{}`)
	if err != nil || len(sessionsOf(search)) != 0 {
		t.Fatal("stale catalog search exposed private session", search, err)
	}
	args, _ := json.Marshal(map[string]any{"session_id": "hidden", "agent_type": "claude", "limit": 1, "cursor": cursor})
	if _, err := hh.read(string(args)); err == nil || err.Code != "SESSION_NOT_FOUND" {
		t.Fatal("private cursor allowed", err)
	}
	hh.index()
	c, _, e := hh.h.catalog.load()
	if e != nil || len(c.sessions()) != 0 {
		t.Fatal("private catalog row retained", e)
	}
	visibilitySet(t, hh.scope.Runtime, "hidden", "public")
	hh.index()
	if _, e := hh.read(`{"session_id":"hidden","agent_type":"claude"}`); e != nil {
		t.Fatal("re-publication history", e)
	}
}

func TestVisibilityFreshCollectorBaselines(t *testing.T) {
	n := node(t)
	n.runtime = t.TempDir()
	n.identity.SessionId, n.identity.Generation = "source", 1
	path := filepath.Join(t.TempDir(), "source.jsonl")
	appendLog(t, path, userText("source", "/p", "u0", "", "initial"))
	c, err := newCollector(path)
	if err != nil {
		t.Fatal(err)
	}
	visibilitySet(t, n.runtime, "source", "private")
	appendLog(t, path, assistantText("source", "/p", "private", "", "never replay"))
	// Re-publication must skip even when no collector scan ran during privacy.
	visibilitySet(t, n.runtime, "source", "public")
	c.scan(n, 1)
	appendLog(t, path, userText("source", "/p", "u1", "", "fresh human")+assistantText("source", "/p", "public", "", "fresh answer"))
	c.scan(n, 1)
	if got := strings.Join(activityTexts(n), ","); got != "fresh answer" {
		t.Fatal("baseline", got)
	}
	// Privacy must not weaken the existing retired-binding generation fence.
	n.mu.Lock()
	n.identity.Generation++
	n.mu.Unlock()
	appendLog(t, path, assistantText("source", "/p", "old", "", "retired collector"))
	c.scan(n, 1)
	if strings.Contains(strings.Join(activityTexts(n), ","), "retired collector") {
		t.Fatal("retired collector published")
	}
}

func TestVisibilityRelayBacklog(t *testing.T) {
	n := node(t)
	n.runtime = t.TempDir()
	n.identity.SessionId = "relay"
	n.identity.Generation = 1
	visibilitySet(t, n.runtime, "relay", "private")
	visibilitySet(t, n.runtime, "relay", "public")
	policy, err := loadVisibility(n.runtime, "relay")
	if err != nil {
		t.Fatal(err)
	}
	c := newOpenCodeCollector()
	for _, at := range []int64{0, policy.PublicAfter - 1, policy.PublicAfter} {
		c.handle(n, 1, bridgeFrame{Type: "input", Kind: "user", EmittedAt: at})
		c.handle(n, 1, bridgeFrame{Type: "text", Text: "backlog", EmittedAt: at})
	}
	if len(activityTexts(n)) != 0 {
		t.Fatal("buffered private activity replayed")
	}
	fresh := policy.PublicAfter + 1
	c.handle(n, 1, bridgeFrame{Type: "text", Text: "old turn", EmittedAt: fresh})
	if len(activityTexts(n)) != 0 {
		t.Fatal("resumed without human input")
	}
	c.handle(n, 1, bridgeFrame{Type: "input", Kind: "user", EmittedAt: fresh})
	c.handle(n, 1, bridgeFrame{Type: "text", Text: "fresh", EmittedAt: fresh})
	if strings.Join(activityTexts(n), ",") != "fresh" {
		t.Fatal("fresh relay input not accepted")
	}
}

func TestVisibilitySharedExportPolicy(t *testing.T) {
	runtime := t.TempDir()
	visibilitySet(t, runtime, "hidden", "private")
	if gatewayExportable(runtime, &pb.Identity{SessionId: "hidden", GatewayVersion: 1}) {
		t.Fatal("private gateway route")
	}
	if !gatewayExportable(runtime, &pb.Identity{SessionId: "public", GatewayVersion: 1}) {
		t.Fatal("unrelated gateway route")
	}
	now := time.Now()
	c := &catalogFile{Files: []*indexedFile{
		{Path: "/hidden", Agent: "claude", Session: "hidden", LastAt: &now},
		{Path: "/public", Agent: "claude", Session: "public", LastAt: &now},
	}}
	rows, omitted := recentFromCatalog(c, time.Hour, now, "host", nil, func(s *historySession) string { return s.SessionID }, runtime)
	if omitted != 0 || len(rows) != 1 || rows[0].SessionID != "public" {
		t.Fatal("private recent export", rows, omitted)
	}
}
