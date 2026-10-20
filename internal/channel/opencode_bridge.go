package channel

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	bridgeProtocol   = 1
	maxBridgeFrame   = 64 * 1024
	maxBridgeFaults  = 16
	bridgeAckTimeout = 5 * time.Second
	maxSocketPath    = 103 // macOS sun_path is 104 bytes including the NUL
)

// errAckMissing means a delivery was written but not acknowledged; OpenCode may
// or may not have accepted it, so it is never retried.
var errAckMissing = errors.New("OpenCode delivery acknowledgement missing (acceptance unknown; not retried)")

// bridgeFrame is one newline-delimited JSON message between the OpenCode plugin
// and Rhizome. Fields are a union; Type selects which are meaningful.
type bridgeFrame struct {
	Type      string `json:"type"`
	Version   int    `json:"version,omitempty"`
	OpenCode  string `json:"opencode,omitempty"`
	SessionID string `json:"sessionID,omitempty"`
	ParentID  string `json:"parentID,omitempty"`
	Directory string `json:"directory,omitempty"`
	Title     string `json:"title,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Rhizome   bool   `json:"rhizome,omitempty"`
	MessageID string `json:"messageID,omitempty"`
	Text      string `json:"text,omitempty"`
	CallID    string `json:"callID,omitempty"`
	Name      string `json:"name,omitempty"`
	Phase     string `json:"phase,omitempty"`
	Dropped   int    `json:"dropped,omitempty"`
	ID        string `json:"id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// bridgeHandler receives plugin traffic. Calls run on the connection's read
// goroutine, which also receives acks, so implementations must return promptly
// and must never call deliver from a callback. connected and disconnected are
// serialized with each other in state-change order; frame is not. Different
// connections may call concurrently, so implementations serialize their state.
type bridgeHandler interface {
	frame(bridgeFrame)
	connected(reconnect bool)
	disconnected()
}

type bridgeStats struct{ Frames, Malformed, Oversized, Connections uint64 }

// bridgeSocketPath pairs one OpenCode plugin instance with the Rhizome process
// it spawned: the OpenCode process ID plus a hash of the project directory.
// These are pairing keys only, never routing keys.
func bridgeSocketPath(runtime string, pid int, dir string) (string, error) {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(abs))
	path := filepath.Join(runtime, fmt.Sprintf("opencode-%d-%s.sock", pid, hex.EncodeToString(sum[:])[:12]))
	if len(path) > maxSocketPath {
		return "", fmt.Errorf("bridge socket path exceeds %d bytes; set a shorter RHIZOME_RUNTIME_DIR", maxSocketPath)
	}
	return path, nil
}

// bridge serves exactly one plugin connection at a time on a private socket.
type bridge struct {
	path       string
	ln         net.Listener
	handler    bridgeHandler
	ackTimeout time.Duration
	wg         sync.WaitGroup
	cbMu       sync.Mutex // serializes connected/disconnected with the state change deciding them
	closed     bool       // guarded by mu
	writeMu    sync.Mutex
	mu         sync.Mutex
	conn       net.Conn
	ready      bool // hello accepted on conn
	helloSeen  bool
	pending    map[string]chan bridgeFrame
	stats      bridgeStats
}

func listenBridge(path string, h bridgeHandler) (*bridge, error) {
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		ln.Close()
		return nil, err
	}
	return &bridge{path: path, ln: ln, handler: h, ackTimeout: bridgeAckTimeout, pending: map[string]chan bridgeFrame{}}, nil
}

// start accepts connections until close. A new connection replaces the old one,
// which covers OpenCode reloading the plugin.
func (b *bridge) start() {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		for {
			c, err := b.ln.Accept()
			if err != nil {
				return
			}
			b.cbMu.Lock()
			b.mu.Lock()
			if b.closed {
				b.mu.Unlock()
				b.cbMu.Unlock()
				c.Close()
				return
			}
			old, wasReady := b.conn, b.ready
			b.conn, b.ready = c, false
			b.stats.Connections++
			b.mu.Unlock()
			if wasReady {
				b.handler.disconnected()
			}
			b.cbMu.Unlock()
			if old != nil {
				old.Close()
			}
			b.wg.Add(1)
			go func() { defer b.wg.Done(); b.read(c) }()
		}
	}()
}

func (b *bridge) count(f func(*bridgeStats)) {
	b.mu.Lock()
	f(&b.stats)
	b.mu.Unlock()
}

