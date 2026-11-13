package channel

import (
	"context"
	"strings"
	"time"
)

// Hook delivery (claude.delivery = "hooks") serves Claude Code accounts whose
// organization policy discards notifications/claude/channel: the notification
// is fire-and-forget, so Rhizome cannot tell when Claude drops it. In this mode
// the delivery loop holds each selected event in a bounded inbox, wrapped in
// the untrusted-peer envelope, and the next main-thread UserPromptSubmit or
// PreToolUse observe_session hook returns the held events as
// hookSpecificOutput.additionalContext. Events leave the inbox only in a
// response produced while the hook request is live and within
// hookDeliveryBudget, so a hook that runs into Claude's five-second timeout
// (whose output Claude discards) leaves them for the next hook.
const (
	hookInboxLimit     = 64 * 1024
	hookDeliveryBudget = 3 * time.Second
)

// holdForHook is the notify function for hook delivery. A full inbox reports
// the destination unavailable, so the delivery loop retains the event in its
// bounded queue instead of counting it written.
func (n *Node) holdForHook(_ context.Context, text string, meta map[string]string) error {
	envelope := peerEnvelope(text, meta)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.hookInboxBytes+len(envelope) > hookInboxLimit {
		return errDestinationUnavailable
	}
	n.hookInbox = append(n.hookInbox, envelope)
	n.hookOrigins = append(n.hookOrigins, meta["session_id"])
	n.hookInboxBytes += len(envelope)
	return nil
}

// hookOutput drains held events into Claude hook JSON for event. It returns an
// empty object for other events, in channel mode, and when the hook's context
// is done or its budget since started is spent; held events then stay queued.
func (n *Node) hookOutput(ctx context.Context, event string, started time.Time) map[string]any {
	if event != "UserPromptSubmit" && event != "PreToolUse" {
		return map[string]any{}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.publicLocked() || !n.hookDelivery || len(n.hookInbox) == 0 || ctx.Err() != nil || time.Since(started) >= hookDeliveryBudget {
		return map[string]any{}
	}
	held := n.hookInbox
	n.hookInbox, n.hookInboxBytes, n.hookOrigins = nil, 0, nil
	n.stats.HookInjected += uint64(len(held))
	return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": event, "additionalContext": strings.Join(held, "\n\n")}}
}
