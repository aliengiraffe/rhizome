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

// DreamProgress records how far dreaming has read local history. Sessions with
// a cursor resume after it; others start after Since (and the lookback). Runs
// therefore drain history that did not fit one input budget instead of skipping it.
type DreamProgress struct {
	Since   time.Time            `json:"since"`
	Cursors map[string]time.Time `json:"cursors,omitempty"`
}

// DreamKey is the progress key of one local session.
func DreamKey(agent, sessionID string) string { return agent + ":" + sessionID }

// DreamBatch is one bounded history selection.
type DreamBatch struct {
	Excerpts []DreamExcerpt
	// Scanned maps sessions read to their end to the cursor that marks them done.
	Scanned map[string]time.Time
	// Partial is the session cut by the budget, page or deadline bound, if any.
	Partial string
	// Remaining counts eligible sessions not read to their end in this batch.
	Remaining int
	// Projects maps session keys with selected events to their project
	// identity. It is filled only when [gateway.dream] is enabled.
	Projects map[string]string
}

// DreamHistory reads only the local catalog and existing confined snapshots.
// Sessions with unread events are selected oldest activity first so repeated
// runs consolidate history chronologically; events within each are read from
// byte zero in source order. A 30-second deadline, 64 sessions, 128 pages and
// the input budget bound work.
func DreamHistory(ctx context.Context, c config.Config, progress DreamProgress, now time.Time, budget int) (DreamBatch, error) {
	var batch DreamBatch
	if !c.Dream.Enabled {
		return batch, errors.New("dream disabled; enable [dream].enabled in the configuration file")
	}
	if err := c.Dream.Validate(); err != nil {
		return batch, err
	}
	if budget < 0 || budget > c.Dream.MaxInputBytes {
		return batch, errors.New("invalid dream history budget")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	h := newHistoryService(historyScope{Runtime: c.RuntimeDir, ClaudeRoot: c.Claude.ProjectsDir, CodexRoot: c.Codex.SessionsDir, OpenCodeRoot: c.OpenCode.DataDir, PiRoot: c.Pi.SessionsDir})
	h.details = c.Dream.IncludePrompts || c.Dream.IncludeToolBodies
	h.peerActivity = config.PeerActivityNone
	cat, _, err := h.catalog.loadWithStatus(false)
	if err != nil {
		return batch, err
	}
	cutoff := c.Dream.Cutoff(now)
	floor := func(s *historySession) time.Time {
		f := progress.Since
		if cur, ok := progress.Cursors[DreamKey(s.AgentType, s.SessionID)]; ok && cur.After(f) {
			f = cur
		}
		return f
	}
	var sessions []*historySession
	for _, s := range cat.sessions() {
		if s.LastActivityAt == nil || s.LastActivityAt.Before(cutoff) || !s.LastActivityAt.After(floor(s)) || s.LastActivityAt.After(now) || s.source() == nil || (s.Availability != "available" && s.Availability != "partial") {
			continue
		}
		sessions = append(sessions, s)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].LastActivityAt.Equal(*sessions[j].LastActivityAt) {
			return sessions[i].AgentType+sessions[i].SessionID < sessions[j].AgentType+sessions[j].SessionID
		}
		return sessions[i].LastActivityAt.Before(*sessions[j].LastActivityAt)
	})
	batch.Remaining = len(sessions)
	batch.Scanned = map[string]time.Time{}
	if c.Gateway.Dream.Enabled() {
		batch.Projects = map[string]string{}
	}
	if len(sessions) > 64 {
		sessions = sessions[:64]
	}
	used, pages := 2, 0
	for _, session := range sessions {
		key := DreamKey(session.AgentType, session.SessionID)
		since := floor(session)
		if batch.Projects != nil && session.ProjectPath != "" {
			batch.Projects[key] = DigestProjectKey(session.ProjectPath)
		}
		snap, err := h.openSnapshot(ctx, session.AgentType, session.SessionID, "dream", session.source())
		if err != nil {
			return DreamBatch{}, err
		}
		for {
			if ctx.Err() != nil {
				batch.Partial = key
				return batch, nil
			}
			if pages >= 128 {
				batch.Partial = key
				return batch, nil
			}
			pages++
			events, done, err := h.readPage(ctx, snap, 100)
			if err != nil {
				return DreamBatch{}, err
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
					batch.Partial = key
					return batch, nil
				}
				batch.Excerpts = append(batch.Excerpts, ex)
				used += len(b) + 1
			}
			if done {
				break
			}
		}
		batch.Scanned[key] = session.LastActivityAt.UTC()
		batch.Remaining--
	}
	return batch, nil
}
