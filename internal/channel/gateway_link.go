package channel

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
	pb "rhizome/proto"
)

type gatewayFrame struct {
	Type       string            `json:"type"`
	ID         string            `json:"id,omitempty"`
	Sessions   []*pb.Identity    `json:"sessions,omitempty"`
	Recent     []recentSession   `json:"recent,omitempty"` // metadata only; older peers ignore it
	Send       *pb.SendRequest   `json:"send,omitempty"`
	Follow     *pb.FollowRequest `json:"follow,omitempty"`
	Event      *pb.Event         `json:"event,omitempty"`
	Acceptance *pb.Acceptance    `json:"acceptance,omitempty"`
	Error      string            `json:"error,omitempty"`
}
type gatewayLink struct {
	opencode     bool // the peer negotiated gatewaySubprotocolOpenCode
	lastSnapshot time.Time
	established  time.Time
	stopOnce     sync.Once
	reason       string // first stop reason; written once by stop
	g            *gateway
	host         string
	grant        string
	outgoing     bool
	conn         *websocket.Conn
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	writes       chan gatewayFrame
	snapshots    chan []*pb.Identity
	mu           sync.Mutex
	proxies      map[string]*gatewayProxy // remote node ID -> local proxy
	pending      map[string]chan gatewayFrame
	active       map[string]context.CancelFunc
	wg           sync.WaitGroup
}
type gatewayProxy struct {
	closed sync.Once
	link   *gatewayLink
	remote *pb.Identity
	node   *Node
}

// gatewayHostLabel shortens a host ID for diagnostics.
func gatewayHostLabel(host string) string {
	if len(host) > 12 {
		return host[:12]
	}
	return host
}
func gatewayDirection(outgoing bool) string {
	if outgoing {
		return "outgoing"
	}
	return "incoming"
}

// attachRejection returns why a connection cannot become the link for host,
// or "" when it can. The caller holds g.mu.
func (g *gateway) attachRejection(host string, outgoing bool, replacing bool) string {
	p, ok := g.state.Peers[host]
	switch {
	case g.ctx.Err() != nil:
		return "shutdown"
	case !ok:
		return "untrusted"
	case p.Pending:
		return "pending"
	}
	old := g.links[host]
	if !replacing && old != nil && (old.outgoing == (g.host < host) || outgoing != (g.host < host)) {
		// Both hosts dial; the lower host ID keeps its outgoing connection.
		return "duplicate"
	}
	return ""
}

// attach makes c the link for host and reports whether it did. It replaces an
// existing link only when the tie-break prefers the new connection.
func (g *gateway) attach(host string, c *websocket.Conn, outgoing bool) bool {
	g.attachMu.Lock()
	defer g.attachMu.Unlock()
	g.mu.Lock()
	if reason := g.attachRejection(host, outgoing, false); reason != "" {
		g.mu.Unlock()
		c.Close()
		g.logger().Info("gateway_attach_rejected", "host", gatewayHostLabel(host), "direction", gatewayDirection(outgoing), "reason", reason)
		return false
	}
	old := g.links[host]
	if old != nil {
		old.stop("replaced")
	}
	g.mu.Unlock()
	if old != nil {
		<-old.done
	}
	ctx, cancel := context.WithCancel(g.ctx)
	l := &gatewayLink{opencode: c.Subprotocol() == gatewaySubprotocolOpenCode, lastSnapshot: time.Now(), established: time.Now(), g: g, host: host, outgoing: outgoing, conn: c, ctx: ctx, cancel: cancel, done: make(chan struct{}), writes: make(chan gatewayFrame, 64), snapshots: make(chan []*pb.Identity, 1), proxies: map[string]*gatewayProxy{}, pending: map[string]chan gatewayFrame{}, active: map[string]context.CancelFunc{}}
	g.mu.Lock()
	if reason := g.attachRejection(host, outgoing, true); reason != "" {
		g.mu.Unlock()
		cancel()
		c.Close()
		g.logger().Info("gateway_attach_rejected", "host", gatewayHostLabel(host), "direction", gatewayDirection(outgoing), "reason", reason)
		return false
	}
	l.grant = g.state.Peers[host].Grant
	g.links[host] = l
	g.wg.Add(1)
	g.mu.Unlock()
	g.logger().Info("gateway_link_up", "host", gatewayHostLabel(host), "direction", gatewayDirection(outgoing))
	go func() { defer g.wg.Done(); l.run() }()
	return true
}

