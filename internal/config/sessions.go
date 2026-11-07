package config

import (
	"fmt"
	"time"
)

// Sessions holds the file-only [sessions] table. recent_window is how long an
// ended local session stays advertised as `recent` (never live) after its
// last recorded activity. share_prompts_and_tools (default false) lets this
// host publish human prompts and tool inputs/outputs, after secret redaction,
// in live activity and in the history it serves. SharePeerTriggeredActivity
// separately controls responses to peer input (default none).
type Sessions struct {
	SharePeerTriggeredActivity PeerActivityPolicy `toml:"share_peer_triggered_activity"`
	RecentWindow               string             `toml:"recent_window"`
	SharePromptsAndTools       bool               `toml:"share_prompts_and_tools"`
}

// DefaultRecentWindow applies when [sessions].recent_window is omitted.
const DefaultRecentWindow = 24 * time.Hour

var sessionsKeys = []string{"recent_window", "share_prompts_and_tools", "share_peer_triggered_activity"}

// ParseRecentWindow accepts a finite, nonnegative Go duration such as "24h",
// "90m" or "0s". Zero disables recent-only advertisements; live discovery is
// unaffected. Negative, malformed and overflowing values are rejected.
func ParseRecentWindow(value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("sessions.recent_window %q must be a nonnegative duration such as \"24h\" or \"0s\"", value)
	}
	if d < 0 {
		return 0, fmt.Errorf("sessions.recent_window %q must not be negative", value)
	}
	return d, nil
}

// PeerActivityPolicy controls which otherwise eligible events a host shares
// from peer-only turns. The zero value fails closed, like PeerActivityNone.
type PeerActivityPolicy string

const (
	// PeerActivityNone suppresses automatic activity from peer-only turns.
	PeerActivityNone PeerActivityPolicy = "none"
	// PeerActivityResponses permits only ordinary assistant response text.
	PeerActivityResponses PeerActivityPolicy = "responses"
	// PeerActivityAll permits all otherwise eligible kinds from peer-only turns.
	PeerActivityAll PeerActivityPolicy = "all"
)

// Allows reports whether kind may pass the peer-only-turn suppression gate.
// It does not replace content filtering, detail negotiation or delivery limits.
func (p PeerActivityPolicy) Allows(peerOnly bool, kind string) bool {
	return !peerOnly || p == PeerActivityAll || p == PeerActivityResponses && kind == "activity"
}

func parsePeerActivity(value PeerActivityPolicy) (PeerActivityPolicy, error) {
	switch value {
	case PeerActivityNone, PeerActivityResponses, PeerActivityAll:
		return value, nil
	default:
		return "", fmt.Errorf("sessions.share_peer_triggered_activity %q must be none, responses, or all", value)
	}
}
