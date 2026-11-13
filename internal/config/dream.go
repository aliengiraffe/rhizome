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

// Digest-sharing modes for [gateway.dream].
const (
	DreamShareOff    = "off"
	DreamShareReview = "review"
	DreamShareAuto   = "auto"
)

// DreamSharing is the serving host's file-only [gateway.dream] policy for
// redacted team digests. Off (the default) generates, serves and requests
// nothing and keeps the dream gateway subprotocol unadvertised. The service
// reads it at start; a restart applies a change.
type DreamSharing struct {
	Mode     string `toml:"mode"`
	Lookback string `toml:"lookback"`
}

var dreamSharingKeys = []string{"mode", "lookback"}

func resolveDreamSharing(d DreamSharing, defined func(string) bool) (DreamSharing, error) {
	if !defined("mode") {
		d.Mode = DreamShareOff
	}
	if !defined("lookback") {
		d.Lookback = "7d"
	}
	return d, d.Validate()
}

// Validate rejects unknown modes and lookbacks outside 1d through 365d.
func (d DreamSharing) Validate() error {
	switch d.Mode {
	case DreamShareOff, DreamShareReview, DreamShareAuto:
	default:
		return errors.New("gateway.dream.mode must be off, review or auto")
	}
	days, err := ParseLookback(d.Lookback)
	if err != nil || days < 1 || days > 365 {
		return errors.New("gateway.dream.lookback must be 1d through 365d")
	}
	return nil
}

// Enabled reports whether digests may be generated, served or requested. The
// zero value is off.
func (d DreamSharing) Enabled() bool {
	return d.Mode == DreamShareReview || d.Mode == DreamShareAuto
}

// Cutoff is the oldest creation time a published digest may have at now.
func (d DreamSharing) Cutoff(now time.Time) time.Time {
	days, err := ParseLookback(d.Lookback)
	if err != nil || days < 1 {
		days = 7
	}
	return now.UTC().Add(-time.Duration(days) * 24 * time.Hour)
}
