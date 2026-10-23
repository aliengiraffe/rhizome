package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"rhizome/internal/config"
)

// Peer history lets a directly paired host search and read this host's history
// catalog under the owner's [gateway.history] policy. Authorization happens on
// the serving host for every request and page; requesting hosts only relay.
const (
	peerHistoryDeadline  = 7 * time.Second // below the gateway server's 10 s write timeout
	peerHistoryTimeout   = 9 * time.Second
	maxPeerHistoryActive = 2 // concurrent requests per paired host, within maxHistoryConcurrent overall
	maxPeerHistoryBody   = 16 * 1024
	maxPeerHistoryReply  = 2 * 1024 * 1024
)

// GatewayHistory is what the service's gateway needs from the shared
// configuration beyond [gateway]: the observer's log roots and peer-history
// policy, the recent window it advertises to paired hosts, and the device's
// display label. All of it is read once at service start.
type GatewayHistory struct {
	ClaudeRoot, CodexRoot, OpenCodeRoot string
	Policy                              config.HistoryPolicy
	RecentWindow                        time.Duration
	Hostname                            string
}

// historyAccess narrows one request from an authenticated paired host. now is
// fixed per request, so each page recomputes the rolling cutoffs.
type historyAccess struct {
	host, grant string
	policy      config.HistoryPolicy
	now         time.Time
}

func (a *historyAccess) owner() string {
	if a == nil {
		return ""
	}
	return a.host
}

// bind ties pagination to the requesting host, its trust grant and the policy.
func (a *historyAccess) bind() string {
	return queryHash("peer", a.host, a.grant, a.policy.Fingerprint())
}

// session reports whether a catalog identity is visible: its source agent is
// shared and, under a finite window, its recorded last activity is at or after
// the cutoff. An unknown timestamp cannot prove eligibility.
func (a *historyAccess) session(s *historySession) bool {
	p := a.policy.Agent(s.AgentType)
	if !p.Enabled {
		return false
	}
	cut := p.Cutoff(a.now)
	return cut == nil || s.LastActivityAt != nil && !s.LastActivityAt.Before(*cut)
}

// event returns the gap code that withholds a record, or "" when it is visible.
func (a *historyAccess) event(agent string, at *time.Time) string {
	cut := a.policy.Agent(agent).Cutoff(a.now)
	switch {
	case cut == nil:
		return ""
	case at == nil:
		return "undated_excluded"
	case at.Before(*cut):
		return "outside_lookback"
	}
	return ""
}

// describe is the `policy` block of a paired-host response.
func (a *historyAccess) describe() map[string]any {
	agents := map[string]any{}
	for _, agent := range config.HistoryAgents {
		p := a.policy.Agent(agent)
		agents[agent] = map[string]any{"enabled": p.Enabled, "lookback": p.Lookback(), "cutoff": rfc3339(p.Cutoff(a.now))}
	}
	return map[string]any{"evaluated_at": *rfc3339(&a.now), "agents": agents}
}

// splitHost removes an optional host_id from tool arguments. Without it the
// request stays local and its arguments are untouched.
func splitHost(raw json.RawMessage) (string, json.RawMessage, bool, *historyError) {
	if len(raw) > maxHistoryArguments {
		return "", nil, false, herr("INVALID_ARGUMENT", "arguments exceed 8 KiB", false)
	}
	var fields map[string]json.RawMessage
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &fields) != nil || fields["host_id"] == nil {
		return "", raw, false, nil
	}
	var host string
	if json.Unmarshal(fields["host_id"], &host) != nil || !validHostID(host) {
		return "", nil, false, herr("INVALID_ARGUMENT", "host_id must be the 64-character lowercase hex fingerprint of a paired host, as listed by `rhizome peers`", false)
	}
	delete(fields, "host_id")
	rest, _ := json.Marshal(fields)
	return host, rest, true, nil
}

