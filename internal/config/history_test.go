package config

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestPeerHistoryPolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	type want struct {
		enabled               bool
		claude, codex         bool
		claudeDays, codexDays int64
	}
	cases := []struct {
		name, body string
		want       want
	}{
		{"omitted", ``, want{false, false, false, 30, 30}},
		{"global only", "[gateway.history]\nenabled = true\n", want{true, true, true, 30, 30}},
		{"global lookback", "[gateway.history]\nenabled = true\nlookback = \"15d\"\n", want{true, true, true, 15, 15}},
		{"global all", "[gateway.history]\nenabled = true\nlookback = \"all\"\n", want{true, true, true, 0, 0}},
		{"mixed windows", "[gateway.history]\nenabled = true\n[claude.peer_history]\nlookback = \"15d\"\n[codex.peer_history]\nlookback = \"all\"\n", want{true, true, true, 15, 0}},
		{"disable one agent", "[gateway.history]\nenabled = true\n[codex.peer_history]\nenabled = false\n", want{true, true, false, 30, 30}},
		{"agent cannot bypass master gate", "[gateway.history]\nenabled = false\n[claude.peer_history]\nenabled = true\nlookback = \"all\"\n", want{false, false, false, 0, 30}},
		{"agent enabled without global", "[codex.peer_history]\nenabled = true\n", want{false, false, false, 30, 30}},
		{"explicit agent enable inherits window", "[gateway.history]\nenabled = true\nlookback = \"7d\"\n[claude.peer_history]\nenabled = true\n", want{true, true, true, 7, 7}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Resolve(map[string]string{"config": fixture(t, tc.body)}, noenv)
			if err != nil {
				t.Fatal(err)
			}
			p := c.PeerHistory
			got := want{p.Enabled, p.Agent("claude").Enabled, p.Agent("codex").Enabled, p.Agent("claude").Days, p.Agent("codex").Days}
			if got != tc.want {
				t.Fatalf("policy %+v, want %+v", got, tc.want)
			}
			if p.Agent("opencode").Enabled {
				t.Fatal("unsupported agent enabled")
			}
		})
	}
}

func TestPeerHistoryRejectsInvalidValues(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, body := range []string{
		"[gateway.history]\nlookback = \"0d\"",
		"[gateway.history]\nlookback = \"-1d\"",
		"[gateway.history]\nlookback = \"+5d\"",
		"[gateway.history]\nlookback = \"015d\"",
		"[gateway.history]\nlookback = \"15\"",
		"[gateway.history]\nlookback = \"2w\"",
		"[gateway.history]\nlookback = \"1.5d\"",
		"[gateway.history]\nlookback = \"ALL\"",
		"[gateway.history]\nlookback = \" 15d\"",
		"[gateway.history]\nlookback = \"d\"",
		"[gateway.history]\nlookback = \"\"",
		"[codex.peer_history]\nlookback = \"\"",
		"[gateway.history]\nlookback = \"106752d\"",
		"[gateway.history]\nlookback = \"99999999999999999999d\"",
		"[gateway.history]\nlookback = 30",
		"[gateway.history]\nenabled = \"yes\"",
		"[gateway.history]\nenable = true",
		"[gateway.history]\nwindow = \"30d\"",
		"[claude.peer_history]\nlookback = \"0d\"",
		"[codex.peer_history]\nlookback = \"all \"",
		"[claude.peer_histroy]\nenabled = true",
		"[claude.history]\nenabled = true",
		"[opencode.peer_history]\nenabled = true",
		"[Claude.peer_history]\nenabled = true",
		"[gateway.History]\nenabled = true",
		"[gateway.history.claude]\nenabled = true",
	} {
		if _, err := Resolve(map[string]string{"config": fixture(t, body)}, noenv); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
}

func TestParseLookbackBounds(t *testing.T) {
	for value, days := range map[string]int64{"1d": 1, "15d": 15, "30d": 30, "106751d": 106751, "all": 0} {
		got, err := ParseLookback(value)
		if err != nil || got != days {
			t.Errorf("%s: %d %v", value, got, err)
		}
	}
	a := AgentHistory{Enabled: true, Days: 106751}
	if a.Cutoff(time.Now()) == nil {
		t.Fatal("maximum window has no cutoff")
	}
}

func TestAgentHistoryCutoff(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.FixedZone("x", 5*3600))
	cut := AgentHistory{Enabled: true, Days: 15}.Cutoff(now)
	if want := time.Date(2026, 10, 6, 7, 0, 0, 0, time.UTC); !cut.Equal(want) || cut.Location() != time.UTC {
		t.Fatalf("cutoff %v", cut)
	}
	if (AgentHistory{Enabled: true}).Cutoff(now) != nil {
		t.Fatal("all has a cutoff")
	}
}

func TestPeerHistoryFingerprintAndOutput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resolve := func(body string) Config {
		t.Helper()
		c, err := Resolve(map[string]string{"config": fixture(t, body)}, noenv)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	open := resolve("[gateway.history]\nenabled = true\n[claude.peer_history]\nlookback = \"15d\"\n")
	narrower := resolve("[gateway.history]\nenabled = true\n[claude.peer_history]\nlookback = \"7d\"\n")
	if open.PeerHistory.Fingerprint() == narrower.PeerHistory.Fingerprint() {
		t.Fatal("policy change keeps fingerprint")
	}
	var out bytes.Buffer
	if err := open.Write(&out); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"gateway.history.enabled = true # source: file",
		`gateway.history.lookback = "30d" # source: default`,
		`claude.peer_history.lookback = "15d" # effective; source: file`,
		"codex.peer_history.enabled = true # effective; source: inherited",
		`codex.peer_history.lookback = "30d" # effective; source: inherited`,
	} {
		if !strings.Contains(out.String(), line+"\n") {
			t.Errorf("missing %q in\n%s", line, out.String())
		}
	}
	var none bytes.Buffer
	if err := resolve("").Write(&none); err != nil || !strings.Contains(none.String(), "gateway.history.enabled = false # source: default\n") || !strings.Contains(none.String(), "claude.peer_history.enabled = false") {
		t.Fatalf("default output %v\n%s", err, none.String())
	}
}
