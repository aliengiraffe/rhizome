package channel

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pb "rhizome/proto"
)

// statusFixture mirrors `tailscale status --json` from Tailscale 1.104 on
// macOS, reduced to the fields Rhizome reads plus a few it ignores.
const statusFixture = `{
 "Version": "1.104.1-t7f4efe814-g4939ea2e4",
 "BackendState": "Running",
 "TUN": true,
 "Self": {"ID": "n1", "HostName": "delta", "DNSName": "delta.tail2fdf06.ts.net.", "OS": "macOS", "TailscaleIPs": ["100.93.74.31", "fd7a:115c:a1e0::df39:4a20"], "Online": true, "Relay": "sfo"},
 "MagicDNSSuffix": "tail2fdf06.ts.net",
 "CurrentTailnet": {"Name": "user@example.com", "MagicDNSSuffix": "tail2fdf06.ts.net", "MagicDNSEnabled": true},
 "Peer": {
  "nodekey:bb": {"ID": "n3", "HostName": "server", "DNSName": "server.tail2fdf06.ts.net.", "OS": "linux", "TailscaleIPs": ["100.70.1.2", "fd7a:115c:a1e0::1"], "Online": false},
  "nodekey:aa": {"ID": "n2", "HostName": "Nico’s MacBook Pro", "DNSName": "nicos-macbook-pro.tail2fdf06.ts.net.", "OS": "macOS", "TailscaleIPs": ["100.114.80.29", "fd7a:115c:a1e0::6e39:501e"], "Online": true, "Relay": "sfo"}
 }
}`

type fakeTailscale struct {
	mu  sync.Mutex
	st  *tailscaleStatus
	err error
}

func (f *fakeTailscale) Status(context.Context) (*tailscaleStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.st, nil
}

