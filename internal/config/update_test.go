package config

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestUpdatePolicyPrecedence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cases := []struct {
		name, body    string
		env, flags    map[string]string
		wantEnabled   bool
		wantInterval  time.Duration
		enabledSource string
		intervalSrc   string
	}{
		{"omitted defaults on", "", nil, nil, true, 24 * time.Hour, "default", "default"},
		{"empty table", "[update]\n", nil, nil, true, 24 * time.Hour, "default", "default"},
		{"file disables", "[update]\nenabled = false\n", nil, nil, false, 24 * time.Hour, "file", "default"},
		{"file explicit on", "[update]\nenabled = true\ncheck_interval = \"6h\"\n", nil, nil, true, 6 * time.Hour, "file", "file"},
		{"dotted keys", "update.enabled = false\nupdate.check_interval = \"1h\"\n", nil, nil, false, time.Hour, "file", "file"},
		{"env overrides file", "[update]\nenabled = true\n", map[string]string{"RHIZOME_UPDATE": "0"}, nil, false, 24 * time.Hour, "env", "default"},
		{"env enables", "[update]\nenabled = false\n", map[string]string{"RHIZOME_UPDATE": "yes", "RHIZOME_UPDATE_CHECK_INTERVAL": "2h"}, nil, true, 2 * time.Hour, "env", "env"},
		{"flag overrides env", "[update]\nenabled = true\n", map[string]string{"RHIZOME_UPDATE": "1", "RHIZOME_UPDATE_CHECK_INTERVAL": "2h"}, map[string]string{"update": "false", "update-check-interval": "3h"}, false, 3 * time.Hour, "flag", "flag"},
		{"flag alone", "", nil, map[string]string{"update": "off"}, false, 24 * time.Hour, "flag", "default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flags := map[string]string{"config": fixture(t, tc.body)}
			for k, v := range tc.flags {
				flags[k] = v
			}
			c, err := Resolve(flags, func(k string) string { return tc.env[k] })
			if err != nil {
				t.Fatal(err)
			}
			if c.AutoUpdate.Enabled != tc.wantEnabled || c.AutoUpdate.CheckInterval != tc.wantInterval {
				t.Fatalf("policy %+v", c.AutoUpdate)
			}
			if c.Sources["update.enabled"] != tc.enabledSource || c.Sources["update.check_interval"] != tc.intervalSrc {
				t.Fatalf("sources %s/%s", c.Sources["update.enabled"], c.Sources["update.check_interval"])
			}
			var out bytes.Buffer
			if err := c.Write(&out); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				"update.enabled = " + map[bool]string{true: "true", false: "false"}[tc.wantEnabled] + " # source: " + tc.enabledSource,
				"update.check_interval = \"" + tc.wantInterval.String() + "\" # source: " + tc.intervalSrc,
			} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("config output lacks %q:\n%s", want, out.String())
				}
			}
		})
	}
}

func TestUpdatePolicyRejectsInvalidValues(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, body := range []string{
		"[update]\nenabled = \"true\"",
		"[update]\nenabled = 1",
		"[update]\ncheck_interval = \"30m\"",
		"[update]\ncheck_interval = \"59m59s\"",
		"[update]\ncheck_interval = \"721h\"",
		"[update]\ncheck_interval = \"\"",
		"[update]\ncheck_interval = \"1d\"",
		"[update]\ncheck_interval = 24",
		"[update]\ninterval = \"24h\"",
		"[update]\nauto = true",
		"[Update]\nenabled = true",
		"[update]\nEnabled = true",
		"[updates]\nenabled = true",
		"[claude.update]\nenabled = true",
		"[update.claude]\nenabled = true",
		"[claude]\nupdate = true",
	} {
		if _, err := Resolve(map[string]string{"config": fixture(t, body)}, noenv); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
	p := fixture(t, "")
	for _, env := range []map[string]string{{"RHIZOME_UPDATE": "maybe"}, {"RHIZOME_UPDATE_CHECK_INTERVAL": "5m"}, {"RHIZOME_UPDATE_CHECK_INTERVAL": "abc"}} {
		if _, err := Resolve(map[string]string{"config": p}, func(k string) string { return env[k] }); err == nil {
			t.Errorf("accepted env %v", env)
		}
	}
	for _, flags := range []map[string]string{{"update": ""}, {"update": "2"}, {"update-check-interval": "0s"}} {
		flags["config"] = p
		if _, err := Resolve(flags, noenv); err == nil {
			t.Errorf("accepted flags %v", flags)
		}
	}
	for _, in := range []string{"1h", "24h", "720h", "1h30m"} {
		if _, err := ParseUpdateInterval(in); err != nil {
			t.Errorf("%s: %v", in, err)
		}
	}
}
