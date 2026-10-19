package channel

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"math/big"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	sockaddr "github.com/hashicorp/go-sockaddr"
	"github.com/hashicorp/mdns"
	"google.golang.org/grpc/credentials"
)

// Options configures discovery and transport for one process. The zero value
// is the loopback-only cluster; LAN mode is opt-in and changes the trust
// boundary from "this user on this machine" to "any holder of the token on
// the local network".
type Options struct {
	Runtime       string        // registry directory; default ~/.rhizome/run
	LAN           bool          // bind and advertise on the local network
	Tailscale     bool          // bind the Tailscale IP and discover tailnet peers via the local daemon; implies the LAN transport
	BindAddr      string        // LAN: listen address, default 0.0.0.0 (Tailscale: the Tailscale IP)
	AdvertiseAddr string        // LAN: address peers dial, default first private interface (Tailscale: the Tailscale IP)
	Join          []string      // LAN: gossip host:port seeds on other machines
	MDNS          bool          // LAN: advertise and browse _rhizome._tcp
	Token         string        // explicit cluster token (64 hex characters)
	TokenFile     string        // file holding the cluster token
	GossipPort    int           // Tailscale: well-known gossip port tried first (the beacon); 0 means 7946
	Discovery     time.Duration // Tailscale: tailnet peer enumeration interval; 0 means 20s

	tailscale tailscaleClient // test hook; nil uses the tailscale CLI
}

const mdnsService = "_rhizome._tcp"

// mode selects the address policy, transport and discovery. Processes in
// different modes never peer, even on one device.
type mode uint8

const (
	modeLocal     mode = iota // loopback only, plaintext
	modeLAN                   // private ranges, TLS pinned via gossip, mDNS
	modeTailscale             // Tailscale ranges and loopback, TLS, tailnet enumeration
)

func (m mode) String() string {
	switch m {
	case modeLAN:
		return "lan"
	case modeTailscale:
		return "tailscale"
	}
	return "local"
}
func (o Options) mode() mode {
	switch {
	case o.Tailscale:
		return modeTailscale
	case o.LAN:
		return modeLAN
	}
	return modeLocal
}
func (o Options) validate() error {
	if o.mode() == modeLocal && (o.BindAddr != "" || o.AdvertiseAddr != "" || len(o.Join) > 0) {
		return errors.New("bind/advertise/join require LAN mode")
	}
	if !o.Tailscale && (o.GossipPort != 0 || o.Discovery != 0) {
		return errors.New("gossip-port/discovery-interval require Tailscale mode")
	}
	if o.GossipPort < 0 || o.GossipPort > 65535 || o.Discovery < 0 {
		return errors.New("gossip port must be 1-65535 and the discovery interval positive")
	}
	return nil
}

// allowedIP is the address policy. Local mode accepts exactly 127.0.0.1, as
// before. LAN mode adds RFC 1918, IPv6 ULA and link-local ranges. Tailscale
// mode accepts loopback plus the Tailscale ranges only, so a plain-LAN
// endpoint never passes. Public addresses are rejected in every mode.
func allowedIP(ip net.IP, m mode) bool {
	if ip == nil {
		return false
	}
	switch m {
	case modeLAN:
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	case modeTailscale:
		return ip.IsLoopback() || tailscaleIP(ip)
	}
	return ip.Equal(net.IPv4(127, 0, 0, 1))
}
func allowedEndpoint(endpoint string, m mode) bool {
	h, p, err := net.SplitHostPort(endpoint)
	port, e := strconv.Atoi(p)
	return err == nil && e == nil && port > 0 && port <= 65535 && allowedIP(net.ParseIP(h), m)
}
func (n *Node) allowed(endpoint string) bool { return allowedEndpoint(endpoint, n.mode) }

func validToken(s string) bool {
	_, err := hex.DecodeString(s)
	return len(s) == 64 && err == nil
}
func validFingerprint(s string) bool { return validToken(s) }

// advertiseAddress picks the IP peers dial. Auto-detection fails loudly rather
// than falling back to loopback, which would advertise an unreachable node.
func advertiseAddress(o Options) (string, error) {
	addr := o.AdvertiseAddr
	if addr == "" {
		if bind := net.ParseIP(o.BindAddr); bind != nil && !bind.IsUnspecified() {
			addr = bind.String()
		} else if o.mode() == modeTailscale {
			// start fills this from the daemon; reaching here means it had no address.
			return "", errors.New("no Tailscale IP found; set --advertise-addr")
		} else {
			var err error
			if addr, err = sockaddr.GetPrivateIP(); err != nil {
				return "", fmt.Errorf("detect advertise address: %w", err)
			}
			if addr == "" {
				return "", errors.New("no private interface address found; set --advertise-addr")
			}
		}
	}
	ip := net.ParseIP(addr)
	if !allowedIP(ip, o.mode()) {
		ranges := "loopback, private or link-local"
		if o.mode() == modeTailscale {
			ranges = "loopback or Tailscale"
		}
		return "", fmt.Errorf("advertise address %q must be a %s IP", addr, ranges)
	}
	return ip.String(), nil
}

func fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

// newCertificate makes an ephemeral self-signed server certificate. Peers pin
// its SPKI fingerprint from gossip metadata, so no CA is distributed.
func newCertificate(ip net.IP) (tls.Certificate, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "rhizome"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(365 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{ip}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, fingerprint(leaf), nil
}
func serverCredentials(cert tls.Certificate) credentials.TransportCredentials {
	return credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
}

// pinned verifies the server's SPKI fingerprint during the handshake, before
// any request metadata (and therefore the bearer token) is sent.
func pinned(expected string) credentials.TransportCredentials {
	return credentials.NewTLS(&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13, VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return errors.New("no peer certificate")
		}
		c, err := x509.ParseCertificate(raw[0])
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(fingerprint(c)), []byte(expected)) != 1 {
			return errors.New("peer certificate does not match gossip fingerprint")
		}
		return nil
	}})
}

func shortHost() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	h, _, _ = strings.Cut(h, ".")
	return clip(h, 64)
}

// clusterTag lets mDNS browsing skip clusters with other tokens without
// revealing anything recoverable about the token.
func (n *Node) clusterTag() string {
	sum := sha256.Sum256([]byte("rhizome-cluster:" + n.token))
	return hex.EncodeToString(sum[:8])
}

// join seeds gossip from one address. memberlist authenticates the push/pull
// with the token-derived key, so a seed with another token fails here and
// never becomes a peer; gRPC identity is verified later by probe.
func (n *Node) join(source, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		ips, e := net.DefaultResolver.LookupIPAddr(n.ctx, host)
		if e != nil {
			return e
		}
		// Prefer IPv4: MagicDNS returns both families and v4 is sufficient.
		for _, candidate := range ips {
			if !allowedIP(candidate.IP, n.mode) {
				continue
			}
			if ip == nil || (ip.To4() == nil && candidate.IP.To4() != nil) {
				ip = candidate.IP
			}
		}
	}
	if ip == nil {
		return fmt.Errorf("no allowed address for %q", host)
	}
	target := net.JoinHostPort(ip.String(), port)
	if !n.allowed(target) {
		return fmt.Errorf("seed %s is outside the allowed address policy", target)
	}
	if _, err := n.members.Join([]string{target}); err != nil {
		return err
	}
	slog.Info("seed_joined", "source", source, "addr", target)
	return nil
}

func (n *Node) startMDNS(ip net.IP, gossipPort int) error {
	logger := log.New(gossipLog{"mdns"}, "", 0)
	service, err := mdns.NewMDNSService(n.identity.NodeId, mdnsService, "local.", "rhizome-"+n.identity.NodeId[:12]+".local.", gossipPort, []net.IP{ip}, []string{"id=" + n.identity.NodeId, "c=" + n.clusterTag(), "p=1"})
	if err != nil {
		return err
	}
	server, err := mdns.NewServer(&mdns.Config{Zone: service, Logger: logger})
	if err != nil {
		return err
	}
	n.mdns = server
	n.wg.Add(1)
	go func() { defer n.wg.Done(); n.browse(logger) }()
	return nil
}
func (n *Node) browse(logger *log.Logger) {
	delay := time.Duration(0)
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = 15 * time.Second
		entries := make(chan *mdns.ServiceEntry, 64)
		go func() {
			defer close(entries)
			ctx, cancel := context.WithTimeout(n.ctx, 3*time.Second)
			defer cancel()
			_ = mdns.QueryContext(ctx, &mdns.QueryParam{Service: mdnsService, Domain: "local", Timeout: 2 * time.Second, Entries: entries, DisableIPv6: true, Logger: logger})
		}()
		for e := range entries {
			n.seed(e)
		}
	}
}
func (n *Node) seed(e *mdns.ServiceEntry) {
	var id, tag string
	for _, f := range e.InfoFields {
		if v, ok := strings.CutPrefix(f, "id="); ok {
			id = v
		} else if v, ok := strings.CutPrefix(f, "c="); ok {
			tag = v
		}
	}
	if id == "" || id == n.identity.NodeId || tag != n.clusterTag() || e.Port <= 0 || !allowedIP(e.AddrV4, n.mode) {
		return
	}
	n.mu.Lock()
	_, known := n.discovered[id]
	n.mu.Unlock()
	if known {
		return
	}
	addr := net.JoinHostPort(e.AddrV4.String(), strconv.Itoa(e.Port))
	if err := n.join("mdns", addr); err != nil {
		slog.Warn("seed_join", "source", "mdns", "addr", addr, "error", err)
	}
}
