package channel

import (
	"context"
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

const (
	openCodeMetaKey  = "ai.opencode/sessionID"
	maxOpenCodeNodes = 64
	openCodeIdleTTL  = 24 * time.Hour
)

var openCodeSession = regexp.MustCompile(`^ses_[A-Za-z0-9]{1,64}$`)

var errOpenCodeUnbound = errors.New("UNBOUND: this OpenCode session has not bound yet; install the plugin with `rhizome setup --agents opencode`, restart OpenCode, and submit a prompt")

// openCodeCaller returns the calling session from transport metadata only.
// OpenCode sets _meta["ai.opencode/sessionID"] itself; model-written tool
// arguments can never select an identity.
func openCodeCaller(r *mcp.CallToolRequest) (string, error) {
	if r == nil || r.Params == nil {
		return "", errors.New("OpenCode caller metadata missing")
	}
	s, ok := r.Params.Meta[openCodeMetaKey].(string)
	if !ok || !openCodeSession.MatchString(s) {
		return "", errors.New(`OpenCode requires verified _meta["ai.opencode/sessionID"]; update OpenCode to 2.x`)
	}
	return s, nil
}

type openCodeSessionState struct {
	node      *Node
	collector *openCodeCollector
	last      time.Time
}

// openCodeNodes keeps one Node per bound top-level OpenCode session. Binding
// comes only from the plugin bridge; tool calls only select an existing node.
type openCodeNodes struct {
	ctx      context.Context
	runtime  string
	opts     Options // network and hostname settings shared by every session node
	bridge   *bridge
	limit    int
	now      func() time.Time
	frameMu  sync.Mutex // serializes plugin frames across connection goroutines
	mu       sync.Mutex
	sessions map[string]*openCodeSessionState
	pluginUp bool
	wg       sync.WaitGroup
}

func newOpenCodeNodes(ctx context.Context, runtime string) *openCodeNodes {
	return &openCodeNodes{ctx: ctx, runtime: runtime, limit: maxOpenCodeNodes, now: time.Now, sessions: map[string]*openCodeSessionState{}}
}

func (m *openCodeNodes) resolve(r *mcp.CallToolRequest) (*Node, error) {
	sid, err := openCodeCaller(r)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[sid]
	if s == nil {
		return nil, errOpenCodeUnbound
	}
	s.last = m.now()
	return s.node, nil
}

func (m *openCodeNodes) frame(f bridgeFrame) {
	m.frameMu.Lock()
	defer m.frameMu.Unlock()
	switch f.Type {
	case "bind":
		if err := m.bind(f); err != nil {
			slog.Warn("opencode_bind", "error", err)
		}
	case "forget":
		m.retire(f.SessionID, "deleted")
	case "overflow":
		m.gapAll(fmt.Sprintf("plugin dropped %d events", f.Dropped))
	case "title", "input", "text", "tool", "turn":
		m.mu.Lock()
		s := m.sessions[f.SessionID]
		if s != nil {
			s.last = m.now()
		}
		m.mu.Unlock()
		if s == nil {
			return
		}
		gen := s.node.local().Generation
		if f.Type == "title" {
			s.node.setNativeTitle(gen, true, f.Title)
			return
		}
		s.collector.handle(s.node, gen, f)
	default:
		slog.Debug("opencode_frame_ignored", "type", f.Type)
	}
}

func (m *openCodeNodes) bind(f bridgeFrame) error {
	if !openCodeSession.MatchString(f.SessionID) {
		return errors.New("invalid OpenCode session ID")
	}
	if f.ParentID != "" {
		return errors.New("child OpenCode session cannot bind")
	}
	if !filepath.IsAbs(f.Directory) {
		return errors.New("bind requires an absolute directory")
	}
	m.mu.Lock()
	s := m.sessions[f.SessionID]
	full := s == nil && len(m.sessions) >= m.limit
	m.mu.Unlock()
	if full {
		return fmt.Errorf("OpenCode session limit reached (%d)", m.limit)
	}
	created := false
	if s == nil {
		n, err := m.newSessionNode(f.SessionID)
		if err != nil {
			return err
		}
		s, created = &openCodeSessionState{node: n, collector: newOpenCodeCollector()}, true
		m.mu.Lock()
		m.sessions[f.SessionID] = s
		m.mu.Unlock()
	}
	m.mu.Lock()
	s.last = m.now()
	m.mu.Unlock()
	n := s.node
	n.mu.Lock()
	n.identity.Project = clip(f.Directory, 180)
	n.identity.DisplayName = clip(filepath.Base(f.Directory), 40)
	if title := cleanName(f.Title); title != "" {
		n.names.custom = title
	}
	changed := n.resolveNameLocked()
	n.mu.Unlock()
	if changed || created {
		n.advertise()
	}
	return nil
}

func (m *openCodeNodes) newSessionNode(sid string) (*Node, error) {
	n := newNode(m.ctx)
	n.identity.AgentType = "opencode"
	n.ready = true
	n.external = true
	m.mu.Lock()
	n.deliveryReady = m.pluginUp
	m.mu.Unlock()
	n.collectionState = "watching"
	if !n.deliveryReady {
		n.collectionState = "plugin disconnected"
	}
	n.notify = func(ctx context.Context, text string, meta map[string]string) error {
		if m.bridge == nil {
			return errDestinationUnavailable
		}
		return m.bridge.deliver(ctx, sid, peerEnvelope(text, meta))
	}
	o := m.opts
	o.Runtime = m.runtime
	if err := n.start(o); err != nil {
		n.close()
		return nil, err
	}
	n.mu.Lock()
	n.identity.SessionId = sid
	n.identity.Generation = 1
	n.names = nameState{explicit: loadOverride(n.runtime, sid)}
	n.mu.Unlock()
	n.wg.Add(1)
	go func() { defer n.wg.Done(); n.deliver(n.ctx) }()
	slog.Info("binding", "agent", "opencode", "session_id", sid, "generation", 1)
	return n, nil
}

func (m *openCodeNodes) nodes() []*Node {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Node, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s.node)
	}
	return out
}

