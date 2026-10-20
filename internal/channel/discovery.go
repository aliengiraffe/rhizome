package channel

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/memberlist"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"rhizome/internal/config"
	"rhizome/internal/localstate"
	pb "rhizome/proto"
)

// NodeMeta serializes the identity into memberlist's bounded metadata. Labels
// (session_name, project) give way rune by rune, longest first, so routing and
// authentication fields are never the reason an advertisement is dropped.
func (n *Node) NodeMeta(limit int) []byte {
	identity := n.local()
	b, _ := json.Marshal(identity)
	for len(b) > limit && (len(identity.Project) > 0 || len(identity.SessionName) > 0) {
		if len(identity.SessionName) >= len(identity.Project) {
			identity.SessionName = trimRune(identity.SessionName)
		} else {
			identity.Project = trimRune(identity.Project)
		}
		b, _ = json.Marshal(identity)
	}
	for len(b) > limit && len(identity.Host) > 0 {
		identity.Host = trimRune(identity.Host)
		b, _ = json.Marshal(identity)
	}
	if len(b) > limit {
		return nil
	}
	return b
}
func (*Node) NotifyMsg([]byte)                {}
func (*Node) GetBroadcasts(int, int) [][]byte { return nil }
func (*Node) LocalState(bool) []byte          { return nil }
func (*Node) MergeRemoteState([]byte, bool)   {}
func (n *Node) authorized(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	v := md.Get("authorization")
	if len(v) != 1 || subtle.ConstantTimeCompare([]byte(v[0]), []byte("Bearer "+n.token)) != 1 {
		return status.Error(codes.Unauthenticated, "invalid cluster credential")
	}
	return nil
}
func (n *Node) outgoing(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+n.token)
}

