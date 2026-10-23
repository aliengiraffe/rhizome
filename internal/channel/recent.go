package channel

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"rhizome/internal/localstate"
)

// Recent-only advertisements let peers see sessions that recorded local
// activity within [sessions].recent_window even though no live process owns
// them. They are metadata derived from the observer's catalog on the owning
// host: no node, generation, endpoint, lease or queue exists for them, so they
// are never a delivery destination. Recency is `now - last_activity_at <=
// window`, inclusive, computed in UTC by the owner from recorded activity
// timestamps only; heartbeats, probes, peer queries, restarts and catalog
// writes never renew it.
const (
	maxRecentSessions   = 64              // per host, newest activity first
	recentSkewTolerance = 5 * time.Minute // timestamps further in the future are treated as unknown
	gatewayRecentName   = "recent.json"   // <runtime>/gateway/: entries received from paired hosts
)

// recentSession is the metadata-only advertisement of a session that is not
// verified live. It carries no node ID, generation or endpoint.
type recentSession struct {
	SessionID      string    `json:"session_id"`
	AgentType      string    `json:"agent_type"`
	DisplayName    string    `json:"display_name,omitempty"`
	Project        string    `json:"project,omitempty"`
	SessionName    string    `json:"session_name,omitempty"`
	Host           string    `json:"host,omitempty"`
	HostID         string    `json:"host_id,omitempty"` // authenticated paired host; empty on the owning device
	LastActivityAt time.Time `json:"last_activity_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// recentStatus is the `recent_sessions` block of list_sessions. It makes the
// effective window, the service dependency and partial coverage visible.
type recentStatus struct {
	State          string `json:"state"` // enabled, disabled (window 0) or unavailable
	Reason         string `json:"reason,omitempty"`
	Window         string `json:"window"`
	EvaluatedAt    string `json:"evaluated_at"`
	CatalogState   string `json:"catalog_state,omitempty"`
	Advertised     int    `json:"advertised"`
	Omitted        int    `json:"omitted"`
	PeerAdvertised int    `json:"peer_advertised"`
	Limit          int    `json:"limit"`
}

// recentExportable is the recent-row counterpart of gatewayExportable: session
// privacy (#37) must extend both. Only sessions with exactly one readable
// source qualify; tombstones and duplicate sources are never advertised.
func recentExportable(s *historySession) bool {
	return s != nil && s.SessionID != "" && len(s.SessionID) <= 128 && (s.AgentType == "claude" || s.AgentType == "codex") && (s.Availability == "available" || s.Availability == "partial")
}

// recentUntil applies the recency rule to one recorded timestamp and returns
// the expiry. Unknown timestamps and timestamps beyond the skew tolerance fail
// closed, so a future-dated record cannot stay advertised indefinitely.
func recentUntil(last *time.Time, window time.Duration, now time.Time) (time.Time, bool) {
	if last == nil || window <= 0 || last.After(now.Add(recentSkewTolerance)) {
		return time.Time{}, false
	}
	expires := last.UTC().Add(window)
	return expires, !now.After(expires)
}

// recentFromCatalog selects the catalog sessions that are recent at now. The
// result is sorted by newest activity, then agent and session ID, and capped at
// maxRecentSessions; the second value counts rows dropped by the cap.
func recentFromCatalog(c *catalogFile, window time.Duration, now time.Time, host string, name func(*historySession) string) ([]recentSession, int) {
	if c == nil || window <= 0 {
		return nil, 0
	}
	now = now.UTC()
	var out []recentSession
	for _, s := range c.sessions() {
		if !recentExportable(s) {
			continue
		}
		expires, ok := recentUntil(s.LastActivityAt, window, now)
		if !ok {
			continue
		}
		out = append(out, recentSession{SessionID: s.SessionID, AgentType: s.AgentType, DisplayName: s.DisplayName, Project: clip(s.ProjectPath, 180), SessionName: name(s), Host: host, LastActivityAt: s.LastActivityAt.UTC(), ExpiresAt: expires})
	}
	sortRecent(out)
	omitted := 0
	if len(out) > maxRecentSessions {
		omitted = len(out) - maxRecentSessions
		out = out[:maxRecentSessions]
	}
	return out, omitted
}
func sortRecent(rows []recentSession) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.HostID != b.HostID {
			return a.HostID < b.HostID
		}
		if !a.LastActivityAt.Equal(b.LastActivityAt) {
			return a.LastActivityAt.After(b.LastActivityAt)
		}
		if a.AgentType != b.AgentType {
			return a.AgentType < b.AgentType
		}
		return a.SessionID < b.SessionID
	})
}

// validRecentSession bounds an entry received from a paired host with the
// same limits as exported identities. Peer-supplied host IDs are ignored.
func validRecentSession(r recentSession) bool {
	for _, s := range []string{r.SessionID, r.AgentType, r.DisplayName, r.Project, r.SessionName, r.Host} {
		if !utf8.ValidString(s) {
			return false
		}
	}
	return r.SessionID != "" && len(r.SessionID) <= 128 && (r.AgentType == "claude" || r.AgentType == "codex") && len(r.DisplayName) <= 40 && len(r.Project) <= 180 && len(r.SessionName) <= 96 && len(r.Host) <= 64 && !r.LastActivityAt.IsZero() && !r.ExpiresAt.IsZero() && !r.ExpiresAt.Before(r.LastActivityAt)
}

// recentSessions returns this device's recent-only rows, the recorded last
// activity of every cataloged local session (to annotate live rows) and the
// status block. Without a running observer nothing is advertised: a stopped
// service never fabricates presence.
func (n *Node) recentSessions(now time.Time) ([]recentSession, map[sessionKey]time.Time, recentStatus) {
	status := recentStatus{Window: n.recentWindow.String(), EvaluatedAt: *rfc3339(&now), Limit: maxRecentSessions}
	if n.recentWindow <= 0 {
		status.State = "disabled"
		return nil, nil, status
	}
	status.State = "unavailable"
	if n.archive == nil {
		status.Reason = "no_catalog"
		return nil, nil, status
	}
	c, running, herr := n.archive.catalog.load()
	if herr != nil {
		status.Reason = "catalog_unavailable"
		return nil, nil, status
	}
	status.CatalogState = catalogState(c, running).State
	if !running {
		status.Reason = "service_stopped"
		return nil, nil, status
	}
	activity := map[sessionKey]time.Time{}
	for key, s := range c.sessions() {
		if s.LastActivityAt != nil {
			activity[key] = s.LastActivityAt.UTC()
		}
	}
	overrides := n.archive.names()
	rows, omitted := recentFromCatalog(c, n.recentWindow, now, n.local().Host, func(s *historySession) string {
		name, _ := n.archive.effectiveName(s, overrides)
		return name
	})
	status.State = "enabled"
	status.Advertised, status.Omitted = len(rows), omitted
	return rows, activity, status
}

// recentOnly reports whether session is currently advertised as recent (here
// or by a paired host) so send can explain the refusal precisely.
func (n *Node) recentOnly(session string) bool {
	now := time.Now()
	if n.archive != nil {
		now = n.archive.clock()
	}
	rows, _, _ := n.recentSessions(now)
	for _, r := range append(rows, gatewayRecentPeers(n.runtime, now)...) {
		if r.SessionID == session {
			return true
		}
	}
	return false
}

// gatewayRecentFile is the private registry the gateway service writes for
// local MCP processes. Entries are kept only while the authenticated link that
// advertised them is connected; the file is removed when the service stops.
type gatewayRecentFile struct {
	Version int                        `json:"version"`
	Hosts   map[string][]recentSession `json:"hosts"`
}
type recentPeer struct {
	link    *gatewayLink
	entries []recentSession
}
type gatewayRecent struct {
	path    string
	mu      sync.Mutex
	hosts   map[string]recentPeer
	written []byte
}

func newGatewayRecent(dir string) *gatewayRecent {
	return &gatewayRecent{path: filepath.Join(dir, gatewayRecentName), hosts: map[string]recentPeer{}}
}

// set replaces one host's entries as received over l. Invalid and duplicate
// entries are dropped and the authenticated host is stamped on every row.
func (r *gatewayRecent) set(l *gatewayLink, entries []recentSession) {
	if r == nil {
		return
	}
	kept := make([]recentSession, 0, len(entries))
	seen := map[sessionKey]bool{}
	for _, e := range entries {
		if len(kept) == maxRecentSessions {
			break
		}
		key := sessionKey{e.AgentType, e.SessionID}
		if !validRecentSession(e) || seen[key] {
			continue
		}
		seen[key] = true
		e.HostID = l.host
		e.LastActivityAt, e.ExpiresAt = e.LastActivityAt.UTC(), e.ExpiresAt.UTC()
		kept = append(kept, e)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hosts[l.host] = recentPeer{link: l, entries: kept}
	r.flushLocked()
}

// clear forgets a host's entries when the link that owned them ends.
func (r *gatewayRecent) clear(l *gatewayLink) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.hosts[l.host]; ok && p.link == l {
		delete(r.hosts, l.host)
		r.flushLocked()
	}
}
func (r *gatewayRecent) flushLocked() {
	out := gatewayRecentFile{Version: 1, Hosts: map[string][]recentSession{}}
	for host, p := range r.hosts {
		if len(p.entries) > 0 {
			out.Hosts[host] = p.entries
		}
	}
	b, err := json.Marshal(out)
	if err != nil || string(b) == string(r.written) {
		return
	}
	if err := gatewayWrite(r.path, out); err == nil {
		r.written = b
	}
}
func (r *gatewayRecent) close() { _ = os.Remove(r.path) }

// gatewayRunning probes the gateway service lock without holding it.
func gatewayRunning(runtime string) bool {
	f, err := localstate.Lock(filepath.Join(runtime, "gateway", "service.lock"), false)
	if f != nil {
		f.Close()
	}
	return errors.Is(err, localstate.ErrBusy)
}

// gatewayRecentPeers reads paired hosts' recent entries for list_sessions. A
// stopped gateway, an unknown file version or an expired entry yields nothing:
// peers can never extend the owner's window from here.
func gatewayRecentPeers(runtime string, now time.Time) []recentSession {
	if runtime == "" || !gatewayRunning(runtime) {
		return nil
	}
	var f gatewayRecentFile
	if gatewayRead(filepath.Join(runtime, "gateway", gatewayRecentName), &f) != nil || f.Version != 1 {
		return nil
	}
	var out []recentSession
	for host, entries := range f.Hosts {
		if !validHostID(host) {
			continue
		}
		for _, e := range entries[:min(len(entries), maxRecentSessions)] {
			if !validRecentSession(e) || now.After(e.ExpiresAt) {
				continue
			}
			e.HostID = host
			out = append(out, e)
		}
	}
	sortRecent(out)
	return out
}