// stop closes the link. The first reason is kept and logged when the link
// ends; reasons are short stable labels, never session data. Once the gateway
// is shutting down every reason is "shutdown".
func (l *gatewayLink) stop(reason string) {
	if l.g.ctx.Err() != nil {
		reason = "shutdown"
	}
	l.stopOnce.Do(func() { l.reason = reason })
	l.cancel()
	_ = l.conn.Close()
}

// writeFailure classifies a write error as a stop reason. This side never
// sends a close frame itself, so ErrCloseSent means the read side received the
// peer's close and the default close handler answered it: the remote closed,
// whichever goroutine notices first.
func writeFailure(err error) string {
	if errors.Is(err, websocket.ErrCloseSent) {
		return "remote_closed"
	}
	return "write_error"
}

// readFailure classifies a read error as a stop reason.
func readFailure(err error) string {
	var closed *websocket.CloseError
	var syntax *json.SyntaxError
	var typed *json.UnmarshalTypeError
	var timeout net.Error
	switch {
	case errors.Is(err, websocket.ErrReadLimit):
		return "frame_too_large"
	case errors.As(err, &closed):
		return "remote_closed"
	case errors.As(err, &syntax), errors.As(err, &typed):
		return "invalid_frame"
	case errors.As(err, &timeout) && timeout.Timeout():
		return "read_timeout"
	}
	return "read_error"
}

