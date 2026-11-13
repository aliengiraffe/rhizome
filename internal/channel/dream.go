package channel

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"rhizome/internal/config"
)

// DreamSource identifies an actual local event included in a dream input.
type DreamSource struct {
	SessionID string    `json:"session_id"`
	Agent     string    `json:"agent"`
	HostID    string    `json:"host_id"`
	At        time.Time `json:"at"`
}

// DreamExcerpt contains normalized, secret-filtered owner history, never names or paths.
type DreamExcerpt struct {
	Source DreamSource `json:"source"`
	Kind   string      `json:"kind"`
	Text   string      `json:"text"`
}

// RedactDream applies the shared fail-closed secret filter to assembled inputs
// and model output. It never logs the text or findings.
func RedactDream(text string) string {
	filtered, _, _ := sharedRedactor().redact(text)
	return filtered
}

// DreamHistory reads only the local catalog and existing confined snapshots.
// Newest sessions are selected first; events within each are read from byte zero.
// A 30-second deadline, 64 sessions, 128 pages and the input budget bound work.
func DreamHistory(ctx context.Context, c config.Config, since, now time.Time, budget int) ([]DreamExcerpt, error) {
	if !c.Dream.Enabled {
		return nil, errors.New("dream disabled; enable [dream].enabled in the configuration file")
	}
	if err := c.Dream.Validate(); err != nil {
		return nil, err
	}
	if budget < 0 || budget > c.Dream.MaxInputBytes {
		return nil, errors.New("invalid dream history budget")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	h := newHistoryService(historyScope{Runtime: c.RuntimeDir, ClaudeRoot: c.Claude.ProjectsDir, CodexRoot: c.Codex.SessionsDir, OpenCodeRoot: c.OpenCode.DataDir, PiRoot: c.Pi.SessionsDir})
	h.details = c.Dream.IncludePrompts || c.Dream.IncludeToolBodies
	h.peerActivity = config.PeerActivityNone
	cat, _, err := h.catalog.loadWithStatus(false)
	if err != nil {
		return nil, err
	}
	cutoff := c.Dream.Cutoff(now)
	var sessions []*historySession
	for _, s := range cat.sessions() {
		if s.LastActivityAt == nil || s.LastActivityAt.Before(cutoff) || !s.LastActivityAt.After(since) || s.LastActivityAt.After(now) || s.source() == nil || (s.Availability != "available" && s.Availability != "partial") {
			continue
		}
		sessions = append(sessions, s)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].LastActivityAt.Equal(*sessions[j].LastActivityAt) {
			return sessions[i].AgentType+sessions[i].SessionID < sessions[j].AgentType+sessions[j].SessionID
		}
		return sessions[i].LastActivityAt.After(*sessions[j].LastActivityAt)
	})
	if len(sessions) > 64 {
		sessions = sessions[:64]
	}
	var out []DreamExcerpt
	used, pages := 2, 0
	for _, session := range sessions {
		snap, err := h.openSnapshot(ctx, session.AgentType, session.SessionID, "dream", session.source())
		if err != nil {
			return nil, err
		}
		for {
			if ctx.Err() != nil {
				return nil, errors.New("dream history deadline exceeded")
			}
			if pages >= 128 {
				return out, nil
			}
			pages++
			events, done, err := h.readPage(ctx, snap, 100)
			if err != nil {
				return nil, err
			}
			for _, ev := range events {
				if ev.Text == nil || ev.at == nil || ev.at.Before(cutoff) || !ev.at.After(since) || ev.at.After(now) {
					continue
				}
				if ev.Kind == "user_prompt" && !c.Dream.IncludePrompts || (ev.Kind == "tool_input" || ev.Kind == "tool_output") && !c.Dream.IncludeToolBodies {
					continue
				}
				ex := DreamExcerpt{Source: DreamSource{SessionID: session.SessionID, Agent: session.AgentType, At: ev.at.UTC()}, Kind: ev.Kind, Text: *ev.Text}
				b, _ := json.Marshal(ex)
				if used+len(b)+1 > budget {
					return out, nil
				}
				out = append(out, ex)
				used += len(b) + 1
			}
			if done {
				break
			}
		}
	}
	return out, nil
}
