package config

import (
	"errors"
	"time"
	"unicode"
	"unicode/utf8"
)

// Dream is the file-only, owner-only policy for explicit local consolidation.
// It never changes session sharing or enables background work.
type Dream struct {
	Enabled           bool   `toml:"enabled"`
	IncludePrompts    bool   `toml:"include_prompts"`
	IncludeToolBodies bool   `toml:"include_tool_bodies"`
	Lookback          string `toml:"lookback"`
	Model             string `toml:"model"`
	MaxInputBytes     int    `toml:"max_input_bytes"`
}

var dreamKeys = []string{"enabled", "include_prompts", "include_tool_bodies", "lookback", "model", "max_input_bytes"}

func resolveDream(d Dream, defined func(string) bool) (Dream, error) {
	if !defined("lookback") {
		d.Lookback = "7d"
	}
	if !defined("max_input_bytes") {
		d.MaxInputBytes = 256 << 10
	}
	return d, d.Validate()
}

// Validate rejects unbounded input and invalid model or lookback settings.
func (d Dream) Validate() error {
	days, err := ParseLookback(d.Lookback)
	if err != nil || days < 1 || days > 365 {
		return errors.New("dream.lookback must be 1d through 365d")
	}
	if d.MaxInputBytes < 4096 || d.MaxInputBytes > 1<<20 {
		return errors.New("dream.max_input_bytes must be 4096 through 1048576")
	}
	if len(d.Model) > 200 || !utf8.ValidString(d.Model) {
		return errors.New("invalid dream.model")
	}
	for _, r := range d.Model {
		if unicode.IsControl(r) {
			return errors.New("invalid dream.model")
		}
	}
	return nil
}

// Cutoff returns the UTC lookback bound for a validated policy.
func (d Dream) Cutoff(now time.Time) time.Time {
	days, _ := ParseLookback(d.Lookback)
	return now.UTC().Add(-time.Duration(days) * 24 * time.Hour)
}
