package channel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rhizome/internal/config"
)

var (
	dreamReview = config.DreamSharing{Mode: config.DreamShareReview, Lookback: "7d"}
	dreamOff    = config.DreamSharing{Mode: config.DreamShareOff, Lookback: "7d"}
)

// fastDigestPulls shortens the pull cadence for one test.
func fastDigestPulls(t *testing.T) {
	t.Helper()
	first, retry, interval := digestFirstPull, digestRetry, digestInterval
	digestFirstPull, digestRetry, digestInterval = 20*time.Millisecond, 50*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { digestFirstPull, digestRetry, digestInterval = first, retry, interval })
}

// startDreamGateway starts a gateway with a [gateway.dream] policy and seeds
// its local store with one approved digest whose text is marker.
func startDreamGateway(t *testing.T, policy config.DreamSharing, marker string) *gatewayHarness {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	runtime := digestRuntime(t)
	if marker != "" {
		now := time.Now().UTC()
		e := DigestEntry{Type: "working_on", Text: marker, Project: "github.com/acme/app", Sources: []DigestSource{{SessionID: "s-" + marker, Agent: "codex"}}, ExpiresAt: now.Add(DigestDefaultTTL)}
		if err := (DigestStore{Runtime: runtime}).Save(Digest{Version: 1, ID: "00000000000000c1", CreatedAt: now, Status: DigestApproved, Approved: []int{0}, Entries: []DigestEntry{e}}, now); err != nil {
			t.Fatal(err)
		}
		// A pending digest is never served in review mode.
		pending := e
		pending.Text = marker + "-pending"
		if err := (DigestStore{Runtime: runtime}).Save(Digest{Version: 1, ID: "00000000000000c2", CreatedAt: now, Status: DigestPending, Entries: []DigestEntry{pending}}, now); err != nil {
			t.Fatal(err)
		}
	}
	h := &gatewayHarness{runtime: runtime, cfg: config.Gateway{Mode: "lan", Listen: address, Dream: policy}}
	h.start(t)
	t.Cleanup(func() { h.stop(t) })
	return h
}

func receivedTexts(runtime string) map[string]string {
	out := map[string]string{}
	for _, d := range PeerDigests(runtime, time.Now().UTC()) {
		out[d.Entry.Text] = d.HostID
	}
	return out
}

