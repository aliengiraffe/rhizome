package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBeaconPort        = 7946
	defaultDiscoveryInterval = 20 * time.Second
)

// tailscaleRanges are the addresses Tailscale assigns: CGNAT IPv4, which Go
// does not consider private, and the Tailscale IPv6 ULA prefix.
var tailscaleRanges = []*net.IPNet{mustCIDR("100.64.0.0/10"), mustCIDR("fd7a:115c:a1e0::/48")}

func mustCIDR(s string) *net.IPNet {
	_, network, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return network
}
func tailscaleIP(ip net.IP) bool {
	for _, r := range tailscaleRanges {
		if r.Contains(ip) {
			return true
		}
	}
	return false
}

// tailscalePeer is one device as the local daemon reports it.
type tailscalePeer struct {
	HostName string
	DNSName  string // MagicDNS FQDN with trailing dot, e.g. "delta.tail2fdf06.ts.net."
	IPs      []net.IP
	Online   bool
}

// tailscaleStatus is the subset of `tailscale status --json` Rhizome uses.
type tailscaleStatus struct {
	BackendState    string
	Self            tailscalePeer
	Peers           []tailscalePeer
	MagicDNSSuffix  string
	MagicDNSEnabled bool
}

// tailscaleClient reports the local daemon's view of the tailnet. Tests inject
// fixtures; production shells out to the CLI.
type tailscaleClient interface {
	Status(context.Context) (*tailscaleStatus, error)
}

// tailscaleCLI runs `tailscale status --json`. The CLI works on Linux
// tailscaled and on both macOS variants, whose LocalAPI socket paths differ.
type tailscaleCLI struct{}

func tailscaleBinary() (string, error) {
	if path, err := exec.LookPath("tailscale"); err == nil {
		return path, nil
	}
	if runtime.GOOS == "darwin" {
		const app = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
		if _, err := os.Stat(app); err == nil {
			return app, nil
		}
	}
	return "", errors.New("tailscale CLI not found on PATH; install Tailscale or add its CLI to PATH")
}
func (tailscaleCLI) Status(ctx context.Context) (*tailscaleStatus, error) {
	binary, err := tailscaleBinary()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "status", "--json")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("tailscale status: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return parseTailscaleStatus(out)
}

// parseTailscaleStatus reads the fields Rhizome needs from the status JSON.
// Peers are sorted by DNS name so join attempts are deterministic.
func parseTailscaleStatus(raw []byte) (*tailscaleStatus, error) {
	type node struct {
		HostName, DNSName string
		TailscaleIPs      []string
		Online            bool
	}
	var doc struct {
		BackendState   string
		Self           *node
		Peer           map[string]*node
		MagicDNSSuffix string
		CurrentTailnet *struct {
			MagicDNSSuffix  string
			MagicDNSEnabled bool
		}
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse tailscale status: %w", err)
	}
	convert := func(n *node) tailscalePeer {
		p := tailscalePeer{HostName: n.HostName, DNSName: n.DNSName, Online: n.Online}
		for _, s := range n.TailscaleIPs {
			if ip := net.ParseIP(s); ip != nil {
				p.IPs = append(p.IPs, ip)
			}
		}
		return p
	}
	st := &tailscaleStatus{BackendState: doc.BackendState, MagicDNSSuffix: doc.MagicDNSSuffix}
	if doc.Self != nil {
		st.Self = convert(doc.Self)
	}
	if doc.CurrentTailnet != nil {
		st.MagicDNSEnabled = doc.CurrentTailnet.MagicDNSEnabled
		if doc.CurrentTailnet.MagicDNSSuffix != "" {
			st.MagicDNSSuffix = doc.CurrentTailnet.MagicDNSSuffix
		}
	}
	for _, p := range doc.Peer {
		if p != nil {
			st.Peers = append(st.Peers, convert(p))
		}
	}
	sort.Slice(st.Peers, func(i, j int) bool { return st.Peers[i].DNSName < st.Peers[j].DNSName })
	return st, nil
}

func (p tailscalePeer) ipv4() net.IP {
	for _, ip := range p.IPs {
		if v4 := ip.To4(); v4 != nil {
			return v4
		}
	}
	return nil
}

// label is the host label list_sessions shows: the MagicDNS short name, or
// the Tailscale hostname when MagicDNS is off.
func (p tailscalePeer) label(magicDNS bool) string {
	if magicDNS {
		if short := magicDNSShort(p.DNSName); short != "" {
			return short
		}
	}
	return p.HostName
}

// dialHost is where a peer's beacon is tried: its MagicDNS name, or its
// Tailscale IPv4 when MagicDNS is off. Empty when neither is available.
func (p tailscalePeer) dialHost(magicDNS bool) string {
	if magicDNS && p.DNSName != "" {
		return strings.TrimSuffix(p.DNSName, ".")
	}
	if ip := p.ipv4(); ip != nil {
		return ip.String()
	}
	return ""
}
func magicDNSShort(dnsName string) string {
	short, _, _ := strings.Cut(strings.TrimSuffix(dnsName, "."), ".")
	return short
}