// now is the gateway's clock for recency decisions; it follows the history
// service's clock so tests can pin it and falls back to wall time without one.
func (g *gateway) now() time.Time {
	if g.history != nil {
		return g.history.clock()
	}
	return time.Now()
}
func (l *gatewayLink) write(f gatewayFrame) error {
	select {
	case <-l.ctx.Done():
		return errors.New("gateway disconnected")
	default:
	}
	select {
	case l.writes <- f:
		return nil
	default:
		l.stop("write_overflow")
		return errors.New("gateway queue overflow")
	}
}
func (l *gatewayLink) run() {
	defer close(l.done)
	l.conn.SetReadLimit(gatewayMaxFrame)
	l.wg.Add(3)
	go func() { defer l.wg.Done(); l.writer() }()
	go func() { defer l.wg.Done(); l.advertise() }()
	go func() { defer l.wg.Done(); l.reconcile() }()
	for l.ctx.Err() == nil {
		_ = l.conn.SetReadDeadline(time.Now().Add(15 * time.Second))
		var f gatewayFrame
		if err := l.conn.ReadJSON(&f); err != nil {
			l.stop(readFailure(err))
			break
		}
		switch f.Type {
		case "snapshot":
			l.mu.Lock()
			l.lastSnapshot = time.Now()
			l.mu.Unlock()
			if len(f.Sessions) > gatewayMaxSessions || len(f.Recent) > maxRecentSessions {
				l.stop("snapshot_too_large")
				break
			}
			l.g.recent.set(l, f.Recent)
			select {
			case l.snapshots <- f.Sessions:
			default:
				select {
				case <-l.snapshots:
				default:
				}
				select {
				case l.snapshots <- f.Sessions:
				default:
				}
			}
		case "send", "follow":
			if len(f.ID) != 32 {
				l.stop("invalid_request")
				break
			}
			l.mu.Lock()
			_, duplicate := l.active[f.ID]
			if duplicate || len(l.active) >= 64 {
				l.mu.Unlock()
				l.stop("request_limit")
				break
			}
			ctx, cancel := context.WithCancel(l.ctx)
			l.active[f.ID] = cancel
			l.wg.Add(1)
			l.mu.Unlock()
			go func() {
				defer l.wg.Done()
				defer cancel()
				defer func() { l.mu.Lock(); delete(l.active, f.ID); l.mu.Unlock() }()
				l.serve(ctx, f)
			}()
		case "cancel":
			l.mu.Lock()
			if cancel := l.active[f.ID]; cancel != nil {
				cancel()
			}
			l.mu.Unlock()
		case "event", "done":
			l.mu.Lock()
			ch := l.pending[f.ID]
			if ch != nil {
				select {
				case ch <- f:
				default:
					l.stop("event_overflow")
				}
			}
			l.mu.Unlock()
		default:
			l.stop("unknown_frame")
		}
	}
	l.stop("closed")
	l.wg.Wait()
	l.mu.Lock()
	proxies := l.proxies
	l.proxies = map[string]*gatewayProxy{}
	l.mu.Unlock()
	for _, p := range proxies {
		p.close()
	}
	// stopOnce has completed, so reason is stable. Record the outcome before
	// the link leaves g.links so reconnect never dials ahead of the backoff.
	lifetime := time.Since(l.established)
	l.g.mu.Lock()
	if l.g.links[l.host] == l {
		delete(l.g.links, l.host)
	}
	failures, backoff := l.g.linkEndedLocked(l.host, lifetime, l.reason)
	l.g.mu.Unlock()
	// Advertisement lifetime is the connection: a disconnected or revoked
	// host's recent entries disappear with its proxies.
	l.g.recent.clear(l)
	l.g.logger().Info("gateway_link_stop", "host", gatewayHostLabel(l.host), "direction", gatewayDirection(l.outgoing), "reason", l.reason, "uptime_ms", lifetime.Milliseconds(), "proxies", len(proxies), "failures", failures, "backoff_ms", backoff.Milliseconds())
}
func (l *gatewayLink) writer() {
	for {
		select {
		case <-l.ctx.Done():
			return
		case f := <-l.writes:
			_ = l.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			encoded, err := json.Marshal(f)
			if err != nil || len(encoded) > gatewayMaxFrame {
				l.stop("frame_too_large")
				return
			}
			if err := l.conn.WriteMessage(websocket.TextMessage, encoded); err != nil {
				l.stop(writeFailure(err))
				return
			}
		}
	}
}
func (l *gatewayLink) advertise() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		l.mu.Lock()
		stale := time.Since(l.lastSnapshot) > l.g.snapshotStaleAfter()
		l.mu.Unlock()
		if stale {
			l.stop("stale_snapshot")
			return
		}
		sessions := []*pb.Identity{}
		scanCtx, stopScan := context.WithTimeout(l.ctx, 3*time.Second)
		for _, p := range logicalPeers(l.g.base.peers()) {
			if !gatewayExportable(p) || !gatewayPeerAccepts(p, l.opencode) {
				continue
			}
			// Probe before export: stale gossip never establishes remote authority.
			ctx, cancel := context.WithTimeout(scanCtx, 700*time.Millisecond)
			actual, err := l.g.base.probe(ctx, p)
			cancel()
			if err != nil || !matches(actual, p) || !gatewayExportable(actual) {
				continue
			}
			actual.Endpoint = ""
			actual.GossipEndpoint = ""
			actual.CertFingerprint = ""
			sessions = append(sessions, actual)
			if len(sessions) == gatewayMaxSessions {
				break
			}
		}
		stopScan()
		if l.write(gatewayFrame{Type: "snapshot", Sessions: sessions, Recent: l.g.recentExport(l.g.now())}) != nil {
			return
		}
		select {
		case <-l.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func validGatewayIdentity(p *pb.Identity) bool {
	return p != nil && p.Protocol == 1 && p.GatewayVersion == 1 && len(p.NodeId) == 32 && p.SessionId != "" && len(p.SessionId) <= 128 && p.Generation > 0 && p.GatewayHost == "" && p.Endpoint == "" && p.GossipEndpoint == "" && p.CertFingerprint == "" && len(p.Host) <= 64 && len(p.SessionName) <= 96 && len(p.Project) <= 180 && len(p.DisplayName) <= 40 && (p.AgentType == "claude" || p.AgentType == "codex" || p.AgentType == "opencode")
}
func (l *gatewayLink) reconcile() {
	for {
		select {
		case <-l.ctx.Done():
			return
		case list := <-l.snapshots:
			wanted := map[string]*pb.Identity{}
			for _, p := range list {
				if !validGatewayIdentity(p) || wanted[p.NodeId] != nil {
					l.stop("invalid_snapshot")
					return
				}
				wanted[p.NodeId] = p
			}
			l.mu.Lock()
			remove := []*gatewayProxy{}
			for k, p := range l.proxies {
				next := wanted[k]
				if next == nil || !matches(next, p.remote) || next.ObservationOnly != p.remote.ObservationOnly || next.AgentType != p.remote.AgentType {
					delete(l.proxies, k)
					remove = append(remove, p)
				}
			}
			l.mu.Unlock()
			for _, p := range remove {
				p.close()
			}
			for key, remote := range wanted {
				if l.ctx.Err() != nil {
					return
				}
				l.mu.Lock()
				p := l.proxies[key]
				l.mu.Unlock()
				if p != nil {
					p.node.mu.Lock()
					changed := p.node.identity.SessionName != remote.SessionName || p.node.identity.DisplayName != remote.DisplayName || p.node.identity.Project != remote.Project || p.node.identity.Host != remote.Host
					p.node.identity.SessionName = remote.SessionName
					p.node.identity.DisplayName = remote.DisplayName
					p.node.identity.Project = remote.Project
					p.node.identity.Host = remote.Host
					p.node.mu.Unlock()
					if changed {
						p.node.advertise()
					}
					continue
				}
				select {
				case l.g.proxySlots <- struct{}{}:
				default:
					l.stop("proxy_slots_exhausted")
					return
				}
				n := newNode(l.ctx)
				n.identity.Protocol = 2
				n.identity.SessionId = remote.SessionId
				n.identity.Generation = remote.Generation
				n.identity.AgentType = remote.AgentType
				n.identity.Project = remote.Project
				n.identity.DisplayName = remote.DisplayName
				n.identity.SessionName = remote.SessionName
				n.identity.ObservationOnly = remote.ObservationOnly
				n.identity.GatewayHost = l.host
				p = &gatewayProxy{link: l, remote: cloneIdentity(remote), node: n}
				n.bridge = p
				if err := n.start(Options{Runtime: l.g.runtime, Hostname: remote.Host}); err != nil {
					p.close()
					l.g.logger().Warn("gateway_proxy_start", "host", gatewayHostLabel(l.host), "error", err)
					l.stop("proxy_start_failed")
					return
				}
				l.mu.Lock()
				l.proxies[key] = p
				l.mu.Unlock()
			}
		}
	}
}
func (l *gatewayLink) request(ctx context.Context, f gatewayFrame, emit func(*pb.Event) error) (*pb.Acceptance, error) {
	f.ID = id()
	ch := make(chan gatewayFrame, 64)
	l.mu.Lock()
	if len(l.pending) >= 64 {
		l.mu.Unlock()
		return nil, errors.New("gateway request limit reached")
	}
	l.pending[f.ID] = ch
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.pending, f.ID)
		l.mu.Unlock()
		_ = l.write(gatewayFrame{Type: "cancel", ID: f.ID})
	}()
	if err := l.write(f); err != nil {
		return nil, err
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-l.ctx.Done():
			return nil, errors.New("gateway disconnected")
		case out := <-ch:
			if out.Type == "done" {
				if out.Error != "" {
					return nil, errors.New("remote gateway request rejected")
				}
				return out.Acceptance, nil
			}
			if emit == nil || out.Event == nil {
				return nil, errors.New("unexpected gateway event")
			}
			if err := emit(out.Event); err != nil {
				return nil, err
			}
		}
	}
}

