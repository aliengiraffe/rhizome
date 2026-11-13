package channel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"rhizome/internal/localstate"
)

// gatewaySubprotocolDream is offered first, and only, by hosts whose
// [gateway.dream] policy is not off. It implies the Pi and OpenCode
// subprotocols. Digests travel only over links that negotiated it, so a peer
// without it (an older build or one with sharing off) negotiates the Pi or
// OpenCode subprotocol instead, keeps its link, and never sees digests.
const gatewaySubprotocolDream = "rhizome.gateway.dream.v1"

const (
	maxDigestActive = 2 // concurrent /v1/dream/digests requests per paired host
	digestTimeout   = 9 * time.Second
)

// Pull cadence; tests shorten these.
var (
	digestFirstPull = time.Second
	digestRetry     = 30 * time.Second
	digestInterval  = 5 * time.Minute
)

// gatewaySubprotocols lists the subprotocols this host offers, most capable first.
func gatewaySubprotocols(dream bool) []string {
	if dream {
		return []string{gatewaySubprotocolDream, gatewaySubprotocolPi, gatewaySubprotocolOpenCode}
	}
	return []string{gatewaySubprotocolPi, gatewaySubprotocolOpenCode}
}

// dreamHTTP serves this host's own servable digests to a trusted, connected
// peer whose link negotiated the dream subprotocol. Policy is checked on every
// request; the caller's host ID comes only from its pinned certificate.
func (g *gateway) dreamHTTP(w http.ResponseWriter, r *http.Request) {
	host := gatewayCaller(r)
	l, err := g.fileLink(host)
	if err != nil || !l.dream {
		http.Error(w, "untrusted peer", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if !g.cfg.Dream.Enabled() {
		http.Error(w, "digest sharing disabled", http.StatusForbidden)
		return
	}
	g.dreamMu.Lock()
	if g.dreamActive[host] >= maxDigestActive {
		g.dreamMu.Unlock()
		http.Error(w, "busy", http.StatusTooManyRequests)
		return
	}
	g.dreamActive[host]++
	g.dreamMu.Unlock()
	defer func() {
		g.dreamMu.Lock()
		if g.dreamActive[host]--; g.dreamActive[host] == 0 {
			delete(g.dreamActive, host)
		}
		g.dreamMu.Unlock()
	}()
	if digestServeHook != nil {
		digestServeHook()
	}
	digests := servableDigests(g.runtime, g.cfg.Dream, g.now().UTC())
	// Revocation or disconnection while selecting fails closed.
	if _, err := g.fileLink(host); err != nil || l.ctx.Err() != nil {
		http.Error(w, "untrusted peer", http.StatusForbidden)
		return
	}
	b, err := json.Marshal(digestReply{Version: 1, Digests: digests})
	if err != nil || len(b) > digestMaxResponse {
		http.Error(w, "digest response too large", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
	g.logger().Info("gateway_dream_served", "host", gatewayHostLabel(host), "digests", len(digests))
}

// digestServeHook lets tests hold requests open to exercise the concurrency bound.
var digestServeHook func()

// pullDigests periodically fetches the peer's digests while the link lives.
// It runs only on links that negotiated the dream subprotocol, which requires
// this host's own policy to be enabled.
func (l *gatewayLink) pullDigests() {
	timer := time.NewTimer(digestFirstPull)
	defer timer.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-timer.C:
		}
		n, code := l.fetchDigests()
		next := digestInterval
		if code != "ok" {
			next = digestRetry
		}
		l.g.logger().Info("gateway_dream_pull", "host", gatewayHostLabel(l.host), "digests", n, "code", code)
		timer.Reset(next)
	}
}

// fetchDigests requests the peer's digests once and stores them privately,
// stamped with the authenticated host ID. It reports a count and a stable code.
func (l *gatewayLink) fetchDigests() (int, string) {
	ctx, cancel := context.WithTimeout(l.ctx, digestTimeout)
	defer cancel()
	l.g.mu.Lock()
	p := l.g.state.Peers[l.host]
	l.g.mu.Unlock()
	if p.Grant != l.grant || p.Pending {
		return 0, "revoked"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+p.Endpoint+"/v1/dream/digests", nil)
	if err != nil {
		return 0, "request_error"
	}
	tr := &http.Transport{TLSClientConfig: l.g.clientTLS(l.host), Proxy: nil}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}
	res, err := client.Do(req)
	if err != nil {
		return 0, "unavailable"
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusTooManyRequests || res.StatusCode == http.StatusServiceUnavailable:
		return 0, "busy"
	case res.StatusCode == http.StatusForbidden:
		return 0, "forbidden"
	case res.StatusCode != http.StatusOK:
		return 0, "rejected"
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, digestMaxResponse+1))
	if err != nil {
		return 0, "unavailable"
	}
	digests, err := parsePeerDigests(body, l.g.now().UTC())
	if err != nil {
		return 0, "invalid_response"
	}
	// Store only while this exact link is still the trusted one.
	if cur, err := l.g.fileLink(l.host); err != nil || cur != l {
		return 0, "revoked"
	}
	if err := l.g.storePeerDigests(l.host, digests); err != nil {
		return 0, "store_error"
	}
	return len(digests), "ok"
}

// storePeerDigests replaces one host's received digests. An empty list removes
// the file.
func (g *gateway) storePeerDigests(host string, digests []PeerDigest) error {
	dir := peerDigestDir(g.runtime)
	path := filepath.Join(dir, host+".json")
	if len(digests) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := localstate.SecureDir(dir); err != nil {
		return err
	}
	return gatewayWrite(path, peerDigestFile{Version: 1, HostID: host, ReceivedAt: g.now().UTC(), Digests: digests})
}

// clearPeerDigests deletes one host's received digests when its link ends,
// including on revocation and unpairing.
func (g *gateway) clearPeerDigests(host string) {
	if validHostID(host) {
		_ = os.Remove(filepath.Join(peerDigestDir(g.runtime), host+".json"))
	}
}

// resetPeerDigests removes every received digest; the service calls it at
// start and stop because no link exists then.
func (g *gateway) resetPeerDigests() {
	_ = os.RemoveAll(peerDigestDir(g.runtime))
}