func TestDreamDigestsAcrossGateways(t *testing.T) {
	fastDigestPulls(t)
	a := startDreamGateway(t, dreamReview, "from-a")
	b := startDreamGateway(t, dreamReview, "from-b")
	c := startDreamGateway(t, dreamReview, "from-c")
	hostA, hostB, hostC := gatewayHost(t, a), gatewayHost(t, b), gatewayHost(t, c)
	a.command(t, GatewayCommand{Action: "join", Code: b.invite(t)})
	b.command(t, GatewayCommand{Action: "join", Code: c.invite(t)})
	gatewayConnected(t, a, hostB)
	gatewayConnected(t, c, hostB)

	// Approved digests are exchanged in both directions over each direct link,
	// stamped with the authenticated host ID; pending ones are never served.
	gatewayUntil(t, func() bool { return receivedTexts(a.runtime)["from-b"] == hostB })
	gatewayUntil(t, func() bool {
		got := receivedTexts(b.runtime)
		return got["from-a"] == hostA && got["from-c"] == hostC
	})
	gatewayUntil(t, func() bool { return receivedTexts(c.runtime)["from-b"] == hostB })
	for _, h := range []*gatewayHarness{a, b, c} {
		for text := range receivedTexts(h.runtime) {
			if strings.HasSuffix(text, "-pending") {
				t.Fatalf("pending digest served: %s", text)
			}
		}
	}
	// No forwarding: across several more pulls, A's digest never reaches C and
	// C's never reaches A.
	time.Sleep(5 * digestInterval)
	if got := receivedTexts(c.runtime); len(got) != 1 || got["from-a"] != "" {
		t.Fatalf("C received %v", got)
	}
	if got := receivedTexts(a.runtime); len(got) != 1 || got["from-c"] != "" {
		t.Fatalf("A received %v", got)
	}
	info, err := os.Stat(filepath.Join(a.runtime, "gateway", "dream", hostB+".json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("received file: %v %v", info, err)
	}

	// Revocation deletes received digests on both ends of the link.
	b.command(t, GatewayCommand{Action: "revoke", ID: hostA})
	gatewayUntil(t, func() bool {
		_, errA := os.Stat(filepath.Join(a.runtime, "gateway", "dream", hostB+".json"))
		_, errB := os.Stat(filepath.Join(b.runtime, "gateway", "dream", hostA+".json"))
		return os.IsNotExist(errA) && os.IsNotExist(errB)
	})
	if got := receivedTexts(a.runtime); len(got) != 0 {
		t.Fatalf("revoked host's digests kept: %v", got)
	}
	if got := receivedTexts(b.runtime); got["from-a"] != "" || got["from-c"] != hostC {
		t.Fatalf("B after revoking A: %v", got)
	}

	// Stopping the service removes every received digest.
	c.stop(t)
	if _, err := os.Stat(filepath.Join(c.runtime, "gateway", "dream")); !os.IsNotExist(err) {
		t.Fatalf("received digests survived service stop: %v", err)
	}
}

func TestDreamDigestsNeedBothHosts(t *testing.T) {
	fastDigestPulls(t)
	for _, tc := range []struct {
		name string
		a, b config.DreamSharing
	}{
		{"peer off", dreamReview, dreamOff},
		{"both off", dreamOff, dreamOff},
		{"zero value peer", dreamReview, config.DreamSharing{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := startDreamGateway(t, tc.a, "from-a")
			b := startDreamGateway(t, tc.b, "from-b")
			hostA, hostB := gatewayHost(t, a), gatewayHost(t, b)
			a.command(t, GatewayCommand{Action: "join", Code: b.invite(t)})
			gatewayConnected(t, a, hostB)
			gatewayConnected(t, b, hostA)
			time.Sleep(5 * digestInterval)
			// The link stays up and nothing is exchanged either way.
			gatewayConnected(t, a, hostB)
			if got := receivedTexts(a.runtime); len(got) != 0 {
				t.Fatalf("A received %v", got)
			}
			if got := receivedTexts(b.runtime); len(got) != 0 {
				t.Fatalf("B received %v", got)
			}
		})
	}
}

// dreamHandlerGateway builds a gateway with one trusted, connected peer whose
// link negotiated dream as chosen, for direct handler tests.
func dreamHandlerGateway(t *testing.T, policy config.DreamSharing, dream bool) (*gateway, func(method string) *httptest.ResponseRecorder) {
	t.Helper()
	cert, host, err := gatewayCertificate(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runtime := digestRuntime(t)
	now := time.Now().UTC()
	e := DigestEntry{Type: "gotcha", Text: "served", Project: "github.com/acme/app", Sources: []DigestSource{{SessionID: "s1", Agent: "claude"}}, ExpiresAt: now.Add(time.Hour)}
	if err := (DigestStore{Runtime: runtime}).Save(Digest{Version: 1, ID: "00000000000000d1", CreatedAt: now, Status: DigestApproved, Approved: []int{0}, Entries: []DigestEntry{e}}, now); err != nil {
		t.Fatal(err)
	}
	g := &gateway{ctx: ctx, runtime: runtime, cfg: config.Gateway{Mode: "lan", Dream: policy}, dreamActive: map[string]int{}, state: gatewayState{Peers: map[string]gatewayPeer{host: {Host: host, Grant: "grant"}}}, links: map[string]*gatewayLink{}}
	lctx, lcancel := context.WithCancel(ctx)
	t.Cleanup(lcancel)
	g.links[host] = &gatewayLink{g: g, host: host, grant: "grant", dream: dream, ctx: lctx, cancel: lcancel}
	do := func(method string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/v1/dream/digests", nil)
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
		w := httptest.NewRecorder()
		g.dreamHTTP(w, r)
		return w
	}
	return g, do
}

func TestDreamHTTPPolicy(t *testing.T) {
	_, do := dreamHandlerGateway(t, dreamReview, true)
	w := do(http.MethodGet)
	var reply digestReply
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &reply) != nil || len(reply.Digests) != 1 || reply.Digests[0].Entries[0].Text != "served" {
		t.Fatalf("serve: %d %s", w.Code, w.Body)
	}
	if w := do(http.MethodPost); w.Code != http.StatusBadRequest {
		t.Fatalf("POST: %d", w.Code)
	}
	if _, do := dreamHandlerGateway(t, dreamReview, false); do(http.MethodGet).Code != http.StatusForbidden {
		t.Fatal("served a link without the dream subprotocol")
	}
	if _, do := dreamHandlerGateway(t, dreamOff, true); do(http.MethodGet).Code != http.StatusForbidden {
		t.Fatal("served with policy off")
	}
	// Unknown and revoked peers fail closed.
	g, do := dreamHandlerGateway(t, dreamReview, true)
	for host := range g.state.Peers {
		g.state.Peers[host] = gatewayPeer{Host: host, Grant: "rotated"}
	}
	if do(http.MethodGet).Code != http.StatusForbidden {
		t.Fatal("served a peer whose grant changed")
	}
	g.state.Peers = map[string]gatewayPeer{}
	if do(http.MethodGet).Code != http.StatusForbidden {
		t.Fatal("served an unknown peer")
	}
}

func TestDreamHTTPConcurrencyBound(t *testing.T) {
	_, do := dreamHandlerGateway(t, dreamReview, true)
	entered := make(chan struct{}, maxDigestActive)
	release := make(chan struct{})
	digestServeHook = func() { entered <- struct{}{}; <-release }
	t.Cleanup(func() { digestServeHook = nil })
	var wg sync.WaitGroup
	codes := make(chan int, maxDigestActive)
	for i := 0; i < maxDigestActive; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- do(http.MethodGet).Code }()
	}
	for i := 0; i < maxDigestActive; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("requests did not start")
		}
	}
	if code := do(http.MethodGet).Code; code != http.StatusTooManyRequests {
		t.Fatalf("third concurrent request: %d", code)
	}
	close(release)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("bounded request: %d", code)
		}
	}
}
