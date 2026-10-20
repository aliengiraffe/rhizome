package channel

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"
	"rhizome/internal/config"
	"rhizome/internal/localstate"
)

type gatewayRetry struct {
	failures int
	next     time.Time
}
type gateway struct {
	files                              *gatewayFiles
	retry                              map[string]gatewayRetry
	proxySlots                         chan struct{}
	enrollmentRate                     *rate.Limiter
	ctx                                context.Context
	cancel                             context.CancelFunc
	cfg                                config.Gateway
	runtime, host, endpoint, statePath string
	cert                               tls.Certificate
	base                               *Node
	attachMu                           sync.Mutex // serializes connection replacement and proxy teardown
	mu                                 sync.Mutex // state and links; never held over network I/O
	state                              gatewayState
	links                              map[string]*gatewayLink
	dialing                            map[string]bool
	wg                                 sync.WaitGroup
}
type gatewayControl struct{ Endpoint, Secret string }

// GatewayCommand is the local service administration request. Secrets are never logged.
type GatewayCommand struct {
	Action  string `json:"action"`
	Code    string `json:"code,omitempty"`
	ID      string `json:"id,omitempty"`
	Expires string `json:"expires,omitempty"`
	Session string `json:"session,omitempty"`
	Target  string `json:"target,omitempty"`
	Path    string `json:"path,omitempty"`
}
type gatewayEnrollment struct{ ID, Secret, Endpoint, Mode string }

