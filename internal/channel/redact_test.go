package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rhizome/internal/config"
)

// Synthetic, never-issued credential shapes. Betterleaks allowlists documented
// example values, so these use random-looking bodies; they are assembled from
// parts so that no credential-shaped literal sits in the source tree.
var (
	testGitHubPAT = "ghp_" + "q8Fz3kR1mN7vT2wX9bL4pH6sC0dJ5aG8eY1u"
	testAWSKey    = "AKIA" + "Q3EGTQ7X2K5N4MZJ"
	testSlackBot  = "xox" + "b-2912345678901-2912345678901-Qz8Kf3Lm9Xp2Rt7Vw4Yb6N"
	testOpenAIKey = "sk-" + "proj-Vq8Zt3Rm2Xn7Kw4Pb9Ly1Hc6Jd0Fs5Ga2Qe8Tz3Ub7Nv4Mx1Rk9Wp6Yh2Lc5Dj0Bf8Sa3Ge7Qt1Xz4"
	testPEM       = "-----BEGIN RSA " + "PRIVATE KEY-----\nMIIEowIBAAKCAQEAq3Zt8Fm2Xn7Kw4Pb9Ly1Hc6Jd0Fs5Ga2Qe8Tz3Ub7Nv4Mx1Rk9Wp6Yh2L\nc5Dj0Bf8Sa3Ge7Qt1Xz4Vq8Zt3Rm2Xn7Kw4Pb9Ly1Hc6Jd0Fs5Ga2Qe8Tz3Ub7Nv4Mx1Rk9W\n-----END RSA " + "PRIVATE KEY-----"
)

var testSecrets = []string{testGitHubPAT, testAWSKey, testSlackBot, testOpenAIKey, testPEM}

func assertNoSecret(t *testing.T, s string) {
	t.Helper()
	for _, secret := range testSecrets {
		if strings.Contains(s, secret) {
			t.Fatalf("raw secret leaked (rule family %s)", secret[:4])
		}
	}
}

// fakeScanner injects findings, errors and stalls.
type fakeScanner struct {
	findings []secretFinding
	err      error
	stall    time.Duration
}

