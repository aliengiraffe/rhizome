package channel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rhizome/internal/localstate"
)

func TestRecentUntil(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	window := 24 * time.Hour
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	cases := []struct {
		name    string
		last    *time.Time
		window  time.Duration
		want    bool
		expires time.Time
	}{
		{"just now", at(0), window, true, now.Add(window)},
		{"one hour ago", at(-time.Hour), window, true, now.Add(23 * time.Hour)},
		{"exactly at the cutoff is inclusive", at(-window), window, true, now},
		{"one nanosecond past the cutoff", at(-window - time.Nanosecond), window, false, time.Time{}},
		{"unknown timestamp", nil, window, false, time.Time{}},
		{"future within skew tolerance", at(time.Minute), window, true, now.Add(time.Minute + window)},
		{"future at the skew tolerance", at(recentSkewTolerance), window, true, now.Add(recentSkewTolerance + window)},
		{"future beyond skew tolerance fails closed", at(recentSkewTolerance + time.Nanosecond), window, false, time.Time{}},
		{"zero window disables", at(0), 0, false, time.Time{}},
		{"short window", at(-2 * time.Minute), time.Minute, false, time.Time{}},
		{"maximum window", at(-time.Hour), time.Duration(1<<63 - 1), true, now.Add(-time.Hour).Add(time.Duration(1<<63 - 1))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expires, ok := recentUntil(tc.last, tc.window, now)
			if ok != tc.want || (ok && !expires.Equal(tc.expires)) {
				t.Fatalf("got %v at %s, want %v at %s", ok, expires, tc.want, tc.expires)
			}
		})
	}
}

func TestRecentSelectionIsBoundedAndDeterministic(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	c := &catalogFile{}
	add := func(agent, session string, last time.Time) {
		c.Files = append(c.Files, &indexedFile{Path: "/" + agent + "/" + session, Agent: agent, Session: session, Cwd: "/work/" + session, LastAt: &last})
	}
	for i := 0; i < maxRecentSessions+2; i++ {
		add("claude", fmt.Sprintf("s%02d", i), now.Add(-time.Duration(i)*time.Minute))
	}
	// Ties break on agent, then session ID; a tombstone and a duplicate never qualify.
	add("codex", "tie", now.Add(-30*time.Second))
	add("claude", "tie", now.Add(-30*time.Second))
	missing := now
	c.Files = append(c.Files, &indexedFile{Path: "/claude/gone", Agent: "claude", Session: "gone", LastAt: &now, MissingAt: &missing})
	add("claude", "dup", now)
	add("claude", "dup", now)
	rows, omitted := recentFromCatalog(c, 24*time.Hour, now, "dev", func(s *historySession) string { return "name:" + s.SessionID })
	if len(rows) != maxRecentSessions || omitted != 4 {
		t.Fatalf("rows %d omitted %d", len(rows), omitted)
	}
	if rows[0].SessionID != "s00" || rows[1].SessionID != "tie" || rows[1].AgentType != "claude" || rows[2].SessionID != "tie" || rows[2].AgentType != "codex" || rows[3].SessionID != "s01" {
		t.Fatalf("order %s/%s %s/%s %s/%s %s", rows[0].SessionID, rows[0].AgentType, rows[1].SessionID, rows[1].AgentType, rows[2].SessionID, rows[2].AgentType, rows[3].SessionID)
	}
	if rows[0].Host != "dev" || rows[0].SessionName != "name:s00" || rows[0].DisplayName != "s00" || rows[0].Project != "/work/s00" || !rows[0].ExpiresAt.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("row %+v", rows[0])
	}
	for _, r := range rows {
		if r.SessionID == "gone" || r.SessionID == "dup" || r.HostID != "" {
			t.Fatalf("unexpected row %+v", r)
		}
	}
	if again, _ := recentFromCatalog(c, 24*time.Hour, now, "dev", func(s *historySession) string { return "" }); fmt.Sprint(again) != fmt.Sprint(rows) && len(again) != len(rows) {
		t.Fatal("selection is not deterministic")
	}
	if rows, omitted := recentFromCatalog(c, 0, now, "dev", nil); rows != nil || omitted != 0 {
		t.Fatal("zero window advertised rows")
	}
}

