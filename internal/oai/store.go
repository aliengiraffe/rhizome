// Package oai implements the experimental, explicit-only Sign in with ChatGPT CLI.
package oai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"rhizome/internal/localstate"
)

const maxRecord = 128 << 10
const chunkSize = 1800

var errStore = errors.New("OS credential store unavailable; unlock Keychain, start Secret Service with secret-tool installed, or enable Windows Credential Manager; no plaintext fallback")
var errMissing = errors.New("credential not found")
var errSignIn = errors.New("not signed in; run rhizome auth oai")

// SecretStore stores opaque chunks in the OS credential store, never files.
// Implementations must not include secrets in errors or process arguments.
type SecretStore interface {
	Get(context.Context, string) (string, error)
	Set(context.Context, string, string) error
	Delete(context.Context, string) error
}

type secretRef struct {
	ID     string `json:"id"`
	Chunks int    `json:"chunks"`
}

// State contains only non-secret registration and display metadata.
type State struct {
	Version  int        `json:"version"`
	HostID   string     `json:"host_id"`
	ClientID string     `json:"client_id,omitempty"`
	Port     int        `json:"redirect_port,omitempty"`
	Subject  string     `json:"subject,omitempty"`
	Account  string     `json:"account,omitempty"`
	Scopes   []string   `json:"scopes,omitempty"`
	Expires  time.Time  `json:"expires_at,omitempty"`
	Earliest time.Time  `json:"earliest_refresh_at,omitempty"`
	Secrets  *secretRef `json:"secrets,omitempty"`
	// Pending requires verification before any bearer token can be used.
	Pending    bool        `json:"pending_verification,omitempty"`
	Refreshing bool        `json:"refresh_uncertain,omitempty"`
	Garbage    []secretRef `json:"obsolete_records,omitempty"`
}

// SignedIn reports whether verified credentials are available locally.
func (s State) SignedIn() bool { return s.Secrets != nil && !s.Pending && !s.Refreshing }

// PlanUsage reports the permission granted by the token endpoint.
func (s State) PlanUsage() bool {
	for _, v := range s.Scopes {
		if v == "chatgpt.tokens.use.direct" {
			return true
		}
	}
	return false
}

type credentials struct {
	Access  string `json:"access"`
	Refresh string `json:"refresh"`
	ID      string `json:"id"`
}
type vault struct {
	dir   string
	store SecretStore
	state State
	lock  *os.File
}

func openVault(dir string, store SecretStore, readonly bool) (*vault, error) {
	v := &vault{dir: dir, store: store}
	if readonly {
		st, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			return v, nil
		}
		if err != nil || !st.IsDir() || (runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0) {
			return nil, errors.New("unsafe OAI state directory")
		}
	} else {
		if err := localstate.SecureDir(dir); err != nil {
			return nil, errors.New("OAI state directory must be private and owned by this user")
		}
		lp := filepath.Join(dir, "lock")
		if _, err := os.Lstat(lp); err == nil {
			if err = localstate.PrivateFile(lp); err != nil {
				return nil, errors.New("unsafe OAI lock file")
			}
		}
		f, err := localstate.Lock(lp, false)
		if err != nil {
			return nil, errors.New("cannot lock OAI state; another auth/test command may be running")
		}
		v.lock = f
	}
	fail := func(err error) (*vault, error) { v.close(); return nil, err }
	path := filepath.Join(dir, "state.json")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return v, nil
	}
	if err := checkStateFile(path, readonly); err != nil {
		return fail(errors.New("unsafe OAI state file"))
	}
	data, err := readFile(path, maxRecord)
	if err != nil {
		return fail(errors.New("cannot read OAI state"))
	}
	if json.Unmarshal(data, &v.state) != nil || v.state.Version != 1 || !validState(v.state) {
		return fail(errors.New("invalid OAI state; restore a known-good registration"))
	}
	return v, nil
}

