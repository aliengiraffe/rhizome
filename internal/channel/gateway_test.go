package channel

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"golang.org/x/time/rate"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"rhizome/internal/config"
	pb "rhizome/proto"
)

func gatewayUntil(t *testing.T, check func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if check() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("gateway condition timed out")
		case <-ticker.C:
		}
	}
}

type gatewayHarness struct {
	runtime string
	cfg     config.Gateway
	history GatewayHistory
	cancel  context.CancelFunc
	done    chan error
}

func startGatewayHarness(t *testing.T) *gatewayHarness {
	t.Helper()
	return startGatewayHarnessWith(t, "", GatewayHistory{})
}

// startGatewayHarnessWith serves history from runtime (a fresh one when empty).
func startGatewayHarnessWith(t *testing.T, runtime string, history GatewayHistory) *gatewayHarness {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	if runtime == "" {
		runtime = filepath.Join(t.TempDir(), "run")
	}
	h := &gatewayHarness{runtime: runtime, cfg: config.Gateway{Mode: "lan", Listen: address}, history: history}
	h.start(t)
	t.Cleanup(func() { h.stop(t) })
	return h
}
func (h *gatewayHarness) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)
	go func() { h.done <- RunGateway(ctx, h.runtime, h.cfg, h.history) }()
	gatewayUntil(t, func() bool {
		select {
		case err := <-h.done:
			t.Fatalf("gateway startup: %v", err)
		default:
		}
		_, err := GatewayAdmin(context.Background(), h.runtime, GatewayCommand{Action: "peers"})
		return err == nil
	})
}
func (h *gatewayHarness) stop(t *testing.T) {
	t.Helper()
	if h.cancel == nil {
		return
	}
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil {
			t.Errorf("gateway stop: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Error("gateway shutdown timeout")
	}
	h.cancel = nil
}
func (h *gatewayHarness) command(t *testing.T, cmd GatewayCommand) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := GatewayAdmin(ctx, h.runtime, cmd)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func (h *gatewayHarness) invite(t *testing.T) string {
	t.Helper()
	var out map[string]string
	if err := json.Unmarshal(h.command(t, GatewayCommand{Action: "invite", Expires: "10m"}), &out); err != nil {
		t.Fatal(err)
	}
	return out["invitation"]
}
func gatewayTestNode(t *testing.T, h *gatewayHarness, session string) *Node {
	t.Helper()
	n := node(t)
	if err := n.start(Options{Runtime: h.runtime}); err != nil {
		t.Fatal(err)
	}
	bind(t, n, session, t.TempDir())
	return n
}
func TestGatewayPairRouteRevokeRestart(t *testing.T) {
	a, b := startGatewayHarness(t), startGatewayHarness(t)
	na, nb := gatewayTestNode(t, a, "gateway-a"), gatewayTestNode(t, b, "gateway-b")
	tokenA, _ := os.ReadFile(filepath.Join(a.runtime, "token"))
	tokenB, _ := os.ReadFile(filepath.Join(b.runtime, "token"))
	if string(tokenA) == string(tokenB) {
		t.Fatal("tokens should differ")
	}
	code := a.invite(t)
	inv, err := parseGatewayInvitation(code)
	if err != nil {
		t.Fatal(err)
	}
	b.command(t, GatewayCommand{Action: "join", Code: code})
	gatewayUntil(t, func() bool {
		p, e := na.resolve(context.Background(), "gateway-b")
		return e == nil && p.GatewayHost != ""
	})
	gatewayUntil(t, func() bool { _, e := nb.resolve(context.Background(), "gateway-a"); return e == nil })
	for _, pair := range [][2]*Node{{na, nb}, {nb, na}} {
		result, err := pair[0].send(context.Background(), pair[1].local().SessionId, "gateway direct", "")
		if err != nil || !result.Accepted {
			t.Fatalf("send: %v %v", result, err)
		}
		pair[1].mu.Lock()
		if len(pair[1].incoming) != 1 || pair[1].incoming[0].event.GatewayHost == "" || pair[1].incoming[0].event.GatewayGrant == "" {
			t.Error("missing attributed delivery")
		}
		pair[1].mu.Unlock()
	}
	if err := nb.subscribe("gateway-a"); err != nil {
		t.Fatal(err)
	}
	gatewayUntil(t, func() bool { na.mu.Lock(); defer na.mu.Unlock(); return len(na.listeners) > 0 })
	na.activity(na.local().Generation, "activity", "gateway activity")
	gatewayUntil(t, func() bool {
		nb.mu.Lock()
		defer nb.mu.Unlock()
		for _, p := range nb.incoming {
			if p.event.Text == "gateway activity" {
				return true
			}
		}
		return false
	})
	// Same admitted identity can recover a lost enrollment response.
	b.command(t, GatewayCommand{Action: "join", Code: code})
	a.stop(t)
	a.start(t)
	gatewayUntil(t, func() bool { _, e := nb.resolve(context.Background(), "gateway-a"); return e == nil })
	gatewayUntil(t, func() bool { _, e := nb.send(context.Background(), "gateway-a", "after restart", ""); return e == nil })
	b.command(t, GatewayCommand{Action: "revoke", ID: inv.Host})
	nb.wg.Add(1)
	go func() { defer nb.wg.Done(); nb.deliver(nb.ctx) }()
	gatewayUntil(t, func() bool {
		nb.mu.Lock()
		defer nb.mu.Unlock()
		return len(nb.incoming) == 0 && nb.feeds["gateway-a"] == nil
	})
	if _, err := GatewayAdmin(context.Background(), b.runtime, GatewayCommand{Action: "join", Code: code}); err == nil {
		t.Fatal("old invitation undid local revocation")
	}
	if grants := gatewayDeliveryGrants(b.runtime); grants[inv.Host] != "" {
		t.Fatal("revoked grant retained")
	}
	gatewayUntil(t, func() bool { _, e := nb.resolve(context.Background(), "gateway-a"); return e != nil })
	if _, err := nb.send(context.Background(), "gateway-a", "forbidden", ""); err == nil {
		t.Fatal("revoked route accepted")
	}
	afterA, _ := os.ReadFile(filepath.Join(a.runtime, "token"))
	afterB, _ := os.ReadFile(filepath.Join(b.runtime, "token"))
	if string(afterA) != string(tokenA) || string(afterB) != string(tokenB) {
		t.Fatal("local token changed")
	}
}
func TestGatewayInvitationValidation(t *testing.T) {
	secret, _ := gatewayRandom()
	in := gatewayInvitation{Version: 1, Mode: "lan", Endpoint: "127.0.0.1:7947", Host: secret, ID: secret, Secret: secret}
	b, _ := json.Marshal(in)
	code := encodeGatewayInvitation(b)
	if _, err := parseGatewayInvitation(code); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "rhz2_bad", strings.Repeat("x", 2049), "rhz1_!!!"} {
		if _, err := parseGatewayInvitation(bad); err == nil {
			t.Error("accepted malformed invitation")
		}
	}
	for _, endpoint := range []string{"8.8.8.8:7947", "0.0.0.0:7947", "localhost:7947", "127.0.0.1:0", "100.64.0.1:7947"} {
		if gatewayEndpoint(endpoint, "lan") == nil {
			t.Errorf("accepted %s", endpoint)
		}
	}
	if gatewayEndpoint("100.64.0.1:7947", "tailscale") != nil {
		t.Fatal("rejected tailscale endpoint")
	}
}
func TestGatewayNonTransitiveAndConsumedInvitation(t *testing.T) {
	a, b, c := startGatewayHarness(t), startGatewayHarness(t), startGatewayHarness(t)
	na, nb, nc := gatewayTestNode(t, a, "mesh-a"), gatewayTestNode(t, b, "mesh-b"), gatewayTestNode(t, c, "mesh-c")
	code := a.invite(t)
	b.command(t, GatewayCommand{Action: "join", Code: code})
	if _, err := GatewayAdmin(context.Background(), c.runtime, GatewayCommand{Action: "join", Code: code}); err == nil {
		t.Fatal("different host redeemed consumed invite")
	}
	c.command(t, GatewayCommand{Action: "join", Code: b.invite(t)})
	gatewayUntil(t, func() bool {
		_, e := nb.resolve(context.Background(), "mesh-a")
		_, e2 := nb.resolve(context.Background(), "mesh-c")
		return e == nil && e2 == nil
	})
	// A fresh snapshot after both pairings must still export only B's local node.
	gatewayUntil(t, func() bool { _, e := nc.resolve(context.Background(), "mesh-b"); return e == nil })
	for _, p := range nc.peers() {
		if p.SessionId == na.local().SessionId {
			t.Fatal("transitive export")
		}
	}
	if _, err := nc.send(context.Background(), "mesh-a", "forbidden", ""); err == nil {
		t.Fatal("transitive send")
	}
	// A remote session colliding with a local one must fail closed, including observers.
	collision := gatewayTestNode(t, c, "mesh-a")
	collision.mu.Lock()
	collision.identity.ObservationOnly = true
	collision.mu.Unlock()
	collision.advertise()
	gatewayUntil(t, func() bool { _, err := nb.resolve(context.Background(), "mesh-a"); return err != nil })
}
func TestGatewayCorruptStateFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadGatewayState(path); err == nil {
		t.Fatal("accepted corrupt state")
	}
}
func TestGatewayIdentityBounds(t *testing.T) {
	p := &pb.Identity{Protocol: 1, GatewayVersion: 1, NodeId: id(), SessionId: "s", Generation: 1, AgentType: "claude"}
	if !validGatewayIdentity(p) {
		t.Fatal("rejected valid identity")
	}
	p.GatewayHost = "other"
	if validGatewayIdentity(p) {
		t.Fatal("accepted forwarded identity")
	}
}