func TestValidRecentSession(t *testing.T) {
	now := time.Now()
	ok := recentSession{SessionID: "s", AgentType: "claude", DisplayName: "d", Project: "/p", SessionName: "n", Host: "h", LastActivityAt: now, ExpiresAt: now.Add(time.Hour)}
	if !validRecentSession(ok) {
		t.Fatal("valid entry rejected")
	}
	for name, mutate := range map[string]func(*recentSession){
		"empty session":     func(r *recentSession) { r.SessionID = "" },
		"long session":      func(r *recentSession) { r.SessionID = strings.Repeat("x", 129) },
		"unknown agent":     func(r *recentSession) { r.AgentType = "opencode" },
		"long display":      func(r *recentSession) { r.DisplayName = strings.Repeat("x", 41) },
		"long project":      func(r *recentSession) { r.Project = strings.Repeat("x", 181) },
		"long name":         func(r *recentSession) { r.SessionName = strings.Repeat("x", 97) },
		"long host":         func(r *recentSession) { r.Host = strings.Repeat("x", 65) },
		"invalid utf8":      func(r *recentSession) { r.SessionName = "\xff" },
		"zero activity":     func(r *recentSession) { r.LastActivityAt = time.Time{} },
		"zero expiry":       func(r *recentSession) { r.ExpiresAt = time.Time{} },
		"expiry before use": func(r *recentSession) { r.ExpiresAt = r.LastActivityAt.Add(-time.Second) },
	} {
		r := ok
		mutate(&r)
		if validRecentSession(r) {
			t.Errorf("%s accepted", name)
		}
	}
}

// recentRow finds one session row in a list_sessions payload after JSON round-trip.
func recentRow(t *testing.T, state map[string]any, session string) map[string]any {
	t.Helper()
	for _, s := range roundTrip(t, state)["sessions"].([]any) {
		row := s.(map[string]any)
		if row["identity"].(map[string]any)["session_id"] == session {
			return row
		}
	}
	return nil
}
func rowStatus(t *testing.T, state map[string]any, session string) string {
	t.Helper()
	row := recentRow(t, state, session)
	if row == nil {
		return "absent"
	}
	return fmt.Sprintf("%v/%v", row["status"], row["delivery_capable"])
}