// localRoute accepts only an exact, locally owned target and a source advertised
// by this authenticated connection. Endpoints supplied by the peer are never dialed.
func (l *gatewayLink) localRoute(ctx context.Context, source, target *pb.Identity) (*pb.Identity, *pb.Identity, error) {
	if source == nil || target == nil {
		return nil, nil, errors.New("missing identity")
	}
	l.mu.Lock()
	p := l.proxies[source.NodeId]
	l.mu.Unlock()
	if p == nil || !matches(p.remote, source) {
		return nil, nil, errors.New("unadvertised source")
	}
	dst, err := l.g.base.resolve(ctx, target.SessionId)
	if err != nil || !gatewayExportable(dst) || !matches(dst, target) {
		return nil, nil, errors.New("stale or ambiguous local target")
	}
	src, err := l.g.base.resolve(ctx, source.SessionId)
	if err != nil || !matches(src, p.node.local()) {
		return nil, nil, errors.New("stale or ambiguous remote source")
	}
	return src, dst, nil
}
func (l *gatewayLink) serve(ctx context.Context, f gatewayFrame) {
	var err error
	var acceptance *pb.Acceptance
	defer func() {
		out := gatewayFrame{Type: "done", ID: f.ID, Acceptance: acceptance}
		if err != nil {
			out.Error = "request rejected"
		}
		_ = l.write(out)
	}()
	if f.Type == "send" {
		if f.Send == nil || f.Send.Event == nil {
			err = errors.New("invalid send")
			return
		}
		req := proto.Clone(f.Send).(*pb.SendRequest)
		var src, dst *pb.Identity
		src, dst, err = l.localRoute(ctx, req.Sender, req.Target)
		if err != nil {
			return
		}
		if req.Event.SessionId != req.Sender.SessionId || req.Event.NodeId != req.Sender.NodeId || req.Event.Generation != req.Sender.Generation {
			err = errors.New("invalid event source")
			return
		}
		req.Sender = src
		req.Target = dst
		req.Event.NodeId = src.NodeId
		req.Event.GatewayHost = l.host
		req.Event.GatewayGrant = l.grant
		conn, e := l.g.base.dial(dst)
		if e != nil {
			err = e
			return
		}
		defer conn.Close()
		timeout, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		acceptance, err = pb.NewChannelClient(conn).Send(l.g.base.outgoing(timeout), req)
	} else {
		if f.Follow == nil {
			err = errors.New("invalid follow")
			return
		}
		req := proto.Clone(f.Follow).(*pb.FollowRequest)
		req.Subscriber, req.Target, err = l.localRoute(ctx, req.Subscriber, req.Target)
		if err != nil {
			return
		}
		conn, e := l.g.base.dial(req.Target)
		if e != nil {
			err = e
			return
		}
		defer conn.Close()
		stream, e := pb.NewChannelClient(conn).Subscribe(l.g.base.outgoing(ctx), req)
		if e != nil {
			err = e
			return
		}
		for {
			event, e := stream.Recv()
			if e != nil {
				if e != io.EOF {
					err = e
				}
				return
			}
			if err = l.write(gatewayFrame{Type: "event", ID: f.ID, Event: event}); err != nil {
				return
			}
		}
	}
}
func (p *gatewayProxy) source(ctx context.Context, source, target *pb.Identity) (*pb.Identity, error) {
	if p.link.ctx.Err() != nil {
		return nil, errors.New("gateway disconnected")
	}
	if !matches(p.node.local(), target) {
		return nil, errors.New("stale gateway target")
	}
	actual, err := p.node.resolve(ctx, source.GetSessionId())
	if err != nil || !gatewayExportable(actual) || !matches(actual, source) {
		return nil, errors.New("invalid local gateway source")
	}
	actual.Endpoint = ""
	actual.GossipEndpoint = ""
	actual.CertFingerprint = ""
	return actual, nil
}
func (p *gatewayProxy) send(ctx context.Context, req *pb.SendRequest) (*pb.Acceptance, error) {
	source, err := p.source(ctx, req.Sender, req.Target)
	if err != nil {
		return nil, err
	}
	if req.Event == nil || req.Event.Kind != "direct" || len(req.Event.Text) > maxText || len(req.Event.MessageId) > 128 || len(req.Event.ReplyTo) > 128 || len(req.Event.StreamId) > 128 {
		return nil, errors.New("invalid gateway event")
	}
	req = proto.Clone(req).(*pb.SendRequest)
	req.Sender = source
	req.Target = cloneIdentity(p.remote)
	if req.Event != nil {
		req.Event.GatewayHost = ""
		req.Event.GatewayGrant = ""
	}
	return p.link.request(ctx, gatewayFrame{Type: "send", Send: req}, nil)
}
func (p *gatewayProxy) subscribe(req *pb.FollowRequest, ss pb.Channel_SubscribeServer) error {
	source, err := p.source(ss.Context(), req.Subscriber, req.Target)
	if err != nil {
		return err
	}
	if len(req.StreamId) > 128 {
		return errors.New("invalid gateway cursor")
	}
	req = proto.Clone(req).(*pb.FollowRequest)
	req.Subscriber = source
	req.Target = cloneIdentity(p.remote)
	_, err = p.link.request(ss.Context(), gatewayFrame{Type: "follow", Follow: req}, func(e *pb.Event) error {
		if e.NodeId != p.remote.NodeId || e.SessionId != p.remote.SessionId || e.Generation != p.remote.Generation || len(e.Text) > maxText {
			return errors.New("invalid gateway event origin")
		}
		e.NodeId = p.node.local().NodeId
		e.GatewayHost = p.link.host
		e.GatewayGrant = p.link.grant
		return ss.Send(e)
	})
	return err
}

