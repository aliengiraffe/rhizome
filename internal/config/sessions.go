package config

import (
	"fmt"
	"time"
)

// Sessions holds the file-only [sessions] table. recent_window is how long an
// ended local session stays advertised as `recent` (never live) after its
// last recorded activity.
type Sessions struct {
	RecentWindow string `toml:"recent_window"`
}

// DefaultRecentWindow applies when [sessions].recent_window is omitted.
const DefaultRecentWindow = 24 * time.Hour

var sessionsKeys = []string{"recent_window"}

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