func (m *openCodeNodes) setReady(ready bool, state string) {
	m.mu.Lock()
	m.pluginUp = ready
	m.mu.Unlock()
	for _, n := range m.nodes() {
		n.mu.Lock()
		n.deliveryReady, n.collectionState = ready, state
		n.mu.Unlock()
	}
}

func (m *openCodeNodes) connected(reconnect bool) {
	m.setReady(true, "watching")
	if reconnect {
		m.frameMu.Lock()
		m.gapAll("plugin reconnected")
		m.frameMu.Unlock()
	}
}

func (m *openCodeNodes) disconnected() { m.setReady(false, "plugin disconnected") }

// gapAll requires frameMu so gaps order correctly with collected activity.
func (m *openCodeNodes) gapAll(reason string) {
	for _, n := range m.nodes() {
		openCodeGap(n, reason)
	}
}

func (m *openCodeNodes) retire(sid, why string) {
	m.mu.Lock()
	s := m.sessions[sid]
	delete(m.sessions, sid)
	m.mu.Unlock()
	if s == nil {
		return
	}
	s.node.close()
	slog.Info("opencode_retire", "session_id", sid, "reason", why)
}

// sweep retires sessions with no binds, activity or tool calls for the TTL.
func (m *openCodeNodes) sweep() {
	cutoff := m.now().Add(-openCodeIdleTTL)
	m.mu.Lock()
	var idle []string
	for sid, s := range m.sessions {
		if s.last.Before(cutoff) {
			idle = append(idle, sid)
		}
	}
	m.mu.Unlock()
	for _, sid := range idle {
		m.frameMu.Lock()
		m.retire(sid, "idle")
		m.frameMu.Unlock()
	}
}

func (m *openCodeNodes) sweepLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.sweep()
		}
	}
}

func (m *openCodeNodes) close() {
	m.wg.Wait()
	m.mu.Lock()
	var ids []string
	for sid := range m.sessions {
		ids = append(ids, sid)
	}
	m.mu.Unlock()
	for _, sid := range ids {
		m.retire(sid, "shutdown")
	}
}

// RunOpenCode serves one OpenCode location with resolved configuration and
// network options: MCP on stdio for tool calls and a private socket for the
// paired plugin instance.
func RunOpenCode(c config.Config, o Options) error {
	o.Runtime = c.RuntimeDir
	o.Hostname = c.Hostname
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	runtime, err := runtimeDir(c.RuntimeDir)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	path, err := bridgeSocketPath(runtime, os.Getppid(), cwd)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	manager := newOpenCodeNodes(ctx, runtime)
	manager.opts = o
	b, err := listenBridge(path, manager)
	if err != nil {
		return err
	}
	manager.bridge = b
	b.start()
	defer b.close()
	defer manager.close()
	manager.wg.Add(1)
	go func() { defer manager.wg.Done(); manager.sweepLoop(ctx) }()
	slog.Info("ready", "agent", "opencode", "binding", "plugin")
	history := newHistoryService(historyScope{Runtime: c.RuntimeDir, ClaudeRoot: c.Claude.ProjectsDir, CodexRoot: c.Codex.SessionsDir})
	session, err := makeServer(manager.resolve, agentOpenCode, func() {}, history).Connect(ctx, &mcp.StdioTransport{}, nil)
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