// loopbackStatus describes a tailnet where every device is 127.0.0.1 and
// MagicDNS is off, so peers are dialed by address; "name" is a short name.
func loopbackStatus(self string, online []string, offline []string) *tailscaleStatus {
	device := func(name string, up bool) tailscalePeer {
		return tailscalePeer{HostName: name, DNSName: name + ".example.ts.net.", IPs: []net.IP{net.ParseIP("127.0.0.1")}, Online: up}
	}
	st := &tailscaleStatus{BackendState: "Running", Self: device(self, true), MagicDNSSuffix: "example.ts.net"}
	for _, name := range online {
		st.Peers = append(st.Peers, device(name, true))
	}
	for _, name := range offline {
		st.Peers = append(st.Peers, device(name, false))
	}
	return st
}
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
func port(t *testing.T, endpoint string) int {
	t.Helper()
	_, p, err := net.SplitHostPort(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return n
}

// tsNode starts a Tailscale-mode node on loopback with an injected daemon
// view and its own runtime directory. close is idempotent for manual exits.
func tsNode(t *testing.T, token string, beacon int, ts tailscaleClient, join ...string) (*Node, func()) {
	t.Helper()
	n := newNode(context.Background())
	var once sync.Once
	stop := func() { once.Do(n.close) }
	t.Cleanup(stop)
	if err := n.start(Options{Runtime: filepath.Join(t.TempDir(), "runtime"), Tailscale: true, BindAddr: "127.0.0.1", Token: token, GossipPort: beacon, Discovery: 200 * time.Millisecond, Join: join, tailscale: ts}); err != nil {
		t.Fatal(err)
	}
	return n, stop
}
func beacon(n *Node) bool { n.mu.Lock(); defer n.mu.Unlock(); return n.beacon != nil }

func TestTailscaleAddressPolicy(t *testing.T) {
	cases := []struct {
		ip             string
		local, lan, ts bool
	}{
		{"127.0.0.1", true, true, true},
		{"::1", false, true, true},
		{"100.93.74.31", false, false, true},
		{"100.64.0.1", false, false, true},
		{"100.127.255.254", false, false, true},
		{"100.63.255.255", false, false, false},
		{"100.128.0.1", false, false, false},
		// The Tailscale ULA sits inside fc00::/7, which LAN mode already accepts.
		{"fd7a:115c:a1e0::df39:4a20", false, true, true},
		{"fd7a:115c:a1e1::1", false, true, false},
		{"192.168.1.5", false, true, false},
		{"10.0.0.1", false, true, false},
		{"169.254.1.1", false, true, false},
		{"8.8.8.8", false, false, false},
		{"2001:db8::1", false, false, false},
		{"", false, false, false},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if got := allowedIP(ip, modeLocal); got != c.local {
			t.Errorf("local %q: got %v want %v", c.ip, got, c.local)
		}
		if got := allowedIP(ip, modeLAN); got != c.lan {
			t.Errorf("lan %q: got %v want %v", c.ip, got, c.lan)
		}
		if got := allowedIP(ip, modeTailscale); got != c.ts {
			t.Errorf("tailscale %q: got %v want %v", c.ip, got, c.ts)
		}
	}
	// Plain LAN mode rejects a Tailscale address exactly as before.
	if _, err := advertiseAddress(Options{LAN: true, AdvertiseAddr: "100.93.74.31"}); err == nil || !strings.Contains(err.Error(), "loopback, private or link-local") {
		t.Fatalf("lan accepted a CGNAT advertise address: %v", err)
	}
	if _, err := advertiseAddress(Options{Tailscale: true, AdvertiseAddr: "192.168.1.5"}); err == nil || !strings.Contains(err.Error(), "loopback or Tailscale") {
		t.Fatalf("tailscale accepted a private advertise address: %v", err)
	}
	if addr, err := advertiseAddress(Options{Tailscale: true, AdvertiseAddr: "100.93.74.31"}); err != nil || addr != "100.93.74.31" {
		t.Fatalf("tailscale rejected its own address: %q %v", addr, err)
	}
	// No interface fallback in Tailscale mode.
	if _, err := advertiseAddress(Options{Tailscale: true}); err == nil || !strings.Contains(err.Error(), "no Tailscale IP") {
		t.Fatalf("tailscale fell back to an interface address: %v", err)
	}
	if (Options{GossipPort: 7946}).validate() == nil || (Options{LAN: true, Discovery: time.Second}).validate() == nil || (Options{Tailscale: true, GossipPort: 70000}).validate() == nil {
		t.Fatal("Tailscale-only options accepted elsewhere")
	}
	if err := (Options{Tailscale: true, LAN: true, BindAddr: "127.0.0.1", GossipPort: 7946}).validate(); err != nil {
		t.Fatal(err)
	}
	if (Options{Tailscale: true, LAN: true}).mode() != modeTailscale {
		t.Fatal("--tailscale with --lan must select Tailscale mode")
	}
}
func TestParseTailscaleStatus(t *testing.T) {
	st, err := parseTailscaleStatus([]byte(statusFixture))
	if err != nil {
		t.Fatal(err)
	}
	if st.BackendState != "Running" || !st.MagicDNSEnabled || st.MagicDNSSuffix != "tail2fdf06.ts.net" {
		t.Fatalf("tailnet fields: %+v", st)
	}
	if st.Self.ipv4().String() != "100.93.74.31" || st.Self.dialHost(true) != "delta.tail2fdf06.ts.net" {
		t.Fatalf("self: %+v", st.Self)
	}
	if len(st.Peers) != 2 || st.Peers[0].DNSName != "nicos-macbook-pro.tail2fdf06.ts.net." || st.Peers[1].HostName != "server" {
		t.Fatalf("peers not sorted by DNS name: %+v", st.Peers)
	}
	if !st.Peers[0].Online || st.Peers[1].Online || st.Peers[0].ipv4().String() != "100.114.80.29" || len(st.Peers[0].IPs) != 2 {
		t.Fatalf("peer fields: %+v", st.Peers)
	}
	if st.Peers[0].dialHost(false) != "100.114.80.29" {
		t.Fatalf("MagicDNS-off dial host: %+v", st.Peers[0])
	}
	if _, err := tailscaleSelf(context.Background(), &fakeTailscale{st: st}); err != nil {
		t.Fatal(err)
	}
	off, err := parseTailscaleStatus([]byte(strings.Replace(statusFixture, `"MagicDNSEnabled": true`, `"MagicDNSEnabled": false`, 1)))
	if err != nil || off.MagicDNSEnabled {
		t.Fatalf("MagicDNSEnabled=false: %v %+v", err, off)
	}
	for _, c := range []struct{ name, raw, want string }{
		{"logged out", strings.Replace(statusFixture, `"BackendState": "Running"`, `"BackendState": "NeedsLogin"`, 1), "NeedsLogin"},
		{"no address", strings.Replace(statusFixture, `["100.93.74.31", "fd7a:115c:a1e0::df39:4a20"]`, `[]`, 1), "no Tailscale IPv4"},
	} {
		st, err := parseTailscaleStatus([]byte(c.raw))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tailscaleSelf(context.Background(), &fakeTailscale{st: st}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	if _, err := parseTailscaleStatus([]byte("not json")); err == nil {
		t.Fatal("parsed garbage")
	}
}
func TestTailscaleStartFailsLoudly(t *testing.T) {
	loggedOut, _ := parseTailscaleStatus([]byte(strings.Replace(statusFixture, `"BackendState": "Running"`, `"BackendState": "Stopped"`, 1)))
	for _, c := range []struct {
		name string
		ts   tailscaleClient
		want string
	}{
		{"not installed", &fakeTailscale{err: errors.New("tailscale CLI not found on PATH")}, "not found"},
		{"stopped", &fakeTailscale{st: loggedOut}, "Stopped"},
		{"no address", &fakeTailscale{st: &tailscaleStatus{BackendState: "Running"}}, "no Tailscale IPv4"},
	} {
		n := node(t)
		err := n.start(Options{Runtime: filepath.Join(t.TempDir(), "runtime"), Tailscale: true, tailscale: c.ts})
		if err == nil || !strings.Contains(err.Error(), "tailscale mode") || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v", c.name, err)
		}
		if n.rpc != nil || n.members != nil {
			t.Fatalf("%s: started listeners despite the failure", c.name)
		}
	}
}
func TestTailnetTargets(t *testing.T) {
	n := node(t)
	n.mode, n.beaconPort, n.advertiseAddr = modeTailscale, 7946, "100.1.1.1"
	n.discovered[n.identity.NodeId] = &pb.Identity{Endpoint: "100.1.1.1:5000", GossipEndpoint: "100.1.1.1:7946"}
	n.discovered["known"] = &pb.Identity{Endpoint: "100.2.2.2:5000", GossipEndpoint: "100.2.2.2:7946"}
	device := func(name, ip string, up bool) tailscalePeer {
		p := tailscalePeer{HostName: name, DNSName: name + ".ex.ts.net.", Online: up}
		if ip != "" {
			p.IPs = []net.IP{net.ParseIP(ip), net.ParseIP("fd7a:115c:a1e0::" + name[:1])}
		}
		return p
	}
	st := &tailscaleStatus{BackendState: "Running", Self: device("a", "100.1.1.1", true), MagicDNSEnabled: true, Peers: []tailscalePeer{
		device("a", "100.1.1.1", true),  // self listed defensively
		device("b", "100.9.9.9", false), // offline
		device("c", "100.2.2.2", true),  // already in the cluster
		device("d", "100.3.3.3", true),  // new device
		{DNSName: "", Online: true},     // nothing to dial
	}}
	if got := n.tailnetTargets(st); len(got) != 1 || got[0] != "d.ex.ts.net:7946" {
		t.Fatalf("MagicDNS targets: %v", got)
	}
	st.MagicDNSEnabled = false
	if got := n.tailnetTargets(st); len(got) != 1 || got[0] != "100.3.3.3:7946" {
		t.Fatalf("IP targets: %v", got)
	}
}
func TestTailscaleBeaconDiscoveryAndReacquire(t *testing.T) {
	beaconPort := freePort(t)
	alpha, stopAlpha := tsNode(t, "", beaconPort, &fakeTailscale{st: loopbackStatus("alpha", nil, nil)})
	if !beacon(alpha) || port(t, alpha.local().GossipEndpoint) == beaconPort || alpha.local().Host != shortHost() || !validFingerprint(alpha.local().CertFingerprint) {
		t.Fatalf("beacon holder identity: %v", alpha.local())
	}
	// Same device, port busy: no beacon; another device (beta) finds alpha
	// purely through enumeration and the beacon port, no --join, no mDNS.
	beta, _ := tsNode(t, alpha.token, beaconPort, &fakeTailscale{st: loopbackStatus("beta", []string{"alpha"}, []string{"gamma"})})
	if beacon(beta) {
		t.Fatalf("second process took the beacon: %v", beta.local())
	}
	dir := t.TempDir()
	bind(t, alpha, "session-alpha", dir)
	bind(t, beta, "session-beta", dir)
	eventually(t, func() bool { _, err := beta.resolve(context.Background(), "session-alpha"); return err == nil })
	eventually(t, func() bool { _, err := alpha.resolve(context.Background(), "session-beta"); return err == nil })
	for _, p := range alpha.peers() {
		if p.SessionId == "session-beta" && p.Host != shortHost() {
			t.Fatalf("peer host label: %v", p)
		}
	}
	if err := beta.subscribe("session-alpha"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		beta.mu.Lock()
		defer beta.mu.Unlock()
		return beta.feeds["session-alpha"].status == "following"
	})
	if err := alpha.announce("over the tailnet", ""); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		beta.mu.Lock()
		defer beta.mu.Unlock()
		return len(beta.incoming) == 1 && beta.incoming[0].event.Text == "over the tailnet"
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if accepted, err := alpha.send(ctx, "session-beta", "direct", ""); err != nil || !accepted.Accepted {
		t.Fatalf("send %v %v", accepted, err)
	}
	eventually(t, func() bool { beta.mu.Lock(); defer beta.mu.Unlock(); return len(beta.incoming) == 2 })
	// --join with a hostname resolves (preferring IPv4) and passes the policy.
	delta, _ := tsNode(t, alpha.token, beaconPort, &fakeTailscale{st: loopbackStatus("delta", nil, nil)}, "localhost:"+strconv.Itoa(beaconPort))
	bind(t, delta, "session-delta", dir)
	eventually(t, func() bool { _, err := alpha.resolve(context.Background(), "session-delta"); return err == nil })
	// The holder exits; whichever remaining process wins re-acquires the port
	// on its tick and the device stays discoverable for a new device.
	stopAlpha()
	eventually(t, func() bool { return beacon(beta) != beacon(delta) })
	if _, err := delta.resolve(context.Background(), "session-beta"); err != nil {
		t.Fatalf("gossip identity changed with the beacon: %v", err)
	}
	gamma, _ := tsNode(t, alpha.token, beaconPort, &fakeTailscale{st: loopbackStatus("gamma", []string{"beta"}, nil)})
	bind(t, gamma, "session-gamma", dir)
	eventually(t, func() bool { _, err := gamma.resolve(context.Background(), "session-beta"); return err == nil })
	eventually(t, func() bool { _, err := beta.resolve(context.Background(), "session-gamma"); return err == nil })
	if _, err := gamma.resolve(context.Background(), "session-alpha"); err == nil {
		t.Fatal("exited holder still resolvable")
	}
}
func TestTailscaleModeIsolation(t *testing.T) {
	runtime := filepath.Join(t.TempDir(), "runtime")
	lan := node(t)
	if err := lan.start(Options{Runtime: runtime, LAN: true, BindAddr: "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	ts := node(t)
	if err := ts.start(Options{Runtime: runtime, Tailscale: true, BindAddr: "127.0.0.1", GossipPort: freePort(t), tailscale: &fakeTailscale{st: loopbackStatus("delta", nil, nil)}}); err != nil {
		t.Fatal(err)
	}
	if ts.token != lan.token || ts.mode != modeTailscale {
		t.Fatal("Tailscale node did not adopt the machine token")
	}
	// Same token, same device, both on loopback: gossip keys differ per mode.
	if err := ts.join("test", lan.local().GossipEndpoint); err == nil {
		t.Fatal("Tailscale node joined a LAN cluster")
	}
	if err := lan.join("test", ts.local().GossipEndpoint); err == nil {
		t.Fatal("LAN node joined a Tailscale cluster")
	}
	time.Sleep(500 * time.Millisecond)
	for _, pair := range [][2]*Node{{lan, ts}, {ts, lan}} {
		pair[0].mu.Lock()
		_, seen := pair[0].discovered[pair[1].identity.NodeId]
		pair[0].mu.Unlock()
		if seen {
			t.Fatalf("%s node discovered a %s peer", pair[0].mode, pair[1].mode)
		}
	}
	// A tailnet device with another token is enumerated, fails to join, is
	// logged and retried, and never becomes a peer; nothing crashes.
	stranger := node(t)
	target := "127.0.0.1:" + strconv.Itoa(ts.beaconPort)
	if err := stranger.start(Options{Runtime: filepath.Join(t.TempDir(), "runtime"), Tailscale: true, BindAddr: "127.0.0.1", GossipPort: ts.beaconPort, Discovery: 100 * time.Millisecond, tailscale: &fakeTailscale{st: loopbackStatus("stranger", []string{"delta"}, nil)}}); err != nil {
		t.Fatal(err)
	}
	if stranger.token == ts.token {
		t.Fatal("expected a distinct token")
	}
	eventually(t, func() bool { stranger.mu.Lock(); defer stranger.mu.Unlock(); return stranger.warned[target] })
	time.Sleep(300 * time.Millisecond)
	for _, pair := range [][2]*Node{{stranger, ts}, {ts, stranger}} {
		pair[0].mu.Lock()
		_, seen := pair[0].discovered[pair[1].identity.NodeId]
		pair[0].mu.Unlock()
		if seen {
			t.Fatal("wrong-token tailnet device became a peer")
		}
	}
	if _, err := stranger.members.Join([]string{target}); err == nil {
		t.Fatal("stranger joined with the wrong key")
	}
}
