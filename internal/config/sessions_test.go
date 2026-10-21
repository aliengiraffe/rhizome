package config

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestRecentWindow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cases := []struct {
		name, body string
		want       time.Duration
		source     string
	}{
		{"omitted", "", 24 * time.Hour, "default"},
		{"empty table", "[sessions]\n", 24 * time.Hour, "default"},
		{"explicit default", "[sessions]\nrecent_window = \"24h\"\n", 24 * time.Hour, "file"},
		{"shorter", "[sessions]\nrecent_window = \"90m\"\n", 90 * time.Minute, "file"},
		{"longer", "[sessions]\nrecent_window = \"72h\"\n", 72 * time.Hour, "file"},
		{"dotted key", "sessions.recent_window = \"30s\"\n", 30 * time.Second, "file"},
		{"zero disables", "[sessions]\nrecent_window = \"0s\"\n", 0, "file"},
		{"zero without unit", "[sessions]\nrecent_window = \"0\"\n", 0, "file"},
		{"mixed units", "[sessions]\nrecent_window = \"1h30m\"\n", 90 * time.Minute, "file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Resolve(map[string]string{"config": fixture(t, tc.body)}, noenv)
			if err != nil {
				t.Fatal(err)
			}
			if c.RecentWindow != tc.want || c.Sources["sessions.recent_window"] != tc.source {
				t.Fatalf("window %s (%s), want %s (%s)", c.RecentWindow, c.Sources["sessions.recent_window"], tc.want, tc.source)
			}
			var out bytes.Buffer
			if err := c.Write(&out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "sessions.recent_window = \""+tc.want.String()+"\" # source: "+tc.source) {
				t.Fatalf("config output lacks the effective window:\n%s", out.String())
			}
		})
	}
}

func TestRecentWindowRejectsInvalidValues(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, body := range []string{
		"[sessions]\nrecent_window = \"\"",
		"[sessions]\nrecent_window = \"-1h\"",
		"[sessions]\nrecent_window = \"24\"",
		"[sessions]\nrecent_window = \"1d\"",
		"[sessions]\nrecent_window = \"1w\"",
		"[sessions]\nrecent_window = \" 24h\"",
		"[sessions]\nrecent_window = \"24H\"",
		"[sessions]\nrecent_window = \"infinite\"",
		"[sessions]\nrecent_window = \"9999999999999h\"",
		"[sessions]\nrecent_window = \"2562047h47m16.854775808s\"",
		"[sessions]\nrecent_window = 24",
		"[sessions]\nrecent_window = true",
		"[sessions]\nwindow = \"24h\"",
		"[sessions]\nrecent-window = \"24h\"",
		"[session]\nrecent_window = \"24h\"",
		"[Sessions]\nrecent_window = \"24h\"",
		"[sessions]\nRecent_Window = \"24h\"",
		"[claude.sessions]\nrecent_window = \"24h\"",
		"[sessions.claude]\nrecent_window = \"24h\"",
	} {
		if _, err := Resolve(map[string]string{"config": fixture(t, body)}, noenv); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
	// The largest representable duration is accepted; one unit more is not.
	if d, err := ParseRecentWindow("2562047h47m16.854775807s"); err != nil || d != time.Duration(1<<63-1) {
		t.Fatalf("maximum duration: %s %v", d, err)
	}
}