func (p *gatewayProxy) close() {
	p.closed.Do(func() { p.node.close(); <-p.link.g.proxySlots })
}

// gatewayExportable is the single export/access policy hook. Session privacy
// (#37) must extend this predicate; paired peers currently see all bound local
// sessions from bridge-capable processes, but never forwarded identities.
// gatewaySubprotocolOpenCode is offered and selected on the link's websocket
// handshake by builds whose snapshots may carry OpenCode sessions. Builds that
// predate OpenCode support select no subprotocol and reject any snapshot
// with an unknown agent type by dropping the link, so those peers never get
// OpenCode sessions.
const gatewaySubprotocolOpenCode = "rhizome.gateway.opencode.v1"

func newGatewayUpgrader() *websocket.Upgrader {
	return &websocket.Upgrader{HandshakeTimeout: 5 * time.Second, Subprotocols: []string{gatewaySubprotocolOpenCode}, CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}
}

func newGatewayDialer(tlsCfg *tls.Config) *websocket.Dialer {
	return &websocket.Dialer{TLSClientConfig: tlsCfg, HandshakeTimeout: 5 * time.Second, Proxy: nil, Subprotocols: []string{gatewaySubprotocolOpenCode}}
}

// gatewayPeerAccepts reports whether a snapshot sent to a peer may include p.
func gatewayPeerAccepts(p *pb.Identity, opencode bool) bool {
	return opencode || p.AgentType != "opencode"
}

func gatewayExportable(p *pb.Identity) bool {
	return p != nil && p.SessionId != "" && p.GatewayHost == "" && p.GatewayVersion == 1
}
