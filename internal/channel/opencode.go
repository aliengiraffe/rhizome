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
	"rhizome/internal/localstate"
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
	saved     time.Time // last value handed to the binding store
}

// openCodeNodes keeps one Node per bound top-level OpenCode session. Binding
// comes only from the plugin bridge; tool calls only select an existing node.
type openCodeNodes struct {
	ctx          context.Context
	runtime      string
	history      *historyService
	recentWindow time.Duration
	debug        bool
	opts         Options // network and hostname settings shared by every session node
	bridge       *bridge
	store        *openCodeStore // nil disables restoration (tests)
	limit        int
	now          func() time.Time
	frameMu      sync.Mutex // serializes frame processing; guards closed with mu
	mu           sync.Mutex // lock order: frameMu, then mu, then Node.mu
	sessions     map[string]*openCodeSessionState
	// restoring maps IDs of the last restore list to their stored activity
	// time, so a restored bind keeps it instead of looking fresh. Guarded by
	// frameMu.
	restoring map[string]time.Time
	pluginUp  bool
	closed    bool // set under frameMu and mu; bind refuses afterwards
	// retries maps a session waiting for another process to release its
	// collection lease to the bind attempt in flight. Guarded by frameMu.
	retries    map[string]*bindRetry
	retryBase  time.Duration // first lease retry delay; doubles per attempt
	retryFast  time.Duration // doubling window, measured from the first failure
	retrySlow  time.Duration // fixed delay after the doubling window
	retryLimit time.Duration // total lease wait before failing closed
	// handoverChecks counts socket-ownership rechecks while disconnected;
	// handoverTimer schedules the next one. Both guarded by frameMu.
	handoverChecks int
	handoverTimer  *time.Timer
	handoverFast   time.Duration // recheck delay for the first handoverBurst checks
	handoverBurst  int
	handoverSlow   time.Duration                  // recheck delay afterwards, until reconnect or close
	onHandover     func(retired bool)             // test hook, called by the worker
	onRetry        func(sid string, r *bindRetry) // test hook: nil r means no retry pending
	queue          chan workItem
	dropped        atomic.Uint64
	stop           chan struct{}
	stopOnce       sync.Once
	wg             sync.WaitGroup
}

// workItem is one unit of ordered work for the manager's worker: a plugin
// frame, a gap marker (gap != "") ordered after earlier frames, a request to
// send the plugin its restore list (restore), a socket-ownership check after a
// disconnect (handover), or a retried bind (retry).
type workItem struct {
	frame    bridgeFrame
	gap      string
	restore  bool
	handover bool
	retry    *bindRetry
}

// bindRetry is one pending bind waiting for another process's collection
// lease; deadline bounds the total wait.
type bindRetry struct {
	frame   bridgeFrame
	attempt int
	first   time.Time // first busy attempt
	slow    bool      // this retry waits at the slow cadence
}

const (
	openCodeLeaseRetryBase  = 100 * time.Millisecond
	openCodeLeaseRetryFast  = 10 * time.Second
	openCodeLeaseRetrySlow  = 5 * time.Second
	openCodeLeaseRetryLimit = 60 * time.Second
	openCodeHandoverFast    = 500 * time.Millisecond
	openCodeHandoverBurst   = 20
	openCodeHandoverSlow    = 5 * time.Second
)

const openCodeQueueSize = 512

func newOpenCodeNodes(ctx context.Context, runtime string) *openCodeNodes {
	m := &openCodeNodes{ctx: ctx, runtime: runtime, limit: maxOpenCodeNodes, now: time.Now,
		sessions: map[string]*openCodeSessionState{}, queue: make(chan workItem, openCodeQueueSize), stop: make(chan struct{}),
		retries: map[string]*bindRetry{}, retryBase: openCodeLeaseRetryBase, retryFast: openCodeLeaseRetryFast,
		retrySlow: openCodeLeaseRetrySlow, retryLimit: openCodeLeaseRetryLimit,
		handoverFast: openCodeHandoverFast, handoverBurst: openCodeHandoverBurst, handoverSlow: openCodeHandoverSlow}
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
			m.flushStore() // after process: frameMu is released
		}
	}
}

func (m *openCodeNodes) process(w workItem) {
	// Node start and close (network work) deliberately run under frameMu so
	// session lifecycle stays serialized. Tool calls are not blocked by this:
	// resolve takes only mu.
	m.frameMu.Lock()
	defer m.frameMu.Unlock()
	if n := m.dropped.Swap(0); n > 0 {
		m.gapAll(fmt.Sprintf("rhizome dropped %d plugin events", n))
	}
	if w.gap != "" {
		m.gapAll(w.gap)
		return
	}
	if w.restore {
		m.stopHandoverLocked()
		m.handoverChecks = 0
		m.sendRestore()
		return
	}
	if w.handover {
		m.checkHandover()
		return
	}
	if w.retry != nil {
		if m.retries[w.retry.frame.SessionID] != w.retry {
			return // superseded by a newer bind or cancelled
		}
		m.bindWithRetry(w.retry)
		return
	}
	m.handleLocked(w.frame)
}

