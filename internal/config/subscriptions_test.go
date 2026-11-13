package config

import (
	"bytes"
	"strings"
	"testing"
)

func TestSubscriptionDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		body, mode, source string
		budget             int
	}{
		{"", "full", "default", 0},
		{"[subscriptions]\ndefault_mode = 'markers'\nwindow = '10m'\nmax_per_minute = 20", "markers", "file", 20},
	} {
		c, err := Preview(tc.body)
		if err != nil {
			t.Fatal(err)
		}
		if c.Subscriptions.DefaultMode != tc.mode || c.Subscriptions.MaxPerMinute != tc.budget {
			t.Fatalf("%+v", c.Subscriptions)
		}
		var out bytes.Buffer
		if err := c.Write(&out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "subscriptions.default_mode = \""+tc.mode+"\" # source: "+tc.source) {
			t.Fatal(out.String())
		}
		for _, key := range []string{"default_mode", "window", "max_per_minute"} {
			if c.Sources["subscriptions."+key] != tc.source {
				t.Fatal(c.Sources)
			}
		}
	}
	for _, body := range []string{"default_mode = 'oops'", "window = '0s'", "window = '25h'", "max_per_minute = 30", "max_per_minute = -1", "Default_Mode = 'tools'", "unexpected = true"} {
		if _, err := Preview("[subscriptions]\n" + body); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}
