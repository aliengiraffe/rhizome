package config

import (
	"fmt"
	"time"
)

// Subscriptions holds file-only defaults for new subscriptions.
type Subscriptions struct {
	DefaultMode  string `toml:"default_mode"`
	Window       string `toml:"window"`
	MaxPerMinute int    `toml:"max_per_minute"`
}

// SubscriptionModes lists the supported subscription presets.
const SubscriptionModes = "full, markers, tools, text, digest"

// ValidateSubscriptionMode rejects unknown presets.
func ValidateSubscriptionMode(mode string) error {
	switch mode {
	case "full", "markers", "tools", "text", "digest":
		return nil
	default:
		return fmt.Errorf("unknown subscription mode %q; valid values: %s", mode, SubscriptionModes)
	}
}

// SubscriptionWindow bounds digest latency and retained state.
func SubscriptionWindow(value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil || d < time.Second || d > 24*time.Hour {
		return 0, fmt.Errorf("subscription window must be a duration from 1s to 24h")
	}
	return d, nil
}