func (b *bridge) snapshot() bridgeStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stats
}

func (b *bridge) read(c net.Conn) {
	defer b.drop(c)
	s := bufio.NewScanner(c)
	s.Buffer(make([]byte, 0, 4096), maxBridgeFrame)
	faults, hello := 0, false
	for s.Scan() {
		var f bridgeFrame
		if err := json.Unmarshal(s.Bytes(), &f); err != nil || f.Type == "" {
			b.count(func(st *bridgeStats) { st.Malformed++ })
			if faults++; faults > maxBridgeFaults {
				return
			}
			continue
		}
		b.count(func(st *bridgeStats) { st.Frames++ })
		if !hello {
			if f.Type != "hello" || f.Version != bridgeProtocol {
				slog.Warn("opencode_bridge_protocol", "type", clip(f.Type, 32), "version", f.Version)
				return
			}
			hello = true
			b.cbMu.Lock()
			b.mu.Lock()
			current := b.conn == c
			reconnect := b.helloSeen
			if current {
				b.ready, b.helloSeen = true, true
			}
			b.mu.Unlock()
			if !current {
				b.cbMu.Unlock()
				return
			}
			slog.Info("opencode_plugin_connected", "opencode", clip(f.OpenCode, 32), "reconnect", reconnect)
			b.handler.connected(reconnect)
			b.cbMu.Unlock()
			continue
		}
		switch f.Type {
		case "ack", "fail":
			b.mu.Lock()
			ch := b.pending[f.ID]
			b.mu.Unlock()
			if ch != nil {
				select {
				case ch <- f:
				default:
				}
			}
		default:
			b.handler.frame(f)
		}
	}
	if errors.Is(s.Err(), bufio.ErrTooLong) {
		b.count(func(st *bridgeStats) { st.Oversized++ })
	}
}

// drop closes c. Only a current connection that completed hello reports a
// disconnect; a replaced connection was already reported at replacement time.
func (b *bridge) drop(c net.Conn) {
	c.Close()
	b.cbMu.Lock()
	defer b.cbMu.Unlock()
	b.mu.Lock()
	current := b.conn == c
	wasReady := current && b.ready
	if current {
		b.conn, b.ready = nil, false
	}
	b.mu.Unlock()
	if wasReady {
		b.handler.disconnected()
	}
}

func (b *bridge) write(c net.Conn, f bridgeFrame) error {
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(raw) >= maxBridgeFrame {
		return fmt.Errorf("bridge frame exceeds %d bytes", maxBridgeFrame)
	}
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err = c.Write(append(raw, '\n'))
	return err
}

// deliver hands text to the plugin for one session. No plugin connection means
// errDestinationUnavailable (retained by the caller); a sent but unacknowledged
// delivery is errAckMissing (never retried). A failed write means nothing
// reached OpenCode, so it is errDestinationUnavailable (retained).
func (b *bridge) deliver(ctx context.Context, sessionID, text string) error {
	msgID := id()
	ch := make(chan bridgeFrame, 1)
	b.mu.Lock()
	c := b.conn
	if c == nil || !b.ready {
		b.mu.Unlock()
		return errDestinationUnavailable
	}
	b.pending[msgID] = ch
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(b.pending, msgID); b.mu.Unlock() }()
	if err := b.write(c, bridgeFrame{Type: "deliver", ID: msgID, SessionID: sessionID, Text: text}); err != nil {
		c.Close()
		return errDestinationUnavailable
	}
	timer := time.NewTimer(b.ackTimeout)
	defer timer.Stop()
	select {
	case f := <-ch:
		if f.Type == "ack" {
			return nil
		}
		return fmt.Errorf("OpenCode refused delivery: %s", clip(f.Reason, 200))
	case <-timer.C:
		return errAckMissing
	case <-ctx.Done():
		return ctx.Err()
	}
}

// close stops accepting, closes the connection, waits for goroutines and removes
// the socket file.
func (b *bridge) close() {
	b.mu.Lock()
	b.closed = true
	c := b.conn
	b.mu.Unlock()
	b.ln.Close()
	if c != nil {
		c.Close()
	}
	b.wg.Wait()
	if err := os.Remove(b.path); err != nil && !os.IsNotExist(err) {
		slog.Warn("opencode_bridge_cleanup", "error", err)
	}
}
