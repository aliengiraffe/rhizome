package dream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"rhizome/internal/channel"
	"rhizome/internal/config"
	"rhizome/internal/oai"
)

// Client is the existing OAI client or a synthetic test implementation.
type Client interface {
	Status() (oai.State, error)
	Respond(context.Context, oai.Request) (string, error)
}

// Run performs at most one model call. Disabled policy and missing sign-in are
// checked before reading history or creating any memory state.
func Run(ctx context.Context, c config.Config, client Client, dry bool, out, metadata io.Writer) error {
	return run(ctx, c, client, dry, out, metadata, channel.DreamHistory)
}

func run(ctx context.Context, c config.Config, client Client, dry bool, out, metadata io.Writer, history func(context.Context, config.Config, channel.DreamProgress, time.Time, int) (channel.DreamBatch, error)) error {
	if !c.Dream.Enabled {
		return errors.New("dream disabled; explicitly set [dream].enabled = true in the configuration file to send redacted local history to OpenAI")
	}
	if err := c.Dream.Validate(); err != nil {
		return err
	}
	state, err := client.Status()
	if err != nil || !state.SignedIn() {
		return errors.New("dream requires sign-in; run rhizome auth oai")
	}
	store := Store{Runtime: c.RuntimeDir}
	unlock, err := store.Lock(dry)
	if err != nil {
		return err
	}
	defer unlock()
	revs, err := store.Load()
	if err != nil {
		return err
	}
	items, progress := current(revs)
	now := time.Now().UTC()
	memory, _ := json.Marshal(items)
	// Reserve framing and instructions as well as memory under the same cap.
	budget := c.Dream.MaxInputBytes - len(Instructions) - len(memory) - 512
	if budget < 0 {
		return errors.New("dream input budget too small for current memory")
	}
	batch, err := history(ctx, c, progress, now, budget)
	if err != nil {
		return err
	}
	excerpts := batch.Excerpts
	if len(excerpts) == 0 {
		if batch.Partial != "" && len(batch.Scanned) == 0 {
			return errors.New("dream history bound reached without progress; increase max_input_bytes or narrow lookback")
		}
		next := advance(progress, batch, nil, now, c.Dream.Cutoff(now))
		if !dry && len(batch.Scanned) > 0 {
			// Record scanned-but-empty sessions so later runs do not re-read them.
			if err = store.Commit(revs, Revision{At: now, Since: next.Since, Cursors: next.Cursors, Operations: []Operation{}, Items: items}); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(metadata, "dream: 0 eligible events; no model call; remaining_sessions=%d\n", batch.Remaining)
		return err
	}
	assemble := func(events []channel.DreamExcerpt) (oai.Request, []byte) {
		input, _ := json.Marshal(struct {
			Memory json.RawMessage        `json:"memory"`
			Events []channel.DreamExcerpt `json:"events"`
		}{memory, events})
		request := oai.Request{Model: c.Dream.Model, Instructions: Instructions, Input: []oai.Message{{Role: "user", Content: string(input)}}}
		raw, _ := json.Marshal(request)
		return request, raw
	}
	// Account for JSON escaping in the actual envelope, not just excerpt text.
	// Reserve 256 bytes for store/stream and an automatically selected model.
	low, high := 0, len(excerpts)
	for low < high {
		mid := (low + high + 1) / 2
		_, raw := assemble(excerpts[:mid])
		if len(raw)+256 <= c.Dream.MaxInputBytes {
			low = mid
		} else {
			high = mid - 1
		}
	}
	if low == 0 {
		return errors.New("dream input budget too small for an event")
	}
	if low < len(excerpts) {
		// Sessions cut by the envelope limit are not complete; the last kept one is partial.
		cut := map[string]bool{}
		for _, ex := range excerpts[low:] {
			cut[channel.DreamKey(ex.Source.Agent, ex.Source.SessionID)] = true
		}
		for key := range cut {
			if _, ok := batch.Scanned[key]; ok {
				delete(batch.Scanned, key)
				batch.Remaining++
			}
		}
		last := excerpts[low-1].Source
		if key := channel.DreamKey(last.Agent, last.SessionID); cut[key] {
			batch.Partial = key
		}
	}
	excerpts = excerpts[:low]
	excerpts = holdTies(excerpts, batch.Partial)
	if len(excerpts) == 0 {
		return errors.New("dream input budget too small for a complete timestamp group; increase max_input_bytes")
	}
	request, raw := assemble(excerpts)
	// Scan the fully assembled JSON request and fail closed if source metadata
	// or framing cannot survive redaction.
	filtered := channel.RedactDream(string(raw))
	var safe oai.Request
	if json.Unmarshal([]byte(filtered), &safe) != nil {
		return errors.New("dream request redaction invalidated framing")
	}
	var data struct {
		Memory []Item                 `json:"memory"`
		Events []channel.DreamExcerpt `json:"events"`
	}
	if len(safe.Input) != 1 || safe.Instructions != Instructions || safe.Model != request.Model || safe.Input[0].Role != "user" || json.Unmarshal([]byte(safe.Input[0].Content), &data) != nil || len(data.Events) != len(excerpts) {
		return errors.New("dream request redaction invalidated metadata")
	}
	for i := range excerpts {
		if data.Events[i].Source != excerpts[i].Source {
			return errors.New("dream source metadata was redacted")
		}
	}
	if len(filtered)+256 > c.Dream.MaxInputBytes {
		return errors.New("redacted dream request exceeds input budget")
	}
	sessions := map[string]bool{}
	for _, ex := range excerpts {
		sessions[ex.Source.Agent+":"+ex.Source.SessionID] = true
	}
	if _, err = fmt.Fprintf(metadata, "dream: input_bytes=%d sessions=%d remaining_sessions=%d\n", len(filtered), len(sessions), batch.Remaining); err != nil {
		return err
	}
	reply, err := client.Respond(ctx, safe)
	if err != nil {
		return err
	}
	proposal, next, err := Validate(reply, items, data.Events, now)
	if err != nil {
		return err
	}
	if dry {
		if err := json.NewEncoder(out).Encode(proposal); err != nil {
			return err
		}
		return digest(ctx, c, client, true, out, metadata, data.Events, batch.Projects, next, now)
	}
	a, u, r := counts(items, next)
	cursor := advance(progress, batch, excerpts, now, c.Dream.Cutoff(now))
	err = store.Commit(revs, Revision{At: now, Since: cursor.Since, Cursors: cursor.Cursors, Operations: proposal.Operations, Items: next, Added: a, Updated: u, Removed: r})
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(metadata, "dream: revision=%d added=%d updated=%d removed=%d\n", len(revs)+1, a, u, r); err != nil {
		return err
	}
	// The memory revision stands even if the separate digest step fails.
	if err := digest(ctx, c, client, false, out, metadata, data.Events, batch.Projects, next, now); err != nil {
		return fmt.Errorf("memory revision %d committed; digest not created: %w", len(revs)+1, err)
	}
	return nil
}

// holdTies drops the partial session's trailing events that share the last
// kept timestamp: the cursor resumes strictly after it, so a group split by the
// budget is resent whole next run instead of partly skipped.
func holdTies(excerpts []channel.DreamExcerpt, partial string) []channel.DreamExcerpt {
	if partial == "" || len(excerpts) == 0 {
		return excerpts
	}
	last := excerpts[len(excerpts)-1].Source
	if channel.DreamKey(last.Agent, last.SessionID) != partial {
		return excerpts
	}
	n := len(excerpts)
	for n > 0 {
		s := excerpts[n-1].Source
		if channel.DreamKey(s.Agent, s.SessionID) != partial || !s.At.Equal(last.At) {
			break
		}
		n--
	}
	return excerpts[:n]
}

// advance returns progress after sending excerpts: fully scanned sessions are
// done through their last activity, the partial one through its last sent
// event. Cursors at or before the lookback cutoff are pruned; they cannot
// select anything again.
func advance(prev channel.DreamProgress, batch channel.DreamBatch, sent []channel.DreamExcerpt, now, cutoff time.Time) channel.DreamProgress {
	next := channel.DreamProgress{Since: prev.Since, Cursors: map[string]time.Time{}}
	for k, v := range prev.Cursors {
		next.Cursors[k] = v
	}
	for k, v := range batch.Scanned {
		next.Cursors[k] = v
	}
	if batch.Partial != "" {
		for _, ex := range sent {
			if channel.DreamKey(ex.Source.Agent, ex.Source.SessionID) == batch.Partial && ex.Source.At.After(next.Cursors[batch.Partial]) {
				next.Cursors[batch.Partial] = ex.Source.At
			}
		}
	}
	for k, v := range next.Cursors {
		if !v.After(cutoff) || v.After(now) {
			delete(next.Cursors, k)
		}
	}
	if len(next.Cursors) == 0 {
		next.Cursors = nil
	}
	return next
}
