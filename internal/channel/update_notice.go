package channel

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"rhizome/internal/release"
	"rhizome/internal/update"
)

// defaultNoticeRefresh is how often a live node re-reads the observer's
// health file to compare the observer's version with its own build.
const defaultNoticeRefresh = 30 * time.Second

// maxHealthBytes bounds the observer health read; the file is a few hundred
// bytes in practice.
const maxHealthBytes = 64 * 1024

// updateNoticeKind is the transport metadata kind of the restart notice.
const updateNoticeKind = "update_notice"

// Stable outcome codes of an observer version check, logged with versions only.
const (
	noticeNewer           = "newer"
	noticeNotNewer        = "not_newer"
	noticeObserverLocal   = "observer_local_build"
	noticeHealthMissing   = "health_unavailable"
	noticeHealthMalformed = "health_malformed"
	noticeDelivered       = "delivered"
	noticeDeliveryFailed  = "delivery_failed"
)

// updateNotice tells one bound session, once per observer version, that the
// observer service now runs a newer release than this MCP process. It is a
// Rhizome system notice: it never enters activity, retention, gossip, the
// gateway or the history catalog, and it bypasses peer-feed pause, batching
// and rate limiting. Only the owning delivery goroutine touches it.
type updateNotice struct {
	self      release.Version
	enabled   bool
	interval  time.Duration
	next      time.Time
	pending   string // observer version awaiting delivery
	announced string // last observer version announced to the session
	last      string // last logged check outcome, so each change logs once
}

// newUpdateNotice enables the notice only for a release build with the
// [update].notify_sessions policy on; dev and snapshot builds never notify.
func newUpdateNotice(o Options, interval time.Duration) *updateNotice {
	u := &updateNotice{interval: interval}
	if u.interval <= 0 {
		u.interval = defaultNoticeRefresh
	}
	if !o.UpdateNotice {
		return u
	}
	u.self, u.enabled = update.ParseRunning(o.Version)
	return u
}

// observerUpgrade reads the observer's version from <runtime>/observer-health.json
// and reports it with a stable outcome code. Only a parsed release version is
// ever returned; unparseable text never leaves this function.
func observerUpgrade(runtime string, self release.Version) (string, string) {
	f, err := os.Open(filepath.Join(runtime, "observer-health.json"))
	if err != nil {
		return "", noticeHealthMissing
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxHealthBytes+1))
	if err != nil {
		return "", noticeHealthMissing
	}
	var health struct {
		Version string `json:"version"`
	}
	if len(b) > maxHealthBytes || json.Unmarshal(b, &health) != nil {
		return "", noticeHealthMalformed
	}
	v, ok := update.ParseRunning(health.Version)
	if !ok {
		return "", noticeObserverLocal
	}
	if update.Compare(v, self) <= 0 {
		return v.Version, noticeNotNewer
	}
	return v.Version, noticeNewer
}

// poll re-reads the observer version when the refresh interval has elapsed
// and arms a notice for a newer observer version not yet announced.
func (u *updateNotice) poll(runtime string, now time.Time) {
	if !u.enabled || now.Before(u.next) {
		return
	}
	u.next = now.Add(u.interval)
	observer, outcome := observerUpgrade(runtime, u.self)
	switch {
	case outcome == noticeNewer && observer != u.announced:
		u.pending = observer
	case outcome != noticeNewer:
		u.pending = "" // the observer moved back before the session was told
	}
	if key := outcome + " " + observer; key != u.last {
		u.last = key
		slog.Debug("update_notice_check", "from", u.self.Version, "to", observer, "outcome", outcome)
	}
}

// message returns the pending notice text and its transport metadata. The
// metadata carries no peer session, node or host: the notice is from Rhizome.
func (u *updateNotice) message() (string, map[string]string) {
	text := fmt.Sprintf("Rhizome was updated to v%s; this session still runs v%s. Restart this coding session to load the new version.", u.pending, u.self.Version)
	return text, map[string]string{"kind": updateNoticeKind, "from": u.self.Version, "to": u.pending}
}

// finish records a delivery attempt. An unavailable destination keeps the
// notice pending; any other result announces the version so it never repeats.
func (u *updateNotice) finish(err error) {
	if errors.Is(err, errDestinationUnavailable) {
		return
	}
	outcome := noticeDelivered
	if err != nil {
		outcome = noticeDeliveryFailed
	}
	slog.Info("update_notice_delivered", "from", u.self.Version, "to", u.pending, "outcome", outcome)
	u.announced, u.pending = u.pending, ""
}
