package channel

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/betterleaks/betterleaks/v2/config"
	"github.com/betterleaks/betterleaks/v2/report"
	"github.com/betterleaks/betterleaks/v2/scan"
	"github.com/betterleaks/betterleaks/v2/sources"
)

// Collected activity text (live streams, replay, queued deliveries and history
// pages) is scanned for secrets before it is retained or returned; explicit
// send/publish text is never scanned. Detection uses Betterleaks' default rules
// with the standard-library regexp engine: no network access, no CGO.
const (
	// redactionTimeout bounds how long one text may be scanned before it is
	// masked unscanned. Live records are at most a few KiB and scan in
	// microseconds; a 2 MiB history record takes well under a second, so the
	// budget only trips on pathological input or a stalled scanner.
	redactionTimeout = 5 * time.Second
	// redactedUnscanned replaces an entire text that could not be scanned.
	redactedUnscanned = "[REDACTED:unscanned]"
)

// secretFinding is one detected secret: the rule that matched and the byte
// span [start, end) of the secret value in the scanned text. A span outside
// the text marks a finding the scanner could not place; it fails closed.
type secretFinding struct {
	rule       string
	start, end int
}

// secretScanner finds secrets in one text. Implementations must be safe for
// concurrent use; the fake in tests injects errors and stalls.
type secretScanner interface {
	scan(ctx context.Context, text string) ([]secretFinding, error)
}

// betterleaksScanner adapts a shared Betterleaks scanner.
type betterleaksScanner struct{ s *scan.Scanner }

// fragmentSource yields one in-memory fragment.
type fragmentSource struct{ raw string }

func (f fragmentSource) Fragments(_ context.Context, yield sources.FragmentsFunc) error {
	return yield(sources.Fragment{Raw: f.raw}, nil)
}

func newBetterleaksScanner() (*betterleaksScanner, error) {
	// The scanner's own logger could carry fragment errors; discard it so no
	// text ever reaches stderr through the dependency.
	quiet := slog.New(slog.DiscardHandler)
	cfg, err := config.Default(config.WithLogger(quiet))
	if err != nil {
		return nil, err
	}
	s, err := scan.New(cfg, scan.WithLogger(quiet), scan.WithPrecompile())
	if err != nil {
		return nil, err
	}
	return &betterleaksScanner{s: s}, nil
}

func (b *betterleaksScanner) scan(ctx context.Context, text string) ([]secretFinding, error) {
	var out []secretFinding
	var lines []int // byte offset of each line start, computed on first finding
	_, err := b.s.Scan(ctx, fragmentSource{text}, func(f report.Finding) error {
		if lines == nil {
			lines = lineStarts(text)
		}
		out = append(out, locate(text, lines, f))
		return nil
	})
	return out, err
}

// lineStarts returns the byte offset of every line start, beginning with 0.
func lineStarts(text string) []int {
	starts := []int{0}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' && i+1 < len(text) {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// locate converts a finding's one-based line and byte-column location into the
// span of its secret value. The full match must reproduce exactly; otherwise
// the span is invalid and the caller masks the whole text.
func locate(text string, lines []int, f report.Finding) secretFinding {
	invalid := secretFinding{rule: f.RuleID, start: -1, end: -1}
	l := f.Location
	if f.DecodeDepth != 0 || l.StartLine < 1 || l.StartLine > len(lines) || l.EndLine < 1 || l.EndLine > len(lines) {
		return invalid
	}
	start, end := lines[l.StartLine-1]+l.StartColumn-1, lines[l.EndLine-1]+l.EndColumn
	if start < 0 || end > len(text) || start >= end || text[start:end] != f.Match.Full {
		return invalid
	}
	if v := f.Match.Value; v != "" {
		if i := strings.Index(text[start:end], v); i >= 0 {
			start, end = start+i, start+i+len(v)
		}
	}
	return secretFinding{rule: f.RuleID, start: start, end: end}
}

// redactor replaces detected secrets in activity text and fails closed: a
// scanner that is unavailable, errors, exceeds the time budget, or reports a
// finding it cannot place in the text masks the whole text instead.
type redactor struct {
	scanner secretScanner
	err     error // construction failure; every non-empty text is masked
	timeout time.Duration

	mu     sync.Mutex
	warned map[string]bool
}

func newRedactor(s secretScanner, err error, timeout time.Duration) *redactor {
	return &redactor{scanner: s, err: err, timeout: timeout, warned: map[string]bool{}}
}

// sharedRedactor builds the process-wide scanner on first use so commands that
// never collect activity (such as --version) pay nothing.
var sharedRedactor = sync.OnceValue(func() *redactor {
	s, err := newBetterleaksScanner()
	if err != nil {
		return newRedactor(nil, err, redactionTimeout)
	}
	return newRedactor(s, nil, redactionTimeout)
})

type scanResult struct {
	findings []secretFinding
	err      error
}

// redact returns text with every detected secret replaced by
// "[REDACTED:<rule_id>]". changed reports that the output differs from the
// input; failed reports that the text was masked without a completed scan.
func (r *redactor) redact(text string) (out string, changed, failed bool) {
	if text == "" {
		return text, false, false
	}
	if r.err != nil {
		return r.fail("scanner_unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	done := make(chan scanResult, 1)
	go func() {
		findings, err := r.scanner.scan(ctx, text)
		done <- scanResult{findings, err}
	}()
	var res scanResult
	select {
	case res = <-done:
	case <-ctx.Done():
		// The scan goroutine finishes on its own; the caller never waits past the budget.
		return r.fail("timeout")
	}
	if res.err != nil {
		if errors.Is(res.err, context.DeadlineExceeded) {
			return r.fail("timeout")
		}
		return r.fail("scan_error")
	}
	if len(res.findings) == 0 {
		return text, false, false
	}
	findings := append([]secretFinding(nil), res.findings...)
	for _, f := range findings {
		if f.start < 0 || f.end > len(text) || f.start >= f.end {
			return r.fail("unlocated_finding")
		}
	}
	// One pass over the text: overlapping spans merge into the earliest
	// finding's marker, so output size is linear in the input.
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].start < findings[j].start })
	var b strings.Builder
	b.Grow(len(text))
	pos := 0
	for i := 0; i < len(findings); {
		f := findings[i]
		end := f.end
		for i++; i < len(findings) && findings[i].start < end; i++ {
			end = max(end, findings[i].end)
		}
		b.WriteString(text[pos:f.start])
		b.WriteString("[REDACTED:" + ruleLabel(f.rule) + "]")
		pos = end
	}
	b.WriteString(text[pos:])
	return b.String(), true, false
}

// fail masks a text and records the reason once per process. Neither the text
// nor any finding is ever logged.
func (r *redactor) fail(reason string) (string, bool, bool) {
	r.mu.Lock()
	first := !r.warned[reason]
	r.warned[reason] = true
	r.mu.Unlock()
	if first {
		slog.Warn("activity_redaction_failed", "reason", reason)
	}
	return redactedUnscanned, true, true
}

// ruleLabel keeps markers to a safe ASCII subset of the rule identifier.
func ruleLabel(rule string) string {
	var b strings.Builder
	for _, c := range rule {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
			b.WriteRune(c)
		}
	}
	if b.Len() == 0 || b.Len() > 64 {
		return "secret"
	}
	return b.String()
}
