package config

import (
	"fmt"
	"strings"
	"time"
)

// Update holds the file-side [update] table. The background observer checks
// GitHub Releases for newer signed builds when enabled; MCP processes and
// one-shot commands never do.
type Update struct {
	Enabled       *bool  `toml:"enabled"`
	CheckInterval string `toml:"check_interval"`
}

// UpdatePolicy is the effective auto-update setting after flag, environment,
// file and default resolution.
type UpdatePolicy struct {
	Enabled       bool
	CheckInterval time.Duration
}

const (
	// DefaultUpdateInterval applies when [update].check_interval is omitted.
	DefaultUpdateInterval = 24 * time.Hour
	// MinUpdateInterval bounds how often the observer may contact GitHub.
	MinUpdateInterval = time.Hour
	// MaxUpdateInterval keeps the interval representable in schedules and
	// state files.
	MaxUpdateInterval = 30 * 24 * time.Hour
)

var updateKeys = []string{"enabled", "check_interval"}

// ParseUpdateInterval accepts a Go duration between MinUpdateInterval and
// MaxUpdateInterval, such as "24h" or "6h30m".
func ParseUpdateInterval(value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("update.check_interval %q must be a duration such as \"24h\"", value)
	}
	if d < MinUpdateInterval || d > MaxUpdateInterval {
		return 0, fmt.Errorf("update.check_interval %q must be between %s and %s", value, MinUpdateInterval, MaxUpdateInterval)
	}
	return d, nil
}

// ParseUpdateEnabled accepts the boolean spellings used by RHIZOME_UPDATE and
// --update: 1/0, true/false, yes/no and on/off (case-insensitive).
func ParseUpdateEnabled(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("update.enabled %q must be true or false", value)
}

// resolveUpdate layers flag > env > file > default for the [update] table.
func resolveUpdate(raw Update, defined func(key string) bool, flags map[string]string, getenv func(string) string) (UpdatePolicy, map[string]string, error) {
	p := UpdatePolicy{Enabled: true, CheckInterval: DefaultUpdateInterval}
	sources := map[string]string{"update.enabled": "default", "update.check_interval": "default"}
	var err error
	if defined("enabled") && raw.Enabled != nil {
		p.Enabled, sources["update.enabled"] = *raw.Enabled, "file"
	}
	if defined("check_interval") {
		if p.CheckInterval, err = ParseUpdateInterval(raw.CheckInterval); err != nil {
			return p, sources, err
		}
		sources["update.check_interval"] = "file"
	}
	for _, layer := range []struct {
		name  string
		value func(key, env string) (string, bool)
	}{
		{"env", func(_, env string) (string, bool) { v := getenv(env); return v, v != "" }},
		{"flag", func(key, _ string) (string, bool) { v, ok := flags[key]; return v, ok }},
	} {
		if v, ok := layer.value("update", "RHIZOME_UPDATE"); ok {
			if p.Enabled, err = ParseUpdateEnabled(v); err != nil {
				return p, sources, fmt.Errorf("%s: %w", layer.name, err)
			}
			sources["update.enabled"] = layer.name
		}
		if v, ok := layer.value("update-check-interval", "RHIZOME_UPDATE_CHECK_INTERVAL"); ok {
			if p.CheckInterval, err = ParseUpdateInterval(v); err != nil {
				return p, sources, fmt.Errorf("%s: %w", layer.name, err)
			}
			sources["update.check_interval"] = layer.name
		}
	}
	return p, sources, nil
}
