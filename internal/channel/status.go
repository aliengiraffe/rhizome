package channel

import (
	"fmt"
	"log/slog"
	"strings"

	pb "rhizome/proto"
)

// Session statuses shared by list_sessions and history live_status.
const (
	statusLive          = "live"           // one or more direct processes report the session
	statusRecent        = "recent"         // known only through passive observers or the catalog window
	statusNotDiscovered = "not_discovered" // history rows absent from current discovery
)

// liveInfo counts the current discovery entries claiming one session_id.
type liveInfo struct {
	direct, observed, hosts int
	self                    bool
}

// liveInfos tallies logical peers per session_id. hosts counts distinct
// owners (this device plus each paired host) so cross-host collisions stay
// visible to callers that must fail closed.
func liveInfos(peers []*pb.Identity, self string) map[string]liveInfo {
	out := map[string]liveInfo{}
	owners := map[string]map[string]bool{}
	for _, p := range peers {
		if p.SessionId == "" {
			continue
		}
		li := out[p.SessionId]
		if p.ObservationOnly {
			li.observed++
		} else {
			li.direct++
		}
		li.self = li.self || p.NodeId == self
		if owners[p.SessionId] == nil {
			owners[p.SessionId] = map[string]bool{}
		}
		owners[p.SessionId][p.GatewayHost] = true
		li.hosts = len(owners[p.SessionId])
		out[p.SessionId] = li
	}
	return out
}

// status derives the user-facing status and whether exactly one route
// exists. Several direct processes or hosts still read as live, but they are
// not routable: Node.resolve fails closed on them, so callers must not offer
// them as delivery capable. With debug the underlying substatus is appended
// in parentheses, e.g. "live (direct x2)" or "recent (observer)".
func (li liveInfo) status(debug bool) (string, bool) {
	var status string
	var parts []string
	switch {
	case li.direct > 0:
		status = statusLive
		parts = append(parts, counted("direct", li.direct))
		if li.observed > 0 {
			parts = append(parts, counted("observer", li.observed))
		}
		if li.self {
			parts = append(parts, "self")
		}
	case li.observed > 0:
		status = statusRecent
		parts = append(parts, counted("observer", li.observed))
	default:
		return statusNotDiscovered, false
	}
	if li.hosts > 1 {
		parts = append(parts, counted("hosts", li.hosts))
	}
	routable := li.direct == 1 && li.hosts == 1
	if debug {
		status += " (" + strings.Join(parts, ", ") + ")"
	}
	return status, routable
}

// recentCatalogStatus is the status of a recent-only row from a history catalog.
func recentCatalogStatus(debug bool) string {
	if debug {
		return statusRecent + " (catalog)"
	}
	return statusRecent
}

func counted(label string, n int) string {
	if n > 1 {
		return fmt.Sprintf("%s x%d", label, n)
	}
	return label
}

// LogOptions selects the diagnostic level: debug enables slog.LevelDebug.
// Debug records carry counters and metadata only, never transcript content, session names or tokens.
func LogOptions(debug bool) *slog.HandlerOptions {
	if debug {
		return &slog.HandlerOptions{Level: slog.LevelDebug}
	}
	return nil
}
