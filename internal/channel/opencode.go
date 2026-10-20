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
	"sync/atomic"
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
	frameMu  sync.Mutex // serializes frame processing; guards closed with mu
	mu       sync.Mutex // lock order: frameMu, then mu, then Node.mu
	sessions map[string]*openCodeSessionState
	pluginUp bool
	closed   bool // set under frameMu and mu; bind refuses afterwards
	queue    chan workItem
	dropped  atomic.Uint64
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// workItem is one unit of ordered work for the manager's worker: a plugin
// frame, or (gap != "") a gap marker ordered after earlier frames.
type workItem struct {
	frame bridgeFrame
	gap   string
}

const openCodeQueueSize = 512

func newOpenCodeNodes(ctx context.Context, runtime string) *openCodeNodes {
	m := &openCodeNodes{ctx: ctx, runtime: runtime, limit: maxOpenCodeNodes, now: time.Now,
		sessions: map[string]*openCodeSessionState{}, queue: make(chan workItem, openCodeQueueSize), stop: make(chan struct{})}
	m.wg.Add(1)
	go func() { defer m.wg.Done(); m.worker() }()
	return m
}

// enqueue never blocks: it runs on the bridge read goroutine, which must keep
// receiving acks. A full queue drops the item and the worker reports a gap.
func (m *openCodeNodes) enqueue(w workItem) {
	select {
	case m.queue <- w:
	default:
		m.dropped.Add(1)
	}
}

// worker owns all session lifecycle work, in order. It stops on close or ctx.
func (m *openCodeNodes) worker() {
	for {
		select {
		case <-m.stop:
			return
		case <-m.ctx.Done():
			return
		case w := <-m.queue:
			m.process(w)
		}
	}
}

func (m *openCodeNodes) process(w workItem) {
	m.frameMu.Lock()
	defer m.frameMu.Unlock()
	if n := m.dropped.Swap(0); n > 0 {
		m.gapAll(fmt.Sprintf("rhizome dropped %d plugin events", n))
	}
	if w.gap != "" {
		m.gapAll(w.gap)
		return
	}
	m.handleLocked(w.frame)
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

// frame implements bridgeHandler. It only enqueues; see worker.
func (m *openCodeNodes) frame(f bridgeFrame) { m.enqueue(workItem{frame: f}) }

// handle processes a frame synchronously (used by tests).
func (m *openCodeNodes) handle(f bridgeFrame) {
	m.frameMu.Lock()
	defer m.frameMu.Unlock()
	m.handleLocked(f)
}

func (m *openCodeNodes) handleLocked(f bridgeFrame) {
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
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return errors.New("OpenCode manager closed")
	}
	if full {
		return fmt.Errorf("OpenCode session limit reached (%d)", m.limit)
	}
	created := false
	if s == nil {
		n, err := m.newSessionNode(f.SessionID)
		if err != nil {
			return err
		}
		// Insert and read pluginUp in one critical section so a concurrent
		// connected()/disconnected() cannot leave this node's state stale.
		m.mu.Lock()
		if existing := m.sessions[f.SessionID]; existing != nil || m.closed || len(m.sessions) >= m.limit {
			m.mu.Unlock()
			n.close()
			if existing == nil {
				return errors.New("OpenCode session rejected during bind")
			}
			s = existing
		} else {
			s, created = &openCodeSessionState{node: n, collector: newOpenCodeCollector()}, true
			n.mu.Lock()
			n.deliveryReady, n.collectionState = m.pluginUp, readyState(m.pluginUp)
			n.mu.Unlock()
			m.sessions[f.SessionID] = s
			m.mu.Unlock()
		}
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

func readyState(up bool) string {
	if up {
		return "watching"
	}
	return "plugin disconnected"
}

// setReady updates every node while holding m.mu so a bind inserting a node
// concurrently sees either the old state before insertion or this one after.
func (m *openCodeNodes) setReady(ready bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pluginUp = ready
	for _, s := range m.sessions {
		s.node.mu.Lock()
		s.node.deliveryReady, s.node.collectionState = ready, readyState(ready)
		s.node.mu.Unlock()
	}
}

func (m *openCodeNodes) connected(reconnect bool) {
	m.setReady(true)
	if reconnect {
		m.enqueue(workItem{gap: "plugin reconnected"})
	}
}

func (m *openCodeNodes) disconnected() { m.setReady(false) }

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
		m.mu.Lock()
		s := m.sessions[sid]
		stale := s != nil && s.last.Before(cutoff) // re-check: a call may have touched it
		m.mu.Unlock()
		if stale {
			m.retire(sid, "idle")
		}
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
		case <-m.stop:
			return
		case <-ticker.C:
			m.sweep()
		}
	}
}

// close stops the worker and sweeper, then retires every session. Callers
// should close the bridge first so no further frames arrive.
func (m *openCodeNodes) close() {
	m.stopOnce.Do(func() { close(m.stop) })
	m.wg.Wait()
	m.frameMu.Lock()
	defer m.frameMu.Unlock()
	m.mu.Lock()
	m.closed = true
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
	defer manager.close() // runs after b.close: no frames can arrive during teardown
	defer b.close()
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
