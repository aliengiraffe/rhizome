package channel

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"rhizome/internal/config"
	"rhizome/internal/localstate"
)

var peerNow = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

const (
	peerHostA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	peerHostB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	peerGrant = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func peerPolicy(claude, codex config.AgentHistory) config.HistoryPolicy {
	return config.HistoryPolicy{Enabled: true, Claude: claude, Codex: codex}
}

// peerHistoryFixture indexes sessions around a 15-day Claude cutoff at
// peerNow (2026-10-06T12:00:00Z), plus one recent Codex session.
func peerHistoryFixture(t *testing.T) *historyHarness {
	t.Helper()
	hh := newHistoryHarness(t)
	proj := filepath.Join(hh.o.opts.ClaudeRoot, "-work-app")
	if err := os.MkdirAll(proj, 0700); err != nil {
		t.Fatal(err)
	}
	appendLog(t, filepath.Join(proj, "c-mixed.jsonl"),
		assistantText("c-mixed", "/work/app", "m1", "2026-09-01T00:00:00Z", "old answer")+
			claudeLine{typ: "assistant", uuid: "m2", session: "c-mixed", cwd: "/work/app", content: []map[string]any{{"type": "text", "text": "undated answer"}}}.String()+
			assistantText("c-mixed", "/work/app", "m3", "2026-10-06T12:00:00Z", "boundary answer")+
			assistantText("c-mixed", "/work/app", "m4", "2026-10-07T00:00:00Z", "next day answer")+
			assistantText("c-mixed", "/work/app", "m5", "2026-10-20T00:00:00Z", "recent answer"))
	appendLog(t, filepath.Join(proj, "c-old.jsonl"), assistantText("c-old", "/work/app", "o1", "2026-09-01T00:00:00Z", "old only"))
	appendLog(t, filepath.Join(proj, "c-edge.jsonl"), assistantText("c-edge", "/work/app", "e1", "2026-10-06T12:00:00Z", "edge"))
	appendLog(t, filepath.Join(proj, "c-before.jsonl"), assistantText("c-before", "/work/app", "b1", "2026-10-06T11:59:59.999999999Z", "just before"))
	appendLog(t, filepath.Join(proj, "c-untimed.jsonl"), claudeLine{typ: "assistant", uuid: "u1", session: "c-untimed", cwd: "/work/app", content: []map[string]any{{"type": "text", "text": "untimed"}}}.String())
	codexDir := filepath.Join(hh.o.opts.CodexRoot, "2026", "10", "19")
	if err := os.MkdirAll(codexDir, 0700); err != nil {
		t.Fatal(err)
	}
	appendLog(t, filepath.Join(codexDir, "rollout-x.jsonl"), codexMeta("x-recent", "/work/app", "2026-10-19T08:00:00Z")+codexAssistant("x1", "2026-10-19T08:00:02Z", "codex answer"))
	hh.index()
	lock, err := localstate.Lock(filepath.Join(hh.scope.Runtime, "observer.lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	return hh
}

func (hh *historyHarness) peerSearch(a *historyAccess, args string) (map[string]any, *historyError) {
	hh.t.Helper()
	out, err := hh.h.searchWith(context.Background(), nil, a, json.RawMessage(args))
	return roundTrip(hh.t, out), err
}
func (hh *historyHarness) peerRead(a *historyAccess, args string) (map[string]any, *historyError) {
	hh.t.Helper()
	out, err := hh.h.readWith(context.Background(), nil, a, json.RawMessage(args))
	return roundTrip(hh.t, out), err
}
func rowIDs(m map[string]any) []string {
	out := []string{}
	for _, r := range sessionsOf(m) {
		out = append(out, r["session_id"].(string))
	}
	return out
}
func eventTexts(m map[string]any) []string {
	out := []string{}
	for _, e := range eventsOf(m) {
		if s, ok := e["text"].(string); ok {
			out = append(out, s)
		}
	}
	return out
}
func gapCounts(m map[string]any) map[string]float64 {
	out := map[string]float64{}
	for _, g := range m["coverage"].(map[string]any)["gaps"].([]any) {
		g := g.(map[string]any)
		out[g["code"].(string)] = g["count"].(float64)
	}
	return out
}

func TestPeerHistorySearchPolicy(t *testing.T) {
	hh := peerHistoryFixture(t)
	d15, d30, all := config.AgentHistory{Enabled: true, Days: 15}, config.AgentHistory{Enabled: true, Days: 30}, config.AgentHistory{Enabled: true}
	off := config.AgentHistory{Days: 30}
	cases := []struct {
		name   string
		policy config.HistoryPolicy
		args   string
		want   []string
	}{
		{"claude 15d codex 30d", peerPolicy(d15, d30), `{}`, []string{"c-mixed", "x-recent", "c-edge"}},
		{"inclusive boundary only", peerPolicy(d15, off), `{"session_id":"c-before"}`, []string{}},
		{"codex disabled", peerPolicy(d15, off), `{}`, []string{"c-mixed", "c-edge"}},
		{"disabled agent filter", peerPolicy(d15, off), `{"agent_type":"codex"}`, []string{}},
		{"date filter cannot widen", peerPolicy(d15, d30), `{"updated_after":"2000-01-01T00:00:00Z"}`, []string{"c-mixed", "x-recent", "c-edge"}},
		{"date filter narrows", peerPolicy(d15, d30), `{"updated_after":"2026-10-10T00:00:00Z"}`, []string{"c-mixed", "x-recent"}},
		{"direct id outside window", peerPolicy(d15, d30), `{"session_id":"c-old"}`, []string{}},
		{"all includes old and undated", peerPolicy(all, off), `{}`, []string{"c-mixed", "c-edge", "c-before", "c-old", "c-untimed"}},
		{"global master gate", config.HistoryPolicy{Claude: all, Codex: all}, `{}`, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &historyAccess{host: peerHostA, grant: peerGrant, policy: tc.policy, now: peerNow}
			out, e := hh.peerSearch(a, tc.args)
			if e != nil {
				t.Fatal(e)
			}
			if got := rowIDs(out); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("rows %v, want %v", got, tc.want)
			}
			for _, r := range sessionsOf(out) {
				if r["live_status"] != liveNotReported || r["delivery_capable"] != false {
					t.Fatalf("paired row claims liveness: %v", r)
				}
			}
			agents := out["policy"].(map[string]any)["agents"].(map[string]any)
			if agents["claude"].(map[string]any)["enabled"] != tc.policy.Agent("claude").Enabled {
				t.Fatalf("policy block %v", agents)
			}
		})
	}
	// Local callers are unaffected by any peer policy.
	out, e := hh.search(`{}`)
	if e != nil || len(sessionsOf(out)) != 6 || out["policy"] != nil {
		t.Fatalf("local search changed: %v %v", e, out)
	}
}

func TestPeerHistoryReadPolicy(t *testing.T) {
	hh := peerHistoryFixture(t)
	d15, all := config.AgentHistory{Enabled: true, Days: 15}, config.AgentHistory{Enabled: true}
	a := &historyAccess{host: peerHostA, grant: peerGrant, policy: peerPolicy(d15, config.AgentHistory{Days: 30}), now: peerNow}
	out, e := hh.peerRead(a, `{"agent_type":"claude","session_id":"c-mixed"}`)
	if e != nil {
		t.Fatal(e)
	}
	if got := eventTexts(out); !reflect.DeepEqual(got, []string{"boundary answer", "next day answer", "recent answer"}) {
		t.Fatalf("events %v", got)
	}
	if g := gapCounts(out); g["outside_lookback"] != 1 || g["undated_excluded"] != 1 || out["coverage"].(map[string]any)["state"] != "partial" {
		t.Fatalf("coverage %v", out["coverage"])
	}
	if out["session"].(map[string]any)["live_status"] != liveNotReported || out["policy"] == nil {
		t.Fatalf("session %v", out["session"])
	}
	// Hidden and absent sessions are indistinguishable.
	_, hidden := hh.peerRead(a, `{"agent_type":"claude","session_id":"c-old"}`)
	_, absent := hh.peerRead(a, `{"agent_type":"claude","session_id":"nope"}`)
	_, untimed := hh.peerRead(a, `{"agent_type":"claude","session_id":"c-untimed"}`)
	for _, e := range []*historyError{hidden, absent, untimed} {
		if e == nil || e.Code != "SESSION_NOT_FOUND" || e.Message != absent.Message {
			t.Fatalf("hidden session distinguishable: %v", e)
		}
	}
	// A disabled source agent is denied before any lookup.
	_, known := hh.peerRead(a, `{"agent_type":"codex","session_id":"x-recent"}`)
	_, unknown := hh.peerRead(a, `{"agent_type":"codex","session_id":"nope"}`)
	if known == nil || unknown == nil || known.Code != "POLICY_DENIED" || known.Message != unknown.Message || unknown.Code != known.Code {
		t.Fatalf("policy denial %v %v", known, unknown)
	}
	// "all" permits undated records and every age.
	a.policy = peerPolicy(all, all)
	out, e = hh.peerRead(a, `{"agent_type":"claude","session_id":"c-mixed"}`)
	if e != nil || len(eventTexts(out)) != 5 || len(gapCounts(out)) != 0 {
		t.Fatalf("all policy %v %v", e, out)
	}
	if _, e = hh.peerRead(a, `{"agent_type":"claude","session_id":"c-untimed"}`); e != nil {
		t.Fatal(e)
	}
}

func TestPeerHistoryPagesRecheckTheClock(t *testing.T) {
	hh := peerHistoryFixture(t)
	a := &historyAccess{host: peerHostA, grant: peerGrant, policy: peerPolicy(config.AgentHistory{Enabled: true, Days: 15}, config.AgentHistory{}), now: peerNow}
	args := `{"agent_type":"claude","session_id":"c-mixed","limit":1}`
	out, e := hh.peerRead(a, args)
	if e != nil || !reflect.DeepEqual(eventTexts(out), []string{"boundary answer"}) {
		t.Fatalf("page 1 %v %v", e, out)
	}
	// One day later the next-day event has aged out mid-pagination.
	a.now = peerNow.Add(24 * time.Hour)
	out, e = hh.peerRead(a, `{"agent_type":"claude","session_id":"c-mixed","limit":1,"cursor":"`+cursorOf(out)+`"}`)
	if e != nil || !reflect.DeepEqual(eventTexts(out), []string{"recent answer"}) || gapCounts(out)["outside_lookback"] != 2 {
		t.Fatalf("page 2 %v %v", e, out)
	}
	// Search rows that age out between pages are dropped and counted.
	a.now = peerNow
	out, e = hh.peerSearch(a, `{"agent_type":"claude","limit":1}`)
	if e != nil || !reflect.DeepEqual(rowIDs(out), []string{"c-mixed"}) {
		t.Fatalf("search page 1 %v %v", e, out)
	}
	a.now = peerNow.Add(time.Nanosecond)
	out, e = hh.peerSearch(a, `{"agent_type":"claude","limit":1,"cursor":"`+cursorOf(out)+`"}`)
	if e != nil || len(rowIDs(out)) != 0 || !strings.Contains(string(mustJSON(out["catalog"])), "snapshot_rows_removed") {
		t.Fatalf("aged-out search row %v %v", e, out)
	}
	// A session whose last activity ages out stops paging entirely.
	a.now = peerNow
	out, e = hh.peerRead(a, args)
	if e != nil || cursorOf(out) == "" {
		t.Fatalf("read page 1 %v %v", e, out)
	}
	a.now = time.Date(2026, 11, 4, 0, 0, 1, 0, time.UTC)
	if _, e = hh.peerRead(a, `{"agent_type":"claude","session_id":"c-mixed","limit":1,"cursor":"`+cursorOf(out)+`"}`); e == nil || e.Code != "SESSION_NOT_FOUND" {
		t.Fatalf("aged-out read continued: %v", e)
	}
}

func TestPeerHistoryCursorBinding(t *testing.T) {
	hh := peerHistoryFixture(t)
	d30 := config.AgentHistory{Enabled: true, Days: 30}
	base := historyAccess{host: peerHostA, grant: peerGrant, policy: peerPolicy(d30, d30), now: peerNow}
	issue := func() string {
		a := base
		out, e := hh.peerSearch(&a, `{"limit":1}`)
		if e != nil || cursorOf(out) == "" {
			t.Fatalf("issue %v %v", e, out)
		}
		return cursorOf(out)
	}
	other := base
	other.host = peerHostB
	regranted := base
	regranted.grant = peerHostB
	narrowed := base
	narrowed.policy = peerPolicy(config.AgentHistory{Enabled: true, Days: 15}, d30)
	for name, a := range map[string]*historyAccess{"other host": &other, "new grant": &regranted, "policy change": &narrowed, "local caller": nil} {
		cursor := issue()
		_, e := hh.h.searchWith(context.Background(), func() *liveView { return &liveView{} }, a, json.RawMessage(`{"limit":1,"cursor":"`+cursor+`"}`))
		if e == nil || e.Code != "INVALID_CURSOR" {
			t.Fatalf("%s reused cursor: %v", name, e)
		}
	}
	// A local cursor cannot be continued by a paired host.
	out, _ := hh.search(`{"limit":1}`)
	a := base
	if _, e := hh.peerSearch(&a, `{"limit":1,"cursor":"`+cursorOf(out)+`"}`); e == nil || e.Code != "INVALID_CURSOR" {
		t.Fatalf("local cursor accepted remotely: %v", e)
	}
}

func TestPeerSnapshotsAreBoundedPerHost(t *testing.T) {
	st := newSnapshotStore()
	keep := &snapshot{kind: "search", owner: peerHostB}
	st.put(keep)
	st.release(keep, false)
	local := &snapshot{kind: "search"}
	st.put(local)
	st.release(local, false)
	for i := 0; i < 3*maxPeerSnapshots; i++ {
		s := &snapshot{kind: "search", owner: peerHostA}
		st.put(s)
		st.release(s, false)
	}
	count := 0
	for _, s := range st.items {
		if s.owner == peerHostA {
			count++
		}
	}
	if count != maxPeerSnapshots || st.items[keep.id] == nil || st.items[local.id] == nil {
		t.Fatalf("peer A holds %d snapshots; others kept: %v %v", count, st.items[keep.id] != nil, st.items[local.id] != nil)
	}
}

func TestSplitHost(t *testing.T) {
	host, rest, remote, e := splitHost(json.RawMessage(`{"host_id":"` + peerHostA + `","limit":3}`))
	if e != nil || !remote || host != peerHostA || string(rest) != `{"limit":3}` {
		t.Fatalf("split %q %s %v %v", host, rest, remote, e)
	}
	for _, raw := range []string{``, `{}`, `{"limit":3}`} {
		if _, rest, remote, e := splitHost(json.RawMessage(raw)); e != nil || remote || string(rest) != raw {
			t.Fatalf("local %q changed: %s %v", raw, rest, e)
		}
	}
	for _, bad := range []string{`"laptop"`, `"` + strings.ToUpper(peerHostA) + `"`, `1`, `""`, `null`} {
		if _, _, _, e := splitHost(json.RawMessage(`{"host_id":` + bad + `}`)); e == nil || e.Code != "INVALID_ARGUMENT" {
			t.Fatalf("accepted host_id %s", bad)
		}
	}
}

func TestQualifyPeerHistory(t *testing.T) {
	body := `{"sessions":[{"session_id":"s","agent_type":"claude","host_id":"forged","live_status":"live","delivery_capable":true}],"next_cursor":null,"catalog":{}}`
	out, err := qualifyPeerHistory([]byte(body), peerHostB)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(out, &m)
	row := sessionsOf(m)[0]
	if row["host_id"] != peerHostB || row["live_status"] != liveNotReported || row["delivery_capable"] != false || m["origin"].(map[string]any)["host_id"] != peerHostB {
		t.Fatalf("not qualified: %s", out)
	}
	if _, err := qualifyPeerHistory([]byte(`{"unexpected":1}`), peerHostB); err == nil {
		t.Fatal("accepted unexpected shape")
	}
	if _, err := qualifyPeerHistory([]byte(`{"error":{"message":"no code"}}`), peerHostB); err == nil {
		t.Fatal("accepted error without code")
	}
}

// seedPeerCatalog indexes logs for runtime the way the service observer does.
func seedPeerCatalog(t *testing.T, runtime string, logs map[string]string) GatewayHistory {
	t.Helper()
	dir := t.TempDir()
	o := observerHarness(t)
	o.opts = ObserverOptions{Runtime: runtime, ClaudeRoot: filepath.Join(dir, "claude"), CodexRoot: filepath.Join(dir, "codex")}
	for rel, body := range logs {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		appendLog(t, path, body)
	}
	if err := os.MkdirAll(runtime, 0700); err != nil {
		t.Fatal(err)
	}
	scope := historyScope{Runtime: runtime, ClaudeRoot: o.opts.ClaudeRoot, CodexRoot: o.opts.CodexRoot}
	o.index = newHistoryIndex(scope, nil)
	(&historyHarness{t: t, o: o, scope: scope}).index()
	return GatewayHistory{ClaudeRoot: scope.ClaudeRoot, CodexRoot: scope.CodexRoot}
}

func gatewayHost(t *testing.T, h *gatewayHarness) string {
	t.Helper()
	in, err := parseGatewayInvitation(h.invite(t))
	if err != nil {
		t.Fatal(err)
	}
	return in.Host
}
func gatewayConnected(t *testing.T, h *gatewayHarness, host string) {
	t.Helper()
	gatewayUntil(t, func() bool {
		var peers []map[string]any
		raw, err := GatewayAdmin(context.Background(), h.runtime, GatewayCommand{Action: "peers"})
		if err != nil || json.Unmarshal(raw, &peers) != nil {
			return false
		}
		for _, p := range peers {
			if p["host_id"] == host && p["connected"] == true {
				return true
			}
		}
		return false
	})
}
func peerHistory(t *testing.T, h *gatewayHarness, tool, host, args string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(h.command(t, GatewayCommand{Action: "history-" + tool, Host: host, Args: json.RawMessage(args)}), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func peerHistoryError(t *testing.T, out map[string]any, code string) {
	t.Helper()
	e, ok := out["error"].(map[string]any)
	if !ok || e["code"] != code {
		t.Fatalf("want %s, got %v", code, out)
	}
}

func TestPeerHistoryAcrossGateways(t *testing.T) {
	ts := func(d time.Duration) string { return time.Now().UTC().Add(-d).Format(time.RFC3339Nano) }
	logs := map[string]string{
		"claude/-work/shared.jsonl": assistantText("shared-id", "/work/b", "s1", ts(40*24*time.Hour), "too old") + assistantText("shared-id", "/work/b", "s2", ts(2*time.Hour), "recent b one") + assistantText("shared-id", "/work/b", "s3", ts(time.Hour), "recent b two"),
		"claude/-work/old.jsonl":    assistantText("b-old", "/work/b", "o1", ts(20*24*time.Hour), "older than 15 days"),
		"codex/2026/rollout.jsonl":  codexMeta("b-codex", "/work/b", ts(time.Hour)) + codexAssistant("x1", ts(time.Hour), "codex text"),
	}
	runtimeB := filepath.Join(t.TempDir(), "run")
	history := seedPeerCatalog(t, runtimeB, logs)
	history.Policy = config.HistoryPolicy{Enabled: true, Claude: config.AgentHistory{Enabled: true, Days: 15}, Codex: config.AgentHistory{Days: 30}}
	a := startGatewayHarness(t)
	b := startGatewayHarnessWith(t, runtimeB, history)
	c := startGatewayHarness(t) // pairing alone grants nothing: default policy
	hostA, hostB, hostC := gatewayHost(t, a), gatewayHost(t, b), gatewayHost(t, c)
	a.command(t, GatewayCommand{Action: "join", Code: b.invite(t)})
	b.command(t, GatewayCommand{Action: "join", Code: c.invite(t)})
	gatewayConnected(t, a, hostB)
	gatewayConnected(t, c, hostB)

	// Same session ID locally on A stays a distinct, unqualified identity.
	localA := newHistoryService(historyScope{Runtime: a.runtime})
	out := peerHistory(t, a, "search", hostB, `{}`)
	rows := sessionsOf(out)
	if len(rows) != 1 || rows[0]["session_id"] != "shared-id" || rows[0]["host_id"] != hostB || rows[0]["live_status"] != liveNotReported {
		t.Fatalf("remote rows %v", out)
	}
	if o := out["origin"].(map[string]any); o["host_id"] != hostB || out["policy"] == nil {
		t.Fatalf("origin %v", out)
	}
	out = peerHistory(t, a, "read", hostB, `{"agent_type":"claude","session_id":"shared-id","limit":1}`)
	if got := eventTexts(out); !reflect.DeepEqual(got, []string{"recent b one"}) || out["session"].(map[string]any)["host_id"] != hostB {
		t.Fatalf("read page 1 %v", out)
	}
	cursor := cursorOf(out)
	peerHistoryError(t, peerHistory(t, a, "read", hostB, `{"agent_type":"codex","session_id":"b-codex"}`), "POLICY_DENIED")
	peerHistoryError(t, peerHistory(t, a, "read", hostB, `{"agent_type":"claude","session_id":"b-old"}`), "SESSION_NOT_FOUND")
	// Another paired host cannot continue A's cursor.
	peerHistoryError(t, peerHistory(t, c, "read", hostB, `{"agent_type":"claude","session_id":"shared-id","limit":1,"cursor":"`+cursor+`"}`), "INVALID_CURSOR")
	out = peerHistory(t, a, "read", hostB, `{"agent_type":"claude","session_id":"shared-id","limit":1,"cursor":"`+cursor+`"}`)
	if got := eventTexts(out); !reflect.DeepEqual(got, []string{"recent b two"}) || gapCounts(out)["outside_lookback"] != 1 {
		t.Fatalf("read page 2 %v", out)
	}
	// Selection is exact and direct: self, unknown and second-hop hosts fail closed.
	peerHistoryError(t, peerHistory(t, a, "search", hostA, `{}`), "INVALID_ARGUMENT")
	peerHistoryError(t, peerHistory(t, a, "search", strings.Repeat("d", 64), `{}`), "PEER_UNAVAILABLE")
	peerHistoryError(t, peerHistory(t, a, "search", hostC, `{}`), "PEER_UNAVAILABLE")
	peerHistoryError(t, peerHistory(t, b, "search", hostC, `{}`), "REMOTE_HISTORY_DISABLED")
	// The MCP path relays through the local gateway service.
	res := localA.remote(context.Background(), "search", hostB, json.RawMessage(`{"agent_type":"claude"}`))
	if res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, hostB) {
		t.Fatalf("mcp relay %v", res.Content[0])
	}
	if res := (&historyService{scope: historyScope{Runtime: t.TempDir()}}).remote(context.Background(), "search", hostB, nil); !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "GATEWAY_UNAVAILABLE") {
		t.Fatal("missing local gateway not reported")
	}
	// Older peers report an explicit unsupported capability.
	c.stop(t)
	gatewayServesHistory = false
	c.start(t)
	gatewayServesHistory = true
	gatewayConnected(t, b, hostC)
	peerHistoryError(t, peerHistory(t, b, "search", hostC, `{}`), "REMOTE_HISTORY_UNSUPPORTED")
	// A service restart invalidates outstanding cursors.
	out = peerHistory(t, a, "read", hostB, `{"agent_type":"claude","session_id":"shared-id","limit":1}`)
	cursor = cursorOf(out)
	b.stop(t)
	b.start(t)
	gatewayConnected(t, a, hostB)
	peerHistoryError(t, peerHistory(t, a, "read", hostB, `{"agent_type":"claude","session_id":"shared-id","limit":1,"cursor":"`+cursor+`"}`), "CURSOR_EXPIRED")
	// Revocation stops all further pages.
	cursor = cursorOf(peerHistory(t, a, "read", hostB, `{"agent_type":"claude","session_id":"shared-id","limit":1}`))
	b.command(t, GatewayCommand{Action: "revoke", ID: hostA})
	peerHistoryError(t, peerHistory(t, a, "read", hostB, `{"agent_type":"claude","session_id":"shared-id","limit":1,"cursor":"`+cursor+`"}`), "PEER_UNAVAILABLE")
	// Remote access never wrote local state on the serving host.
	if _, err := os.Stat(filepath.Join(runtimeB, "names")); err == nil {
		t.Fatal("history access created name overrides")
	}
}