func (f fakeScanner) scan(ctx context.Context, _ string) ([]secretFinding, error) {
	if f.stall > 0 {
		select {
		case <-time.After(f.stall):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.findings, f.err
}

func TestRedactorTable(t *testing.T) {
	r := sharedRedactor()
	if r.err != nil {
		t.Fatalf("scanner construction: %v", r.err)
	}
	for _, tc := range []struct {
		name, in, want string
		changed        bool
	}{
		{"empty", "", "", false},
		{"no findings", "Edited internal/channel/node.go and ran the tests; password handling unchanged.", "Edited internal/channel/node.go and ran the tests; password handling unchanged.", false},
		{"github pat in prose", "pushed with " + testGitHubPAT + " earlier", "pushed with [REDACTED:github-pat] earlier", true},
		{"two rules in one text", "export AWS_ACCESS_KEY_ID=" + testAWSKey + " and token " + testGitHubPAT + " done", "export AWS_ACCESS_KEY_ID=[REDACTED:aws-access-token] and token [REDACTED:github-pat] done", true},
		{"slack and github", "slack " + testSlackBot + " and again " + testGitHubPAT, "slack [REDACTED:slack-bot-token] and again [REDACTED:github-pat]", true},
		{"assignment keeps context", "line1\nSet OPENAI_API_KEY = \"" + testOpenAIKey + "\"\nline3", "line1\nSet OPENAI_API_KEY = \"[REDACTED:generic-api-key]\"\nline3", true},
		{"repeated secret", testGitHubPAT + " then " + testGitHubPAT, "[REDACTED:github-pat] then [REDACTED:github-pat]", true},
		{"multi-line private key", "key:\n" + testPEM + "\nend", "key:\n[REDACTED:private-key]\nend", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, changed, failed := r.redact(tc.in)
			if failed {
				t.Fatal("scan reported failure")
			}
			if got != tc.want || changed != tc.changed {
				t.Fatalf("got %q changed=%v\nwant %q changed=%v", got, changed, tc.want, tc.changed)
			}
		})
	}
}

func TestRedactorFailsClosed(t *testing.T) {
	text := "token " + testGitHubPAT
	for _, tc := range []struct {
		name string
		r    *redactor
	}{
		{"scanner unavailable", newRedactor(nil, errors.New("boom"), time.Second)},
		{"scan error", newRedactor(fakeScanner{err: errors.New("boom")}, nil, time.Second)},
		{"timeout", newRedactor(fakeScanner{stall: time.Minute}, nil, 50*time.Millisecond)},
		{"finding outside text", newRedactor(fakeScanner{findings: []secretFinding{{"decoded", 2, 99}}}, nil, time.Second)},
		{"finding unplaced", newRedactor(fakeScanner{findings: []secretFinding{{"rule", -1, -1}}}, nil, time.Second)},
		{"empty span", newRedactor(fakeScanner{findings: []secretFinding{{"rule", 3, 3}}}, nil, time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			got, changed, failed := tc.r.redact(text)
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("redact stalled for %v", elapsed)
			}
			if got != redactedUnscanned || !changed || !failed {
				t.Fatalf("got %q changed=%v failed=%v", got, changed, failed)
			}
			if out, changed, failed := tc.r.redact(""); out != "" || changed || failed {
				t.Fatal("empty text must pass through without a scan")
			}
		})
	}
	// Overlapping and adjacent findings merge into the earliest marker; a
	// disjoint one keeps its own.
	r := newRedactor(fakeScanner{findings: []secretFinding{{"inner", 6, 9}, {"outer", 4, 11}, {"late", 12, 16}}}, nil, time.Second)
	if got, _, failed := r.redact("see xxabcxx here"); got != "see [REDACTED:outer] [REDACTED:late]" || failed {
		t.Fatalf("overlap: %q failed=%v", got, failed)
	}
	if got := ruleLabel("weird rule/\x00id!"); got != "weirdruleid" {
		t.Fatalf("ruleLabel %q", got)
	}
	if got := ruleLabel("///"); got != "secret" {
		t.Fatalf("ruleLabel fallback %q", got)
	}
}

