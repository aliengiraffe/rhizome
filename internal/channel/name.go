package channel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// maxSessionName bounds every advertised session name in UTF-8 bytes. Explicit
// input above the limit is rejected; native titles and fallbacks are cut on a
// rune boundary so the gossip metadata budget stays predictable.
const maxSessionName = 96

// nameState holds the inputs to session-name resolution. Explicit overrides win,
// then the agent's native title (a user-chosen title over an automatic one),
// then a deterministic fallback. Names are labels and never routing keys.
type nameState struct {
	explicit string
	custom   string // native user-chosen title (Claude custom-title, Codex thread name)
	auto     string // native automatic title (Claude ai-title)
}

// cleanName turns arbitrary text into a single-line label: surrounding
// whitespace is trimmed, control and line/paragraph separators become spaces,
// runs of whitespace collapse, and the result is bounded on a rune boundary.
func cleanName(s string) string { return boundName(normalizeName(s)) }

// normalizeName applies every cleanName rule except the byte bound.
func normalizeName(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// boundName cuts s to maxSessionName bytes without splitting a UTF-8 sequence.
func boundName(s string) string {
	if len(s) <= maxSessionName {
		return s
	}
	cut := maxSessionName
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut])
}

// trimRune removes the last rune of s.
func trimRune(s string) string {
	_, size := utf8.DecodeLastRuneInString(s)
	return s[:len(s)-size]
}

// fallbackName is the deterministic label for a bound session: the project
// directory label, the agent type and a short prefix of the exact session ID. It
// is stable across process restarts for the same session and project.
func fallbackName(display, agent, session string) string {
	if display == "" {
		display = "session"
	}
	if agent == "" {
		agent = "claude"
	}
	short := session
	if len(short) > 8 {
		short = short[:8]
	}
	return boundName(display + " · " + agent + " · " + short)
}

// resolveNameLocked recomputes identity.session_name from the current state and
// reports whether it changed. Unbound nodes never carry a name. Caller holds n.mu.
func (n *Node) resolveNameLocked() bool {
	var name string
	if n.identity.SessionId != "" {
		switch {
		case n.names.explicit != "":
			name = n.names.explicit
		case n.names.custom != "":
			name = n.names.custom
		case n.names.auto != "":
			name = n.names.auto
		default:
			name = fallbackName(n.identity.DisplayName, n.identity.AgentType, n.identity.SessionId)
		}
	}
	if n.identity.SessionName == name {
		return false
	}
	n.identity.SessionName = name
	return true
}

// nameSourceLocked reports which input produced the effective name.
func (n *Node) nameSourceLocked() string {
	switch {
	case n.names.explicit != "":
		return "explicit"
	case n.names.custom != "" || n.names.auto != "":
		return "native"
	default:
		return "fallback"
	}
}

// advertise refreshes gossip metadata after a name-only change. Binding
// generation, streams, subscriptions and queues are untouched. Must be called
// without n.mu held: memberlist calls NodeMeta synchronously.
func (n *Node) advertise() {
	if n.members == nil {
		return
	}
	if err := n.members.UpdateNode(time.Second); err != nil {
		slog.Warn("discovery_update", "error", err)
	}
}

// setNativeTitle records a verified native title for the given binding
// generation. Delayed metadata from a retired binding is ignored. An empty title
// is a confirmed removal; callers must not call this on transient read failures.
func (n *Node) setNativeTitle(gen uint64, custom bool, title string) {
	title = cleanName(title)
	n.mu.Lock()
	if n.identity.Generation != gen || n.identity.SessionId == "" {
		n.mu.Unlock()
		return
	}
	if custom {
		n.names.custom = title
	} else {
		n.names.auto = title
	}
	changed := n.resolveNameLocked()
	n.mu.Unlock()
	if changed {
		n.advertise()
		slog.Info("session_name", "source", "native", "generation", gen)
	}
}

// setSessionName applies an explicit override for the caller's bound session
// and returns the effective name with its source. Empty or whitespace-only
// input clears the override.
func (n *Node) setSessionName(name string) (map[string]string, error) {
	cleaned := normalizeName(name)
	if len(cleaned) > maxSessionName {
		return nil, fmt.Errorf("session name exceeds %d UTF-8 bytes after whitespace normalization", maxSessionName)
	}
	n.gate.Lock()
	defer n.gate.Unlock()
	n.mu.Lock()
	if n.identity.SessionId == "" {
		n.mu.Unlock()
		return nil, errUnbound
	}
	session, runtime := n.identity.SessionId, n.runtime
	n.mu.Unlock()
	if err := storeOverride(runtime, session, cleaned); err != nil {
		return nil, err
	}
	n.mu.Lock()
	if n.identity.SessionId != session {
		n.mu.Unlock()
		return nil, errors.New("session binding changed; retry")
	}
	n.names.explicit = cleaned
	changed := n.resolveNameLocked()
	out := map[string]string{"session_id": session, "session_name": n.identity.SessionName, "source": n.nameSourceLocked()}
	n.mu.Unlock()
	if changed {
		n.advertise()
		slog.Info("session_name", "source", out["source"], "session_id", session)
	}
	return out, nil
}

// Overrides live in <runtime>/names, one private file per exact session ID.
// The key is hashed so the file name is fixed-length and never a path fragment.
func overridePath(runtime, session string) string {
	sum := sha256.Sum256([]byte(session))
	return filepath.Join(runtime, "names", hex.EncodeToString(sum[:16]))
}

type overrideRecord struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
}

func storeOverride(runtime, session, name string) error {
	if runtime == "" {
		return errors.New("runtime directory not initialized")
	}
	path := overridePath(runtime, session)
	if name == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := secureDir(filepath.Dir(path)); err != nil {
		return err
	}
	raw, _ := json.Marshal(overrideRecord{SessionID: session, Name: name})
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// loadOverride returns the persisted explicit name for session, or "" when none
// is stored or the record does not belong to that exact session.
func loadOverride(runtime, session string) string {
	if runtime == "" {
		return ""
	}
	raw, err := os.ReadFile(overridePath(runtime, session))
	if err != nil {
		return ""
	}
	var r overrideRecord
	if json.Unmarshal(raw, &r) != nil || r.SessionID != session {
		return ""
	}
	return cleanName(r.Name)
}
