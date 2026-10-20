package channel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "rhizome/proto"
)

func testStore(t *testing.T, runtime, location string, now *time.Time) *openCodeStore {
	t.Helper()
	s, err := newOpenCodeStore(runtime, location)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return *now }
	return s
}

func storedIDs(t *testing.T, s *openCodeStore) []string {
	t.Helper()
	entries, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.SessionID)
	}
	return ids
}

func TestOpenCodeStore(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	for _, tc := range []struct {
		name string
		run  func(s *openCodeStore, now *time.Time)
		want string // comma-separated IDs, newest first
	}{
		{"round trip newest first", func(s *openCodeStore, now *time.Time) {
			s.touch("ses_a", base)
			s.touch("ses_b", base.Add(time.Minute))
		}, "ses_b,ses_a"},
		{"drop removes", func(s *openCodeStore, now *time.Time) {
			s.touch("ses_a", base)
			s.touch("ses_b", base)
			s.flush()
			s.drop("ses_a")
		}, "ses_b"},
		{"touch after drop restores", func(s *openCodeStore, now *time.Time) {
			s.drop("ses_a")
			s.touch("ses_a", base)
		}, "ses_a"},
		{"older touch keeps newer time", func(s *openCodeStore, now *time.Time) {
			s.touch("ses_a", base.Add(time.Hour))
			s.touch("ses_b", base.Add(time.Minute))
			s.flush()
			s.touch("ses_a", base)
		}, "ses_a,ses_b"},
		{"expired after 24h", func(s *openCodeStore, now *time.Time) {
			s.touch("ses_old", base)
			s.touch("ses_new", base.Add(2*time.Hour))
			s.flush()
			*now = base.Add(openCodeIdleTTL + time.Hour)
		}, "ses_new"},
		{"bounded at 64", func(s *openCodeStore, now *time.Time) {
			for i := 0; i < maxOpenCodeNodes+6; i++ {
				s.touch(fmt.Sprintf("ses_%03d", i), base.Add(time.Duration(i)*time.Second))
			}
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime, location := shortDir(t), shortDir(t)
			now := base.Add(time.Hour)
			s := testStore(t, runtime, location, &now)
			tc.run(s, &now)
			if err := s.flush(); err != nil {
				t.Fatal(err)
			}
			if s.dirty() {
				t.Fatal("flush left changes buffered")
			}
			// A second store for the same location reads what the first wrote.
			ids := storedIDs(t, testStore(t, runtime, location, &now))
			if tc.name == "bounded at 64" {
				if len(ids) != maxOpenCodeNodes || ids[0] != fmt.Sprintf("ses_%03d", maxOpenCodeNodes+5) || ids[len(ids)-1] != "ses_006" {
					t.Fatalf("bound not applied newest-first: %d %v", len(ids), ids)
				}
				return
			}
			if got := strings.Join(ids, ","); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestOpenCodeStoreFilesArePrivateAndScoped(t *testing.T) {
	runtime, location, other := shortDir(t), shortDir(t), shortDir(t)
	now := time.Now()
	s := testStore(t, runtime, location, &now)
	s.touch("ses_a", now)
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{s.path, s.lock} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v %v", filepath.Base(p), st.Mode(), err)
		}
	}
	st, err := os.Stat(filepath.Dir(s.path))
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("store dir mode %v %v", st.Mode(), err)
	}
	if _, err := os.Stat(s.path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
	// A symlink to the location resolves to the same store; another location does not.
	link := filepath.Join(shortDir(t), "link")
	if err := os.Symlink(location, link); err != nil {
		t.Fatal(err)
	}
	if ids := storedIDs(t, testStore(t, runtime, link, &now)); len(ids) != 1 {
		t.Fatalf("symlinked location not shared: %v", ids)
	}
	if ids := storedIDs(t, testStore(t, runtime, other, &now)); len(ids) != 0 {
		t.Fatalf("other location sees entries: %v", ids)
	}
	// A record naming another directory is ignored.
	if err := os.WriteFile(s.path, []byte(`{"directory":"/elsewhere","sessions":[{"session_id":"ses_x","last":`+fmt.Sprint(now.Unix())+`}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ids := storedIDs(t, s); len(ids) != 0 {
		t.Fatalf("foreign record accepted: %v", ids)
	}
	// Dropping the last entry removes the file.
	s.touch("ses_a", now)
	s.flush()
	s.drop("ses_a")
	s.flush()
	if _, err := os.Stat(s.path); !os.IsNotExist(err) {
		t.Fatalf("empty store file kept: %v", err)
	}
}

func storeManager(t *testing.T, runtime, location string, now *time.Time) *openCodeNodes {
	t.Helper()
	m := newOpenCodeNodes(context.Background(), runtime)
	m.now = func() time.Time { return *now }
	m.store = testStore(t, runtime, location, now)
	t.Cleanup(m.close)
	return m
}

func TestOpenCodeStoreLifecycle(t *testing.T) {
	runtime := filepath.Join(shortDir(t), "run")
	if err := os.MkdirAll(runtime, 0o700); err != nil {
		t.Fatal(err)
	}
	location := shortDir(t)
	now := time.Unix(1_000_000, 0)
	m := storeManager(t, runtime, location, &now)
	for _, sid := range []string{"ses_a", "ses_b", "ses_c", "ses_d"} {
		m.handle(bindFrame(sid, "", location, ""))
	}
	m.handle(bindFrame("ses_child", "ses_a", location, ""))
	m.flushStore()
	if got := strings.Join(storedIDs(t, m.store), ","); got != "ses_a,ses_b,ses_c,ses_d" {
		t.Fatalf("binds not stored: %q", got)
	}
	m.handle(bridgeFrame{Type: "forget", SessionID: "ses_a"})
	m.handle(bridgeFrame{Type: "unrestorable", SessionID: "ses_b"}) // bound: kept
	m.restoring = map[string]time.Time{"ses_x": now}
	m.store.touch("ses_x", now)
	m.handle(bridgeFrame{Type: "unrestorable", SessionID: "ses_x"}) // not bound: dropped
	now = now.Add(time.Hour)
	m.handle(bridgeFrame{Type: "turn", SessionID: "ses_c"}) // activity keeps ses_c alive
	now = now.Add(openCodeIdleTTL - 30*time.Minute)
	m.sweep() // ses_b and ses_d idle for more than 24h
	m.persistActivity()
	m.flushStore()
	if got := strings.Join(storedIDs(t, m.store), ","); got != "ses_c" {
		t.Fatalf("after forget/unrestorable/idle: %q", got)
	}
	m.close() // shutdown keeps entries for the next process
	if got := strings.Join(storedIDs(t, m.store), ","); got != "ses_c" {
		t.Fatalf("shutdown dropped entries: %q", got)
	}
}

func TestOpenCodeRestoreAcrossProcesses(t *testing.T) {
	runtime := filepath.Join(shortDir(t), "run")
	if err := os.MkdirAll(runtime, 0o700); err != nil {
		t.Fatal(err)
	}
	location := shortDir(t)
	now := time.Unix(2_000_000, 0)
	start := func(sock string) (*openCodeNodes, *bridge) {
		m := storeManager(t, runtime, location, &now)
		b, err := listenBridge(filepath.Join(runtime, sock), m)
		if err != nil {
			t.Fatal(err)
		}
		b.ackTimeout = 5 * time.Second
		m.bridge = b
		b.start()
		t.Cleanup(b.close)
		return m, b
	}

	// First process: a human prompt binds ses_a; nothing to restore yet.
	m1, b1 := start("a.sock")
	p1 := dialPlugin(t, b1.path, true)
	p1.send(t, bindFrame("ses_a", "", location, "Kept"))
	eventually(t, func() bool { return len(storedIDs(t, m1.store)) == 1 })
	boundAt := now
	now = now.Add(time.Hour)
	b1.close()
	m1.close()

	// Second process (OpenCode reloaded the instance): a fresh plugin connects.
	m2, b2 := start("b.sock")
	p2 := dialPlugin(t, b2.path, true)
	f, ok := p2.read(t)
	if !ok || f.Type != "restore" || strings.Join(f.Sessions, ",") != "ses_a" {
		t.Fatalf("restore frame %+v", f)
	}
	m2.mu.Lock()
	unbound := m2.sessions["ses_a"] == nil
	m2.mu.Unlock()
	if !unbound {
		t.Fatal("restore bound a session before the plugin verified it")
	}
	p2.send(t, bindFrame("ses_a", "", location, "Kept"))
	var s *openCodeSessionState
	eventually(t, func() bool { m2.mu.Lock(); defer m2.mu.Unlock(); s = m2.sessions["ses_a"]; return s != nil })
	m2.mu.Lock()
	last := s.last
	m2.mu.Unlock()
	if !last.Equal(boundAt) {
		t.Fatalf("restore refreshed activity: %v want %v", last, boundAt)
	}
	if s.collector.peer {
		t.Fatal("restored session starts in peer suppression")
	}
	n := s.node
	eventually(t, func() bool { n.mu.Lock(); defer n.mu.Unlock(); return n.deliveryReady })
	n.mu.Lock()
	n.enqueueLocked(&pb.Event{SessionId: "peer", NodeId: "peer-node", Generation: 1, StreamId: "s/direct", Sequence: 1, Kind: "direct", Text: "wake", MessageId: id()}, "", "", n.identity.Generation)
	n.mu.Unlock()
	f, ok = p2.read(t)
	if !ok || f.Type != "deliver" || f.SessionID != "ses_a" || !strings.Contains(f.Text, "wake") {
		t.Fatalf("deliver to restored session %+v", f)
	}
	p2.send(t, bridgeFrame{Type: "ack", ID: f.ID})
	eventually(t, func() bool { n.mu.Lock(); defer n.mu.Unlock(); return n.stats.Written == 1 })
	if n.local().SessionName != "Kept" {
		t.Fatalf("restored name %q", n.local().SessionName)
	}
}