func TestRedactorConcurrentAndLargeInput(t *testing.T) {
	r := sharedRedactor()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				in := fmt.Sprintf("worker %d step %d key %s", i, j, testGitHubPAT)
				got, changed, failed := r.redact(in)
				if failed || !changed || strings.Contains(got, testGitHubPAT) || !strings.Contains(got, "[REDACTED:github-pat]") {
					t.Errorf("concurrent redact: %q", got)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	// A history record can be up to 2 MiB; it must finish inside the budget.
	// The race detector slows the scanner several-fold, so it gets a smaller record.
	size := 2 * 1024 * 1024
	if raceEnabled {
		size = 512 * 1024
	}
	line := "The quick brown fox jumps over the lazy dog; token=" + testGitHubPAT + "\n"
	big := strings.Repeat(line, size/len(line))
	start := time.Now()
	got, changed, failed := r.redact(big)
	if elapsed := time.Since(start); elapsed > redactionTimeout {
		t.Fatalf("large scan took %v", elapsed)
	}
	if failed || !changed || strings.Contains(got, testGitHubPAT) || strings.Count(got, "[REDACTED:github-pat]") != strings.Count(big, testGitHubPAT) {
		t.Fatalf("large input: failed=%v changed=%v markers=%d", failed, changed, strings.Count(got, "[REDACTED:"))
	}
	benign := strings.Repeat("Ordinary assistant prose about the lazy dog and key=value pairs.\n", size/66)
	if got, changed, failed := r.redact(benign); got != benign || changed || failed {
		t.Fatal("benign large input must be byte-identical")
	}
}

func TestActivityRedactsBeforeRetentionAndClip(t *testing.T) {
	n := node(t)
	n.identity.SessionId, n.identity.Generation = "s", 1
	prefix := strings.Repeat("x", maxText-10)
	n.activity(1, "activity", prefix+testGitHubPAT+" tail")
	n.activity(1, "tool", "Tool started: Bash "+testAWSKey)
	n.activity(1, "activity", "nothing to hide")
	if len(n.history) != 3 {
		t.Fatalf("history %d", len(n.history))
	}
	for _, e := range n.history {
		assertNoSecret(t, e.Text)
	}
	// The raw text would have been clipped inside the secret, leaving its head
	// behind; redaction runs first so not even a fragment survives.
	if first := n.history[0].Text; !strings.HasPrefix(first, prefix[:maxText-32]) || strings.Contains(first, testGitHubPAT[:4]) || len(first) > maxText {
		t.Fatalf("boundary secret not redacted before clipping: %d bytes", len(first))
	}
	if n.history[1].Text != "Tool started: Bash [REDACTED:aws-access-token]" || n.history[2].Text != "nothing to hide" {
		t.Fatalf("unexpected texts %q %q", n.history[1].Text, n.history[2].Text)
	}
	if n.stats.Redacted != 2 || n.stats.RedactionFailures != 0 {
		t.Fatalf("counters %+v", n.stats)
	}
	// A broken scanner masks the text and counts the failure; nothing unscanned is retained.
	n.filter = newRedactor(fakeScanner{err: errors.New("boom")}, nil, time.Second)
	n.activity(1, "activity", "token "+testSlackBot)
	if n.history[3].Text != redactedUnscanned || n.stats.RedactionFailures != 1 || n.stats.Redacted != 3 {
		t.Fatalf("failure path: %q %+v", n.history[3].Text, n.stats)
	}
}

func TestSendAndPublishBypassRedaction(t *testing.T) {
	a, b := pair(t)
	// Even a scanner that masks everything must never see explicit messages.
	a.filter = newRedactor(fakeScanner{err: errors.New("boom")}, nil, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	direct := "here is my key " + testGitHubPAT
	if accepted, err := a.send(ctx, "session-b", direct, ""); err != nil || !accepted.Accepted {
		t.Fatalf("send %v %v", accepted, err)
	}
	b.mu.Lock()
	if len(b.incoming) != 1 || b.incoming[0].event.Text != direct {
		t.Fatalf("direct text altered: %+v", b.incoming)
	}
	b.mu.Unlock()
	announcement := "announcing " + testAWSKey
	if err := a.announce(announcement, ""); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	last := a.history[len(a.history)-1]
	if last.Kind != "announcement" || last.Text != announcement || a.stats.Redacted != 0 || a.stats.RedactionFailures != 0 {
		t.Fatalf("announcement altered: %q %+v", last.Text, a.stats)
	}
}

func TestLiveCollectionRedactsEveryAgent(t *testing.T) {
	want := "[REDACTED:github-pat]"
	t.Run("claude", func(t *testing.T) {
		n := node(t)
		n.identity.SessionId, n.identity.Generation = "s", 1
		path := filepath.Join(t.TempDir(), "s.jsonl")
		c, err := newCollector(path)
		if err != nil {
			t.Fatal(err)
		}
		appendLog(t, path, assistantText("s", "/p", "a1", "2026-10-01T00:00:00Z", "use "+testGitHubPAT+" now"))
		c.scan(n, 1)
		if len(n.history) != 1 || n.history[0].Text != "use "+want+" now" {
			t.Fatalf("claude: %+v", n.history)
		}
	})
	t.Run("codex", func(t *testing.T) {
		n := node(t)
		n.identity.SessionId, n.identity.Generation = "s", 1
		c, err := newCollector(filepath.Join(t.TempDir(), "rollout.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		c.agent = "codex"
		c.normalize([]byte(strings.TrimSpace(codexAssistant("m1", "2026-10-01T00:00:00Z", "use "+testGitHubPAT+" now"))), n, 1)
		if len(n.history) != 1 || n.history[0].Text != "use "+want+" now" {
			t.Fatalf("codex: %+v", n.history)
		}
	})
	t.Run("opencode", func(t *testing.T) {
		n := node(t)
		n.identity.SessionId, n.identity.Generation = "ses_a", 1
		c := newOpenCodeCollector()
		c.handle(n, 1, bridgeFrame{Type: "input", Kind: "user"})
		c.handle(n, 1, bridgeFrame{Type: "text", MessageID: "m1", Text: "use " + testGitHubPAT + " now"})
		if len(n.history) != 1 || n.history[0].Text != "use "+want+" now" {
			t.Fatalf("opencode: %+v", n.history)
		}
	})
}

func TestHistoryRedactionParity(t *testing.T) {
	hh, f, dir := openCodeHarness(t)
	secretText := "deploy with " + testGitHubPAT + " and " + testAWSKey
	liveText, _, _ := sharedRedactor().redact(secretText)
	if liveText == secretText {
		t.Fatal("fixture secret undetected")
	}
	appendLog(t, filepath.Join(hh.o.opts.ClaudeRoot, "c.jsonl"), assistantText("c", "/p", "a1", "2026-10-01T00:00:00Z", secretText)+assistantText("c", "/p", "a2", "2026-10-01T00:00:01Z", "plain"))
	codexDir := filepath.Join(hh.o.opts.CodexRoot, "2026", "10", "01")
	if err := os.MkdirAll(codexDir, 0700); err != nil {
		t.Fatal(err)
	}
	appendLog(t, filepath.Join(codexDir, "rollout-x.jsonl"), codexMeta("x", "/p", "2026-10-01T00:00:00Z")+codexAssistant("m1", "2026-10-01T00:00:02Z", secretText)+codexAssistant("m2", "2026-10-01T00:00:03Z", "plain"))
	f.session("ses_secret", dir, "secret", "", "2026-10-20T11:00:00Z", "2026-10-20T11:04:00Z")
	f.message("ses_secret", 1, "user", "2026-10-20T11:00:01Z", ocPart{"text": "prompt"})
	f.message("ses_secret", 2, "assistant", "2026-10-20T11:00:05Z", ocAssistant(ocText(secretText), ocText("plain")))
	hh.index()
	check := func(name string, out map[string]any, herr *historyError) {
		t.Helper()
		if herr != nil {
			t.Fatalf("%s: %v", name, herr)
		}
		raw, _ := json.Marshal(out)
		assertNoSecret(t, string(raw))
		events := eventsOf(out)
		if len(events) != 2 {
			t.Fatalf("%s: %d events", name, len(events))
		}
		if events[0]["text"] != liveText || events[0]["redacted"] != true {
			t.Fatalf("%s: history differs from live: %v", name, events[0])
		}
		if events[1]["text"] != "plain" {
			t.Fatalf("%s: plain text altered: %v", name, events[1])
		}
		if _, ok := events[1]["redacted"]; ok {
			t.Fatalf("%s: unredacted event carries the flag", name)
		}
	}
	out, herr := hh.read(`{"session_id":"c","agent_type":"claude","limit":10}`)
	check("claude", out, herr)
	out, herr = hh.read(`{"session_id":"x","agent_type":"codex","limit":10}`)
	check("codex", out, herr)
	out, herr = hh.read(`{"session_id":"ses_secret","agent_type":"opencode","limit":10}`)
	check("opencode", out, herr)
	// Paired-host reads run through the same sink.
	a := &historyAccess{host: peerHostA, grant: peerGrant, policy: peerPolicy(config.AgentHistory{Enabled: true}, config.AgentHistory{Enabled: true}), now: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}
	out, herr = hh.peerRead(a, `{"session_id":"c","agent_type":"claude","limit":10}`)
	check("peer claude", out, herr)
	out, herr = hh.peerRead(a, `{"session_id":"x","agent_type":"codex","limit":10}`)
	check("peer codex", out, herr)
}

func BenchmarkRedact(b *testing.B) {
	r := sharedRedactor()
	benign := strings.Repeat("Some assistant prose with code `x := y` and paths /usr/local/bin. ", 110)[:maxText]
	withSecret := benign[:len(benign)-len(testGitHubPAT)-1] + " " + testGitHubPAT
	for _, bc := range []struct{ name, in string }{{"benign7KiB", benign}, {"secret7KiB", withSecret}} {
		b.Run(bc.name, func(b *testing.B) {
			b.SetBytes(int64(len(bc.in)))
			for i := 0; i < b.N; i++ {
				r.redact(bc.in)
			}
		})
	}
}
