package channel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shortDir returns a private directory with a path short enough for Unix sockets.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rz")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

type fakeBridgeHandler struct {
	frames chan bridgeFrame
	conns  chan bool
	drops  chan struct{}
}

func newFakeBridgeHandler() *fakeBridgeHandler {
	return &fakeBridgeHandler{frames: make(chan bridgeFrame, 64), conns: make(chan bool, 8), drops: make(chan struct{}, 8)}
}
func (h *fakeBridgeHandler) frame(f bridgeFrame)      { h.frames <- f }
func (h *fakeBridgeHandler) connected(reconnect bool) { h.conns <- reconnect }
func (h *fakeBridgeHandler) disconnected()            { h.drops <- struct{}{} }

func recv[T any](t *testing.T, ch chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting")
	}
	var zero T
	return zero
}

func startBridge(t *testing.T) (*bridge, *fakeBridgeHandler, string) {
	t.Helper()
	h := newFakeBridgeHandler()
	path := filepath.Join(shortDir(t), "b.sock")
	b, err := listenBridge(path, h)
	if err != nil {
		t.Fatal(err)
	}
	b.start()
	t.Cleanup(b.close)
	return b, h, path
}

type pluginConn struct {
	c net.Conn
	s *bufio.Scanner
}

func dialPlugin(t *testing.T, path string, hello bool) *pluginConn {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	s := bufio.NewScanner(c)
	s.Buffer(make([]byte, 0, 4096), maxBridgeFrame)
	p := &pluginConn{c, s}
	if hello {
		p.send(t, bridgeFrame{Type: "hello", Version: bridgeProtocol, OpenCode: "2.0.32"})
	}
	return p
}
func (p *pluginConn) send(t *testing.T, f bridgeFrame) {
	t.Helper()
	b, _ := json.Marshal(f)
	if _, err := p.c.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}
func (p *pluginConn) read(t *testing.T) (bridgeFrame, bool) {
	t.Helper()
	_ = p.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if !p.s.Scan() {
		return bridgeFrame{}, false
	}
	var f bridgeFrame
	if err := json.Unmarshal(p.s.Bytes(), &f); err != nil {
		t.Fatal(err)
	}
	return f, true
}

func TestBridgeSocketPath(t *testing.T) {
	dir := shortDir(t)
	real := filepath.Join(dir, "project")
	link := filepath.Join(dir, "link")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	a, err := bridgeSocketPath(dir, 42, real)
	if err != nil {
		t.Fatal(err)
	}
	b, err := bridgeSocketPath(dir, 42, link)
	if err != nil || a != b || !strings.HasPrefix(filepath.Base(a), "opencode-42-") || len(filepath.Base(a)) != len("opencode-42-")+12+len(".sock") {
		t.Fatalf("paths %q %q %v", a, b, err)
	}
	if c, _ := bridgeSocketPath(dir, 43, real); c == a {
		t.Fatal("pid must change the path")
	}
	if _, err := bridgeSocketPath(strings.Repeat("/x", 60), 42, real); err == nil {
		t.Fatal("overlong path accepted")
	}
}

func TestBridgeRequiresHelloAndForwardsFrames(t *testing.T) {
	_, h, path := startBridge(t)
	early := dialPlugin(t, path, false)
	early.send(t, bridgeFrame{Type: "bind", SessionID: "ses_a"})
	if _, ok := early.read(t); ok {
		t.Fatal("frame before hello accepted")
	}
	wrong := dialPlugin(t, path, false)
	wrong.send(t, bridgeFrame{Type: "hello", Version: bridgeProtocol + 1})
	if _, ok := wrong.read(t); ok {
		t.Fatal("protocol mismatch accepted")
	}
	p := dialPlugin(t, path, true)
	if recv(t, h.conns) {
		t.Fatal("first hello reported as reconnect")
	}
	p.send(t, bridgeFrame{Type: "bind", SessionID: "ses_a", Directory: "/tmp"})
	if f := recv(t, h.frames); f.Type != "bind" || f.SessionID != "ses_a" {
		t.Fatalf("frame %+v", f)
	}
}

