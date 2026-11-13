package channel

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"rhizome/internal/config"
	"rhizome/internal/localstate"
	"rhizome/internal/project"
)

// Dream digests (P1) are short, redacted summaries of candidate team knowledge
// that a user dream proposes from this host's own public sessions. They are
// reviewed locally and shared only with directly paired hosts under the serving
// host's [gateway.dream] policy. Received digests are untrusted, attributed to
// the authenticated host ID, and never forwarded.
const (
	DigestMaxEntries   = 16      // entries per digest
	DigestMaxText      = 512     // UTF-8 bytes per entry text
	DigestMaxBytes     = 8 << 10 // encoded digest
	DigestMaxSources   = 16      // sources per entry
	DigestDefaultTTL   = 7 * 24 * time.Hour
	DigestMaxTTL       = 30 * 24 * time.Hour
	digestMaxResponse  = 64 << 10 // one /v1/dream/digests response
	digestMaxStored    = 64       // local digests kept on disk
	digestMaxFile      = 16 << 10 // one stored digest file
	digestFutureLeeway = 5 * time.Minute
)

// Digest statuses. Pending digests wait for review; approved and published
// (auto mode) digests are eligible for serving; rejected ones never are.
const (
	DigestPending   = "pending"
	DigestApproved  = "approved"
	DigestRejected  = "rejected"
	DigestPublished = "published"
)

// DigestTypes are the entry kinds a digest may carry.
var DigestTypes = []string{"working_on", "decision", "gotcha", "blocker"}

var digestID = regexp.MustCompile(`^[0-9a-f]{16}$`)

// DigestSource names a local session an entry was derived from.
type DigestSource struct {
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
}

// DigestEntry is one redacted item of candidate team knowledge.
type DigestEntry struct {
	Type      string         `json:"type"`
	Text      string         `json:"text"`
	Project   string         `json:"project"`
	Sources   []DigestSource `json:"sources"`
	ExpiresAt time.Time      `json:"expires_at"`
}

// Digest is one stored local digest. Approved lists the 0-based entry indices
// selected for publishing in review mode.
type Digest struct {
	Version   int           `json:"version"`
	ID        string        `json:"id"`
	CreatedAt time.Time     `json:"created_at"`
	Status    string        `json:"status"`
	Approved  []int         `json:"approved,omitempty"`
	Entries   []DigestEntry `json:"entries"`
}

// PeerDigest is one digest as served to, and stored by, a paired host.
type PeerDigest struct {
	ID        string        `json:"id"`
	CreatedAt time.Time     `json:"created_at"`
	Entries   []DigestEntry `json:"entries"`
}

// NewDigestID returns a random digest identifier.
func NewDigestID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func digestType(t string) bool {
	for _, k := range DigestTypes {
		if t == k {
			return true
		}
	}
	return false
}

