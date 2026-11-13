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

func run(ctx context.Context, c config.Config, client Client, dry bool, out, metadata io.Writer, history func(context.Context, config.Config, time.Time, time.Time, int) ([]channel.DreamExcerpt, error)) error {
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
	items, since := current(revs)
	now := time.Now().UTC()
	memory, _ := json.Marshal(items)
	// Reserve framing and instructions as well as memory under the same cap.
	budget := c.Dream.MaxInputBytes - len(Instructions) - len(memory) - 512
	if budget < 0 {
		return errors.New("dream input budget too small for current memory")
	}
	excerpts, err := history(ctx, c, since, now, budget)
	if err != nil {
		return err
	}
	if len(excerpts) == 0 {
		_, err = fmt.Fprintln(metadata, "dream: 0 eligible events; no model call")
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
	excerpts = excerpts[:low]
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
	if _, err = fmt.Fprintf(metadata, "dream: input_bytes=%d sessions=%d\n", len(filtered), len(sessions)); err != nil {
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
		return json.NewEncoder(out).Encode(proposal)
	}
	a, u, r := counts(items, next)
	err = store.Commit(revs, Revision{At: now, Since: now, Operations: proposal.Operations, Items: next, Added: a, Updated: u, Removed: r})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(metadata, "dream: revision=%d added=%d updated=%d removed=%d\n", len(revs)+1, a, u, r)
	return err
}