// dial applies the address policy and, in LAN and Tailscale modes, pins TLS
// to the fingerprint gossip advertised for that identity. Local mode stays
// plaintext loopback.
func (n *Node) dial(i *pb.Identity) (*grpc.ClientConn, error) {
	if i == nil || !n.allowed(i.Endpoint) {
		return nil, errors.New("endpoint is outside the allowed address policy")
	}
	creds := insecure.NewCredentials()
	if n.mode != modeLocal {
		if !validFingerprint(i.CertFingerprint) {
			return nil, errors.New("peer advertises no certificate fingerprint; refusing unpinned dial")
		}
		creds = pinned(i.CertFingerprint)
	}
	return grpc.NewClient(i.Endpoint, grpc.WithTransportCredentials(creds))
}
func (n *Node) probe(ctx context.Context, i *pb.Identity) (*pb.Identity, error) {
	c, err := n.dial(i)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	timeout := 700 * time.Millisecond
	if n.mode == modeTailscale {
		// Tailnet peers are often DERP-relayed; a pinned handshake plus one
		// round trip can exceed the loopback budget.
		timeout = 3 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	actual, err := pb.NewChannelClient(c).Info(n.outgoing(ctx), &pb.InfoRequest{})
	if err != nil {
		return nil, err
	}
	if actual.NodeId != i.NodeId || actual.Protocol != 1 || !n.allowed(actual.Endpoint) || !n.allowed(actual.GossipEndpoint) {
		return nil, errors.New("registration identity mismatch")
	}
	return actual, nil
}
func secureDir(dir string) error { return localstate.SecureDir(dir) }

// loadToken reads or creates the runtime token under the registry lock. An
// explicit token (flag, env or file) seeds a machine that has none and must
// agree with one that already exists.
func loadToken(runtime string, o Options) (string, error) {
	provided := strings.TrimSpace(o.Token)
	if o.TokenFile != "" {
		raw, err := os.ReadFile(o.TokenFile)
		if err != nil {
			return "", err
		}
		if provided != "" && provided != strings.TrimSpace(string(raw)) {
			return "", errors.New("RHIZOME_TOKEN and token file disagree")
		}
		provided = strings.TrimSpace(string(raw))
	}
	if provided != "" && !validToken(provided) {
		return "", errors.New("provided token must be 64 hex characters")
	}
	tokenPath := filepath.Join(runtime, "token")
	b, err := os.ReadFile(tokenPath)
	if os.IsNotExist(err) {
		b = []byte(provided)
		if provided == "" {
			b = []byte(id() + id())
		}
		err = os.WriteFile(tokenPath, b, 0600)
	}
	if err != nil {
		return "", err
	}
	if err := localstate.PrivateFile(tokenPath); err != nil {
		return "", err
	}
	token := string(b)
	if len(token) != 64 {
		return "", errors.New("invalid cluster token length")
	}
	if provided != "" && provided != token {
		return "", errors.New("provided token conflicts with the existing runtime token; remove " + tokenPath + " to rotate")
	}
	return token, nil
}

// Token returns this machine's cluster token, creating it if absent, so it can
// be copied to other devices.
func Token(runtime string) (string, error) {
	runtime, err := localstate.Runtime(runtime)
	if err != nil {
		return "", err
	}
	if err := secureDir(runtime); err != nil {
		return "", err
	}
	lock, err := localstate.Lock(filepath.Join(runtime, "registry.lock"), true)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	return loadToken(runtime, Options{})
}
func (n *Node) start(o Options) error {
	hostname, err := config.NormalizeHostname(o.Hostname)
	if err != nil {
		return err
	}
	if hostname == "" {
		hostname = shortHost()
	}
	n.identity.Host = hostname
	if err := o.validate(); err != nil {
		return err
	}
	n.opts = o
	n.mode = o.mode()
	runtime, err := localstate.Runtime(o.Runtime)
	if err != nil {
		return err
	}
	if err := secureDir(runtime); err != nil {
		return err
	}
	n.runtime = runtime
	lock, err := localstate.Lock(filepath.Join(runtime, "registry.lock"), true)
	if err != nil {
		return err
	}
	defer lock.Close()
	if n.token, err = loadToken(runtime, o); err != nil {
		return err
	}
	bind, advertise := "127.0.0.1", "127.0.0.1"
	var ts *tailscaleStatus
	if n.mode == modeTailscale {
		n.ts = o.tailscale
		if n.ts == nil {
			n.ts = tailscaleCLI{}
		}
		if ts, err = tailscaleSelf(n.ctx, n.ts); err != nil {
			return err
		}
		self := ts.Self.ipv4().String()
		if o.BindAddr == "" {
			o.BindAddr = self
		}
		if o.AdvertiseAddr == "" {
			o.AdvertiseAddr = self
		}
		if o.GossipPort == 0 {
			o.GossipPort = defaultBeaconPort
		}
		if o.Discovery == 0 {
			o.Discovery = defaultDiscoveryInterval
		}
		n.beaconPort = o.GossipPort
	}
	n.opts = o
	serverOptions := []grpc.ServerOption{grpc.MaxRecvMsgSize(64 * 1024), grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := n.authorized(ctx); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}), grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := n.authorized(ss.Context()); err != nil {
			return err
		}
		return handler(srv, ss)
	})}
	if n.mode != modeLocal {
		bind = "0.0.0.0"
		if o.BindAddr != "" {
			if net.ParseIP(o.BindAddr) == nil {
				return fmt.Errorf("bind address %q is not an IP", o.BindAddr)
			}
			bind = o.BindAddr
		}
		if advertise, err = advertiseAddress(o); err != nil {
			return err
		}
		cert, fp, err := newCertificate(net.ParseIP(advertise))
		if err != nil {
			return err
		}
		n.cert = cert
		n.identity.CertFingerprint = fp
		serverOptions = append(serverOptions, grpc.Creds(serverCredentials(cert)))
	}
	n.bind, n.advertiseAddr = bind, advertise
	listener, err := net.Listen("tcp", net.JoinHostPort(bind, "0"))
	if err != nil {
		return err
	}
	n.identity.Endpoint = net.JoinHostPort(advertise, strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
	n.rpc = grpc.NewServer(serverOptions...)
	pb.RegisterChannelServer(n.rpc, n)
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		if err := n.rpc.Serve(listener); err != nil && n.ctx.Err() == nil {
			slog.Error("grpc_serve", "error", err)
		}
	}()
	config, err := n.gossipConfig()
	if err != nil {
		return err
	}
	if n.members, err = memberlist.Create(config); err != nil {
		return err
	}
	local := n.members.LocalNode()
	n.gossipAddr = net.JoinHostPort(bind, strconv.Itoa(int(local.Port)))
	n.mu.Lock()
	n.identity.GossipEndpoint = net.JoinHostPort(advertise, strconv.Itoa(int(local.Port)))
	n.mu.Unlock()
	if err = n.members.UpdateNode(time.Second); err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(runtime, "*.json"))
	if err != nil {
		return err
	}
	for _, path := range files {
		base := filepath.Base(path)
		if len(base) != 37 {
			continue
		}
		if _, err := hex.DecodeString(base[:32]); err != nil {
			continue
		}
		raw, e := os.ReadFile(path)
		var peer pb.Identity
		if e != nil || json.Unmarshal(raw, &peer) != nil {
			_ = os.Remove(path)
			continue
		}
		if !n.allowed(peer.Endpoint) || (peer.CertFingerprint != "") != (n.mode != modeLocal) {
			// A registration from the other discovery mode may be live; it is
			// unreachable for us but not stale, so leave it in place.
			slog.Info("bootstrap_skip", "path", filepath.Base(path), "reason", "discovery mode mismatch")
			continue
		}
		actual, e := n.probe(n.ctx, &peer)
		if e != nil {
			_ = os.Remove(path)
			continue
		}
		if _, e = n.members.Join([]string{actual.GossipEndpoint}); e != nil {
			slog.Warn("bootstrap_join", "error", e)
		}
	}
	if n.mode != modeLocal {
		for _, seed := range o.Join {
			if err := n.join("join", seed); err != nil {
				slog.Warn("seed_join", "source", "join", "addr", seed, "error", err)
			}
		}
		if o.MDNS {
			if err := n.startMDNS(net.ParseIP(advertise), int(local.Port)); err != nil {
				// Browsing is a convenience; explicit seeds and the registry still work.
				slog.Warn("mdns_unavailable", "error", err)
			}
		}
	}
	n.registration = filepath.Join(runtime, n.identity.NodeId+".json")
	raw, _ := json.Marshal(n.local())
	if err = os.WriteFile(n.registration, raw, 0600); err != nil {
		return err
	}
	attrs := []any{"binding", "UNBOUND", "node_id", n.identity.NodeId, "mode", n.mode.String(), "bind", bind, "advertise", advertise, "endpoint", n.identity.Endpoint, "gossip_endpoint", n.local().GossipEndpoint, "join", o.Join, "mdns", n.mode != modeLocal && o.MDNS}
	switch n.mode {
	case modeLAN:
		slog.Info("trust_boundary", "mode", "lan", "boundary", "anyone holding the cluster token on the local network")
	case modeTailscale:
		slog.Info("trust_boundary", "mode", "tailscale", "boundary", "anyone holding the cluster token on this tailnet; narrow it further with tailnet ACLs")
		beacon := "ephemeral"
		if n.acquireBeacon() {
			beacon = strconv.Itoa(n.beaconPort)
		} else {
			slog.Info("beacon_busy", "port", n.beaconPort, "message", "another process holds the beacon; reachable through it, re-acquiring when free")
		}
		attrs = append(attrs, "tailscale_ip", advertise, "magicdns", strings.TrimSuffix(ts.Self.DNSName, "."), "magicdns_enabled", ts.MagicDNSEnabled, "beacon_port", beacon, "discovery_interval", o.Discovery.String())
		n.wg.Add(1)
		go func() { defer n.wg.Done(); n.tailnet(o.Discovery) }()
	}
	slog.Info("ready", attrs...)
	return nil
}