func TestGatewayEnrollmentAtomicExpiryAndRevocation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	state, err := loadGatewayState(path)
	if err != nil {
		t.Fatal(err)
	}
	_, host, err := gatewayCertificate(state.Key)
	if err != nil {
		t.Fatal(err)
	}
	g := &gateway{ctx: context.Background(), statePath: path, state: state, host: host, endpoint: "127.0.0.1:7947", cfg: config.Gateway{Mode: "lan"}, links: map[string]*gatewayLink{}, enrollmentRate: rate.NewLimiter(rate.Inf, 1)}
	value, err := g.command(context.Background(), GatewayCommand{Action: "invite", Expires: "10m"})
	if err != nil {
		t.Fatal(err)
	}
	in, err := parseGatewayInvitation(value.(map[string]string)["invitation"])
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(in.Secret)) {
		t.Fatal("plaintext invite persisted")
	}
	keys := make([]tls.Certificate, 2)
	for i := range keys {
		other, err := loadGatewayState(filepath.Join(t.TempDir(), "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		keys[i], _, err = gatewayCertificate(other.Key)
		if err != nil {
			t.Fatal(err)
		}
	}
	enroll := func(cert tls.Certificate) int {
		payload, _ := json.Marshal(gatewayEnrollment{ID: in.ID, Secret: in.Secret, Endpoint: "127.0.0.1:7948", Mode: "lan"})
		r := httptest.NewRequest("POST", "https://gateway/v1/enroll", bytes.NewReader(payload))
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert.Leaf}}
		w := httptest.NewRecorder()
		g.enroll(w, r)
		return w.Code
	}
	results := make(chan int, 2)
	var wg sync.WaitGroup
	for _, key := range keys {
		wg.Add(1)
		go func() { defer wg.Done(); results <- enroll(key) }()
	}
	wg.Wait()
	close(results)
	successes := 0
	for code := range results {
		if code == 200 {
			successes++
		} else if code != 403 {
			t.Fatalf("unexpected response %d", code)
		}
	}
	if successes != 1 {
		t.Fatal("invitation admitted multiple hosts")
	}
	g.mu.Lock()
	record := g.state.Invites[in.ID]
	g.mu.Unlock()
	var winner tls.Certificate
	for _, key := range keys {
		if fingerprint(key.Leaf) == record.Redeemed {
			winner = key
		}
	}
	if enroll(winner) != 200 {
		t.Fatal("same-host recovery rejected")
	}
	if _, err := g.command(context.Background(), GatewayCommand{Action: "revoke", ID: record.Redeemed}); err != nil {
		t.Fatal(err)
	}
	if enroll(winner) != 403 {
		t.Fatal("replay undid inviter revocation")
	}
	g.mu.Lock()
	record.Redeemed = ""
	record.Expires = time.Now().Add(-time.Second)
	g.state.Invites[in.ID] = record
	g.mu.Unlock()
	if enroll(winner) != 403 {
		t.Fatal("expired invitation admitted")
	}
}

