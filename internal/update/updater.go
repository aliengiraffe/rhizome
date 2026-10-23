package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"rhizome/internal/localstate"
	"rhizome/internal/release"
)

// Outcome codes recorded in the state file and logs.
const (
	OutcomeDevBuild        = "dev_build"
	OutcomeUpToDate        = "up_to_date"
	OutcomeNoRelease       = "no_release"
	OutcomeAvailable       = "update_available"
	OutcomeUpdated         = "updated"
	OutcomeInstalled       = "installed"
	OutcomeRestartPending  = "restart_pending"
	OutcomeRestartRequired = "restart_required"
	OutcomeRolledBack      = "rolled_back"
	OutcomeRestartFailed   = "restart_failed"
	OutcomeNotWritable     = "not_writable"
	OutcomeLocked          = "locked"
)

// LockFile serializes update attempts across processes sharing a runtime.
const LockFile = "update.lock"

const (
	maxManifest  = 64 << 10
	maxSignature = 4 << 10
	lockWait     = 30 * time.Second
	maxBackoff   = 3 // doublings
	defaultInit  = time.Minute
)

// ErrDeferred is returned by a Restarter when a detached finisher will
// complete the restart and record its outcome.
var ErrDeferred = errors.New("restart deferred to the update finisher")

// ErrRestartRequired is returned by a Restarter when the calling process must
// exit so its supervisor restarts it onto the new binary.
var ErrRestartRequired = errors.New("update installed; restart required")

// Restarter restarts the observer onto the installed binary and reports
// whether it came up healthy with the given version.
type Restarter interface {
	Restart(ctx context.Context, version string) error
}

// Options configures an Updater. Source, Keys and Runtime are required.
type Options struct {
	Runtime      string
	Version      string // version the running binary reports
	Commit       string
	GOOS, GOARCH string
	Source       Source
	Keys         []ed25519.PublicKey
	Verifier     BinaryVerifier // nil: no platform signature check
	VersionCheck func(ctx context.Context, bin, version, commit string) error
	Restarter    Restarter // nil: install only
	Interval     time.Duration
	Initial      time.Duration // delay before the first background check
	Target       string        // executable to replace; default Target()
	Logger       *slog.Logger
	Now          func() time.Time
}

// Candidate is an eligible release selected by Check.
type Candidate struct {
	Version release.Version
}

// Result summarizes one check or apply.
type Result struct {
	Outcome   string
	Available string // newest eligible version, when any
	Installed string // version installed by Apply
	Message   string // sanitized human-readable detail
}

// Updater checks for, verifies and installs releases.
type Updater struct {
	o      Options
	log    *slog.Logger
	arch   string
	target string
}