// gossipConfig builds the memberlist profile for the mode. Gossip always
// binds an ephemeral port; in Tailscale mode the beacon forwards to it. The
// key is derived from the token; Tailscale mode derives a distinct key so
// clusters in other modes never merge with a tailnet cluster, even on one
// device.
func (n *Node) gossipConfig() (*memberlist.Config, error) {
	config := memberlist.DefaultLocalConfig()
	key, err := hex.DecodeString(n.token[:32])
	if err != nil {
		return nil, err
	}
	switch n.mode {
	case modeLAN:
		// Local timeouts are tuned for loopback and flap over Wi-Fi.
		config = memberlist.DefaultLANConfig()
	case modeTailscale:
		// Tailnet peers are often DERP-relayed with RTTs of hundreds of ms.
		config = memberlist.DefaultWANConfig()
		config.TCPTimeout = 10 * time.Second
		sum := sha256.Sum256([]byte("rhizome-tailscale:" + n.token))
		key = sum[:16]
	}
	config.Name = n.identity.NodeId
	config.BindAddr = n.bind
	config.AdvertiseAddr = n.advertiseAddr
	config.BindPort = 0
	config.AdvertisePort = 0
	config.Delegate = n
	config.Events = n
	config.Logger = log.New(gossipLog{"gossip"}, "", 0)
	config.SecretKey = key
	return config, nil
}