// checkHandover runs after the plugin disconnects. If another process (an
// OpenCode instance reload's successor) has taken over this location's socket
// path, this process will not get the plugin back: retire every session as a
// shutdown, keeping store entries, so the successor can claim the collection
// leases without waiting for this process to exit. While the socket is still
// ours a reconnect keeps the sessions; recheck quickly at first, then slowly
// until the plugin reconnects or the manager closes. Requires frameMu.
func (m *openCodeNodes) checkHandover() {
	m.stopHandoverLocked() // at most one recheck is ever scheduled
	m.mu.Lock()
	up := m.pluginUp
	m.mu.Unlock()
	if up || m.bridge == nil {
		return
	}
	if m.bridge.owned() {
		delay := m.handoverSlow
		if m.handoverChecks < m.handoverBurst {
			delay = m.handoverFast
		}
		m.handoverChecks++
		m.handoverTimer = time.AfterFunc(delay, func() { m.enqueue(workItem{handover: true}) })
		if m.onHandover != nil {
			m.onHandover(false)
		}
		return
	}
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for sid := range m.sessions {
		ids = append(ids, sid)
	}
	m.mu.Unlock()
	clear(m.retries) // this process no longer serves the location
	for _, sid := range ids {
		m.retire(sid, "shutdown")
	}
	if len(ids) > 0 {
		slog.Info("opencode_handover", "sessions", len(ids))
	}
	if m.onHandover != nil {
		m.onHandover(true)
	}
}

// stopHandoverLocked cancels a scheduled ownership recheck. Requires frameMu.
func (m *openCodeNodes) stopHandoverLocked() {
	if m.handoverTimer != nil {
		m.handoverTimer.Stop()
		m.handoverTimer = nil
	}
}

// bindWithRetry binds r.frame. When another process still holds the session's
// collection lease (typically the predecessor during an instance reload), it
// retries with doubling delays during retryFast, then every retrySlow until
// retryLimit, then fails closed and keeps the store entry so the next prompt or
// reload can restore it. Requires frameMu; each wait runs in a manager-owned
// goroutine that holds no lock.
func (m *openCodeNodes) bindWithRetry(r *bindRetry) {
	sid := r.frame.SessionID
	err := m.bind(r.frame)
	if err == nil || !errors.Is(err, localstate.ErrBusy) {
		delete(m.retries, sid)
		if err != nil {
			slog.Warn("opencode_bind", "error", err)
		}
		m.retryHook(sid, nil)
		return
	}
	now := m.now()
	first := r.first
	if r.attempt == 0 {
		first = now
	}
	delay, slow := m.retryBase<<min(r.attempt, 6), false
	if now.Add(delay).After(first.Add(m.retryFast)) {
		delay, slow = m.retrySlow, true
	}
	if !now.Add(delay).Before(first.Add(m.retryLimit)) {
		delete(m.retries, sid)
		slog.Warn("opencode_bind", "error", err, "attempts", r.attempt+1)
		m.retryHook(sid, nil)
		return
	}
	next := &bindRetry{frame: r.frame, attempt: r.attempt + 1, first: first, slow: slow}
	m.retries[sid] = next
	m.retryHook(sid, next)
	m.wg.Add(1) // the worker holds a count, so this never races wg.Wait
	go func() {
		defer m.wg.Done()
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-m.stop:
			return
		case <-m.ctx.Done():
			return
		}
		select {
		case m.queue <- workItem{retry: next}:
		default:
			// Queue full: give up this retry instead of leaving it pending.
			m.dropped.Add(1)
			m.frameMu.Lock()
			if m.retries[sid] == next {
				delete(m.retries, sid)
				slog.Warn("opencode_bind", "error", "retry dropped: manager queue full")
				m.retryHook(sid, nil)
			}
			m.frameMu.Unlock()
		}
	}()
}

// retryHook reports retry state to tests. Requires frameMu.
func (m *openCodeNodes) retryHook(sid string, r *bindRetry) {
	if m.onRetry != nil {
		m.onRetry(sid, r)
	}
}

