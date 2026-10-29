package channel

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"rhizome/internal/config"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// piNonceEnv carries the per-session nonce the Pi extension generates and
// passes to the MCP server it registers (pi.registerMcpServer env). Together
// with the parent process ID it pairs one MCP process with one Pi session.
const piNonceEnv = "RHIZOME_PI_NONCE"

var (
	piNonce     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	piSessionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

var errPiUnbound = errors.New("UNBOUND: this Pi session has not bound yet; install the extension with `rhizome setup --agents pi` and restart Pi")

// piSocketPath names the private bridge socket for one Pi session: the Pi
// process ID plus a hash of the nonce. The extension computes the same path.
// These are pairing keys only, never routing keys.
func piSocketPath(runtime string, pid int, nonce string) (string, error) {
	if !piNonce.MatchString(nonce) {
		return "", fmt.Errorf("%s must be 32 lowercase hex characters", piNonceEnv)
	}
	sum := sha256.Sum256([]byte(nonce))
	path := filepath.Join(runtime, fmt.Sprintf("pi-%d-%s.sock", pid, hex.EncodeToString(sum[:])[:12]))
	if len(path) > maxSocketPath {
		return "", fmt.Errorf("bridge socket path exceeds %d bytes; set a shorter RHIZOME_RUNTIME_DIR", maxSocketPath)
	}
	return path, nil
}

// piSession serves exactly one Pi session. The extension registers a fresh MCP
// server (a new process) for every session, so a second, different session ID
// on the same bridge fails closed instead of rebinding.
type piSession struct {
	ctx     context.Context
	runtime string
	nonce   string
	history *historyService
	opts    Options
	debug   bool
	window  time.Duration // recent-session window applied to the node
	bridge  *bridge
	mu      sync.Mutex // guards node, up, closed
	node    *Node
	up      bool
	closed  bool
}

func newPiSession(ctx context.Context, runtime, nonce string) *piSession {
	return &piSession{ctx: ctx, runtime: runtime, nonce: nonce}
}

// resolve returns the bound session's node, or an unbound error. The caller is
// the process itself: identity never comes from tool arguments.
func (s *piSession) resolve(*mcp.CallToolRequest) (*Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.node == nil {
		return nil, errPiUnbound
	}
	return s.node, nil
}

func (s *piSession) connected(bool) { s.setReady(true) }
func (s *piSession) disconnected()  { s.setReady(false) }

func (s *piSession) setReady(up bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.up = up
	if s.node != nil {
		s.node.mu.Lock()
		s.node.deliveryReady, s.node.collectionState = up, readyState(up)
		s.node.mu.Unlock()
	}
}

// frame implements bridgeHandler. Only bind and title are meaningful; every
// other frame type is ignored until collection lands.
func (s *piSession) frame(f bridgeFrame) {
	switch f.Type {
	case "bind":
		if err := s.bind(f); err != nil {
			slog.Warn("pi_bind", "error", err)
		}
	case "title":
		s.mu.Lock()
		n := s.node
		s.mu.Unlock()
		if n != nil && f.SessionID == n.local().SessionId {
			n.setNativeTitle(n.local().Generation, true, f.Title)
		}
	default:
		slog.Debug("pi_frame_ignored", "type", clip(f.Type, 32))
	}
}

func (s *piSession) bind(f bridgeFrame) error {
	if subtle.ConstantTimeCompare([]byte(f.Nonce), []byte(s.nonce)) != 1 {
		return errors.New("bind rejected: nonce mismatch")
	}
	if !piSessionID.MatchString(f.SessionID) {
		return errors.New("invalid Pi session ID")
	}
	if !filepath.IsAbs(f.Directory) {
		return errors.New("bind requires an absolute directory")
	}
	s.mu.Lock()
	closed, existing := s.closed, s.node
	s.mu.Unlock()
	if closed {
		return errors.New("Pi session closed")
	}
	if existing != nil {
		// Pi can emit session_start twice for one session (RPC mode); the same ID
		// is an idempotent title/directory refresh, a different one is refused.
		if existing.local().SessionId != f.SessionID {
			return errors.New("bind rejected: process already bound to a different Pi session")
		}
		s.refresh(existing, f)
		return nil
	}
	n, err := s.newNode(f.SessionID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed || s.node != nil {
		s.mu.Unlock()
		n.close()
		return errors.New("Pi session rejected during bind")
	}
	n.mu.Lock()
	n.deliveryReady, n.collectionState = s.up, readyState(s.up)
	n.mu.Unlock()
	s.node = n
	s.mu.Unlock()
	s.refresh(n, f)
	return nil
}

func (s *piSession) refresh(n *Node, f bridgeFrame) {
	n.mu.Lock()
	n.identity.Project = clip(f.Directory, 180)
	n.identity.DisplayName = clip(filepath.Base(f.Directory), 40)
	if title := cleanName(f.Title); title != "" {
		n.names.custom = title
	}
	changed := n.resolveNameLocked()
	n.mu.Unlock()
	if changed {
		n.advertise()
	}
}

func (s *piSession) newNode(sid string) (*Node, error) {
	n := newNode(s.ctx)
	n.archive = s.history
	n.recentWindow = s.window
	n.debug = s.debug
	n.identity.AgentType = "pi"
	n.ready = true
	n.external = true
	n.notify = func(ctx context.Context, text string, meta map[string]string) error {
		if s.bridge == nil {
			return errDestinationUnavailable
		}
		return s.bridge.deliver(ctx, sid, peerEnvelope(text, meta))
	}
	o := s.opts
	o.Runtime = s.runtime
	if err := n.start(o); err != nil {
		n.close()
		return nil, err
	}
	lease, err := n.claimCollection(sid)
	if err != nil {
		n.close()
		return nil, err
	}
	n.mu.Lock()
	n.collectionLease = lease
	n.identity.SessionId = sid
	n.identity.Generation = 1
	n.names = nameState{explicit: loadOverride(n.runtime, sid)}
	n.mu.Unlock()
	n.wg.Add(1)
	go func() { defer n.wg.Done(); n.deliver(n.ctx) }()
	slog.Info("binding", "agent", "pi", "session_id", sid, "generation", 1)
	return n, nil
}

func (s *piSession) close() {
	s.mu.Lock()
	s.closed = true
	n := s.node
	s.node = nil
	s.mu.Unlock()
	if n != nil {
		n.close()
	}
}

// RunPi serves one Pi session with resolved configuration and network options:
// MCP on stdio for tool calls and a private socket for the paired extension.
// Pi spawns this process through pi.registerMcpServer with RHIZOME_PI_NONCE set.
func RunPi(c config.Config, o Options) error {
	o.Runtime = c.RuntimeDir
	o.Hostname = c.Hostname
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, LogOptions(c.Debug))))
	nonce := os.Getenv(piNonceEnv)
	if !piNonce.MatchString(nonce) {
		return fmt.Errorf("%s is missing or malformed; Pi support needs the Rhizome extension (`rhizome setup --agents pi`), which registers this server per session", piNonceEnv)
	}
	runtime, err := runtimeDir(c.RuntimeDir)
	if err != nil {
		return err
	}
	if home, herr := os.UserHomeDir(); herr == nil && c.Sources["runtime_dir"] == "file" && runtime != filepath.Join(home, ".rhizome", "run") {
		slog.Warn("pi_runtime_from_config", "hint", "set RHIZOME_RUNTIME_DIR to the same runtime_dir in the Pi environment, or the extension cannot reach this process")
	}
	path, err := piSocketPath(runtime, os.Getppid(), nonce)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	history := newHistoryService(historyScope{Runtime: c.RuntimeDir, ClaudeRoot: c.Claude.ProjectsDir, CodexRoot: c.Codex.SessionsDir, OpenCodeRoot: c.OpenCode.DataDir})
	sess := newPiSession(ctx, runtime, nonce)
	sess.history = history
	sess.debug = c.Debug
	sess.window = c.RecentWindow
	sess.opts = o
	b, err := listenBridge(path, sess)
	if err != nil {
		return err
	}
	sess.bridge = b
	b.start()
	defer sess.close() // runs after b.close: no frames can arrive during teardown
	defer b.close()
	slog.Info("ready", "agent", "pi", "binding", "extension")
	session, err := makeServer(sess.resolve, agentPi, func() {}, history).Connect(ctx, &mcp.StdioTransport{}, nil)
	if err != nil {
		cancel()
		return err
	}
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case err := <-done:
		cancel()
		return err
	case <-ctx.Done():
		_ = session.Close()
		err := <-done
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
}