// RunGateway runs the optional host gateway until canceled. It does not collect transcripts.
func RunGateway(ctx context.Context, runtime string, cfg config.Gateway) error {
	if cfg.Mode == "" || cfg.Mode == "off" {
		<-ctx.Done()
		return nil
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	dir := filepath.Join(runtime, "gateway")
	if err := localstate.SecureDir(runtime); err != nil {
		return err
	}
	if err := localstate.SecureDir(dir); err != nil {
		return err
	}
	lock, err := localstate.Lock(filepath.Join(dir, "service.lock"), false)
	if err != nil {
		return err
	}
	defer lock.Close()
	statePath := filepath.Join(dir, "state.json")
	state, err := loadGatewayState(statePath)
	if err != nil {
		return err
	}
	cert, host, err := gatewayCertificate(state.Key)
	if err != nil {
		return err
	}
	address, endpoint, err := gatewayAddresses(ctx, cfg, tailscaleCLI{})
	if err != nil {
		return err
	}
	for _, p := range state.Peers {
		if err := gatewayEndpoint(p.Endpoint, cfg.Mode); err != nil {
			return fmt.Errorf("stored peer endpoint: %w", err)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g := &gateway{retry: map[string]gatewayRetry{}, proxySlots: make(chan struct{}, 128), enrollmentRate: rate.NewLimiter(4, 8), ctx: ctx, cancel: cancel, cfg: cfg, runtime: runtime, host: host, endpoint: endpoint, cert: cert, statePath: statePath, state: state, links: map[string]*gatewayLink{}, dialing: map[string]bool{}}
	g.files, err = newGatewayFiles(runtime)
	if err != nil {
		return err
	}
	defer g.files.close()
	g.base = newNode(ctx)
	if err := g.base.start(Options{Runtime: runtime}); err != nil {
		g.base.close()
		return err
	}
	defer g.base.close()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/enroll", g.enroll)
	mux.HandleFunc("/v1/connect", g.connect)
	mux.HandleFunc("/v1/files", g.fileHTTP)
	slots := make(chan struct{}, 32)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			mux.ServeHTTP(w, r)
		default:
			http.Error(w, "busy", 503)
		}
	}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 8192}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAnyClientCert, SessionTicketsDisabled: true}
	controlListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		listener.Close()
		return err
	}
	secret, err := gatewayRandom()
	if err != nil {
		listener.Close()
		controlListener.Close()
		return err
	}
	control := gatewayControl{Endpoint: controlListener.Addr().String(), Secret: secret}
	controlPath := filepath.Join(dir, "control.json")
	if err = gatewayWrite(controlPath, control); err != nil {
		listener.Close()
		controlListener.Close()
		return err
	}
	defer os.Remove(controlPath)
	adminSlots := make(chan struct{}, 8)
	admin := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+secret)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		select {
		case adminSlots <- struct{}{}:
			defer func() { <-adminSlots }()
		default:
			http.Error(w, "busy", 503)
			return
		}
		var cmd GatewayCommand
		if r.Method != "POST" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&cmd) != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
		defer cancel()
		stop := context.AfterFunc(g.ctx, cancel)
		defer stop()
		result, e := g.command(ctx, cmd)
		if e != nil {
			http.Error(w, e.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 125 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	errs := make(chan error, 2)
	go func() {
		errs <- server.Serve(tls.NewListener(&gatewayListener{Listener: listener, slots: make(chan struct{}, 64)}, tlsCfg))
	}()
	go func() {
		errs <- admin.Serve(&gatewayListener{Listener: controlListener, slots: make(chan struct{}, 16)})
	}()
	g.wg.Add(1)
	go func() { defer g.wg.Done(); g.reconnect() }()
	select {
	case <-ctx.Done():
		err = nil
	case err = <-errs:
	}
	cancel()
	_ = server.Close()
	_ = admin.Close()
	g.mu.Lock()
	for _, l := range g.links {
		l.stop()
	}
	g.mu.Unlock()
	g.wg.Wait()
	return err
}

// GatewayAdmin contacts only the private local service control endpoint.
func GatewayAdmin(ctx context.Context, runtime string, cmd GatewayCommand) (json.RawMessage, error) {
	var ctl gatewayControl
	if err := gatewayRead(filepath.Join(runtime, "gateway", "control.json"), &ctl); err != nil {
		return nil, errors.New("gateway service unavailable; configure gateway.mode and start the service")
	}
	host, _, err := net.SplitHostPort(ctl.Endpoint)
	if err != nil || host != "127.0.0.1" || !validFingerprint(ctl.Secret) {
		return nil, errors.New("invalid gateway control state")
	}
	b, _ := json.Marshal(cmd)
	req, err := http.NewRequestWithContext(ctx, "POST", "http://"+ctl.Endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+ctl.Secret)
	timeout := 25 * time.Second
	if cmd.Action == "files-accept" {
		timeout = 120 * time.Second
	}
	client := &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}
	defer client.CloseIdleConnections()
	res, err := client.Do(req)
	if err != nil {
		return nil, errors.New("gateway service unavailable")
	}
	defer res.Body.Close()
	out, err := io.ReadAll(io.LimitReader(res.Body, 64*1024+1))
	if err != nil {
		return nil, err
	}
	if len(out) > 64*1024 {
		return nil, errors.New("oversized gateway response")
	}
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("gateway: %s", bytes.TrimSpace(out))
	}
	return out, nil
}
func (g *gateway) saveLocked() error { return gatewayWrite(g.statePath, g.state) }
func (g *gateway) clientTLS(host string) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{g.cert}, InsecureSkipVerify: true, // Peer identity is pinned below, independent of DNS names.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || fingerprint(cs.PeerCertificates[0]) != host {
				return errors.New("gateway host fingerprint mismatch")
			}
			c := cs.PeerCertificates[0]
			if time.Now().Before(c.NotBefore) || time.Now().After(c.NotAfter) {
				return errors.New("gateway certificate expired")
			}
			return nil
		}}
}
func gatewayCaller(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	c := r.TLS.PeerCertificates[0]
	if time.Now().Before(c.NotBefore) || time.Now().After(c.NotAfter) {
		return ""
	}
	return fingerprint(c)
}
func (g *gateway) enroll(w http.ResponseWriter, r *http.Request) {
	if !g.enrollmentRate.Allow() {
		http.Error(w, "enrollment rate limit", http.StatusTooManyRequests)
		return
	}
	caller := gatewayCaller(r)
	var in gatewayEnrollment
	if caller == "" || caller == g.host || r.Method != "POST" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil || in.Mode != g.cfg.Mode || gatewayEndpoint(in.Endpoint, in.Mode) != nil {
		http.Error(w, "invalid enrollment", 400)
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	invite, ok := g.state.Invites[in.ID]
	if !ok || time.Now().After(invite.Expires) || subtle.ConstantTimeCompare([]byte(invite.Hash), []byte(gatewayDigest(in.Secret))) != 1 || (invite.Redeemed != "" && invite.Redeemed != caller) {
		http.Error(w, "invalid, expired or consumed invitation", 403)
		return
	}
	old, existed := g.state.Peers[caller]
	if !existed && len(g.state.Peers) >= gatewayMaxPeers {
		http.Error(w, "peer limit reached", 409)
		return
	}
	grant := old.Grant
	if !existed {
		var err error
		grant, err = gatewayRandom()
		if err != nil {
			http.Error(w, "identity generation failed", 500)
			return
		}
	}
	before := invite
	invite.Redeemed = caller
	g.state.Invites[in.ID] = invite
	g.state.Peers[caller] = gatewayPeer{Host: caller, Endpoint: in.Endpoint, Grant: grant}
	if err := g.saveLocked(); err != nil {
		g.state.Invites[in.ID] = before
		if existed {
			g.state.Peers[caller] = old
		} else {
			delete(g.state.Peers, caller)
		}
		http.Error(w, "cannot persist admission", 500)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"host": g.host, "created": invite.Created})
}
func (g *gateway) command(ctx context.Context, cmd GatewayCommand) (any, error) {
	if len(cmd.Action) > 6 && cmd.Action[:6] == "files-" {
		return g.fileCommand(ctx, cmd)
	}
	switch cmd.Action {
	case "invite":
		d, err := time.ParseDuration(cmd.Expires)
		if err != nil || d < time.Minute || d > 24*time.Hour {
			return nil, errors.New("expiry must be between 1m and 24h")
		}
		secret, err := gatewayRandom()
		if err != nil {
			return nil, err
		}
		id, err := gatewayRandom()
		if err != nil {
			return nil, err
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		for k, v := range g.state.Invites {
			if time.Now().After(v.Expires) {
				delete(g.state.Invites, k)
			}
		}
		if len(g.state.Invites) >= 128 {
			return nil, errors.New("invitation limit reached")
		}
		g.state.Invites[id] = gatewayInviteRecord{Created: time.Now(), Hash: gatewayDigest(secret), Expires: time.Now().Add(d)}
		if err := g.saveLocked(); err != nil {
			delete(g.state.Invites, id)
			return nil, err
		}
		in := gatewayInvitation{Version: 1, Mode: g.cfg.Mode, Endpoint: g.endpoint, Host: g.host, ID: id, Secret: secret}
		b, _ := json.Marshal(in)
		return map[string]string{"invitation": encodeGatewayInvitation(b), "id": id}, nil
	case "join":
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		return g.joinHost(ctx, cmd.Code)
	case "peers":
		g.mu.Lock()
		defer g.mu.Unlock()
		out := []map[string]any{}
		for _, p := range g.state.Peers {
			out = append(out, map[string]any{"host_id": p.Host, "endpoint": p.Endpoint, "connected": g.links[p.Host] != nil, "pending": p.Pending})
		}
		return out, nil
	case "invitations":
		g.mu.Lock()
		defer g.mu.Unlock()
		out := []map[string]any{}
		for id, v := range g.state.Invites {
			out = append(out, map[string]any{"id": id, "expires": v.Expires, "redeemed": v.Redeemed != ""})
		}
		return out, nil
	case "revoke-invite":
		g.mu.Lock()
		defer g.mu.Unlock()
		v, ok := g.state.Invites[cmd.ID]
		if !ok {
			return nil, errors.New("unknown invitation")
		}
		delete(g.state.Invites, cmd.ID)
		if err := g.saveLocked(); err != nil {
			g.state.Invites[cmd.ID] = v
			return nil, err
		}
		return map[string]bool{"revoked": true}, nil
	case "revoke":
		g.mu.Lock()
		p, ok := g.state.Peers[cmd.ID]
		if !ok {
			g.mu.Unlock()
			return nil, errors.New("unknown peer")
		}
		oldRevoked := maps.Clone(g.state.Revoked)
		oldReceived := maps.Clone(g.state.Received)
		for id, receipt := range g.state.Received {
			if receipt.Host == cmd.ID {
				g.state.Revoked[id] = receipt.Until
				delete(g.state.Received, id)
			}
		}

		delete(g.state.Peers, cmd.ID)
		// Remove recovery capabilities for this peer so revocation cannot be undone by replay.
		old := g.state.Invites
		g.state.Invites = map[string]gatewayInviteRecord{}
		for k, v := range old {
			if v.Redeemed != cmd.ID {
				g.state.Invites[k] = v
			}
		}
		if err := g.saveLocked(); err != nil {
			g.state.Peers[cmd.ID] = p
			g.state.Revoked = oldRevoked
			g.state.Received = oldReceived
			g.state.Invites = old
			g.mu.Unlock()
			return nil, err
		}
		l := g.links[cmd.ID]
		if l != nil {
			l.stop()
		}
		g.mu.Unlock()
		if l != nil {
			select {
			case <-l.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return map[string]bool{"revoked": true}, nil
	default:
		return nil, errors.New("unknown gateway command")
	}
}
func (g *gateway) joinHost(ctx context.Context, code string) (any, error) {
	in, err := parseGatewayInvitation(code)
	if err != nil {
		return nil, err
	}
	if in.Host == g.host {
		return nil, errors.New("cannot join this host")
	}
	if in.Mode != g.cfg.Mode {
		return nil, errors.New("invitation and gateway network modes differ")
	}
	g.mu.Lock()
	for id, until := range g.state.Revoked {
		if time.Now().After(until) {
			delete(g.state.Revoked, id)
		}
	}
	for id, r := range g.state.Received {
		if time.Now().After(r.Until) {
			delete(g.state.Received, id)
		}
	}
	if _, revoked := g.state.Revoked[in.ID]; revoked {
		g.mu.Unlock()
		return nil, errors.New("peer was revoked; a fresh invitation is required")
	}
	oldPeer, exists := g.state.Peers[in.Host]
	oldReceipt, received := g.state.Received[in.ID]
	if !exists && len(g.state.Peers) >= gatewayMaxPeers {
		g.mu.Unlock()
		return nil, errors.New("peer limit reached")
	}
	if !received && len(g.state.Received)+len(g.state.Revoked) >= 128 {
		g.mu.Unlock()
		return nil, errors.New("join receipt limit reached; retry after invitation expiry")
	}
	if received && oldReceipt.Host != in.Host {
		g.mu.Unlock()
		return nil, errors.New("invitation identity conflict")
	}
	if !exists {
		grant, e := gatewayRandom()
		if e != nil {
			g.mu.Unlock()
			return nil, e
		}
		g.state.Peers[in.Host] = gatewayPeer{Host: in.Host, Endpoint: in.Endpoint, Pending: true, Grant: grant}
	}
	g.state.Received[in.ID] = gatewayReceipt{Host: in.Host, Until: time.Now().Add(24 * time.Hour)}
	if err = g.saveLocked(); err != nil {
		if exists {
			g.state.Peers[in.Host] = oldPeer
		} else {
			delete(g.state.Peers, in.Host)
		}
		if received {
			g.state.Received[in.ID] = oldReceipt
		} else {
			delete(g.state.Received, in.ID)
		}
		g.mu.Unlock()
		return nil, err
	}
	g.mu.Unlock()
	b, _ := json.Marshal(gatewayEnrollment{ID: in.ID, Secret: in.Secret, Endpoint: g.endpoint, Mode: g.cfg.Mode})
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://"+in.Endpoint+"/v1/enroll", bytes.NewReader(b))
	tr := &http.Transport{TLSClientConfig: g.clientTLS(in.Host), Proxy: nil}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}
	res, err := client.Do(req)
	if err != nil {
		return nil, errors.New("cannot enroll: check reachability and host fingerprint; retry the same invitation")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("enrollment rejected (HTTP %d); invitation may be expired, revoked or consumed", res.StatusCode)
	}
	var receipt struct {
		Host string `json:"host"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&receipt) != nil || receipt.Host != in.Host {
		return nil, errors.New("invalid enrollment receipt")
	}
	g.mu.Lock()
	if _, revoked := g.state.Revoked[in.ID]; revoked {
		g.mu.Unlock()
		return nil, errors.New("peer revoked during enrollment")
	}

	current, ok := g.state.Peers[in.Host]
	if !ok {
		g.mu.Unlock()
		return nil, errors.New("peer revoked during enrollment")
	}
	g.state.Peers[in.Host] = gatewayPeer{Host: in.Host, Endpoint: in.Endpoint, Grant: current.Grant}
	if err = g.saveLocked(); err != nil {
		g.state.Peers[in.Host] = current
		g.mu.Unlock()
		return nil, err
	}
	g.mu.Unlock()
	g.tryDial(in.Host)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		g.mu.Lock()
		l := g.links[in.Host]
		g.mu.Unlock()
		if l != nil {
			return map[string]string{"host_id": in.Host, "status": "connected"}, nil
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("trust saved, connection not established; service will retry")
		case <-ticker.C:
		}
	}
}
func (g *gateway) connect(w http.ResponseWriter, r *http.Request) {
	host := gatewayCaller(r)
	g.mu.Lock()
	p, ok := g.state.Peers[host]
	g.mu.Unlock()
	if !ok || p.Pending || host == g.host {
		http.Error(w, "untrusted host", 403)
		return
	}
	c, err := (&websocket.Upgrader{HandshakeTimeout: 5 * time.Second, CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	g.attach(host, c, false)
}
func (g *gateway) reconnect() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		g.files.expire()
		g.mu.Lock()
		hosts := []string{}
		for h, p := range g.state.Peers {
			if !p.Pending && g.links[h] == nil {
				hosts = append(hosts, h)
			}
		}
		g.mu.Unlock()
		for _, h := range hosts {
			g.tryDial(h)
		}
		select {
		case <-g.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (g *gateway) tryDial(host string) {
	g.mu.Lock()
	p, ok := g.state.Peers[host]
	if !ok || p.Pending || g.links[host] != nil || g.dialing[host] || time.Now().Before(g.retry[host].next) || g.ctx.Err() != nil {
		g.mu.Unlock()
		return
	}
	g.dialing[host] = true
	g.wg.Add(1)
	g.mu.Unlock()
	go func() {
		defer g.wg.Done()
		defer func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			delete(g.dialing, host)
			_, stillTrusted := g.state.Peers[host]
			if g.links[host] != nil || !stillTrusted {
				delete(g.retry, host)
				return
			}
			retry := g.retry[host]
			if retry.failures < 5 {
				retry.failures++
			}
			delay := time.Second * time.Duration(1<<retry.failures)
			retry.next = time.Now().Add(delay + time.Duration(rand.Int63n(int64(time.Second))))
			g.retry[host] = retry
		}()
		ctx, cancel := context.WithTimeout(g.ctx, 5*time.Second)
		defer cancel()
		d := websocket.Dialer{TLSClientConfig: g.clientTLS(host), HandshakeTimeout: 5 * time.Second, Proxy: nil}
		c, res, err := d.DialContext(ctx, "wss://"+p.Endpoint+"/v1/connect", nil)
		if res != nil && res.Body != nil {
			res.Body.Close()
		}
		if err == nil {
			g.attach(host, c, true)
		}
	}()
}

func gatewayAddresses(ctx context.Context, cfg config.Gateway, ts tailscaleClient) (string, string, error) {
	address := cfg.Listen
	if cfg.Mode == "tailscale" {
		st, e := ts.Status(ctx)
		if e != nil {
			return "", "", e
		}
		ip := ""
		for _, a := range st.Self.IPs {
			if a.To4() != nil && tailscaleIP(a) {
				ip = a.String()
				break
			}
		}
		if st.BackendState != "Running" || ip == "" {
			return "", "", errors.New("gateway requires running Tailscale with an IPv4 address")
		}
		if address == "" {
			address = net.JoinHostPort(ip, "7947")
		}
	} else if address == "" {
		ip, e := advertiseAddress(Options{LAN: true})
		if e != nil {
			return "", "", e
		}
		address = net.JoinHostPort(ip, "7947")
	}
	if err := gatewayEndpoint(address, cfg.Mode); err != nil {
		return "", "", err
	}
	endpoint := cfg.Advertise
	if endpoint == "" {
		endpoint = address
	}
	if err := gatewayEndpoint(endpoint, cfg.Mode); err != nil {
		return "", "", err
	}
	return address, endpoint, nil
}