// digestText reports whether s is nonblank, bounded, valid UTF-8 without control characters.
func digestText(s string) bool {
	if strings.TrimSpace(s) == "" || len(s) > DigestMaxText || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func digestSourceValid(s DigestSource) bool {
	if s.SessionID == "" || len(s.SessionID) > 128 || !utf8.ValidString(s.SessionID) || strings.ContainsFunc(s.SessionID, unicode.IsControl) {
		return false
	}
	switch s.Agent {
	case "claude", "codex", "opencode", "pi":
		return true
	}
	return false
}

// ValidDigestEntry checks one entry's shape and bounds at now.
func ValidDigestEntry(e DigestEntry, now time.Time) bool {
	if !digestType(e.Type) || !digestText(e.Text) || !project.Valid(e.Project) || len(e.Sources) == 0 || len(e.Sources) > DigestMaxSources {
		return false
	}
	seen := map[DigestSource]bool{}
	for _, s := range e.Sources {
		if !digestSourceValid(s) || seen[s] {
			return false
		}
		seen[s] = true
	}
	return e.ExpiresAt.After(now) && !e.ExpiresAt.After(now.Add(DigestMaxTTL+digestFutureLeeway))
}

// RedactDigestEntry re-applies the shared secret filter to an entry and
// reports whether it is still valid afterwards.
func RedactDigestEntry(e DigestEntry, now time.Time) (DigestEntry, bool) {
	e.Text = RedactDream(e.Text)
	return e, ValidDigestEntry(e, now)
}

// ValidDigest checks a stored digest's envelope and entries.
func ValidDigest(d Digest, now time.Time) error {
	if d.Version != 1 || !digestID.MatchString(d.ID) || d.CreatedAt.IsZero() || d.CreatedAt.After(now.Add(digestFutureLeeway)) || len(d.Entries) == 0 || len(d.Entries) > DigestMaxEntries {
		return errors.New("invalid digest")
	}
	switch d.Status {
	case DigestPending, DigestApproved, DigestRejected, DigestPublished:
	default:
		return errors.New("invalid digest status")
	}
	seen := map[int]bool{}
	for _, i := range d.Approved {
		if i < 0 || i >= len(d.Entries) || seen[i] {
			return errors.New("invalid digest approval")
		}
		seen[i] = true
	}
	if d.Status == DigestApproved && len(d.Approved) == 0 {
		return errors.New("approved digest selects no entries")
	}
	for _, e := range d.Entries {
		// Stored entries may since have expired; shape is checked at creation time.
		if !ValidDigestEntry(e, minTime(now, d.CreatedAt)) {
			return errors.New("invalid digest entry")
		}
	}
	if b, _ := json.Marshal(d.Entries); len(b) > DigestMaxBytes {
		return errors.New("digest exceeds 8 KiB")
	}
	return nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// DigestStore is the private local digest directory, <runtime>/memory/digests.
// Writers hold the dream store lock.
type DigestStore struct{ Runtime string }

func (s DigestStore) dir() string { return filepath.Join(s.Runtime, "memory", "digests") }

func privateDir(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() || runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return errors.New("digest directory must be private and not a symlink")
	}
	return nil
}

func (s DigestStore) check() error {
	for _, p := range []string{s.Runtime, filepath.Join(s.Runtime, "memory"), s.dir()} {
		if err := privateDir(p); err != nil {
			return err
		}
	}
	return nil
}

// Save atomically writes one validated digest. The memory directory must
// already exist (the dream store creates it).
func (s DigestStore) Save(d Digest, now time.Time) error {
	if err := ValidDigest(d, now); err != nil {
		return err
	}
	if err := privateDir(filepath.Join(s.Runtime, "memory")); err != nil {
		return errors.New("unsafe memory directory")
	}
	if err := localstate.SecureDir(s.dir()); err != nil {
		return err
	}
	if err := s.check(); err != nil {
		return err
	}
	b, err := json.Marshal(d)
	if err != nil || len(b) > digestMaxFile {
		return errors.New("digest exceeds storage bound")
	}
	return gatewayWrite(filepath.Join(s.dir(), d.ID+".json"), d)
}

// Load returns valid stored digests, newest first. Invalid, oversized or
// symlinked files are skipped; at most digestMaxStored are read.
func (s DigestStore) Load(now time.Time) ([]Digest, error) {
	if err := s.check(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, errors.New("unsafe digest directory")
	}
	names, err := digestFiles(s.dir())
	if err != nil {
		return nil, err
	}
	var out []Digest
	for _, name := range names {
		var d Digest
		if readJSONBounded(filepath.Join(s.dir(), name), digestMaxFile, &d) != nil || d.ID+".json" != name || ValidDigest(d, now) != nil {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// Get loads one digest by ID.
func (s DigestStore) Get(id string, now time.Time) (Digest, error) {
	if !digestID.MatchString(id) {
		return Digest{}, errors.New("unknown digest id")
	}
	all, err := s.Load(now)
	if err != nil {
		return Digest{}, err
	}
	for _, d := range all {
		if d.ID == id {
			return d, nil
		}
	}
	return Digest{}, errors.New("unknown digest id")
}

// Prune removes digests whose entries have all expired and keeps at most
// digestMaxStored-1 others, so a new digest fits the bound.
func (s DigestStore) Prune(now time.Time) error {
	all, err := s.Load(now)
	if err != nil {
		return err
	}
	kept := 0
	for _, d := range all {
		live := false
		for _, e := range d.Entries {
			if e.ExpiresAt.After(now) {
				live = true
			}
		}
		if live && kept < digestMaxStored-1 {
			kept++
			continue
		}
		if err := os.Remove(filepath.Join(s.dir(), d.ID+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func digestFiles(dir string) ([]string, error) {
	f, err := os.Open(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(4 * digestMaxStored)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// readJSONBounded strictly decodes a private, regular file of at most limit bytes.
func readJSONBounded(path string, limit int64, out any) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	if err := localstate.PrivateFile(path); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return err
	}
	if int64(len(b)) > limit {
		return errors.New("file exceeds bound")
	}
	return strictJSON(b, out)
}

func strictJSON(b []byte, out any) error {
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.More() {
		return errors.New("trailing JSON")
	}
	return nil
}

// servableDigests selects what this host may serve to a paired host now. The
// policy is evaluated here, on every request: off serves nothing; review
// serves only approved entries; auto also serves digests published at
// creation. Digests older than the lookback, expired entries and entries
// citing a session that is no longer public are withheld, and every served
// text is redacted again. Only the local store is read, so digests received
// from paired hosts are never re-served. The encoded result is at most 64 KiB.
func servableDigests(runtime string, policy config.DreamSharing, now time.Time) []PeerDigest {
	if !policy.Enabled() {
		return nil
	}
	all, err := DigestStore{Runtime: runtime}.Load(now)
	if err != nil {
		return nil
	}
	cutoff := policy.Cutoff(now)
	out := []PeerDigest{}
	size := len(`{"version":1,"digests":[]}`)
	for _, d := range all {
		var pick []int
		switch {
		case d.Status == DigestApproved:
			pick = d.Approved
		case d.Status == DigestPublished && policy.Mode == config.DreamShareAuto:
			for i := range d.Entries {
				pick = append(pick, i)
			}
		default:
			continue
		}
		if d.CreatedAt.Before(cutoff) || d.CreatedAt.After(now.Add(digestFutureLeeway)) {
			continue
		}
		pd := PeerDigest{ID: d.ID, CreatedAt: d.CreatedAt.UTC()}
		for _, i := range pick {
			e, ok := RedactDigestEntry(d.Entries[i], now)
			if !ok || !digestSourcesPublic(runtime, e.Sources) {
				continue
			}
			pd.Entries = append(pd.Entries, e)
		}
		if len(pd.Entries) == 0 {
			continue
		}
		b, _ := json.Marshal(pd)
		if size+len(b)+1 > digestMaxResponse {
			break
		}
		size += len(b) + 1
		out = append(out, pd)
	}
	return out
}

// digestSourcesPublic applies the shared session-privacy predicate (#37) to
// every source: a private or unknown visibility record withholds the entry.
func digestSourcesPublic(runtime string, sources []DigestSource) bool {
	for _, s := range sources {
		if !sessionPublic(runtime, s.SessionID) {
			return false
		}
	}
	return true
}

// DigestSessionPublic reports whether a local session may contribute to a digest.
func DigestSessionPublic(runtime, session string) bool { return sessionPublic(runtime, session) }

// DigestProjectKey derives the project identity of a session's project path.
func DigestProjectKey(path string) string { return project.Key(path) }

// digestReply is the /v1/dream/digests wire format.
type digestReply struct {
	Version int          `json:"version"`
	Digests []PeerDigest `json:"digests"`
}

// parsePeerDigests strictly decodes and bounds a paired host's response,
// re-validating and re-redacting every entry. Anything malformed rejects the
// whole response.
func parsePeerDigests(body []byte, now time.Time) ([]PeerDigest, error) {
	if len(body) > digestMaxResponse {
		return nil, errors.New("digest response exceeds 64 KiB")
	}
	var r digestReply
	if err := strictJSON(body, &r); err != nil || r.Version != 1 || r.Digests == nil {
		return nil, errors.New("invalid digest response")
	}
	seen := map[string]bool{}
	for i := range r.Digests {
		d := &r.Digests[i]
		if !digestID.MatchString(d.ID) || seen[d.ID] || d.CreatedAt.IsZero() || len(d.Entries) == 0 || len(d.Entries) > DigestMaxEntries {
			return nil, errors.New("invalid peer digest")
		}
		seen[d.ID] = true
		if b, _ := json.Marshal(d.Entries); len(b) > DigestMaxBytes {
			return nil, errors.New("peer digest exceeds 8 KiB")
		}
		for j := range d.Entries {
			e, ok := RedactDigestEntry(d.Entries[j], now)
			if !ok {
				return nil, errors.New("invalid peer digest entry")
			}
			d.Entries[j] = e
		}
	}
	return r.Digests, nil
}

// peerDigestFile is the private record of one paired host's digests,
// <runtime>/gateway/dream/<host_id>.json. HostID is the authenticated host.
type peerDigestFile struct {
	Version    int          `json:"version"`
	HostID     string       `json:"host_id"`
	ReceivedAt time.Time    `json:"received_at"`
	Digests    []PeerDigest `json:"digests"`
}

// ReceivedDigest is one untrusted entry received from a paired host.
type ReceivedDigest struct {
	HostID     string      `json:"host_id"`
	DigestID   string      `json:"digest_id"`
	CreatedAt  time.Time   `json:"created_at"`
	ReceivedAt time.Time   `json:"received_at"`
	Entry      DigestEntry `json:"entry"`
}

func peerDigestDir(runtime string) string { return filepath.Join(runtime, "gateway", "dream") }

// PeerDigests lists entries received from paired hosts while the gateway
// service runs. Files are present only while the link and trust exist; expired
// entries are skipped. Every value is untrusted peer data.
func PeerDigests(runtime string, now time.Time) []ReceivedDigest {
	if runtime == "" || !gatewayRunning(runtime) {
		return nil
	}
	dir := peerDigestDir(runtime)
	if privateDir(dir) != nil {
		return nil
	}
	names, err := digestFiles(dir)
	if err != nil {
		return nil
	}
	var out []ReceivedDigest
	for _, name := range names {
		var f peerDigestFile
		if readJSONBounded(filepath.Join(dir, name), 2*digestMaxResponse, &f) != nil || f.Version != 1 || !validHostID(f.HostID) || f.HostID+".json" != name {
			continue
		}
		for _, d := range f.Digests {
			for _, e := range d.Entries {
				if ValidDigestEntry(e, now) {
					out = append(out, ReceivedDigest{HostID: f.HostID, DigestID: d.ID, CreatedAt: d.CreatedAt, ReceivedAt: f.ReceivedAt, Entry: e})
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Entry.Project != out[j].Entry.Project {
			return out[i].Entry.Project < out[j].Entry.Project
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}