func TestGatewayTailscaleAddressResolution(t *testing.T) {
	cases := []struct {
		name, state, ip string
		fail            bool
	}{
		{"running", "Running", "100.64.0.12", false},
		{"stopped", "Stopped", "100.64.0.12", true},
		{"missing address", "Running", "", true},
		{"wrong range", "Running", "192.168.1.10", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := &fakeTailscale{st: &tailscaleStatus{BackendState: tc.state, Self: tailscalePeer{IPs: []net.IP{net.ParseIP(tc.ip)}}}}
			listen, advertise, err := gatewayAddresses(context.Background(), config.Gateway{Mode: "tailscale"}, ts)
			if (err != nil) != tc.fail {
				t.Fatalf("unexpected result: %v", err)
			}
			if !tc.fail && (listen != "100.64.0.12:7947" || listen != advertise) {
				t.Fatalf("wrong addresses: %s %s", listen, advertise)
			}
		})
	}
}
func TestGatewayTLSIdentityPinning(t *testing.T) {
	state, err := loadGatewayState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	cert, host, err := gatewayCertificate(state.Key)
	if err != nil {
		t.Fatal(err)
	}
	g := &gateway{cert: cert}
	tlsCfg := g.clientTLS(host)
	if err := tlsCfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert.Leaf}}); err != nil {
		t.Fatal(err)
	}
	if err := tlsCfg.VerifyConnection(tls.ConnectionState{}); err == nil {
		t.Fatal("accepted missing certificate")
	}
	wrong, _ := gatewayRandom()
	if err := g.clientTLS(wrong).VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert.Leaf}}); err == nil {
		t.Fatal("accepted wrong fingerprint")
	}
	cert.Leaf.NotAfter = time.Now().Add(-time.Hour)
	if err := tlsCfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert.Leaf}}); err == nil {
		t.Fatal("accepted expired certificate")
	}
}
func TestGatewayProtocolCompatibility(t *testing.T) {
	local := &pb.Identity{Protocol: 1, SessionId: "local"}
	if !validChannelProtocol(local) || gatewayExportable(local) {
		t.Fatal("legacy node must remain local-only")
	}
	local.GatewayVersion = 1
	if !gatewayExportable(local) {
		t.Fatal("new local node not exportable")
	}
	host, _ := gatewayRandom()
	local.GatewayHost = host
	if validChannelProtocol(local) || gatewayExportable(local) {
		t.Fatal("accepted protocol-1 proxy")
	}
	local.Protocol = 2
	if !validChannelProtocol(local) || gatewayExportable(local) {
		t.Fatal("proxy protocol policy")
	}
}

