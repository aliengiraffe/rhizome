package update

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"rhizome/internal/localstate"
	"rhizome/internal/release"
)

// fakeRelease is a signed release staged on disk for the fake server.
type fakeRelease struct {
	version release.Version
	dir     string
	commit  string
}

func stageRelease(t *testing.T, tag string, key ed25519.PrivateKey, sign bool) fakeRelease {
	t.Helper()
	v, err := release.ParseTag(tag)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	av := v.AssetVersion()
	for _, name := range release.Assets(av) {
		path := filepath.Join(dir, name)
		switch {
		case name == release.ArchiveName(av, runtime.GOOS, archFor(runtime.GOOS)):
			bin := member{name: release.BinaryName(runtime.GOOS), body: []byte("binary " + v.Version)}
			members := []member{bin, {name: "LICENSE", body: []byte("l")}, {name: "README.md", body: []byte("r")}}
			if strings.HasSuffix(name, ".zip") {
				writeZip(t, path, members)
			} else {
				writeTarGz(t, path, members)
			}
		default:
			if err := os.WriteFile(path, []byte("other "+name), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := release.WriteManifest(dir, av); err != nil {
		t.Fatal(err)
	}
	if sign {
		if err := release.SignManifest(dir, av, key); err != nil {
			t.Fatal(err)
		}
	}
	return fakeRelease{version: v, dir: dir, commit: strings.Repeat("a", 40)}
}

func archFor(goos string) string {
	if goos == "darwin" {
		return "all"
	}
	return runtime.GOARCH
}

// fakeServer serves the GitHub endpoints the updater uses.
type fakeServer struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	releases []map[string]any
	files    map[string]string // tag -> dir
	commits  map[string]string
	status   int
	headers  map[string]string
	redirect string
	slow     bool
	pad      int64
	hits     int
}

func newFakeServer(t *testing.T) *fakeServer {
	f := &fakeServer{t: t, files: map[string]string{}, commits: map[string]string{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) add(r fakeRelease, prerelease, draft bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases = append(f.releases, map[string]any{"tag_name": r.version.Tag, "prerelease": prerelease, "draft": draft, "body": "see https://evil.example/x"})
	f.files[r.version.Tag] = r.dir
	f.commits[r.version.Tag] = r.commit
}

func (f *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	if r.Header.Get("User-Agent") == "" {
		http.Error(w, "no user agent", 400)
		return
	}
	for k, v := range f.headers {
		w.Header().Set(k, v)
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	if f.redirect != "" {
		http.Redirect(w, r, f.redirect, http.StatusFound)
		return
	}
	path := r.URL.Path
	switch {
	case path == "/repos/aliengiraffe/rhizome/releases":
		if r.Header.Get("Accept") != "application/vnd.github+json" {
			http.Error(w, "accept", 406)
			return
		}
		list := f.releases
		if list == nil {
			list = []map[string]any{}
		}
		json.NewEncoder(w).Encode(list)
	case strings.HasPrefix(path, "/repos/aliengiraffe/rhizome/git/ref/tags/"):
		tag := strings.TrimPrefix(path, "/repos/aliengiraffe/rhizome/git/ref/tags/")
		sha, ok := f.commits[tag]
		if !ok {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"object": map[string]any{"sha": sha, "type": "commit"}})
	case strings.HasPrefix(path, "/aliengiraffe/rhizome/releases/download/"):
		rest := strings.TrimPrefix(path, "/aliengiraffe/rhizome/releases/download/")
		tag, name, _ := strings.Cut(rest, "/")
		dir, ok := f.files[tag]
		if !ok {
			http.NotFound(w, r)
			return
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if f.slow {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			time.Sleep(2 * time.Second)
			return
		}
		if f.pad > 0 {
			w.Header().Set("Content-Length", fmt.Sprint(int64(len(b))+f.pad))
			w.Write(b)
			w.Write(make([]byte, f.pad))
			return
		}
		w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

type fakeRestarter struct {
	mu       sync.Mutex
	versions []string
	fail     map[string]error
	err      error
}

func (r *fakeRestarter) Restart(_ context.Context, version string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.versions = append(r.versions, version)
	if err, ok := r.fail[version]; ok {
		return err
	}
	return r.err
}

type harness struct {
	t       *testing.T
	server  *fakeServer
	runtime string
	target  string
	key     ed25519.PrivateKey
	pub     ed25519.PublicKey
	opts    Options
}

func newHarness(t *testing.T, current string) *harness {
	t.Helper()
	priv, _, err := release.GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, server: newFakeServer(t), key: priv, pub: priv.Public().(ed25519.PublicKey)}
	base := t.TempDir()
	h.runtime = filepath.Join(base, "run")
	if err := localstate.SecureDir(h.runtime); err != nil {
		t.Fatal(err)
	}
	h.target = filepath.Join(base, "bin", "rhizome")
	if err := os.MkdirAll(filepath.Dir(h.target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.target, []byte("binary "+current), 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := NewGitHub(h.server.srv.URL, h.server.srv.URL, "rhizome-test", h.server.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	h.opts = Options{Runtime: h.runtime, Version: current, Commit: "", GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Source: src, Keys: []ed25519.PublicKey{h.pub}, Target: h.target, Interval: time.Hour, Initial: time.Millisecond, Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		VersionCheck: func(_ context.Context, bin, version, commit string) error {
			b, err := os.ReadFile(bin)
			if err != nil {
				return err
			}
			if string(b) != "binary "+version {
				return fmt.Errorf("binary reports %q", b)
			}
			if commit != strings.Repeat("a", 40) {
				return fmt.Errorf("commit %q", commit)
			}
			return nil
		}}
	return h
}

func (h *harness) updater() *Updater {
	u, err := New(h.opts)
	if err != nil {
		h.t.Fatal(err)
	}
	return u
}

func (h *harness) targetContent() string {
	b, err := os.ReadFile(h.target)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b)
}

func (h *harness) state() State {
	s, err := ReadState(h.runtime)
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func (h *harness) binDirEntries() []string {
	entries, _ := os.ReadDir(filepath.Dir(h.target))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestCheckSelection(t *testing.T) {
	h := newHarness(t, "1.0.0")
	h.server.add(stageRelease(t, "v1.0.0", h.key, true), false, false)
	h.server.add(stageRelease(t, "v1.2.0", h.key, true), false, false)
	h.server.add(stageRelease(t, "v1.1.0", h.key, true), false, false)
	h.server.add(stageRelease(t, "v2.0.0-rc.1", h.key, true), true, false)
	h.server.add(stageRelease(t, "v3.0.0", h.key, true), false, true)
	h.server.add(stageRelease(t, "v0.9.0", h.key, true), false, false)
	c, res, err := h.updater().Check(context.Background())
	if err != nil || c == nil || c.Version.Version != "1.2.0" || res.Outcome != OutcomeAvailable || res.Available != "1.2.0" {
		t.Fatalf("%+v %+v %v", c, res, err)
	}
	if s := h.state(); s.Available != "1.2.0" || s.LastOutcome != OutcomeAvailable {
		t.Fatalf("state %+v", s)
	}
	// A prerelease build may move to the newest prerelease.
	h.opts.Version = "2.0.0-rc.0"
	c, _, err = h.updater().Check(context.Background())
	if err != nil || c == nil || c.Version.Version != "2.0.0-rc.1" {
		t.Fatalf("prerelease build: %+v %v", c, err)
	}
	h.opts.Version = "1.2.0"
	c, res, err = h.updater().Check(context.Background())
	if err != nil || c != nil || res.Outcome != OutcomeUpToDate {
		t.Fatalf("up to date: %+v %+v %v", c, res, err)
	}
	// Mislabelled releases are ignored: a prerelease tag published as stable.
	h.server.add(stageRelease(t, "v9.0.0-rc.1", h.key, true), false, false)
	c, _, _ = h.updater().Check(context.Background())
	if c != nil {
		t.Fatalf("mislabelled prerelease selected: %+v", c)
	}
}

func TestCheckNoReleasesAndDevBuild(t *testing.T) {
	h := newHarness(t, "1.0.0")
	c, res, err := h.updater().Check(context.Background())
	if err != nil || c != nil || res.Outcome != OutcomeNoRelease {
		t.Fatalf("%+v %+v %v", c, res, err)
	}
	h.server.status = 404
	if _, res, err := h.updater().Check(context.Background()); err == nil || res.Outcome != OutcomeNoRelease {
		t.Fatalf("404: %+v %v", res, err)
	}
	h.server.status = 0
	hits := h.server.hits
	for _, dev := range []string{"dev", "", "0.0.0-snapshot+abc123def456"} {
		h.opts.Version = dev
		c, res, err := h.updater().Check(context.Background())
		if err != nil || c != nil || res.Outcome != OutcomeDevBuild {
			t.Fatalf("%q: %+v %+v %v", dev, c, res, err)
		}
		if err := h.updater().Run(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if h.server.hits != hits {
		t.Fatal("development build contacted the server")
	}
}

func TestCheckErrors(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(f *fakeServer)
		code    string
	}{
		{"rate limited 429", func(f *fakeServer) { f.status = 429 }, "rate_limited"},
		{"rate limited 403", func(f *fakeServer) { f.status = 403; f.headers = map[string]string{"X-RateLimit-Remaining": "0"} }, "rate_limited"},
		{"server error", func(f *fakeServer) { f.status = 500 }, "http_error"},
		{"redirect off github", func(f *fakeServer) { f.redirect = "https://evil.example/releases" }, "redirect_refused"},
		{"redirect to plain http", func(f *fakeServer) { f.redirect = "http://github.com/x" }, "redirect_refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "1.0.0")
			tc.prepare(h.server)
			_, res, err := h.updater().Check(context.Background())
			if err == nil || Code(err) != tc.code || res.Outcome != tc.code {
				t.Fatalf("got %+v %v", res, err)
			}
			if s := h.state(); s.LastOutcome != tc.code {
				t.Fatalf("state %+v", s)
			}
		})
	}
	h := newHarness(t, "1.0.0")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := h.updater().Check(ctx); err == nil || Code(err) != "timeout" && Code(err) != "cancelled" && Code(err) != "network_error" {
		t.Fatalf("cancelled check: %v", err)
	}
}

func TestApplySuccess(t *testing.T) {
	h := newHarness(t, "1.0.0")
	h.server.add(stageRelease(t, "v1.1.0", h.key, true), false, false)
	restarter := &fakeRestarter{}
	h.opts.Restarter = restarter
	u := h.updater()
	c, _, err := u.Check(context.Background())
	if err != nil || c == nil {
		t.Fatal(c, err)
	}
	res, err := u.Apply(context.Background(), c)
	if err != nil || res.Outcome != OutcomeUpdated || res.Installed != "1.1.0" {
		t.Fatalf("%+v %v", res, err)
	}
	if got := h.targetContent(); got != "binary 1.1.0" {
		t.Fatalf("target %q", got)
	}
	if entries := h.binDirEntries(); len(entries) != 1 {
		t.Fatalf("backup or temp files left: %v", entries)
	}
	if got := restarter.versions; len(got) != 1 || got[0] != "1.1.0" {
		t.Fatalf("restarts %v", got)
	}
	s := h.state()
	if s.LastOutcome != OutcomeUpdated || s.Pending != nil || s.Version != "1.1.0" || s.Target != h.target {
		t.Fatalf("state %+v", s)
	}
	entries, _ := os.ReadDir(h.runtime)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "update-") && e.IsDir() {
			t.Fatalf("work directory left: %s", e.Name())
		}
	}
	st, _ := os.Stat(h.target)
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o111 == 0 {
		t.Fatal("installed binary not executable")
	}
}

func TestApplyWithoutRestarterKeepsPending(t *testing.T) {
	h := newHarness(t, "1.0.0")
	h.server.add(stageRelease(t, "v1.1.0", h.key, true), false, false)
	u := h.updater()
	c, _, _ := u.Check(context.Background())
	res, err := u.Apply(context.Background(), c)
	if err != nil || res.Outcome != OutcomeInstalled {
		t.Fatalf("%+v %v", res, err)
	}
	s := h.state()
	if s.Pending == nil || s.Pending.To != "1.1.0" || s.Pending.From != "1.0.0" || s.Pending.Backup != h.target+BackupSuffix {
		t.Fatalf("pending %+v", s.Pending)
	}
	if _, err := os.Stat(s.Pending.Backup); err != nil {
		t.Fatal("backup missing", err)
	}
	// The new binary confirms itself on its next start.
	if err := Confirm(h.runtime, "1.1.0", nil); err != nil {
		t.Fatal(err)
	}
	s = h.state()
	if s.Pending != nil || s.LastOutcome != OutcomeUpdated || s.Version != "1.1.0" {
		t.Fatalf("confirmed %+v", s)
	}
	if _, err := os.Stat(h.target + BackupSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("backup kept after confirmation")
	}
	// Confirm with no pending update is a no-op.
	if err := Confirm(h.runtime, "1.1.0", nil); err != nil {
		t.Fatal(err)
	}
}

func TestConfirmUnexpectedVersion(t *testing.T) {
	h := newHarness(t, "1.0.0")
	h.server.add(stageRelease(t, "v1.1.0", h.key, true), false, false)
	u := h.updater()
	c, _, _ := u.Check(context.Background())
	if _, err := u.Apply(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := Confirm(h.runtime, "1.0.0", nil); err != nil {
		t.Fatal(err)
	}
	if s := h.state(); s.Pending != nil || s.LastOutcome != OutcomeRestartFailed {
		t.Fatalf("%+v", s)
	}
}

func TestApplyRollsBackFailedRestart(t *testing.T) {
	h := newHarness(t, "1.0.0")
	h.server.add(stageRelease(t, "v1.1.0", h.key, true), false, false)
	restarter := &fakeRestarter{fail: map[string]error{"1.1.0": errors.New("observer did not come up")}}
	h.opts.Restarter = restarter
	u := h.updater()
	c, _, _ := u.Check(context.Background())
	res, err := u.Apply(context.Background(), c)
	if err == nil || res.Outcome != OutcomeRolledBack || Code(err) != OutcomeRolledBack {
		t.Fatalf("%+v %v", res, err)
	}
	if got := h.targetContent(); got != "binary 1.0.0" {
		t.Fatalf("target %q after rollback", got)
	}
	if got := restarter.versions; len(got) != 2 || got[0] != "1.1.0" || got[1] != "1.0.0" {
		t.Fatalf("restarts %v", got)
	}
	if entries := h.binDirEntries(); len(entries) != 1 {
		t.Fatalf("files left: %v", entries)
	}
	if s := h.state(); s.Pending != nil || s.LastOutcome != OutcomeRolledBack {
		t.Fatalf("state %+v", s)
	}
}

func TestApplyDeferredAndRequiredRestart(t *testing.T) {
	for _, tc := range []struct {
		err     error
		outcome string
	}{{ErrDeferred, OutcomeRestartPending}, {ErrRestartRequired, OutcomeRestartRequired}} {
		h := newHarness(t, "1.0.0")
		h.server.add(stageRelease(t, "v1.1.0", h.key, true), false, false)
		h.opts.Restarter = &fakeRestarter{err: tc.err}
		u := h.updater()
		c, _, _ := u.Check(context.Background())
		res, err := u.Apply(context.Background(), c)
		if err != nil || res.Outcome != tc.outcome {
			t.Fatalf("%v: %+v %v", tc.err, res, err)
		}
		if s := h.state(); s.Pending == nil || s.LastOutcome != tc.outcome {
			t.Fatalf("%v: state %+v", tc.err, s)
		}
		if h.targetContent() != "binary 1.1.0" {
			t.Fatal("binary not installed")
		}
	}
}

func TestFinish(t *testing.T) {
	h := newHarness(t, "1.0.0")
	h.server.add(stageRelease(t, "v1.1.0", h.key, true), false, false)
	h.opts.Restarter = &fakeRestarter{err: ErrDeferred}
	u := h.updater()
	c, _, _ := u.Check(context.Background())
	if _, err := u.Apply(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	finisher := &fakeRestarter{}
	if err := Finish(context.Background(), h.runtime, finisher, nil); err != nil {
		t.Fatal(err)
	}
	if s := h.state(); s.Pending != nil || s.LastOutcome != OutcomeUpdated || s.Version != "1.1.0" {
		t.Fatalf("%+v", s)
	}
	if entries := h.binDirEntries(); len(entries) != 1 {
		t.Fatalf("backup kept: %v", entries)
	}
	if err := Finish(context.Background(), h.runtime, finisher, nil); err == nil {
		t.Fatal("finish without pending succeeded")
	}
	// A failed finish restores the previous binary.
	h.server.add(stageRelease(t, "v1.2.0", h.key, true), false, false)
	h.opts.Version = "1.1.0"
	u = h.updater()
	c, _, _ = u.Check(context.Background())
	if _, err := u.Apply(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	failing := &fakeRestarter{fail: map[string]error{"1.2.0": errors.New("unhealthy")}}
	if err := Finish(context.Background(), h.runtime, failing, nil); err == nil || Code(err) != OutcomeRolledBack {
		t.Fatalf("got %v", err)
	}
	if h.targetContent() != "binary 1.1.0" || len(failing.versions) != 2 {
		t.Fatalf("rollback: %q %v", h.targetContent(), failing.versions)
	}
}

func TestApplyRejects(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, h *harness, r fakeRelease)
		code    string
	}{
		{"unsigned manifest", func(t *testing.T, h *harness, r fakeRelease) {
			os.Remove(filepath.Join(r.dir, release.SignatureName(r.version.AssetVersion())))
		}, "signature_invalid"},
		{"no trusted keys", func(t *testing.T, h *harness, r fakeRelease) { h.opts.Keys = nil }, "signature_invalid"},
		{"unknown key", func(t *testing.T, h *harness, r fakeRelease) {
			other, _, _ := release.GenerateSigningKey()
			h.opts.Keys = []ed25519.PublicKey{other.Public().(ed25519.PublicKey)}
		}, "signature_invalid"},
		{"tampered manifest", func(t *testing.T, h *harness, r fakeRelease) {
			p := filepath.Join(r.dir, release.ManifestName(r.version.AssetVersion()))
			b, _ := os.ReadFile(p)
			os.WriteFile(p, []byte(strings.Replace(string(b), "a", "b", 1)), 0o644)
		}, "signature_invalid"},
		{"tampered archive", func(t *testing.T, h *harness, r fakeRelease) {
			name := release.ArchiveName(r.version.AssetVersion(), runtime.GOOS, archFor(runtime.GOOS))
			writeTarGz(t, filepath.Join(r.dir, name), []member{{name: "rhizome", body: []byte("evil")}})
		}, "digest_mismatch"},
		{"archive with extra member", func(t *testing.T, h *harness, r fakeRelease) {
			av := r.version.AssetVersion()
			name := release.ArchiveName(av, runtime.GOOS, archFor(runtime.GOOS))
			members := []member{{name: release.BinaryName(runtime.GOOS), body: []byte("binary 1.1.0")}, {name: "hook.sh", body: []byte("x")}}
			if strings.HasSuffix(name, ".zip") {
				writeZip(t, filepath.Join(r.dir, name), members)
			} else {
				writeTarGz(t, filepath.Join(r.dir, name), members)
			}
			// Re-sign so only the archive policy rejects it.
			if err := release.WriteManifest(r.dir, av); err != nil {
				t.Fatal(err)
			}
			if err := release.SignManifest(r.dir, av, h.key); err != nil {
				t.Fatal(err)
			}
		}, "archive_invalid"},
		{"version mismatch", func(t *testing.T, h *harness, r fakeRelease) {
			h.opts.VersionCheck = func(context.Context, string, string, string) error { return errors.New("rhizome 1.0.9") }
		}, "version_mismatch"},
		{"codesign rejected", func(t *testing.T, h *harness, r fakeRelease) {
			h.opts.Verifier = &codesignVerifier{run: func(_ context.Context, args ...string) (string, error) {
				if args[0] == "--verify" {
					return "", nil
				}
				return strings.Replace(goodCodesign, "Signature size=9000", "Signature=adhoc", 1), nil
			}}
		}, "codesign_failed"},
		{"oversize archive", func(t *testing.T, h *harness, r fakeRelease) { h.server.pad = maxArchive }, "oversize"},
		{"missing archive", func(t *testing.T, h *harness, r fakeRelease) {
			os.Remove(filepath.Join(r.dir, release.ArchiveName(r.version.AssetVersion(), runtime.GOOS, archFor(runtime.GOOS))))
		}, "not_found"},
		{"missing tag commit", func(t *testing.T, h *harness, r fakeRelease) { delete(h.server.commits, r.version.Tag) }, "not_found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "1.0.0")
			r := stageRelease(t, "v1.1.0", h.key, true)
			h.server.add(r, false, false)
			tc.prepare(t, h, r)
			restarter := &fakeRestarter{}
			h.opts.Restarter = restarter
			u := h.updater()
			c, _, err := u.Check(context.Background())
			if err != nil || c == nil {
				t.Fatal(c, err)
			}
			res, err := u.Apply(context.Background(), c)
			if err == nil || Code(err) != tc.code || res.Outcome != tc.code {
				t.Fatalf("got %+v %v", res, err)
			}
			if h.targetContent() != "binary 1.0.0" {
				t.Fatal("installed binary changed")
			}
			if entries := h.binDirEntries(); len(entries) != 1 {
				t.Fatalf("files left next to the binary: %v", entries)
			}
			if len(restarter.versions) != 0 {
				t.Fatal("service restarted")
			}
			if s := h.state(); s.LastOutcome != tc.code || s.Pending != nil {
				t.Fatalf("state %+v", s)
			}
		})
	}
}

func TestApplySlowBodyTimesOut(t *testing.T) {
	h := newHarness(t, "1.0.0")
	h.server.add(stageRelease(t, "v1.1.0", h.key, true), false, false)
	h.server.slow = true
	u := h.updater()
	c, _, _ := u.Check(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res, err := u.Apply(ctx, c)
	if err == nil || Code(err) != "timeout" || res.Outcome != "timeout" {
		t.Fatalf("%+v %v", res, err)
	}
	if h.targetContent() != "binary 1.0.0" {
		t.Fatal("binary changed")
	}
}

func TestApplyNotWritable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("directory permissions are not enforced for this user")
	}
	h := newHarness(t, "1.0.0")
	h.server.add(stageRelease(t, "v1.1.0", h.key, true), false, false)
	dir := filepath.Dir(h.target)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	hits := h.server.hits
	u := h.updater()
	c, _, _ := u.Check(context.Background())
	apiHits := h.server.hits - hits
	res, err := u.Apply(context.Background(), c)
	if err == nil || res.Outcome != OutcomeNotWritable || !strings.Contains(res.Message, "not writable") || !strings.Contains(res.Message, "rhizome_1.1.0_macos.pkg") {
		t.Fatalf("%+v %v", res, err)
	}
	if h.server.hits-hits != apiHits {
		t.Fatal("downloaded despite a non-writable target")
	}
	if s := h.state(); s.Blocked != OutcomeNotWritable || s.Available != "1.1.0" {
		t.Fatalf("state %+v", s)
	}
	// The background loop reports once and does not download again.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	h.opts.Interval = 50 * time.Millisecond
	if err := h.updater().Run(ctx); err != nil {
		t.Fatal(err)
	}
	if s := h.state(); s.Failures != 0 || s.Blocked != OutcomeNotWritable {
		t.Fatalf("state after loop %+v", s)
	}
}

func TestApplyLocked(t *testing.T) {
	h := newHarness(t, "1.0.0")
	h.server.add(stageRelease(t, "v1.1.0", h.key, true), false, false)
	lock, err := localstate.Lock(filepath.Join(h.runtime, LockFile), false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	u := h.updater()
	c, _, _ := u.Check(context.Background())
	res, err := u.Apply(context.Background(), c)
	if err == nil || res.Outcome != OutcomeLocked {
		t.Fatalf("%+v %v", res, err)
	}
	if h.targetContent() != "binary 1.0.0" {
		t.Fatal("binary changed")
	}
}

func TestRunLoop(t *testing.T) {
	h := newHarness(t, "1.0.0")
	h.server.add(stageRelease(t, "v1.1.0", h.key, true), false, false)
	h.opts.Restarter = &fakeRestarter{err: ErrRestartRequired}
	h.opts.Interval = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.updater().Run(ctx); !errors.Is(err, ErrRestartRequired) {
		t.Fatalf("got %v", err)
	}
	if h.targetContent() != "binary 1.1.0" {
		t.Fatal("binary not installed")
	}
	// Failures back off and the loop stops promptly on cancellation.
	h2 := newHarness(t, "1.0.0")
	h2.server.status = 500
	h2.opts.Interval = 20 * time.Millisecond
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	start := time.Now()
	if err := h2.updater().Run(ctx2); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("loop did not stop at cancellation")
	}
	if s := h2.state(); s.Failures == 0 || s.LastOutcome != "http_error" || s.NextCheckAt.IsZero() {
		t.Fatalf("state %+v", s)
	}
}

func TestNewGitHubEndpoints(t *testing.T) {
	for _, bad := range []string{"", "ftp://github.com", "http://github.com", "https://user:pw@github.com", "https://github.com/?x=1", "https://github.com/#f", "github.com"} {
		if _, err := NewGitHub(bad, DefaultDownloads, "ua", nil); err == nil {
			t.Errorf("accepted api %q", bad)
		}
	}
	g, err := NewGitHub(DefaultAPI, DefaultDownloads, "ua", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Download(context.Background(), "v1", "../x", 10, nil); err == nil || Code(err) != "invalid_request" {
		t.Fatalf("path traversal in asset name: %v", err)
	}
	if _, err := NewGitHub("http://127.0.0.1:1", "http://localhost:1", "ua", nil); err != nil {
		t.Fatalf("loopback http refused: %v", err)
	}
}