// remote relays one tool call to a paired host through the local gateway
// service. The serving host applies its own policy; nothing is fanned out.
func (h *historyService) remote(ctx context.Context, tool, host string, args json.RawMessage) *mcp.CallToolResult {
	raw, err := GatewayAdmin(ctx, h.scope.Runtime, GatewayCommand{Action: "history-" + tool, Host: host, Args: args})
	if err != nil {
		r, _ := historyResult(nil, herr("GATEWAY_UNAVAILABLE", "paired-host history needs the local gateway service; configure gateway.mode and start the service", true))
		return r
	}
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(raw, &probe)
	return &mcp.CallToolResult{IsError: probe.Error != nil, Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}
}

// validHostID accepts only the exact lowercase form host fingerprints use.
func validHostID(s string) bool {
	return validFingerprint(s) && strings.ToLower(s) == s
}

type historyWire struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

// historyHTTP serves /v1/history to currently trusted, connected peers only.
// Disabled sharing is answered without touching the catalog.
func (g *gateway) historyHTTP(w http.ResponseWriter, r *http.Request) {
	host := gatewayCaller(r)
	l, err := g.fileLink(host)
	if err != nil {
		http.Error(w, "untrusted peer", http.StatusForbidden)
		return
	}
	var in historyWire
	if r.Method != "POST" || json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPeerHistoryBody)).Decode(&in) != nil || (in.Tool != "search" && in.Tool != "read") {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	reply := func(b []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}
	if !g.historyPolicy.Enabled {
		reply(historyPayload(nil, herr("REMOTE_HISTORY_DISABLED", "this host does not share session history with paired hosts", false)))
		return
	}
	busy := herr("RESOURCE_LIMIT", "the paired host is serving its maximum number of history queries; retry", true)
	g.historyMu.Lock()
	if g.historyActive[host] >= maxPeerHistoryActive {
		g.historyMu.Unlock()
		reply(historyPayload(nil, busy))
		return
	}
	g.historyActive[host]++
	g.historyMu.Unlock()
	defer func() {
		g.historyMu.Lock()
		if g.historyActive[host]--; g.historyActive[host] == 0 {
			delete(g.historyActive, host)
		}
		g.historyMu.Unlock()
	}()
	select {
	case g.history.sem <- struct{}{}:
		defer func() { <-g.history.sem }()
	default:
		reply(historyPayload(nil, busy))
		return
	}
	// Revocation or disconnection cancels in-flight scans.
	ctx, cancel := context.WithTimeout(r.Context(), peerHistoryDeadline)
	defer cancel()
	stop := context.AfterFunc(l.ctx, cancel)
	defer stop()
	access := &historyAccess{host: host, grant: l.grant, policy: g.historyPolicy, now: g.history.clock().UTC()}
	var value any
	var e *historyError
	if in.Tool == "search" {
		value, e = g.history.searchWith(ctx, nil, access, in.Args)
	} else {
		value, e = g.history.readWith(ctx, nil, access, in.Args)
	}
	if _, err := g.fileLink(host); err != nil || l.ctx.Err() != nil {
		http.Error(w, "untrusted peer", http.StatusForbidden)
		return
	}
	reply(historyPayload(value, e))
}

