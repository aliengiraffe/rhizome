package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// PeerHistory is one level of the opt-in policy that lets directly paired hosts
// search and read this host's session history. [gateway.history] is the global
// default and master switch; [claude.peer_history], [codex.peer_history] and
// [opencode.peer_history] override it for history whose source agent is that agent.
type PeerHistory struct {
	Enabled  bool   `toml:"enabled"`
	Lookback string `toml:"lookback"`
}

// HistoryAgents lists the source agents a peer-history policy can name.
var HistoryAgents = []string{"claude", "codex", "opencode"}

// DefaultLookback applies when sharing is enabled without an explicit lookback.
const DefaultLookback = "30d"

// maxLookbackDays keeps N*24h within time.Duration.
const maxLookbackDays = math.MaxInt64 / int64(24*time.Hour)

var peerHistoryKeys = []string{"enabled", "lookback"}

// AgentHistory is the effective policy for one source agent. Days == 0 means
// every locally indexed record ("all"); otherwise the rolling window in days.
type AgentHistory struct {
	Enabled bool
	Days    int64
}

// Lookback renders the window as configured ("15d" or "all").
func (a AgentHistory) Lookback() string {
	if a.Days == 0 {
		return "all"
	}
	return strconv.FormatInt(a.Days, 10) + "d"
}

// Cutoff returns the inclusive UTC lower bound at now, or nil for "all".
func (a AgentHistory) Cutoff(now time.Time) *time.Time {
	if a.Days == 0 {
		return nil
	}
	t := now.UTC().Add(-time.Duration(a.Days) * 24 * time.Hour)
	return &t
}

// HistoryPolicy is the serving host's effective peer-history policy. The zero
// value shares nothing. It is comparable so service configurations stay so.
type HistoryPolicy struct {
	Enabled                 bool
	Claude, Codex, OpenCode AgentHistory
}

// Agent returns the effective policy for a source agent; unknown agents and a
// disabled global switch are never enabled.
func (p HistoryPolicy) Agent(agent string) AgentHistory {
	var a AgentHistory
	switch agent {
	case "claude":
		a = p.Claude
	case "codex":
		a = p.Codex
	case "opencode":
		a = p.OpenCode
	default:
		return AgentHistory{}
	}
	a.Enabled = a.Enabled && p.Enabled
	return a
}

// Fingerprint identifies the effective policy for binding pagination state.
func (p HistoryPolicy) Fingerprint() string {
	parts := []string{fmt.Sprint(p.Enabled)}
	for _, agent := range HistoryAgents {
		a := p.Agent(agent)
		parts = append(parts, fmt.Sprintf("%s=%t/%d", agent, a.Enabled, a.Days))
	}
	return strings.Join(parts, ",")
}

// ParseLookback accepts a positive whole number of days without sign or leading
// zeros ("15d") or "all" (returned as 0).
func ParseLookback(value string) (int64, error) {
	if value == "all" {
		return 0, nil
	}
	digits, ok := strings.CutSuffix(value, "d")
	bad := fmt.Errorf("lookback %q must be a positive whole number of days such as \"30d\", or \"all\"", value)
	if !ok || digits == "" || digits[0] == '0' || strings.Trim(digits, "0123456789") != "" {
		return 0, bad
	}
	days, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || days > maxLookbackDays {
		return 0, fmt.Errorf("lookback %q is too large", value)
	}
	return days, nil
}

// historyPolicy resolves the global default and per-agent overrides. defined
// reports whether a key is set in the file; unset agent keys inherit.
func historyPolicy(global PeerHistory, agents map[string]PeerHistory, defined func(table, key string) bool) (HistoryPolicy, error) {
	p := HistoryPolicy{Enabled: global.Enabled}
	lookback := global.Lookback
	if !defined("gateway.history", "lookback") {
		lookback = DefaultLookback
	}
	days, err := ParseLookback(lookback)
	if err != nil {
		return p, fmt.Errorf("gateway.history.%w", err)
	}
	for _, agent := range HistoryAgents {
		o, table := agents[agent], agent+".peer_history"
		a := AgentHistory{Enabled: p.Enabled, Days: days}
		if defined(table, "lookback") {
			if a.Days, err = ParseLookback(o.Lookback); err != nil {
				return p, fmt.Errorf("%s.%w", table, err)
			}
		}
		if defined(table, "enabled") {
			a.Enabled = p.Enabled && o.Enabled
		}
		switch agent {
		case "claude":
			p.Claude = a
		case "codex":
			p.Codex = a
		default:
			p.OpenCode = a
		}
	}
	return p, nil
}