// sendRestore offers a newly connected plugin every session this location
// previously bound from a human prompt. The plugin verifies each one exactly as
// for a prompt bind and answers with ordinary bind frames, or "unrestorable".
func (m *openCodeNodes) sendRestore() {
	entries, err := m.store.load()
	if err != nil {
		slog.Warn("opencode_restore", "error", err)
		return
	}
	if len(entries) == 0 || m.bridge == nil {
		return
	}
	m.restoring = make(map[string]time.Time, len(entries))
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		m.restoring[e.SessionID] = time.Unix(e.Last, 0)
		ids = append(ids, e.SessionID)
	}
	if err := m.bridge.send(bridgeFrame{Type: "restore", Sessions: ids}); err != nil {
		slog.Warn("opencode_restore", "error", err, "sessions", len(ids))
		return
	}
	slog.Info("opencode_restore", "sessions", len(ids))
}

// flushStore writes buffered store changes. It must not run under frameMu or mu.
func (m *openCodeNodes) flushStore() {
	if !m.store.dirty() {
		return
	}
	if err := m.store.flush(); err != nil {
		slog.Warn("opencode_store", "error", err)
	}
}

// persistActivity hands each bound session's newer activity time to the store.
func (m *openCodeNodes) persistActivity() {
	if m.store == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for sid, s := range m.sessions {
		if s.last.After(s.saved) {
			m.store.touch(sid, s.last)
			s.saved = s.last
		}
	}
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
		delete(m.retries, f.SessionID) // a newer bind supersedes a pending retry
		m.bindWithRetry(&bindRetry{frame: f})
	case "forget":
		delete(m.retries, f.SessionID)
		m.retire(f.SessionID, "deleted")
	case "unrestorable":
		// The plugin could not verify a restore entry (missing, child or another
		// location's session). Forget it unless it is bound right now.
		if !openCodeSession.MatchString(f.SessionID) {
			return
		}
		delete(m.restoring, f.SessionID)
		m.mu.Lock()
		bound := m.sessions[f.SessionID] != nil
		m.mu.Unlock()
		if !bound {
			m.store.drop(f.SessionID)
		}
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
	last := m.now()
	if t, ok := m.restoring[f.SessionID]; ok {
		delete(m.restoring, f.SessionID)
		if created {
			last = t // a restore is not activity
		}
	}
	m.mu.Lock()
	if last.After(s.last) {
		s.last = last
	}
	if s.last.After(s.saved) {
		m.store.touch(f.SessionID, s.last)
		s.saved = s.last
	}
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
	n.archive = m.history
	n.recentWindow = m.recentWindow
	n.debug = m.debug
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
	// A direct integration holds the per-session collection lease for the
	// node's lifetime, so a second process cannot collect the same session.
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
	m.enqueue(workItem{restore: true})
}

func (m *openCodeNodes) disconnected() {
	m.setReady(false)
	m.enqueue(workItem{handover: true})
}

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
	if why != "shutdown" {
		m.store.drop(sid) // a shutdown keeps the entry for the next process
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
			m.persistActivity()
			m.flushStore()
		}
	}
}

// close stops the worker and sweeper, then retires every session. Callers
// should close the bridge first so no further frames arrive.
func (m *openCodeNodes) close() {
	m.stopOnce.Do(func() { close(m.stop) })
	m.wg.Wait()
	m.frameMu.Lock()
	m.stopHandoverLocked()
	m.persistActivity()
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
	m.frameMu.Unlock()
	m.flushStore()
}

// RunOpenCode serves one OpenCode location with resolved configuration and
// network options: MCP on stdio for tool calls and a private socket for the
// paired plugin instance.
func RunOpenCode(c config.Config, o Options) error {
	o.Runtime = c.RuntimeDir
	o.Hostname = c.Hostname
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, LogOptions(c.Debug))))
	runtime, err := runtimeDir(c.RuntimeDir)
	if err != nil {
		return err
	}
	if home, herr := os.UserHomeDir(); herr == nil && c.Sources["runtime_dir"] == "file" && runtime != filepath.Join(home, ".rhizome", "run") {
		// The plugin cannot read config.toml: it finds the socket only through
		// RHIZOME_RUNTIME_DIR in OpenCode's environment or the default directory.
		slog.Warn("opencode_runtime_from_config", "hint", "set RHIZOME_RUNTIME_DIR to the same runtime_dir in the OpenCode environment, or the plugin cannot reach this process")
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
	store, err := newOpenCodeStore(runtime, os.Getppid(), cwd)
	if err != nil {
		return err
	}
	history := newHistoryService(historyScope{Runtime: c.RuntimeDir, ClaudeRoot: c.Claude.ProjectsDir, CodexRoot: c.Codex.SessionsDir, OpenCodeRoot: c.OpenCode.DataDir})
	manager := newOpenCodeNodes(ctx, runtime)
	manager.history = history
	manager.recentWindow = c.RecentWindow
	manager.debug = c.Debug
	manager.opts = o
	manager.store = store
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