func TestBridgeFaultsDisconnect(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(*testing.T, *pluginConn)
	}{
		{"malformed", func(t *testing.T, p *pluginConn) {
			for i := 0; i <= maxBridgeFaults; i++ {
				p.c.Write([]byte("not json\n"))
			}
		}},
		{"oversized", func(t *testing.T, p *pluginConn) {
			p.c.Write([]byte(strings.Repeat("x", maxBridgeFrame+1) + "\n"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, h, path := startBridge(t)
			p := dialPlugin(t, path, true)
			recv(t, h.conns)
			tc.write(t, p)
			if _, ok := p.read(t); ok {
				t.Fatal("connection survived faults")
			}
			recv(t, h.drops)
			if st := b.snapshot(); st.Malformed+st.Oversized == 0 {
				t.Fatalf("faults not counted: %+v", st)
			}
		})
	}
}

func TestBridgeDeliverOutcomes(t *testing.T) {
	b, h, path := startBridge(t)
	b.ackTimeout = 200 * time.Millisecond
	ctx := context.Background()
	if err := b.deliver(ctx, "ses_a", "x"); !errors.Is(err, errDestinationUnavailable) {
		t.Fatalf("no plugin: %v", err)
	}
	p := dialPlugin(t, path, true)
	recv(t, h.conns)
	for _, tc := range []struct {
		reply string
		check func(error) bool
	}{
		{"ack", func(err error) bool { return err == nil }},
		{"fail", func(err error) bool { return err != nil && strings.Contains(err.Error(), "boom") }},
		{"", func(err error) bool { return errors.Is(err, errAckMissing) }},
	} {
		done := make(chan error, 1)
		go func() { done <- b.deliver(ctx, "ses_a", "payload") }()
		f, ok := p.read(t)
		if !ok || f.Type != "deliver" || f.SessionID != "ses_a" || f.Text != "payload" || f.ID == "" {
			t.Fatalf("deliver frame %+v %v", f, ok)
		}
		if tc.reply != "" {
			p.send(t, bridgeFrame{Type: tc.reply, ID: f.ID, Reason: "boom"})
		}
		if err := recv(t, done); !tc.check(err) {
			t.Fatalf("reply %q: %v", tc.reply, err)
		}
	}
}

func TestBridgeReplacementAndClose(t *testing.T) {
	b, h, path := startBridge(t)
	first := dialPlugin(t, path, true)
	if recv(t, h.conns) {
		t.Fatal("first connection is not a reconnect")
	}
	second := dialPlugin(t, path, true)
	if !recv(t, h.conns) {
		t.Fatal("replacement must report reconnect")
	}
	if _, ok := first.read(t); ok {
		t.Fatal("replaced connection still open")
	}
	recv(t, h.drops) // replacing a ready connection reports its disconnect once
	second.c.Close()
	recv(t, h.drops)
	b.close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket not removed: %v", err)
	}
}

func TestListenBridgeStaleAndForeignFiles(t *testing.T) {
	dir := shortDir(t)
	stale := filepath.Join(dir, "stale.sock")
	l, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	b, err := listenBridge(stale, newFakeBridgeHandler())
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	st, _ := os.Stat(stale)
	if st.Mode().Perm() != 0600 {
		t.Fatalf("mode %v", st.Mode())
	}
	b.start()
	b.close()
	regular := filepath.Join(dir, "file.sock")
	os.WriteFile(regular, []byte("x"), 0600)
	if _, err := listenBridge(regular, newFakeBridgeHandler()); err == nil {
		t.Fatal("regular file replaced")
	}
}

func TestBridgeCloseReturnsWithOpenClient(t *testing.T) {
	b, h, path := startBridge(t)
	dialPlugin(t, path, true)
	recv(t, h.conns)
	done := make(chan struct{})
	go func() { b.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("close blocked on an open client")
	}
}

func TestBridgeReplacementWithoutHelloReportsOneDisconnect(t *testing.T) {
	b, h, path := startBridge(t)
	dialPlugin(t, path, true)
	recv(t, h.conns)
	silent := dialPlugin(t, path, false)
	recv(t, h.drops)
	silent.c.Close()
	b.close()
	if n := len(h.drops); n != 0 {
		t.Fatalf("%d extra disconnects", n)
	}
}

// An OpenCode instance reload starts the successor Rhizome on the same socket
// path before the old one exits; the old one must not unlink the successor's.
func TestBridgeCloseKeepsSuccessorSocket(t *testing.T) {
	path := filepath.Join(shortDir(t), "b.sock")
	old, err := listenBridge(path, newFakeBridgeHandler())
	if err != nil {
		t.Fatal(err)
	}
	old.start()
	h := newFakeBridgeHandler()
	next, err := listenBridge(path, h)
	if err != nil {
		t.Fatal(err)
	}
	next.start()
	t.Cleanup(next.close)
	old.close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("successor socket removed: %v", err)
	}
	dialPlugin(t, path, true)
	if recv(t, h.conns) {
		t.Fatal("first connection reported as reconnect")
	}
	next.close()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("own socket kept: %v", err)
	}
}
