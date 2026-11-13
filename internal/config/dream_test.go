package config

import "testing"

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