func TestListSessionsRecentTransitions(t *testing.T) {
	hh := newHistoryHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	ts := func(d time.Duration) string { return base.Add(d).Format(time.RFC3339Nano) }
	proj := filepath.Join(hh.o.opts.ClaudeRoot, "-work-app")
	if err := os.MkdirAll(proj, 0700); err != nil {
		t.Fatal(err)
	}
	appendLog(t, filepath.Join(proj, "self.jsonl"), assistantText("self", "/work/app", "s1", ts(-time.Minute), "mine"))
	appendLog(t, filepath.Join(proj, "peer.jsonl"), assistantText("peer", "/work/app", "p1", ts(-time.Hour), "peer")+`{"type":"custom-title","customTitle":"Peer work","sessionId":"peer"}`+"\n")
	appendLog(t, filepath.Join(proj, "edge.jsonl"), assistantText("edge", "/work/app", "e1", ts(-24*time.Hour), "edge"))
	appendLog(t, filepath.Join(proj, "old.jsonl"), assistantText("old", "/work/app", "o1", ts(-24*time.Hour-time.Second), "old"))
	appendLog(t, filepath.Join(proj, "undated.jsonl"), claudeLine{typ: "assistant", uuid: "u1", session: "undated", cwd: "/work/app", content: []map[string]any{{"type": "text", "text": "undated"}}}.String())
	appendLog(t, filepath.Join(proj, "future.jsonl"), assistantText("future", "/work/app", "f1", ts(10*time.Minute), "future"))
	appendLog(t, filepath.Join(proj, "skew.jsonl"), assistantText("skew", "/work/app", "k1", ts(time.Minute), "skew"))
	codexDir := filepath.Join(hh.o.opts.CodexRoot, "2026")
	if err := os.MkdirAll(codexDir, 0700); err != nil {
		t.Fatal(err)
	}
	appendLog(t, filepath.Join(codexDir, "rollout.jsonl"), codexMeta("cx", "/work/app", ts(-2*time.Hour)))
	hh.index()
	hh.h.clock = func() time.Time { return base }
	a := hh.n
	if err := a.start(Options{Runtime: hh.scope.Runtime, Hostname: "Device A"}); err != nil {
		t.Fatal(err)
	}
	a.archive = hh.h
	a.recentWindow = 24 * time.Hour
	dir := t.TempDir()
	bind(t, a, "self", dir)
	publishers := func() int {
		n := 0
		for _, f := range hh.o.files {
			if f.n != nil {
				n++
			}
		}
		return n
	}

	// A stopped observer never fabricates presence.
	state := roundTrip(t, a.listSessions())
	if rs := state["recent_sessions"].(map[string]any); rs["state"] != "unavailable" || rs["reason"] != "service_stopped" || rowStatus(t, a.listSessions(), "peer") != "absent" {
		t.Fatalf("stopped service: %v", state)
	}
	lock, err := localstate.Lock(filepath.Join(hh.scope.Runtime, "observer.lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	state = roundTrip(t, a.listSessions())
	want := map[string]string{"self": "live/true", "peer": "recent/false", "edge": "recent/false", "skew": "recent/false", "cx": "recent/false", "old": "absent", "undated": "absent", "future": "absent"}
	for id, w := range want {
		if got := rowStatus(t, a.listSessions(), id); got != w {
			t.Fatalf("%s: %s, want %s\n%v", id, got, w, state)
		}
	}
	rs := state["recent_sessions"].(map[string]any)
	if rs["state"] != "enabled" || rs["advertised"] != 4.0 || rs["omitted"] != 0.0 || rs["window"] != "24h0m0s" || rs["catalog_state"] != "ready" || rs["limit"] != float64(maxRecentSessions) {
		t.Fatalf("status %v", rs)
	}
	// Recent rows carry metadata only, newest first, with the owner's expiry.
	order := []string{}
	for _, s := range state["sessions"].([]any) {
		if row := s.(map[string]any); row["status"] == "recent" {
			order = append(order, row["identity"].(map[string]any)["session_id"].(string))
		}
	}
	if strings.Join(order, ",") != "skew,peer,cx,edge" {
		t.Fatalf("order %v", order)
	}
	peer := recentRow(t, a.listSessions(), "peer")
	identity := peer["identity"].(map[string]any)
	if identity["node_id"] != nil || identity["endpoint"] != nil || identity["generation"] != nil || identity["session_name"] != "Peer work" || identity["host"] != "Device A" || identity["agent_type"] != "claude" || identity["display_name"] != "app" || identity["project"] != "/work/app" {
		t.Fatalf("recent identity %v", identity)
	}
	if peer["last_activity_at"] != ts(-time.Hour) || peer["recent_until"] != ts(23*time.Hour) || peer["host_id"] != nil {
		t.Fatalf("recent row %v", peer)
	}
	if edge := recentRow(t, a.listSessions(), "edge"); edge["recent_until"] != ts(0) {
		t.Fatalf("boundary expiry %v", edge)
	}
	if cx := recentRow(t, a.listSessions(), "cx"); cx["identity"].(map[string]any)["agent_type"] != "codex" {
		t.Fatalf("codex row %v", cx)
	}
	// The caller's own live row is not duplicated and shows recorded activity.
	self := recentRow(t, a.listSessions(), "self")
	if self["last_activity_at"] != ts(-time.Minute) || self["recent_until"] != nil {
		t.Fatalf("self row %v", self)
	}
	if len(state["sessions"].([]any)) != 5 || publishers() != 0 {
		t.Fatalf("rows %d publishers %d", len(state["sessions"].([]any)), publishers())
	}
	// Advancing the clock expires rows; nothing else renews them.
	hh.h.clock = func() time.Time { return base.Add(time.Nanosecond) }
	if got := rowStatus(t, a.listSessions(), "edge"); got != "absent" {
		t.Fatalf("expired boundary row still %s", got)
	}
	hh.h.clock = func() time.Time { return base }

	// Sending to a recent-only session fails before anything is queued.
	if _, err := a.send(context.Background(), "peer", "hello", ""); err == nil || !strings.Contains(err.Error(), "recent, not live") {
		t.Fatalf("recent send: %v", err)
	}
	if _, err := a.send(context.Background(), "old", "hello", ""); err == nil || strings.Contains(err.Error(), "recent") {
		t.Fatalf("expired send: %v", err)
	}

	// recent -> live: a verified process wins its key without a duplicate row.
	b := newNode(context.Background()) // closed explicitly below
	if err := b.start(Options{Runtime: hh.scope.Runtime}); err != nil {
		t.Fatal(err)
	}
	bind(t, b, "peer", dir)
	eventually(t, func() bool { return rowStatus(t, a.listSessions(), "peer") == "live/true" })
	if rows := roundTrip(t, a.listSessions())["sessions"].([]any); len(rows) != 5 {
		t.Fatalf("duplicate rows %v", rows)
	}
	if peer := recentRow(t, a.listSessions(), "peer"); peer["last_activity_at"] != ts(-time.Hour) || peer["recent_until"] != nil {
		t.Fatalf("live row %v", peer)
	}
	if _, err := a.send(context.Background(), "peer", "hello", ""); err != nil {
		t.Fatalf("live send: %v", err)
	}
	// live -> recent: the process ends, visibility downgrades, delivery stops.
	b.close()
	eventually(t, func() bool { return rowStatus(t, a.listSessions(), "peer") == "recent/false" })
	if _, err := a.send(context.Background(), "peer", "hello", ""); err == nil || !strings.Contains(err.Error(), "recent, not live") {
		t.Fatalf("send after exit: %v", err)
	}
	// recent -> live again on reconnect.
	c := node(t)
	if err := c.start(Options{Runtime: hh.scope.Runtime}); err != nil {
		t.Fatal(err)
	}
	bind(t, c, "peer", dir)
	eventually(t, func() bool { return rowStatus(t, a.listSessions(), "peer") == "live/true" })
	if rows := roundTrip(t, a.listSessions())["sessions"].([]any); len(rows) != 5 {
		t.Fatalf("duplicate rows after reconnect %v", rows)
	}
	// A live session older than the window stays discoverable; only the
	// tolerated future-dated row still qualifies for a one-minute window.
	a.recentWindow = time.Minute
	eventually(t, func() bool { return rowStatus(t, a.listSessions(), "peer") == "live/true" })
	if rs := roundTrip(t, a.listSessions())["recent_sessions"].(map[string]any); rs["advertised"] != 1.0 || rowStatus(t, a.listSessions(), "skew") != "recent/false" || rowStatus(t, a.listSessions(), "edge") != "absent" {
		t.Fatalf("short window %v", rs)
	}
	// Window 0 disables recent rows without touching live discovery.
	a.recentWindow = 0
	state = roundTrip(t, a.listSessions())
	if rs := state["recent_sessions"].(map[string]any); rs["state"] != "disabled" || len(state["sessions"].([]any)) != 2 || rowStatus(t, a.listSessions(), "peer") != "live/true" {
		t.Fatalf("disabled %v", state)
	}
	if publishers() != 0 {
		t.Fatal("listing created observer publishers")
	}
}

func TestGatewayRecentAcrossHosts(t *testing.T) {
	ts := func(d time.Duration) string { return time.Now().UTC().Add(-d).Format(time.RFC3339Nano) }
	runtimeA := filepath.Join(t.TempDir(), "run")
	history := seedPeerCatalog(t, runtimeA, map[string]string{
		"claude/-work/ended.jsonl": assistantText("ended-a", "/work/a", "e1", ts(time.Hour), "finished") + `{"type":"custom-title","customTitle":"Ended on A","sessionId":"ended-a"}` + "\n",
		"claude/-work/stale.jsonl": assistantText("stale-a", "/work/a", "s1", ts(30*time.Hour), "too old"),
	})
	history.RecentWindow = 24 * time.Hour
	history.Hostname = "Device A"
	lock, err := localstate.Lock(filepath.Join(runtimeA, "observer.lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { lock.Close() }()
	a := startGatewayHarnessWith(t, runtimeA, history)
	b := startGatewayHarness(t)
	c := startGatewayHarness(t)
	hostA, hostB, hostC := gatewayHost(t, a), gatewayHost(t, b), gatewayHost(t, c)
	a.command(t, GatewayCommand{Action: "join", Code: b.invite(t)})
	gatewayConnected(t, a, hostB)
	nb := gatewayTestNode(t, b, "gateway-b")
	na := gatewayTestNode(t, a, "gateway-a")
	now := time.Now()
	var row map[string]any
	gatewayUntil(t, func() bool { row = recentRow(t, nb.listSessions(), "ended-a"); return row != nil })
	identity := row["identity"].(map[string]any)
	if row["status"] != "recent" || row["delivery_capable"] != false || row["host_id"] != hostA || identity["gateway_host"] != hostA || identity["host"] != "Device A" || identity["session_name"] != "Ended on A" || identity["node_id"] != nil || row["recent_until"] == nil {
		t.Fatalf("remote recent row %v", row)
	}
	if rowStatus(t, nb.listSessions(), "stale-a") != "absent" {
		t.Fatal("expired session advertised across hosts")
	}
	if rs := roundTrip(t, nb.listSessions())["recent_sessions"].(map[string]any); rs["peer_advertised"] != 1.0 {
		t.Fatalf("peer count %v", rs)
	}
	// Recent entries never become proxies: no delivery, no file offers.
	if _, err := nb.send(context.Background(), "ended-a", "hello", ""); err == nil || !strings.Contains(err.Error(), "recent, not live") {
		t.Fatalf("remote recent send: %v", err)
	}
	if _, err := nb.resolve(context.Background(), "ended-a"); err == nil {
		t.Fatal("recent entry resolved as a route")
	}
	file := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := GatewayAdmin(context.Background(), b.runtime, GatewayCommand{Action: "files-send", Session: "gateway-b", Target: "ended-a", Path: file}); err == nil || !strings.Contains(err.Error(), "exact live paired session required") {
		t.Fatalf("file offer to recent: %v", err)
	}
	// Non-transitive: C pairs with B and sees B's live session but never A's entries.
	b.command(t, GatewayCommand{Action: "join", Code: c.invite(t)})
	gatewayConnected(t, b, hostC)
	nc := gatewayTestNode(t, c, "gateway-c")
	gatewayUntil(t, func() bool { return rowStatus(t, nc.listSessions(), "gateway-b") == "live/true" })
	if rowStatus(t, nc.listSessions(), "ended-a") != "absent" || len(gatewayRecentPeers(c.runtime, now)) != 0 {
		t.Fatal("recent entry forwarded through an intermediate host")
	}
	if rowStatus(t, na.listSessions(), "ended-a") != "absent" || len(gatewayRecentPeers(a.runtime, now)) != 0 {
		t.Fatal("owner received its own entries back")
	}
	// The owner's service must be running to advertise: a stopped observer withdraws rows.
	lock.Close()
	gatewayUntil(t, func() bool { return recentRow(t, nb.listSessions(), "ended-a") == nil })
	if lock, err = localstate.Lock(filepath.Join(runtimeA, "observer.lock"), false); err != nil {
		t.Fatal(err)
	}
	gatewayUntil(t, func() bool { return recentRow(t, nb.listSessions(), "ended-a") != nil })
	// A peer without the capability (or with the window disabled) sends plain snapshots.
	a.stop(t)
	a.history.RecentWindow = 0
	a.start(t)
	gatewayConnected(t, b, hostA)
	gatewayUntil(t, func() bool { return rowStatus(t, nb.listSessions(), "gateway-a") == "live/true" })
	if rowStatus(t, nb.listSessions(), "ended-a") != "absent" {
		t.Fatal("rows survived a peer that advertises none")
	}
	a.stop(t)
	a.history.RecentWindow = 24 * time.Hour
	a.start(t)
	gatewayUntil(t, func() bool { return recentRow(t, nb.listSessions(), "ended-a") != nil })
	// Revocation removes the host's entries with its connection.
	b.command(t, GatewayCommand{Action: "revoke", ID: hostA})
	gatewayUntil(t, func() bool { return recentRow(t, nb.listSessions(), "ended-a") == nil })
	// A stopped gateway serves nothing, even if a file were left behind.
	b.stop(t)
	if _, err := os.Stat(filepath.Join(b.runtime, "gateway", gatewayRecentName)); err == nil {
		t.Fatal("recent registry survived shutdown")
	}
	if err := gatewayWrite(filepath.Join(b.runtime, "gateway", gatewayRecentName), gatewayRecentFile{Version: 1, Hosts: map[string][]recentSession{hostA: {{SessionID: "ghost", AgentType: "claude", LastActivityAt: now, ExpiresAt: now.Add(time.Hour)}}}}); err != nil {
		t.Fatal(err)
	}
	if rows := gatewayRecentPeers(b.runtime, now); len(rows) != 0 || rowStatus(t, nb.listSessions(), "ghost") != "absent" {
		t.Fatal("stale registry served without a running gateway")
	}
}

// OpenCode was added after the original recent-session implementation. Its
// bound nodes must use the same catalog and configured window as other callers.
func TestOpenCodeListsRecentSessions(t *testing.T) {
	hh := newHistoryHarness(t)
	now := time.Now().UTC().Truncate(time.Second)
	appendLog(t, filepath.Join(hh.scope.ClaudeRoot, "ended.jsonl"), assistantText("ended", "/work/app", "a1", now.Add(-time.Hour).Format(time.RFC3339Nano), "answer"))
	hh.index()
	hh.h.clock = func() time.Time { return now }
	lock, err := localstate.Lock(filepath.Join(hh.scope.Runtime, "observer.lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	m := newOpenCodeNodes(context.Background(), hh.scope.Runtime)
	t.Cleanup(m.close)
	m.history, m.recentWindow = hh.h, 2*time.Hour
	m.handle(bindFrame("ses_caller", "", shortDir(t), ""))
	if m.sessions["ses_caller"] == nil {
		t.Fatal("OpenCode caller did not bind")
	}
	n := m.sessions["ses_caller"].node
	if got := rowStatus(t, n.listSessions(), "ended"); got != "recent/false" {
		t.Fatalf("recent session: %s", got)
	}
	hh.h.clock = func() time.Time { return now.Add(2 * time.Hour) }
	if got := rowStatus(t, n.listSessions(), "ended"); got != "absent" {
		t.Fatalf("expired session: %s", got)
	}
}