// tailscaleSelf queries the daemon at startup and fails loudly when Tailscale
// is missing, not running, logged out or has no address. Tailscale mode never
// falls back to loopback or to every interface.
func tailscaleSelf(ctx context.Context, client tailscaleClient) (*tailscaleStatus, error) {
	st, err := client.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("tailscale mode: %w", err)
	}
	if st.BackendState != "Running" {
		return nil, fmt.Errorf("tailscale mode: tailscale is not running (state %q); run `tailscale up`", st.BackendState)
	}
	if st.Self.ipv4() == nil {
		return nil, errors.New("tailscale mode: this device has no Tailscale IPv4 address")
	}
	if !st.MagicDNSEnabled {
		slog.Info("magicdns_disabled", "message", "dialing tailnet peers by Tailscale IP and labelling hosts by Tailscale hostname")
	}
	return st, nil
}

// acquireBeacon claims the device's rendezvous port: a TCP forwarder from the
// well-known port to this process's gossip listener. memberlist joins are TCP
// push/pull, so a remote device dialing <magicdns>:7946 learns the real
// membership, with real ephemeral addresses, through whichever process holds
// the port. Gossip itself never moves, so holding or losing the beacon changes
// no identity. Returns whether this process holds it.
func (n *Node) acquireBeacon() bool {
	n.mu.Lock()
	held := n.beacon != nil
	n.mu.Unlock()
	if held || n.beaconPort == 0 {
		return held
	}
	l, err := net.Listen("tcp", net.JoinHostPort(n.bind, strconv.Itoa(n.beaconPort)))
	if err != nil {
		return false
	}
	n.mu.Lock()
	if n.ctx.Err() != nil {
		n.mu.Unlock()
		_ = l.Close()
		return false
	}
	n.beacon = l
	n.mu.Unlock()
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			n.wg.Add(1)
			go func() { defer n.wg.Done(); n.forward(conn) }()
		}
	}()
	slog.Info("beacon_acquired", "addr", l.Addr().String(), "gossip_endpoint", n.local().GossipEndpoint)
	return true
}

// forward relays one beacon connection to the local gossip listener. The
// gossip handler authenticates and decrypts exactly as for a direct dial; the
// beacon only exposes a port that is already exposed under another number.
func (n *Node) forward(conn net.Conn) {
	deadline := time.Now().Add(30 * time.Second)
	_ = conn.SetDeadline(deadline)
	upstream, err := net.DialTimeout("tcp", n.gossipAddr, 5*time.Second)
	if err != nil {
		_ = conn.Close()
		return
	}
	_ = upstream.SetDeadline(deadline)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		pipe := func(dst, src net.Conn) {
			defer wg.Done()
			_, _ = io.Copy(dst, src)
			if tcp, ok := dst.(*net.TCPConn); ok {
				_ = tcp.CloseWrite()
			}
		}
		wg.Add(2)
		go pipe(upstream, conn)
		go pipe(conn, upstream)
		wg.Wait()
	}()
	select {
	case <-done:
	case <-n.ctx.Done():
	}
	_ = conn.Close()
	_ = upstream.Close()
	<-done
}

// tailnetTargets lists the beacon endpoints worth a join attempt this tick:
// online peers other than this device whose addresses are not already in the
// cluster. One reachable node per device is enough; gossip carries the rest.
func (n *Node) tailnetTargets(st *tailscaleStatus) []string {
	known := map[string]bool{}
	n.mu.Lock()
	for key, i := range n.discovered {
		if key == n.identity.NodeId {
			continue
		}
		for _, endpoint := range []string{i.Endpoint, i.GossipEndpoint} {
			if host, _, err := net.SplitHostPort(endpoint); err == nil {
				known[host] = true
			}
		}
	}
	n.mu.Unlock()
	port := strconv.Itoa(n.beaconPort)
	var targets []string
	for _, p := range st.Peers {
		if !p.Online || (p.DNSName == st.Self.DNSName && p.HostName == st.Self.HostName) {
			continue
		}
		skip := false
		for _, ip := range p.IPs {
			skip = skip || known[ip.String()]
		}
		host := p.dialHost(st.MagicDNSEnabled)
		if skip || host == "" {
			continue
		}
		targets = append(targets, net.JoinHostPort(host, port))
	}
	return targets
}

// tailnet (re-)acquires the beacon and seeds gossip from tailnet devices on
// every tick. Like mDNS it only proposes seeds; memberlist's token-keyed
// push/pull remains the authentication step, so a device without Rhizome or
// with another token fails to join and is retried, never fatal.
func (n *Node) tailnet(interval time.Duration) {
	delay := time.Duration(0)
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = interval
		n.acquireBeacon()
		st, err := n.ts.Status(n.ctx)
		if err != nil {
			if n.ctx.Err() != nil {
				return
			}
			slog.Warn("tailscale_status", "error", err)
			continue
		}
		for _, target := range n.tailnetTargets(st) {
			if n.ctx.Err() != nil {
				return
			}
			err := n.join("tailscale", target)
			n.mu.Lock()
			warned := n.warned[target]
			n.warned[target] = err != nil
			n.mu.Unlock()
			switch {
			case err == nil:
			case warned:
				slog.Debug("tailnet_peer_unreachable", "addr", target, "error", err)
			default:
				slog.Warn("tailnet_peer_unreachable", "addr", target, "error", err)
			}
		}
	}
}
