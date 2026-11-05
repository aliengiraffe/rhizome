package channel

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rhizome/internal/update"
	pb "rhizome/proto"
)

func writeHealth(t *testing.T, runtime, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(runtime, "observer-health.json"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestObserverUpgradeMatrix(t *testing.T) {
	self, ok := update.ParseRunning("0.10.1")
	if !ok {
		t.Fatal("self version rejected")
	}
	cases := []struct {
		name, body, version, outcome string
	}{
		{"newer patch", `{"version":"0.10.2"}`, "0.10.2", noticeNewer},
		{"newer with v prefix", `{"version":"v0.11.0"}`, "0.11.0", noticeNewer},
		{"newer prerelease", `{"version":"0.10.2-rc.1"}`, "0.10.2-rc.1", noticeNewer},
		{"equal", `{"version":"0.10.1"}`, "0.10.1", noticeNotNewer},
		{"equal ignoring build metadata", `{"version":"0.10.1+abc"}`, "0.10.1+abc", noticeNotNewer},
		{"older", `{"version":"0.10.0"}`, "0.10.0", noticeNotNewer},
		{"prerelease of own version", `{"version":"0.10.1-rc.1"}`, "0.10.1-rc.1", noticeNotNewer},
		{"dev observer", `{"version":"dev"}`, "", noticeObserverLocal},
		{"empty observer version", `{"version":""}`, "", noticeObserverLocal},
		{"no version field", `{"observed_sessions":1}`, "", noticeObserverLocal},
		{"snapshot observer", `{"version":"0.0.0-snapshot+abcdef123456"}`, "", noticeObserverLocal},
		{"free text never echoed", `{"version":"secret session text"}`, "", noticeObserverLocal},
		{"malformed", `{"version":`, "", noticeHealthMalformed},
		{"oversized", `{"version":"0.10.2","pad":"` + strings.Repeat("x", maxHealthBytes) + `"}`, "", noticeHealthMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runtime := t.TempDir()
			writeHealth(t, runtime, tc.body)
			version, outcome := observerUpgrade(runtime, self)
			if version != tc.version || outcome != tc.outcome {
				t.Fatalf("got %q/%s, want %q/%s", version, outcome, tc.version, tc.outcome)
			}
		})
	}
	if v, outcome := observerUpgrade(t.TempDir(), self); v != "" || outcome != noticeHealthMissing {
		t.Fatalf("missing file: %q/%s", v, outcome)
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "observer-health.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if v, outcome := observerUpgrade(dir, self); v != "" || outcome == noticeNewer {
		t.Fatalf("unreadable file: %q/%s", v, outcome)
	}
}

func TestUpdateNoticeEnablement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		o       Options
		enabled bool
	}{
		{"release build", Options{Version: "0.10.1", UpdateNotice: true}, true},
		{"policy off", Options{Version: "0.10.1"}, false},
		{"dev build", Options{Version: "dev", UpdateNotice: true}, false},
		{"unset version", Options{UpdateNotice: true}, false},
		{"snapshot build", Options{Version: "0.0.0-snapshot+abcdef123456", UpdateNotice: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if u := newUpdateNotice(tc.o, 0); u.enabled != tc.enabled || u.interval != defaultNoticeRefresh {
				t.Fatalf("enabled %t interval %s", u.enabled, u.interval)
			}
		})
	}
	// A disabled notice never reads the health file and never arms.
	runtime := t.TempDir()
	writeHealth(t, runtime, `{"version":"9.0.0"}`)
	u := newUpdateNotice(Options{Version: "0.10.1"}, time.Millisecond)
	u.poll(runtime, time.Now())
	if u.pending != "" {
		t.Fatal("disabled notice armed")
	}
}

