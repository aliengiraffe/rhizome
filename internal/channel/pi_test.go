package channel

import (
	"context"
	"path/filepath"
	"rhizome/internal/config"
	"strings"
	"testing"
	"time"
)

const testPiNonce = "0123456789abcdef0123456789abcdef"

// startPi runs a piSession on a real bridge socket, like RunPi without MCP.
func startPi(t *testing.T) (*piSession, string) {
	t.Helper()
	runtime := shortDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := newPiSession(ctx, runtime, testPiNonce)
	path, err := piSocketPath(runtime, 4242, testPiNonce)
	if err != nil {
		t.Fatal(err)
	}
	b, err := listenBridge(path, s)
	if err != nil {
		t.Fatal(err)
	}
	s.bridge = b
	b.start()
	t.Cleanup(func() { b.close(); s.close() })
	return s, path
}

func waitBound(t *testing.T, s *piSession) *Node {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n, err := s.resolve(nil); err == nil {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("pi session never bound")
	return nil
}

func TestPiSocketPath(t *testing.T) {
	dir := shortDir(t)
	a, err := piSocketPath(dir, 42, testPiNonce)
	if err != nil || !strings.HasPrefix(filepath.Base(a), "pi-42-") || len(filepath.Base(a)) != len("pi-42-")+12+len(".sock") {
		t.Fatalf("path %q %v", a, err)
	}
	if b, _ := piSocketPath(dir, 43, testPiNonce); b == a {
		t.Fatal("pid must change the path")
	}
	if b, _ := piSocketPath(dir, 42, strings.Repeat("f", 32)); b == a {
		t.Fatal("nonce must change the path")
	}
	for _, bad := range []string{"", "xyz", strings.ToUpper(testPiNonce), testPiNonce + "0"} {
		if _, err := piSocketPath(dir, 42, bad); err == nil {
			t.Fatalf("nonce %q accepted", bad)
		}
	}
	if _, err := piSocketPath(strings.Repeat("/x", 60), 42, testPiNonce); err == nil {
		t.Fatal("overlong path accepted")
	}
}

func TestPiBindAndList(t *testing.T) {
	s, path := startPi(t)
	if _, err := s.resolve(nil); err == nil {
		t.Fatal("resolved before bind")
	}
	dir := shortDir(t)
	p := dialPlugin(t, path, true)
	p.send(t, bridgeFrame{Type: "bind", Nonce: testPiNonce, SessionID: "01a184cd-c0f1-704a-998b-dbcef11fd346", Directory: dir})
	n := waitBound(t, s)
	id := n.local()
	if id.AgentType != "pi" || id.SessionId != "01a184cd-c0f1-704a-998b-dbcef11fd346" || id.Generation != 1 {
		t.Fatalf("identity %+v", id)
	}
	if id.Project != dir || id.DisplayName != filepath.Base(dir) {
		t.Fatalf("project %q display %q", id.Project, id.DisplayName)
	}
	if gatewayExportable(id) {
		t.Fatal("pi sessions must not be exported to paired hosts yet")
	}
	if _, ok := n.listSessions()["sessions"]; !ok {
		t.Fatal("list_sessions has no sessions key")
	}
}

func TestPiBindRejections(t *testing.T) {
	s, path := startPi(t)
	dir := shortDir(t)
	p := dialPlugin(t, path, true)
	good := bridgeFrame{Type: "bind", Nonce: testPiNonce, SessionID: "sess-1", Directory: dir}
	for _, mutate := range map[string]func(*bridgeFrame){
		"wrong nonce":    func(f *bridgeFrame) { f.Nonce = strings.Repeat("a", 32) },
		"empty nonce":    func(f *bridgeFrame) { f.Nonce = "" },
		"bad session id": func(f *bridgeFrame) { f.SessionID = "../escape" },
		"relative dir":   func(f *bridgeFrame) { f.Directory = "rel" },
	} {
		f := good
		mutate(&f)
		p.send(t, f)
	}
	time.Sleep(100 * time.Millisecond) // frames are processed in order on one goroutine
	if _, err := s.resolve(nil); err == nil {
		t.Fatal("a rejected bind produced a node")
	}
	p.send(t, good)
	waitBound(t, s)
}

func TestPiBindIdempotentAndSingleSession(t *testing.T) {
	s, path := startPi(t)
	dir := shortDir(t)
	p := dialPlugin(t, path, true)
	p.send(t, bridgeFrame{Type: "bind", Nonce: testPiNonce, SessionID: "sess-1", Directory: dir})
	first := waitBound(t, s)
	// Pi's RPC mode emits session_start twice: the same ID refreshes, never rebinds.
	p.send(t, bridgeFrame{Type: "bind", Nonce: testPiNonce, SessionID: "sess-1", Directory: dir, Title: "Refactor auth"})
	deadline := time.Now().Add(5 * time.Second)
	for first.local().SessionName != "Refactor auth" {
		if time.Now().After(deadline) {
			t.Fatal("title refresh not applied")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A different session on the same process fails closed.
	p.send(t, bridgeFrame{Type: "bind", Nonce: testPiNonce, SessionID: "sess-2", Directory: dir})
	time.Sleep(100 * time.Millisecond)
	if got, _ := s.resolve(nil); got != first || got.local().SessionId != "sess-1" {
		t.Fatal("process rebound to a different session")
	}
}

func TestPiDeliver(t *testing.T) {
	s, path := startPi(t)
	p := dialPlugin(t, path, true)
	p.send(t, bridgeFrame{Type: "bind", Nonce: testPiNonce, SessionID: "sess-1", Directory: shortDir(t)})
	n := waitBound(t, s)
	// Deliveries become ready once hello has been accepted.
	deadline := time.Now().Add(5 * time.Second)
	for {
		n.mu.Lock()
		ready := n.deliveryReady
		n.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("delivery never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	errc := make(chan error, 1)
	go func() { errc <- n.notify(context.Background(), "hello peer", map[string]string{"session_id": "x"}) }()
	f, ok := p.read(t)
	if !ok || f.Type != "deliver" || f.SessionID != "sess-1" || !strings.Contains(f.Text, "<rhizome-peer-event>") || !strings.Contains(f.Text, "hello peer") {
		t.Fatalf("deliver frame %+v ok=%v", f, ok)
	}
	p.send(t, bridgeFrame{Type: "ack", ID: f.ID})
	if err := <-errc; err != nil {
		t.Fatalf("delivery: %v", err)
	}
	// A refusal surfaces as an error and is not retried silently.
	go func() { errc <- n.notify(context.Background(), "again", nil) }()
	f, _ = p.read(t)
	p.send(t, bridgeFrame{Type: "fail", ID: f.ID, Reason: "not the active session"})
	if err := <-errc; err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("refusal error %v", err)
	}
}

func TestPiRunRequiresNonce(t *testing.T) {
	t.Setenv(piNonceEnv, "")
	if err := RunPi(config.Config{RuntimeDir: shortDir(t)}, Options{}); err == nil || !strings.Contains(err.Error(), piNonceEnv) {
		t.Fatalf("RunPi without nonce: %v", err)
	}
}
