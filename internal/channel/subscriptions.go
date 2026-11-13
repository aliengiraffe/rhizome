package channel

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"rhizome/internal/config"
	pb "rhizome/proto"
)

type subscribeInput struct {
	SessionID    string   `json:"session_id"`
	Mode         string   `json:"mode,omitempty" jsonschema:"full, markers, tools, text, or digest"`
	Kinds        []string `json:"kinds,omitempty" jsonschema:"explicit event kinds; overrides mode"`
	IncludeText  *bool    `json:"include_text,omitempty" jsonschema:"false excludes assistant text summaries"`
	Tools        []string `json:"tools,omitempty" jsonschema:"exact tool name allowlist; omitted accepts all tools"`
	MaxPerMinute *int     `json:"max_per_minute,omitempty" jsonschema:"per-subscription event budget, 0-29 per minute"`
	Window       string   `json:"window,omitempty" jsonschema:"digest window, 1s to 24h"`
}

var subscriptionKinds = []string{"activity", "tool", "turn", "position", "gap", "paused", "prompt", "tool_input", "tool_output", "session-ended"}

// Native built-ins whose names are stable across the supported collectors.
// Custom/plugin tools remain available with the tool allowlist omitted.
var subscriptionTools = []string{"Bash", "Edit", "Write", "Read", "Glob", "Grep", "NotebookEdit", "WebFetch", "WebSearch", "Agent", "Task", "TodoWrite", "Skill", "bash", "read", "edit", "write", "grep", "find", "ls", "glob", "list", "patch", "webfetch", "websearch", "task", "todowrite", "todoread", "skill", "question", "commandExecution", "fileChange", "webSearch", "imageGeneration"}

type subscriptionPolicy struct {
	mode         string
	kinds        []string
	tools        []string
	excludeText  bool
	maxPerMinute int
	window       time.Duration
}

func resolveSubscription(defaults config.Subscriptions, previous *subscriptionPolicy, in subscribeInput) (subscriptionPolicy, error) {
	p := subscriptionPolicy{mode: defaults.DefaultMode, maxPerMinute: defaults.MaxPerMinute, window: 5 * time.Minute}
	if p.mode == "" {
		p.mode = "full"
	}
	if defaults.Window != "" {
		var err error
		p.window, err = config.SubscriptionWindow(defaults.Window)
		if err != nil {
			return p, err
		}
	}
	if previous != nil {
		p = *previous
	}
	if in.Mode != "" {
		p.mode = in.Mode
		// An explicit mode replaces a prior custom kind selection.
		p.kinds = nil
	}
	if err := config.ValidateSubscriptionMode(p.mode); err != nil {
		return p, err
	}
	if in.Kinds != nil {
		if len(in.Kinds) > len(subscriptionKinds) {
			return p, fmt.Errorf("too many kinds; valid values: %s", strings.Join(subscriptionKinds, ", "))
		}
		for _, kind := range in.Kinds {
			if !slices.Contains(subscriptionKinds, kind) {
				return p, fmt.Errorf("unknown kind %q; valid values: %s", kind, strings.Join(subscriptionKinds, ", "))
			}
		}
		p.kinds = append([]string{}, in.Kinds...)
	}
	if in.Tools != nil {
		if len(in.Tools) > len(subscriptionTools) {
			return p, fmt.Errorf("too many tools; valid values: %s", strings.Join(subscriptionTools, ", "))
		}
		for _, name := range in.Tools {
			if !slices.Contains(subscriptionTools, name) {
				return p, fmt.Errorf("unknown tool %q; valid values: %s (omit tools to accept custom tools)", name, strings.Join(subscriptionTools, ", "))
			}
		}
		p.tools = append([]string{}, in.Tools...)
	}
	if in.IncludeText != nil {
		p.excludeText = !*in.IncludeText
	}
	if in.MaxPerMinute != nil {
		if *in.MaxPerMinute < 0 || *in.MaxPerMinute > 29 {
			return p, fmt.Errorf("max_per_minute must be 0-29")
		}
		p.maxPerMinute = *in.MaxPerMinute
	}
	if in.Window != "" {
		if p.mode != "digest" {
			return p, fmt.Errorf("window requires mode digest")
		}
		var err error
		p.window, err = config.SubscriptionWindow(in.Window)
		if err != nil {
			return p, err
		}
	}
	return p, nil
}

func (p subscriptionPolicy) allows(e *pb.Event) bool {
	// Explicit communications are independent of activity subscriptions.
	if e.Kind == "direct" || e.Kind == "announcement" {
		return true
	}
	if e.Kind == "activity" && p.excludeText {
		return false
	}
	if p.kinds != nil {
		if !slices.Contains(p.kinds, e.Kind) {
			return false
		}
	} else {
		switch p.mode {
		case "markers":
			if !slices.Contains([]string{"turn", "gap", "paused", "position"}, e.Kind) {
				return false
			}
		case "tools":
			if e.Kind != "tool" {
				return false
			}
		case "text":
			if e.Kind != "activity" {
				return false
			}
		default:
			// Preserve legacy consumption of turn and position markers for full.
			if !p.excludeText && (e.Kind == "turn" || e.Kind == "position") {
				return false
			}
		}
	}
	if p.tools != nil && (e.Kind == "tool" || e.Kind == "tool_input" || e.Kind == "tool_output") {
		name, _, _ := strings.Cut(e.Text, ": ")
		if strings.HasPrefix(name, "Tool ") {
			_, name, _ = strings.Cut(e.Text, ": ")
		}
		return slices.Contains(p.tools, name)
	}
	return true
}

// admit counts only selected automatic events. A saturated subscription drops
// its own events without changing any peer's pause/re-arm state.
func (f *feed) admit(now time.Time) bool {
	if f.policy.maxPerMinute == 0 {
		return true
	}
	if f.budgetStart.IsZero() || !now.Before(f.budgetStart.Add(time.Minute)) {
		f.budgetStart, f.budgetUsed = now, 0
	}
	if f.budgetUsed >= f.policy.maxPerMinute {
		f.suppressed++
		return false
	}
	f.budgetUsed++
	return true
}

func (n *Node) digestPending(p pending) bool {
	f := n.feeds[p.subscription]
	return f != nil && f.token == p.token && f.policy.mode == "digest" && p.event.Kind != "announcement" && p.event.Kind != "direct"
}