// TestGatewayShortLinksBackOff proves that links ending within the short-link
// window count as failures with exponential backoff, a long-lived link resets
// it, and shutdown, replacement and revocation are neutral.
func TestGatewayShortLinksBackOff(t *testing.T) {
	host := strings.Repeat("b", 64)
	short := 5 * time.Second
	steps := []struct {
		lifetime time.Duration
		reason   string
		trusted  bool
		failures int
	}{
		{time.Second, "read_error", true, 1},
		{time.Second, "stale_snapshot", true, 2},
		{0, "remote_closed", true, 3},
		{time.Second, "shutdown", true, 3},
		{time.Second, "replaced", true, 3},
		{time.Second, "revoked", true, 3},
		{time.Second, "invalid_snapshot", true, 4},
		{time.Second, "read_error", true, 5},
		{time.Second, "read_error", true, 5}, // capped
		{short, "read_error", true, 0},       // a long-lived link resets
		{time.Second, "read_error", true, 1},
		{time.Second, "revoked", false, 0}, // revocation drops retry state
		{time.Second, "read_error", true, 1},
		{time.Second, "read_error", false, 0}, // untrusted hosts keep no state
	}
	g := &gateway{retry: map[string]gatewayRetry{}, shortLink: short, state: gatewayState{Peers: map[string]gatewayPeer{}}}
	for i, s := range steps {
		if s.trusted {
			g.state.Peers[host] = gatewayPeer{Host: host}
		} else {
			delete(g.state.Peers, host)
		}
		before := time.Now()
		failures, backoff := g.linkEndedLocked(host, s.lifetime, s.reason)
		retry := g.retry[host]
		if failures != s.failures || retry.failures != s.failures {
			t.Fatalf("step %d (%s): failures %d/%d, want %d", i, s.reason, failures, retry.failures, s.failures)
		}
		counted := s.lifetime < short && s.trusted && s.reason != "shutdown" && s.reason != "replaced" && s.reason != "revoked"
		if !counted {
			if backoff != 0 {
				t.Fatalf("step %d: neutral outcome scheduled backoff %v", i, backoff)
			}
			continue
		}
		minimum := time.Second * time.Duration(1<<s.failures)
		if wait := retry.next.Sub(before); wait < minimum || wait > minimum+time.Second+time.Millisecond*100 {
			t.Fatalf("step %d: backoff %v, want %v plus jitter", i, wait, minimum)
		}
	}
	if _, ok := g.retry[host]; ok {
		t.Fatal("untrusted host retained retry state")
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// TestGatewayLinkStopReasons drives a real link through misbehaving peers and
// checks that the stop reason is logged without session data and that the
// short link counts toward the dial backoff.
func TestGatewayLinkStopReasons(t *testing.T) {
	cases := []struct {
		reason string
		peer   func(*websocket.Conn) error
	}{
		{"invalid_snapshot", func(c *websocket.Conn) error {
			return c.WriteJSON(gatewayFrame{Type: "snapshot", Sessions: []*pb.Identity{{NodeId: "short", SessionId: "secret-session"}}})
		}},
		{"stale_snapshot", func(*websocket.Conn) error { return nil }},
		{"unknown_frame", func(c *websocket.Conn) error { return c.WriteJSON(gatewayFrame{Type: "bogus"}) }},
		{"remote_closed", func(c *websocket.Conn) error {
			return c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			remote := strings.Repeat("c", 64)
			logs := &lockedBuffer{}
			ctx, cancel := context.WithCancel(context.Background())
			runtime := filepath.Join(t.TempDir(), "run")
			g := &gateway{retry: map[string]gatewayRetry{}, proxySlots: make(chan struct{}, 128), ctx: ctx, cancel: cancel, runtime: runtime, host: strings.Repeat("a", 64), state: gatewayState{Peers: map[string]gatewayPeer{remote: {Host: remote, Grant: "grant"}}}, links: map[string]*gatewayLink{}, dialing: map[string]bool{}, log: slog.New(slog.NewJSONHandler(logs, nil)), staleAfter: 50 * time.Millisecond}
			g.base = newNode(ctx)
			if err := g.base.start(Options{Runtime: runtime}); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil); err == nil {
					g.attach(remote, c, false)
				}
			}))
			defer func() {
				cancel()
				g.mu.Lock()
				for _, l := range g.links {
					l.stop("shutdown")
				}
				g.mu.Unlock()
				g.wg.Wait()
				g.base.close()
				srv.Close()
			}()
			client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			gatewayUntil(t, func() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.links[remote] != nil })
			if err := tc.peer(client); err != nil {
				t.Fatal(err)
			}
			gatewayUntil(t, func() bool { return strings.Contains(logs.String(), `"msg":"gateway_link_stop"`) })
			var stop map[string]any
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var m map[string]any
				if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "gateway_link_stop" {
					stop = m
				}
			}
			if stop["reason"] != tc.reason || stop["host"] != remote[:12] || stop["failures"] != float64(1) {
				t.Fatalf("stop log: %v", stop)
			}
			if out := logs.String(); strings.Contains(out, "secret-session") || strings.Contains(out, remote) || strings.Contains(out, "grant") {
				t.Fatalf("link log leaked identifiers: %s", out)
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.links[remote] != nil || g.retry[remote].failures != 1 || !g.retry[remote].next.After(time.Now()) {
				t.Fatalf("short link not counted: %+v", g.retry[remote])
			}
		})
	}
}

// TestGatewayFailureClassification pins the stop reasons for transport errors.
// A write after the peer's close frame was answered is a remote close, so the
// reason does not depend on whether the reader or the writer notices first.
func TestGatewayFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		err   error
		read  bool
		label string
	}{
		{websocket.ErrCloseSent, false, "remote_closed"},
		{fmt.Errorf("wrapped: %w", websocket.ErrCloseSent), false, "remote_closed"},
		{net.ErrClosed, false, "write_error"},
		{&websocket.CloseError{Code: websocket.CloseNormalClosure}, true, "remote_closed"},
		{websocket.ErrReadLimit, true, "frame_too_large"},
		{&json.SyntaxError{}, true, "invalid_frame"},
		{os.ErrDeadlineExceeded, true, "read_timeout"},
		{net.ErrClosed, true, "read_error"},
	} {
		got := writeFailure(tc.err)
		if tc.read {
			got = readFailure(tc.err)
		}
		if got != tc.label {
			t.Errorf("%v (read=%v): %s, want %s", tc.err, tc.read, got, tc.label)
		}
	}
}