// New validates options and returns an Updater.
func New(o Options) (*Updater, error) {
	if o.Runtime == "" || !filepath.IsAbs(o.Runtime) {
		return nil, errors.New("update: absolute runtime directory required")
	}
	if o.Source == nil {
		return nil, errors.New("update: release source required")
	}
	if o.GOOS == "" || o.GOARCH == "" {
		return nil, errors.New("update: GOOS and GOARCH required")
	}
	if o.Interval <= 0 {
		o.Interval = 24 * time.Hour
	}
	if o.Initial <= 0 {
		o.Initial = defaultInit
	}
	if o.VersionCheck == nil {
		o.VersionCheck = func(ctx context.Context, bin, version, commit string) error {
			_, err := release.CheckVersion(ctx, bin, version, commit)
			return err
		}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	u := &Updater{o: o, log: o.Logger, arch: o.GOARCH, target: o.Target}
	if u.log == nil {
		u.log = slog.Default()
	}
	if o.GOOS == "darwin" {
		u.arch = "all"
	}
	return u, nil
}

// Check lists releases and selects the newest eligible one without
// downloading anything else. Development builds make no request.
func (u *Updater) Check(ctx context.Context) (*Candidate, Result, error) {
	current, ok := ParseRunning(u.o.Version)
	if !ok {
		return nil, Result{Outcome: OutcomeDevBuild, Message: "development builds are not updated"}, nil
	}
	releases, err := u.o.Source.Releases(ctx)
	if err != nil {
		code := Code(err)
		if code == "not_found" {
			code = OutcomeNoRelease
		}
		u.record(func(s *State) { s.LastOutcome, s.LastDetail = code, "" })
		return nil, Result{Outcome: code}, err
	}
	var best *Candidate
	for _, r := range releases {
		if r.Draft {
			continue
		}
		v, err := release.ParseTag(r.Tag)
		if err != nil || v.IsPrerelease() != r.Prerelease {
			continue // malformed or mislabelled releases are never candidates
		}
		if ok, _ := Eligible(current, v); !ok {
			continue
		}
		if best == nil || Compare(v, best.Version) > 0 {
			best = &Candidate{Version: v}
		}
	}
	res := Result{Outcome: OutcomeUpToDate}
	if len(releases) == 0 {
		res.Outcome = OutcomeNoRelease
	}
	if best != nil {
		res = Result{Outcome: OutcomeAvailable, Available: best.Version.Version}
	}
	u.record(func(s *State) {
		s.LastOutcome, s.LastDetail, s.Available = res.Outcome, "", res.Available
		if best == nil {
			s.Blocked = ""
		}
	})
	return best, res, nil
}

// Apply downloads, verifies, installs and restarts onto the candidate. Any
// failure leaves the installed binary untouched and is reported with a stable
// outcome code.
func (u *Updater) Apply(ctx context.Context, c *Candidate) (Result, error) {
	if c == nil {
		return Result{Outcome: OutcomeUpToDate}, nil
	}
	if err := localstate.SecureDir(u.o.Runtime); err != nil {
		return Result{Outcome: "runtime_error"}, err
	}
	lock, err := localstate.Lock(filepath.Join(u.o.Runtime, LockFile), false)
	if err != nil {
		if errors.Is(err, localstate.ErrBusy) {
			return Result{Outcome: OutcomeLocked, Message: "another update is in progress"}, coded(OutcomeLocked, err)
		}
		return Result{Outcome: "runtime_error"}, err
	}
	defer lock.Close()
	res, err := u.apply(ctx, c)
	if err != nil {
		detail := res.Message
		if detail == "" {
			detail = Code(err)
		}
		u.record(func(s *State) { s.LastOutcome, s.LastDetail, s.Target = res.Outcome, detail, u.target })
		u.log.Warn("update_failed", "outcome", res.Outcome, "version", c.Version.Version)
	}
	return res, err
}

func (u *Updater) apply(ctx context.Context, c *Candidate) (Result, error) {
	version := c.Version.Version
	fail := func(outcome string, err error) (Result, error) {
		return Result{Outcome: outcome, Available: version}, coded(outcome, err)
	}
	target := u.target
	if target == "" {
		var err error
		if target, err = Target(); err != nil {
			return fail("install_failed", err)
		}
		u.target = target
	}
	if err := checkWritable(target); err != nil {
		if errors.Is(err, ErrNotWritable) {
			msg := fmt.Sprintf("update %s is available but %s is not writable by this user; install it manually (macOS: the release package rhizome_%s_macos.pkg; otherwise replace %s with the verified release binary and restart the service)", version, filepath.Dir(target), c.Version.AssetVersion(), target)
			u.record(func(s *State) { s.Blocked, s.Available, s.Target = OutcomeNotWritable, version, target })
			return Result{Outcome: OutcomeNotWritable, Available: version, Message: msg}, coded(OutcomeNotWritable, err)
		}
		return fail("install_failed", err)
	}
	commit, err := u.o.Source.TagCommit(ctx, c.Version.Tag)
	if err != nil {
		return fail(Code(err), err)
	}
	av := c.Version.AssetVersion()
	manifest, err := u.fetch(ctx, c.Version.Tag, release.ManifestName(av), maxManifest)
	if err != nil {
		return fail(Code(err), err)
	}
	signature, err := u.fetch(ctx, c.Version.Tag, release.SignatureName(av), maxSignature)
	if err != nil {
		if Code(err) == "not_found" {
			return fail("signature_invalid", errors.New("release has no manifest signature"))
		}
		return fail(Code(err), err)
	}
	keyID, err := release.VerifySignature(manifest, signature, u.o.Keys)
	if err != nil {
		return fail("signature_invalid", err)
	}
	sums, err := release.ParseManifest(manifest, av)
	if err != nil {
		return fail("invalid_release", err)
	}
	archive := release.ArchiveName(av, u.o.GOOS, u.arch)
	want := sums[archive]
	work, err := os.MkdirTemp(u.o.Runtime, "update-*")
	if err != nil {
		return fail("install_failed", err)
	}
	defer os.RemoveAll(work)
	archivePath := filepath.Join(work, archive)
	digest, err := u.download(ctx, c.Version.Tag, archive, archivePath)
	if err != nil {
		return fail(Code(err), err)
	}
	if digest != want {
		return fail("digest_mismatch", fmt.Errorf("%s digest does not match the signed manifest", archive))
	}
	bin, err := extractBinary(archivePath, u.o.GOOS, work)
	if err != nil {
		return fail(Code(err), err)
	}
	if u.o.Verifier != nil {
		if err := u.o.Verifier.Verify(ctx, bin); err != nil {
			return fail(Code(err), err)
		}
	}
	if err := u.o.VersionCheck(ctx, bin, version, commit); err != nil {
		return fail("version_mismatch", err)
	}
	u.log.Info("update_verified", "version", version, "asset", archive, "key", keyID)
	staged, err := stageCandidate(bin, target)
	if err != nil {
		if errors.Is(err, ErrNotWritable) {
			return fail(OutcomeNotWritable, err)
		}
		return fail("install_failed", err)
	}
	backup, err := installBinary(staged, target)
	if err != nil {
		return fail("install_failed", err)
	}
	pending := &Pending{From: u.o.Version, To: version, Target: target, Backup: backup, StartedAt: u.o.Now().UTC()}
	u.record(func(s *State) { s.Pending, s.Blocked, s.Target, s.Available = pending, "", target, "" })
	u.log.Info("update_installed", "from", u.o.Version, "to", version, "restart", u.o.Restarter != nil)
	res := Result{Outcome: OutcomeInstalled, Installed: version, Message: "installed " + version + " at " + target}
	if u.o.Restarter == nil {
		u.record(func(s *State) { s.LastOutcome, s.LastDetail = OutcomeInstalled, "" })
		return res, nil
	}
	err = u.o.Restarter.Restart(ctx, version)
	switch {
	case err == nil:
		os.Remove(backup)
		u.record(func(s *State) { s.Pending, s.LastOutcome, s.LastDetail, s.Version = nil, OutcomeUpdated, "", version })
		res.Outcome, res.Message = OutcomeUpdated, "updated to "+version+" and restarted the service"
		return res, nil
	case errors.Is(err, ErrDeferred):
		u.record(func(s *State) { s.LastOutcome, s.LastDetail = OutcomeRestartPending, "" })
		res.Outcome, res.Message = OutcomeRestartPending, "installed "+version+"; the service is restarting"
		return res, nil
	case errors.Is(err, ErrRestartRequired):
		u.record(func(s *State) { s.LastOutcome, s.LastDetail = OutcomeRestartRequired, "" })
		res.Outcome, res.Message = OutcomeRestartRequired, "installed "+version+"; restart the service to run it"
		return res, nil
	}
	return u.rollback(ctx, pending, err)
}

// rollback restores the previous binary after a failed restart and restarts
// onto it.
func (u *Updater) rollback(ctx context.Context, p *Pending, cause error) (Result, error) {
	restoreErr := restoreBinary(p.Backup, p.Target)
	outcome, detail := OutcomeRolledBack, "restart onto "+p.To+" failed; restored "+p.From
	if restoreErr != nil {
		outcome, detail = OutcomeRestartFailed, "restart onto "+p.To+" failed and the previous binary could not be restored"
	} else if err := u.o.Restarter.Restart(ctx, p.From); err != nil && !errors.Is(err, ErrDeferred) && !errors.Is(err, ErrRestartRequired) {
		detail += "; restarting it failed"
	}
	u.record(func(s *State) { s.Pending, s.LastOutcome, s.LastDetail = nil, outcome, detail })
	u.log.Warn("update_rolled_back", "from", p.From, "to", p.To, "restored", restoreErr == nil)
	return Result{Outcome: outcome, Message: detail}, coded(outcome, cause)
}

func (u *Updater) fetch(ctx context.Context, tag, name string, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := u.o.Source.Download(ctx, tag, name, limit, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// download streams an asset to path while hashing it and returns the digest.
func (u *Updater) download(ctx context.Context, tag, name, path string) (string, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", coded("install_failed", err)
	}
	h := sha256.New()
	_, err = u.o.Source.Download(ctx, tag, name, maxArchive, io.MultiWriter(f, h))
	if cerr := f.Close(); err == nil && cerr != nil {
		err = coded("install_failed", cerr)
	}
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// record updates the state file; failures are logged, never fatal.
func (u *Updater) record(mutate func(*State)) {
	s, err := ReadState(u.o.Runtime)
	if err != nil {
		u.log.Warn("update_state_read", "error", err)
		s = State{}
	}
	if s.Version == "" {
		s.Version = u.o.Version
	}
	mutate(&s)
	s.UpdatedAt = u.o.Now().UTC()
	if err := writeState(u.o.Runtime, s); err != nil {
		u.log.Warn("update_state_write", "error", err)
	}
}

// Run checks on the configured interval until ctx ends, applying eligible
// releases. It returns ErrRestartRequired when the process must exit for its
// supervisor to restart it on the new binary, otherwise nil at cancellation.
func (u *Updater) Run(ctx context.Context) error {
	if _, ok := ParseRunning(u.o.Version); !ok {
		u.log.Info("update_disabled", "reason", OutcomeDevBuild)
		u.record(func(s *State) { s.LastOutcome = OutcomeDevBuild })
		return nil
	}
	delay := u.o.Initial
	failures := 0
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		u.record(func(s *State) { s.NextCheckAt = u.o.Now().Add(delay).UTC() })
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		outcome, err := u.cycle(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrRestartRequired) {
			return err
		}
		if err != nil && outcome != OutcomeNotWritable {
			failures = min(failures+1, maxBackoff)
		} else {
			failures = 0
		}
		delay = u.o.Interval << failures
		u.record(func(s *State) { s.Failures = failures })
		timer.Reset(delay)
	}
}

// cycle performs one check and, when a release is eligible, one apply.
func (u *Updater) cycle(ctx context.Context) (string, error) {
	u.record(func(s *State) { s.LastCheckAt = u.o.Now().UTC() })
	c, res, err := u.Check(ctx)
	if err != nil {
		u.log.Warn("update_check", "outcome", res.Outcome)
		return res.Outcome, err
	}
	u.log.Info("update_check", "outcome", res.Outcome, "available", res.Available)
	if c == nil {
		return res.Outcome, nil
	}
	state, _ := ReadState(u.o.Runtime)
	if state.Blocked == OutcomeNotWritable && state.Available == c.Version.Version {
		// Reported once already; keep the status current without downloading again.
		return OutcomeNotWritable, coded(OutcomeNotWritable, ErrNotWritable)
	}
	res, err = u.Apply(ctx, c)
	if res.Outcome == OutcomeNotWritable {
		u.log.Warn("update_blocked", "outcome", res.Outcome, "version", c.Version.Version, "detail", res.Message)
		return res.Outcome, err
	}
	if err != nil {
		return res.Outcome, err
	}
	u.log.Info("update_applied", "outcome", res.Outcome, "version", res.Installed)
	if res.Outcome == OutcomeRestartRequired {
		return res.Outcome, ErrRestartRequired
	}
	return res.Outcome, nil
}

// Confirm runs at process start: a pending update whose target version is now
// running is marked successful and its backup removed; a pending update that
// did not produce the expected version is recorded as failed.
func Confirm(runtime, version string, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	s, err := ReadState(runtime)
	if err != nil || s.Pending == nil {
		return err
	}
	p := s.Pending
	s.Pending, s.Version, s.UpdatedAt = nil, version, time.Now().UTC()
	if version == p.To {
		if err := os.Remove(p.Backup); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Warn("update_backup_remove", "error", err)
		}
		s.LastOutcome, s.LastDetail = OutcomeUpdated, ""
		log.Info("update_confirmed", "from", p.From, "to", p.To)
	} else {
		s.LastOutcome, s.LastDetail = OutcomeRestartFailed, "expected "+p.To+" after restart, running "+version
		log.Warn("update_unconfirmed", "expected", p.To, "running", version)
	}
	return writeState(runtime, s)
}

// Finish completes a deferred restart from a detached process: it restarts
// the service onto the pending version, verifies health, and otherwise
// restores the backup and restarts onto it. The outcome is recorded.
func Finish(ctx context.Context, runtime string, r Restarter, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	lock, err := waitLock(ctx, filepath.Join(runtime, LockFile))
	if err != nil {
		return err
	}
	defer lock.Close()
	s, err := ReadState(runtime)
	if err != nil {
		return err
	}
	if s.Pending == nil {
		return errors.New("no pending update to finish")
	}
	u := &Updater{o: Options{Runtime: runtime, Restarter: r, Now: time.Now, Version: s.Pending.From}, log: log, target: s.Pending.Target}
	p := s.Pending
	err = r.Restart(ctx, p.To)
	if err == nil {
		os.Remove(p.Backup)
		u.record(func(s *State) { s.Pending, s.LastOutcome, s.LastDetail, s.Version = nil, OutcomeUpdated, "", p.To })
		log.Info("update_finished", "from", p.From, "to", p.To)
		return nil
	}
	_, rerr := u.rollback(ctx, p, err)
	return rerr
}

func waitLock(ctx context.Context, path string) (*os.File, error) {
	deadline := time.Now().Add(lockWait)
	for {
		f, err := localstate.Lock(path, false)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, localstate.ErrBusy) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, coded(OutcomeLocked, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
