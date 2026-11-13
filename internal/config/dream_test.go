package config

import (
	"testing"
	"time"
)

func TestDreamConfig(t *testing.T) {
	c, err := Preview("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Dream.Enabled || c.Dream.IncludePrompts || c.Dream.IncludeToolBodies || c.Dream.Lookback != "7d" || c.Dream.Model != "" || c.Dream.MaxInputBytes != 256<<10 {
		t.Fatalf("defaults: %+v", c.Dream)
	}
	for _, body := range []string{`enabled = true`, `include_prompts = true`, `include_tool_bodies = true`, `lookback = "365d"`, `model = "example"`, `max_input_bytes = 4096`, `max_input_bytes = 1048576`} {
		if _, err := Preview("[dream]\n" + body); err != nil {
			t.Errorf("%s: %v", body, err)
		}
	}
	for _, body := range []string{`unknown = true`, `Enabled = true`, `lookback = "all"`, `lookback = "0d"`, `lookback = "366d"`, `lookback = ""`, `max_input_bytes = 0`, `max_input_bytes = 1048577`, `model = "bad\nmodel"`, `enabled = "yes"`} {
		if _, err := Preview("[dream]\n" + body); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	c, err = Resolve(map[string]string{"dream-enabled": "true"}, func(k string) string {
		if k == "RHIZOME_DREAM_ENABLED" {
			return "true"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Dream.Enabled {
		t.Fatal("environment/flag enabled dream")
	}
}

func TestDreamSharingConfig(t *testing.T) {
	c, err := Preview("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Gateway.Dream.Mode != DreamShareOff || c.Gateway.Dream.Lookback != "7d" || c.Gateway.Dream.Enabled() {
		t.Fatalf("defaults: %+v", c.Gateway.Dream)
	}
	if (DreamSharing{}).Enabled() {
		t.Fatal("zero value enabled")
	}
	for body, mode := range map[string]string{`mode = "off"`: "off", `mode = "review"`: "review", "mode = \"auto\"\nlookback = \"365d\"": "auto", `lookback = "1d"`: "off"} {
		c, err := Preview("[gateway.dream]\n" + body)
		if err != nil {
			t.Errorf("%s: %v", body, err)
			continue
		}
		if c.Gateway.Dream.Mode != mode || c.Sources["gateway.dream.mode"] == "" {
			t.Errorf("%s: %+v", body, c.Gateway.Dream)
		}
	}
	for _, body := range []string{`mode = "on"`, `mode = ""`, `Mode = "auto"`, `unknown = 1`, `lookback = "0d"`, `lookback = "all"`, `lookback = "366d"`, `mode = true`} {
		if _, err := Preview("[gateway.dream]\n" + body); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
	// File-only: no environment or flag spelling enables sharing.
	c, err = Resolve(map[string]string{"gateway-dream-mode": "auto"}, func(k string) string {
		if k == "RHIZOME_GATEWAY_DREAM_MODE" {
			return "auto"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Gateway.Dream.Enabled() {
		t.Fatal("environment/flag enabled digest sharing")
	}
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	if got := (DreamSharing{Mode: "review", Lookback: "2d"}).Cutoff(now); !got.Equal(now.Add(-48 * time.Hour)) {
		t.Fatalf("cutoff %v", got)
	}
}