func TestUpdateNoticeOneShotAndRearm(t *testing.T) {
	runtime := t.TempDir()
	u := newUpdateNotice(Options{Version: "0.10.1", UpdateNotice: true}, time.Second)
	now := time.Now()
	step := func(body string) {
		t.Helper()
		if body == "" {
			os.Remove(filepath.Join(runtime, "observer-health.json"))
		} else {
			writeHealth(t, runtime, body)
		}
		now = now.Add(time.Second)
		u.poll(runtime, now)
	}
	step("")
	if u.pending != "" {
		t.Fatal("missing health armed a notice")
	}
	step(`{"version":"0.10.2"}`)
	if u.pending != "0.10.2" {
		t.Fatalf("pending %q", u.pending)
	}
	// Polls before the interval elapses do not re-read the file.
	writeHealth(t, runtime, `{"version":"0.10.0"}`)
	u.poll(runtime, now.Add(time.Millisecond))
	if u.pending != "0.10.2" {
		t.Fatal("polled before the interval")
	}
	text, meta := u.message()
	if text != "Rhizome was updated to v0.10.2; this session still runs v0.10.1. Restart this coding session to load the new version." {
		t.Fatalf("text %q", text)
	}
	if len(meta) != 3 || meta["kind"] != updateNoticeKind || meta["from"] != "0.10.1" || meta["to"] != "0.10.2" {
		t.Fatalf("meta %v", meta)
	}
	u.finish(errDestinationUnavailable)
	if u.pending != "0.10.2" || u.announced != "" {
		t.Fatal("unavailable destination lost the notice")
	}
	u.finish(nil)
	if u.pending != "" || u.announced != "0.10.2" {
		t.Fatal("delivery not recorded")
	}
	for _, body := range []string{`{"version":"0.10.2"}`, "", `{"version":`, `{"version":"0.10.2"}`} {
		step(body)
		if u.pending != "" {
			t.Fatalf("repeated notice after %q", body)
		}
	}
	step(`{"version":"0.10.3"}`)
	if u.pending != "0.10.3" {
		t.Fatalf("no re-arm for a newer observer: %q", u.pending)
	}
	// An observer that moves back before delivery withdraws the notice.
	step(`{"version":"0.10.1"}`)
	if u.pending != "" {
		t.Fatal("notice kept after the observer moved back")
	}
	step(`{"version":"0.10.3"}`)
	u.finish(errors.New("write failed"))
	if u.pending != "" || u.announced != "0.10.3" {
		t.Fatal("failed delivery repeats")
	}
	step(`{"version":"0.10.3"}`)
	if u.pending != "" {
		t.Fatal("notice repeated after a failed delivery")
	}
}

type noticeCall struct {
	text string
	meta map[string]string
}

// noticeNode prepares a node as the given agent's MCP process would, with a
// fake transport standing in for the agent's notify path.
func noticeNode(t *testing.T, agent string, external bool, transport func(text string, meta map[string]string) error) (*Node, string) {
	t.Helper()
	n := node(t)
	runtime := t.TempDir()
	n.runtime = runtime
	n.opts = Options{Runtime: runtime, Version: "0.10.1", UpdateNotice: true}
	n.noticeRefresh = 20 * time.Millisecond
	n.identity.AgentType = agent
	n.ready = true
	n.external = external
	n.deliveryReady = true
	n.notify = func(_ context.Context, text string, meta map[string]string) error {
		return transport(text, meta)
	}
	return n, runtime
}

func TestUpdateNoticeDeliveryPerAgent(t *testing.T) {
	for _, tc := range []struct {
		agent    string
		external bool
		// wire renders the notice as the agent's real transport would.
		wire func(text string, meta map[string]string) string
	}{
		{"claude", false, func(text string, _ map[string]string) string { return text }},
		{"codex", true, peerEnvelope},
		{"opencode", true, peerEnvelope},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			var mu sync.Mutex
			var calls []noticeCall
			n, runtime := noticeNode(t, tc.agent, tc.external, func(text string, meta map[string]string) error {
				mu.Lock()
				defer mu.Unlock()
				calls = append(calls, noticeCall{text, meta})
				return nil
			})
			count := func() int { mu.Lock(); defer mu.Unlock(); return len(calls) }
			writeHealth(t, runtime, `{"version":"0.10.2"}`)
			n.mu.Lock()
			n.identity.SessionId = "session-a"
			n.identity.Generation = 1
			// A paused peer feed must not hold back Rhizome's own notice.
			n.paused = true
			n.mu.Unlock()
			n.wg.Add(1)
			go func() { defer n.wg.Done(); n.deliver(n.ctx) }()
			eventually(t, func() bool { return count() == 1 })
			// Rebinding (a new generation) and many polls never repeat it.
			n.mu.Lock()
			n.identity.SessionId = "session-b"
			n.identity.Generation = 2
			n.mu.Unlock()
			deadline := time.Now().Add(300 * time.Millisecond)
			for time.Now().Before(deadline) {
				if count() != 1 {
					t.Fatalf("duplicate notice: %d", count())
				}
				time.Sleep(10 * time.Millisecond)
			}
			writeHealth(t, runtime, `{"version":"0.11.0"}`)
			eventually(t, func() bool { return count() == 2 })
			mu.Lock()
			first, second := calls[0], calls[1]
			mu.Unlock()
			if !strings.Contains(first.text, "v0.10.2") || !strings.Contains(first.text, "v0.10.1") || first.meta["to"] != "0.10.2" {
				t.Fatalf("first notice %q %v", first.text, first.meta)
			}
			if !strings.Contains(second.text, "v0.11.0") || second.meta["to"] != "0.11.0" {
				t.Fatalf("second notice %q %v", second.text, second.meta)
			}
			for _, key := range []string{"session_id", "node_id", "host", "stream_id"} {
				if _, ok := first.meta[key]; ok {
					t.Fatalf("notice carries peer metadata %s", key)
				}
			}
			if tc.external && !isPeerInput(tc.wire(first.text, first.meta)) {
				t.Fatal("delivered notice would be re-collected as activity")
			}
			n.mu.Lock()
			defer n.mu.Unlock()
			if len(n.history) != 0 || n.sequence != 0 || len(n.notifications) != 0 || len(n.incoming) != 0 || n.stats.Written != 0 {
				t.Fatalf("notice touched activity or peer delivery state: history=%d notifications=%d written=%d", len(n.history), len(n.notifications), n.stats.Written)
			}
		})
	}
}

