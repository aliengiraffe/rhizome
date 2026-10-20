package channel

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/memberlist"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "rhizome/proto"
)

func TestAllowedEndpointPolicy(t *testing.T) {
	cases := []struct {
		endpoint   string
		local, lan bool
	}{
		{"127.0.0.1:4000", true, true},
		{"127.0.0.2:4000", false, true},
		{"[::1]:4000", false, true},
		{"10.1.2.3:4000", false, true},
		{"172.16.5.5:4000", false, true},
		{"192.168.1.20:4000", false, true},
		{"169.254.10.10:4000", false, true},
		{"[fd12::1]:4000", false, true},
		{"[fe80::1]:4000", false, true},
		{"8.8.8.8:4000", false, false},
		{"[2001:db8::1]:4000", false, false},
		{"0.0.0.0:4000", false, false},
		{"127.0.0.1:0", false, false},
		{"127.0.0.1:70000", false, false},
		{"127.0.0.1", false, false},
		{"localhost:4000", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		if got := allowedEndpoint(c.endpoint, false); got != c.local {
			t.Errorf("local %q: got %v want %v", c.endpoint, got, c.local)
		}
		if got := allowedEndpoint(c.endpoint, true); got != c.lan {
			t.Errorf("lan %q: got %v want %v", c.endpoint, got, c.lan)
		}
	}
	if (Options{BindAddr: "0.0.0.0"}).validate() == nil || (Options{Join: []string{"10.0.0.1:1"}}).validate() == nil {
		t.Fatal("LAN-only options accepted in local mode")
	}
}

// lanNode starts a LAN-mode node bound to loopback with its own runtime
// directory, which is how two machines look from one host.
func lanNode(t *testing.T, token string, join ...string) *Node {
	t.Helper()
	n := node(t)
	if err := n.start(Options{Runtime: filepath.Join(t.TempDir(), "runtime"), LAN: true, BindAddr: "127.0.0.1", Token: token, Join: join}); err != nil {
		t.Fatal(err)
	}
	return n
}
func TestLANClusterTLSAndJoin(t *testing.T) {
	a := lanNode(t, "")
	if !validFingerprint(a.local().CertFingerprint) || a.local().Host != shortHost() {
		t.Fatalf("identity missing LAN fields: %v", a.local())
	}
	b := lanNode(t, a.token, a.local().GossipEndpoint)
	if b.token != a.token {
		t.Fatal("token not adopted from options")
	}
	dir := t.TempDir()
	bind(t, a, "session-a", dir)
	bind(t, b, "session-b", dir)
	eventually(t, func() bool { _, err := b.resolve(context.Background(), "session-a"); return err == nil })
	eventually(t, func() bool { _, err := a.resolve(context.Background(), "session-b"); return err == nil })
	if err := b.subscribe("session-a"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.feeds["session-a"].status == "following" })
	if err := a.announce("over the wire", ""); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.incoming) == 1 && b.incoming[0].event.Text == "over the wire"
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	accepted, err := a.send(ctx, "session-b", "direct", "")
	if err != nil || !accepted.Accepted {
		t.Fatalf("send %v %v", accepted, err)
	}
	eventually(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return len(b.incoming) == 2 })
	// Replay across the pinned transport follows the same cursor rules as loopback.
	conn, err := b.dial(a.local())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	stream, err := pb.NewChannelClient(conn).Subscribe(b.outgoing(ctx), &pb.FollowRequest{Target: a.local(), Subscriber: b.local(), StreamId: "old-incarnation", AfterSequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	if e, err := stream.Recv(); err != nil || e.Kind != "gap" {
		t.Fatalf("expected gap %v %v", e, err)
	}
	var sessions int
	for _, p := range b.peers() {
		if p.SessionId != "" && p.Host == "" {
			t.Fatalf("peer without host: %v", p)
		}
		if p.SessionId != "" {
			sessions++
		}
	}
	if sessions != 2 {
		t.Fatalf("expected two sessions, got %d", sessions)
	}
}
func TestLANRejectsWrongTokenAndBadPins(t *testing.T) {
	a := lanNode(t, "")
	stranger := node(t)
	if err := stranger.start(Options{Runtime: filepath.Join(t.TempDir(), "runtime"), LAN: true, BindAddr: "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if stranger.token == a.token {
		t.Fatal("expected distinct cluster token")
	}
	if err := stranger.join("test", a.local().GossipEndpoint); err == nil {
		t.Fatal("gossip join succeeded without the cluster key")
	}
	eventually(t, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, seen := a.discovered[stranger.identity.NodeId]
		return !seen
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Correct pin, wrong bearer: the TLS handshake succeeds and the RPC is refused.
	conn, err := stranger.dial(a.local())
	if err != nil {
		t.Fatal(err)
	}
	_, err = pb.NewChannelClient(conn).Info(stranger.outgoing(ctx), &pb.InfoRequest{})
	conn.Close()
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("wrong token: %v", err)
	}
	// Wrong pin, correct bearer: the handshake fails before any metadata is sent.
	b := lanNode(t, a.token)
	forged := a.local()
	forged.CertFingerprint = strings.Repeat("0", 64)
	conn, err = b.dial(forged)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pb.NewChannelClient(conn).Info(b.outgoing(ctx), &pb.InfoRequest{})
	conn.Close()
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("forged pin: %v", err)
	}
	unpinned := a.local()
	unpinned.CertFingerprint = ""
	if _, err := b.dial(unpinned); err == nil {
		t.Fatal("dialed a LAN peer without a fingerprint")
	}
	public := a.local()
	public.Endpoint = "8.8.8.8:443"
	if _, err := b.dial(public); err == nil {
		t.Fatal("dialed a public endpoint")
	}
	// A seed outside the policy is refused before any network activity.
	if err := b.join("test", "8.8.8.8:7946"); err == nil {
		t.Fatal("joined a public seed")
	}
	// A seed that does not answer is logged by start, not fatal.
	c := node(t)
	if err := c.start(Options{Runtime: filepath.Join(t.TempDir(), "runtime"), LAN: true, BindAddr: "127.0.0.1", Token: a.token, Join: []string{"127.0.0.1:1"}}); err != nil {
		t.Fatalf("unreachable seed aborted startup: %v", err)
	}
}
func TestModeIsolation(t *testing.T) {
	meta := func(endpoint, fp string) []byte {
		b, _ := json.Marshal(&pb.Identity{Protocol: 1, NodeId: "peer", SessionId: "s", Endpoint: endpoint, GossipEndpoint: endpoint, CertFingerprint: fp})
		return b
	}
	discovered := func(n *Node) bool { n.mu.Lock(); defer n.mu.Unlock(); _, ok := n.discovered["peer"]; return ok }
	local, lan := node(t), node(t)
	lan.lan = true
	fp := strings.Repeat("a", 64)
	// Local mode ignores LAN advertisements, even ones reachable on loopback.
	local.updatePeer(&memberlist.Node{Name: "peer", Addr: net.ParseIP("192.168.1.5"), Meta: meta("192.168.1.5:4000", fp)})
	local.updatePeer(&memberlist.Node{Name: "peer", Addr: net.ParseIP("127.0.0.1"), Meta: meta("127.0.0.1:4000", fp)})
	if discovered(local) {
		t.Fatal("local mode accepted a LAN peer")
	}
	// LAN mode ignores unpinned local peers and public addresses.
	lan.updatePeer(&memberlist.Node{Name: "peer", Addr: net.ParseIP("127.0.0.1"), Meta: meta("127.0.0.1:4000", "")})
	lan.updatePeer(&memberlist.Node{Name: "peer", Addr: net.ParseIP("8.8.8.8"), Meta: meta("8.8.8.8:4000", fp)})
	lan.updatePeer(&memberlist.Node{Name: "peer", Addr: net.ParseIP("192.168.1.5"), Meta: meta("8.8.8.8:4000", fp)})
	if discovered(lan) {
		t.Fatal("LAN mode accepted an unpinned or public peer")
	}
	lan.updatePeer(&memberlist.Node{Name: "peer", Addr: net.ParseIP("192.168.1.5"), Meta: meta("192.168.1.5:4000", fp)})
	if !discovered(lan) {
		t.Fatal("LAN mode rejected a private peer")
	}
	// Old peers without the new fields still parse in local mode.
	local.updatePeer(&memberlist.Node{Name: "peer", Addr: net.ParseIP("127.0.0.1"), Meta: meta("127.0.0.1:4000", "")})
	if !discovered(local) {
		t.Fatal("local mode rejected a loopback peer")
	}
	// Sharing one registry across modes must not prune the other mode's live registration.
	runtime := filepath.Join(t.TempDir(), "runtime")
	first := node(t)
	if err := first.start(Options{Runtime: runtime}); err != nil {
		t.Fatal(err)
	}
	second := node(t)
	if err := second.start(Options{Runtime: runtime, LAN: true, BindAddr: "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if second.token != first.token {
		t.Fatal("LAN node did not adopt the machine token")
	}
	for _, path := range []string{first.registration, second.registration} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("registration pruned across modes: %v", err)
		}
	}
	third := node(t)
	if err := third.start(Options{Runtime: runtime, Token: strings.Repeat("b", 64)}); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflicting token accepted: %v", err)
	}
}
func TestTokenCommandAndSeeding(t *testing.T) {
	runtime := filepath.Join(t.TempDir(), "runtime")
	token, err := Token(runtime)
	if err != nil || !validToken(token) {
		t.Fatalf("token %q %v", token, err)
	}
	again, err := Token(runtime)
	if err != nil || again != token {
		t.Fatal("token not stable")
	}
	file := filepath.Join(t.TempDir(), "copied")
	if err := os.WriteFile(file, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	seeded := node(t)
	if err := seeded.start(Options{Runtime: filepath.Join(t.TempDir(), "runtime"), LAN: true, BindAddr: "127.0.0.1", TokenFile: file}); err != nil {
		t.Fatal(err)
	}
	if seeded.token != token {
		t.Fatal("token file not adopted")
	}
	bad := node(t)
	if err := bad.start(Options{Runtime: filepath.Join(t.TempDir(), "runtime"), LAN: true, BindAddr: "127.0.0.1", Token: "short"}); err == nil {
		t.Fatal("invalid token accepted")
	}
}