// Status reads an atomically published snapshot without creating files or changing ACLs.
func checkStateFile(path string, readonly bool) error {
	if !readonly {
		return localstate.PrivateFile(path)
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || (runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0) {
		return errors.New("unsafe state")
	}
	return nil
}

var recordPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func validState(s State) bool {
	if s.ClientID != "" && !validClientID(s.ClientID) {
		return false
	}
	for _, ref := range append(append([]secretRef{}, s.Garbage...), deref(s.Secrets)...) {
		if !recordPattern.MatchString(ref.ID) || ref.Chunks < 1 || ref.Chunks > maxRecord/chunkSize+1 {
			return false
		}
	}
	return len(s.Garbage) <= 128 && s.Port >= 0 && s.Port <= 65535
}
func deref(r *secretRef) []secretRef {
	if r == nil {
		return nil
	}
	return []secretRef{*r}
}
func (v *vault) close() {
	if v.lock != nil {
		v.lock.Close()
	}
}
func readFile(path string, limit int64) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	return bounded(f, limit)
}
func bounded(r io.Reader, limit int64) ([]byte, error) {
	b, e := io.ReadAll(io.LimitReader(r, limit+1))
	if e != nil || int64(len(b)) > limit {
		return nil, errors.New("response exceeds limit or could not be read")
	}
	return b, nil
}
func (v *vault) save() error {
	v.state.Version = 1
	b, e := json.Marshal(v.state)
	if e != nil {
		return errors.New("cannot encode OAI metadata")
	}
	f, e := os.CreateTemp(v.dir, ".state-*")
	if e != nil {
		return errors.New("cannot create OAI metadata")
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e == nil {
		e = localstate.PrivateFile(f.Name())
	}
	if e == nil {
		e = os.Rename(f.Name(), filepath.Join(v.dir, "state.json"))
	}
	if e == nil {
		e = syncStateDir(v.dir)
	}
	if e != nil {
		return errors.New("cannot atomically save OAI metadata")
	}
	return nil
}
func randomID() string { b := make([]byte, 16); rand.Read(b); return hex.EncodeToString(b) }
func hostID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	s := hex.EncodeToString(b)
	return "urn:uuid:" + s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}
func chunkKey(ref secretRef, i int) string { return "rhizome-oai-" + ref.ID + "-" + stringIndex(i) }
func (v *vault) load(ctx context.Context) (credentials, error) {
	var c credentials
	if v.state.Secrets == nil {
		return c, errSignIn
	}
	var b strings.Builder
	for i := 0; i < v.state.Secrets.Chunks; i++ {
		s, e := v.store.Get(ctx, chunkKey(*v.state.Secrets, i))
		if e != nil {
			return c, errStore
		}
		if len(s) > chunkSize {
			return c, errStore
		}
		b.WriteString(s)
	}
	if json.Unmarshal([]byte(b.String()), &c) != nil || c.Access == "" || c.Refresh == "" {
		return c, errStore
	}
	return c, nil
}

// install stages an immutable record, then atomically switches the metadata pointer.
// Chunking accommodates the smaller native Windows/macOS credential limits.
func (v *vault) install(ctx context.Context, c credentials, next State) error {
	b, e := json.Marshal(c)
	if e != nil || len(b) > maxRecord {
		return errors.New("credential record exceeds limit")
	}
	ref := secretRef{ID: randomID(), Chunks: (len(b) + chunkSize - 1) / chunkSize}
	// Persist cleanup information first so interrupted writes remain discoverable.
	v.state.Garbage = append(v.state.Garbage, ref)
	if e = v.save(); e != nil {
		return e
	}
	for i := 0; i < ref.Chunks; i++ {
		end := min((i+1)*chunkSize, len(b))
		if e = v.store.Set(ctx, chunkKey(ref, i), string(b[i*chunkSize:end])); e != nil {
			return errStore
		}
	}
	next.Garbage = append([]secretRef{}, v.state.Garbage[:len(v.state.Garbage)-1]...)
	next.Garbage = append(next.Garbage, deref(v.state.Secrets)...)
	next.Secrets = &ref
	old := v.state
	v.state = next
	if e = v.save(); e != nil {
		v.state = old
		return e
	}
	return v.cleanup(ctx)
}
func (v *vault) cleanup(ctx context.Context) error {
	remaining := []secretRef{}
	for _, ref := range v.state.Garbage {
		failed := false
		for i := 0; i < ref.Chunks; i++ {
			e := v.store.Delete(ctx, chunkKey(ref, i))
			if e != nil && !errors.Is(e, errMissing) {
				failed = true
			}
		}
		if failed {
			remaining = append(remaining, ref)
		}
	}
	v.state.Garbage = remaining
	if e := v.save(); e != nil {
		return e
	}
	if len(remaining) > 0 {
		return errStore
	}
	return nil
}
func (v *vault) clear(ctx context.Context) error {
	v.state.Garbage = append(v.state.Garbage, deref(v.state.Secrets)...)
	v.state.Secrets = nil
	v.state.Pending = false
	v.state.Refreshing = false
	v.state.Scopes = nil
	v.state.Expires = time.Time{}
	v.state.Earliest = time.Time{}
	if e := v.save(); e != nil {
		return e
	}
	return v.cleanup(ctx)
}