// historyCommand relays a local history-search/history-read request to one
// directly paired host and always answers with a history JSON payload.
func (g *gateway) historyCommand(ctx context.Context, cmd GatewayCommand) json.RawMessage {
	fail := func(code, message string, retryable bool) json.RawMessage {
		return historyPayload(nil, &historyError{Code: code, Message: message, Retryable: retryable, Extra: map[string]any{"origin": map[string]any{"host_id": cmd.Host}}})
	}
	if !validHostID(cmd.Host) {
		return historyPayload(nil, herr("INVALID_ARGUMENT", "host_id must be the 64-character lowercase hex fingerprint of a paired host", false))
	}
	if cmd.Host == g.host {
		return historyPayload(nil, herr("INVALID_ARGUMENT", "host_id names this host; omit it to search local history", false))
	}
	l, err := g.fileLink(cmd.Host)
	if err != nil {
		return fail("PEER_UNAVAILABLE", "host_id is not a currently connected, directly paired host", true)
	}
	status, body, err := l.historyRequest(ctx, historyWire{Tool: strings.TrimPrefix(cmd.Action, "history-"), Args: cmd.Args})
	switch {
	case err != nil:
		return fail("PEER_UNAVAILABLE", "the paired host did not answer; it may have disconnected or been revoked", true)
	case status == http.StatusNotFound:
		return fail("REMOTE_HISTORY_UNSUPPORTED", "the paired host's Rhizome version does not support history sharing", false)
	case status == http.StatusForbidden:
		return fail("PEER_UNAVAILABLE", "the paired host does not currently trust this host", false)
	case status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests:
		return fail("RESOURCE_LIMIT", "the paired host is busy; retry", true)
	case status != http.StatusOK:
		return fail("PEER_UNAVAILABLE", "the paired host rejected the history request", false)
	}
	out, err := qualifyPeerHistory(body, cmd.Host)
	if err != nil {
		return fail("PEER_UNAVAILABLE", "the paired host returned an invalid history response", false)
	}
	return out
}

// historyRequest posts one bounded request over pinned host TLS.
func (l *gatewayLink) historyRequest(ctx context.Context, in historyWire) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, peerHistoryTimeout)
	defer cancel()
	stop := context.AfterFunc(l.ctx, cancel)
	defer stop()
	l.g.mu.Lock()
	p := l.g.state.Peers[l.host]
	l.g.mu.Unlock()
	if p.Grant != l.grant || p.Pending {
		return 0, nil, errors.New("peer revoked")
	}
	b, err := json.Marshal(in)
	if err != nil || len(b) > maxPeerHistoryBody {
		return 0, nil, errors.New("history request too large")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "https://"+p.Endpoint+"/v1/history", bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	tr := &http.Transport{TLSClientConfig: l.g.clientTLS(l.host), Proxy: nil}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxPeerHistoryReply+1))
	if err != nil || len(body) > maxPeerHistoryReply {
		return 0, nil, errors.New("invalid history response")
	}
	return res.StatusCode, body, nil
}

// qualifyPeerHistory stamps the authenticated origin onto a paired host's
// response. Peer-supplied host IDs and live claims are overwritten: historical
// rows from another host never assert live reachability here.
func qualifyPeerHistory(body []byte, host string) (json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, err
	}
	hostID, _ := json.Marshal(host)
	origin, _ := json.Marshal(map[string]any{"host_id": host, "scope": "paired_host"})
	if raw, ok := top["error"]; ok {
		var e map[string]json.RawMessage
		var code string
		if json.Unmarshal(raw, &e) != nil || json.Unmarshal(e["code"], &code) != nil || code == "" {
			return nil, errors.New("invalid error payload")
		}
		e["origin"] = origin
		top = map[string]json.RawMessage{"error": mustJSON(e)}
		return mustJSON(top), nil
	}
	qualify := func(raw json.RawMessage) (json.RawMessage, error) {
		var row map[string]json.RawMessage
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, err
		}
		row["host_id"] = hostID
		row["live_status"] = json.RawMessage(`"` + liveNotReported + `"`)
		row["delivery_capable"] = json.RawMessage("false")
		return mustJSON(row), nil
	}
	if raw, ok := top["sessions"]; ok {
		var rows []json.RawMessage
		if err := json.Unmarshal(raw, &rows); err != nil {
			return nil, err
		}
		for i := range rows {
			var err error
			if rows[i], err = qualify(rows[i]); err != nil {
				return nil, err
			}
		}
		top["sessions"] = mustJSON(rows)
	} else if raw, ok := top["session"]; ok {
		row, err := qualify(raw)
		if err != nil {
			return nil, err
		}
		top["session"] = row
	} else {
		return nil, errors.New("unexpected history response")
	}
	top["origin"] = origin
	return mustJSON(top), nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