func TestUpdateNoticeRespectsDeliveryGates(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	n, runtime := noticeNode(t, "codex", true, func(string, map[string]string) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return nil
	})
	count := func() int { mu.Lock(); defer mu.Unlock(); return calls }
	writeHealth(t, runtime, `{"version":"0.10.2"}`)
	n.mu.Lock()
	n.deliveryReady = false
	n.mu.Unlock()
	n.wg.Add(1)
	go func() { defer n.wg.Done(); n.deliver(n.ctx) }()
	hold := func(why string) {
		t.Helper()
		deadline := time.Now().Add(200 * time.Millisecond)
		for time.Now().Before(deadline) {
			if count() != 0 {
				t.Fatalf("notice delivered %s", why)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	hold("to an unbound session")
	n.mu.Lock()
	n.identity.SessionId = threadA
	n.identity.ObservationOnly = true
	n.deliveryReady = true
	n.mu.Unlock()
	hold("to an observation_only identity")
	n.mu.Lock()
	n.identity.ObservationOnly = false
	n.deliveryReady = false
	n.mu.Unlock()
	hold("before the destination was ready")
	n.mu.Lock()
	n.deliveryReady = true
	n.mu.Unlock()
	eventually(t, func() bool { return count() == 1 })
}

func TestUpdateNoticeUnavailableDestinationRetries(t *testing.T) {
	var mu sync.Mutex
	attempts, delivered := 0, 0
	n, runtime := noticeNode(t, "opencode", true, func(string, map[string]string) error {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts == 1 {
			return errDestinationUnavailable
		}
		delivered++
		return nil
	})
	writeHealth(t, runtime, `{"version":"0.10.2"}`)
	n.mu.Lock()
	n.identity.SessionId = "ses_a"
	n.mu.Unlock()
	n.wg.Add(1)
	go func() { defer n.wg.Done(); n.deliver(n.ctx) }()
	eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return delivered == 1 })
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if delivered != 1 || attempts != 2 {
		t.Fatalf("attempts %d delivered %d", attempts, delivered)
	}
}

func TestUpdateNoticeDoesNotBlockPeerDelivery(t *testing.T) {
	var mu sync.Mutex
	var kinds []string
	n, runtime := noticeNode(t, "claude", false, func(_ string, meta map[string]string) error {
		mu.Lock()
		defer mu.Unlock()
		kinds = append(kinds, meta["kind"])
		return nil
	})
	writeHealth(t, runtime, `{"version":"0.10.2"}`)
	n.mu.Lock()
	n.identity.SessionId = "session-a"
	n.identity.Generation = 1
	n.enqueueLocked(&pb.Event{NodeId: "peer", SessionId: "remote", Kind: "direct", Text: "hello", MessageId: "m1"}, "", "", 1)
	n.mu.Unlock()
	n.wg.Add(1)
	go func() { defer n.wg.Done(); n.deliver(n.ctx) }()
	eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(kinds) == 2 })
	mu.Lock()
	defer mu.Unlock()
	if kinds[0] != updateNoticeKind || kinds[1] != "direct" {
		t.Fatalf("kinds %v", kinds)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stats.Written != 1 || len(n.notifications) != 1 {
		t.Fatalf("written %d notifications %d", n.stats.Written, len(n.notifications))
	}
}