func (n *Node) unregister() {
	if n.registration != "" {
		_ = os.Remove(n.registration)
	}
}

// memberlist owns mutable Node pointers. Copy metadata inside callbacks,
// while memberlist holds its lock; never retain pointers from Members().
func (n *Node) NotifyJoin(member *memberlist.Node)   { n.updatePeer(member) }
func (n *Node) NotifyUpdate(member *memberlist.Node) { n.updatePeer(member) }
func (n *Node) NotifyLeave(member *memberlist.Node) {
	n.mu.Lock()
	delete(n.discovered, member.Name)
	n.mu.Unlock()
}
func (n *Node) updatePeer(member *memberlist.Node) {
	if !allowedIP(member.Addr, n.mode) {
		return
	}
	var i pb.Identity
	if json.Unmarshal(member.Meta, &i) != nil || i.Protocol != 1 || i.NodeId != member.Name || !n.allowed(i.Endpoint) {
		return
	}
	// Peers from the other discovery mode cannot be dialed; keep them invisible.
	if lan := n.mode != modeLocal; lan && !validFingerprint(i.CertFingerprint) || !lan && i.CertFingerprint != "" {
		return
	}
	n.mu.Lock()
	n.discovered[i.NodeId] = &i
	n.mu.Unlock()
}
func (n *Node) peers() []*pb.Identity {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := []*pb.Identity{cloneIdentity(n.identity)}
	for key, i := range n.discovered {
		if key != n.identity.NodeId {
			out = append(out, cloneIdentity(i))
		}
	}
	return out
}
func (n *Node) resolve(ctx context.Context, session string) (*pb.Identity, error) {
	var found, observed *pb.Identity
	for _, i := range n.peers() {
		if i.SessionId != session {
			continue
		}
		actual, err := n.probe(ctx, i)
		if err != nil || actual.SessionId != session {
			continue
		}
		if actual.ObservationOnly {
			if observed != nil {
				return nil, fmt.Errorf("ambiguous session %q: multiple observers", session)
			}
			observed = actual
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("ambiguous session %q: multiple live nodes", session)
		}
		found = actual
	}
	if found == nil {
		found = observed
	}
	if found == nil {
		return nil, fmt.Errorf("session %q is not currently available", session)
	}
	return found, nil
}
func (n *Node) Info(context.Context, *pb.InfoRequest) (*pb.Identity, error) { return n.local(), nil }

type gossipLog struct{ source string }

func (g gossipLog) Write(p []byte) (int, error) {
	slog.Info(g.source, "message", strings.TrimSpace(string(p)))
	return len(p), nil
}
