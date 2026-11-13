// Package project derives a cross-machine project identity from a working
// directory's git "origin" remote. It reads git metadata read-only with bounded
// reads, refuses symlinks, never runs git, and never returns credentials.
package project

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	maxConfigBytes = 64 << 10 // .git/config
	maxGitFile     = 4 << 10  // a worktree's .git file or commondir
	maxParents     = 64       // directories searched upward for .git
	// MaxKeyBytes bounds a project key.
	MaxKeyBytes = 256
)

var (
	localKey   = regexp.MustCompile(`^local:[0-9a-f]{12}$`)
	remoteKey  = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(/[A-Za-z0-9._~-]+){2,}$`)
	hostLabel  = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
	pathSegRe  = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
	errNoRepo  = errors.New("no git repository")
	errUnsafe  = errors.New("unsafe git metadata")
	errNoValue = errors.New("no origin remote")
)

// Key returns the project identity for dir: the normalized origin remote
// ("host/owner/repo") when one can be read safely, otherwise
// "local:<sha256(dir)[:12]>", which never matches across machines.
func Key(dir string) string {
	if dir != "" {
		if raw, err := originURL(dir); err == nil {
			if key, ok := NormalizeRemote(raw); ok {
				return key
			}
		}
	}
	return LocalKey(dir)
}

// LocalKey is the machine-local identity of a project path.
func LocalKey(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return "local:" + hex.EncodeToString(sum[:])[:12]
}

// Valid reports whether key is a well-formed project key of either form.
func Valid(key string) bool {
	if len(key) == 0 || len(key) > MaxKeyBytes {
		return false
	}
	return localKey.MatchString(key) || remoteKey.MatchString(key) && !strings.Contains(key, "..")
}

// NormalizeRemote converts an SSH, scp-like or HTTP(S)/git remote URL into
// "host/owner/repo". It lowercases the host and drops the scheme, user
// information (including any credentials), port, query, fragment and a
// trailing ".git". Local paths and file URLs are not remotes.
func NormalizeRemote(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" || len(s) > 2048 || strings.ContainsAny(s, " \t\r\n\x00\\") {
		return "", false
	}
	var host, path string
	if scheme, rest, ok := strings.Cut(s, "://"); ok {
		switch strings.ToLower(scheme) {
		case "ssh", "git", "http", "https", "git+ssh", "ssh+git":
		default:
			return "", false // file:// and unknown transports have no network identity
		}
		if i := strings.IndexAny(rest, "?#"); i >= 0 {
			rest = rest[:i]
		}
		authority, p, _ := strings.Cut(rest, "/")
		if at := strings.LastIndex(authority, "@"); at >= 0 {
			authority = authority[at+1:]
		}
		host, path = stripPort(authority), p
	} else {
		// scp-like: [user@]host:path, where the colon precedes any slash.
		colon := strings.Index(s, ":")
		if colon <= 0 || strings.Contains(s[:colon], "/") {
			return "", false
		}
		authority := s[:colon]
		if at := strings.LastIndex(authority, "@"); at >= 0 {
			authority = authority[at+1:]
		}
		host, path = authority, s[colon+1:]
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if !hostLabel.MatchString(host) {
		return "", false
	}
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.TrimSuffix(path, "/")
	segs := strings.Split(path, "/")
	if len(segs) < 2 {
		return "", false
	}
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." || !pathSegRe.MatchString(seg) {
			return "", false
		}
	}
	key := host + "/" + strings.Join(segs, "/")
	if !Valid(key) {
		return "", false
	}
	return key, true
}

// stripPort removes a port from an authority, including bracketed IPv6.
func stripPort(authority string) string {
	if strings.HasPrefix(authority, "[") {
		if end := strings.Index(authority, "]"); end > 0 {
			return authority[1:end]
		}
		return ""
	}
	if i := strings.LastIndex(authority, ":"); i >= 0 {
		return authority[:i]
	}
	return authority
}

// originURL finds the nearest repository at or above dir and returns its
// origin URL. Every metadata path is checked with Lstat; symlinks are refused.
func originURL(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for i := 0; i < maxParents; i++ {
		gitPath := filepath.Join(dir, ".git")
		st, err := os.Lstat(gitPath)
		if err == nil {
			cfg, err := configPath(gitPath, st)
			if err != nil {
				return "", err
			}
			return readOrigin(cfg)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", errNoRepo
}

// configPath resolves the config file of a .git directory, or of a linked
// worktree or submodule whose .git file names its git directory.
func configPath(gitPath string, st os.FileInfo) (string, error) {
	switch {
	case st.Mode()&os.ModeSymlink != 0:
		return "", errUnsafe
	case st.IsDir():
		return filepath.Join(gitPath, "config"), nil
	case !st.Mode().IsRegular():
		return "", errUnsafe
	}
	b, err := readBounded(gitPath, maxGitFile)
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return "", errUnsafe
	}
	gitDir = strings.TrimSpace(gitDir)
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(filepath.Dir(gitPath), gitDir)
	}
	if err := realDir(gitDir); err != nil {
		return "", err
	}
	// A linked worktree shares the main repository's config via commondir.
	common := filepath.Join(gitDir, "commondir")
	if cst, err := os.Lstat(common); err == nil {
		if !cst.Mode().IsRegular() {
			return "", errUnsafe
		}
		b, err := readBounded(common, maxGitFile)
		if err != nil {
			return "", err
		}
		dir := strings.TrimSpace(string(b))
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(gitDir, dir)
		}
		if err := realDir(dir); err != nil {
			return "", err
		}
		gitDir = dir
	}
	return filepath.Join(gitDir, "config"), nil
}

func realDir(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return errUnsafe
	}
	return nil
}

// readBounded reads a regular, non-symlink file of at most limit bytes.
func readBounded(path string, limit int64) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errUnsafe
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errUnsafe
	}
	return b, nil
}

// readOrigin returns remote.origin.url from a git config file. It understands
// the subset git writes for remotes: section headers, key = value pairs,
// comments and double-quoted values.
func readOrigin(path string) (string, error) {
	b, err := readBounded(path, maxConfigBytes)
	if err != nil {
		return "", err
	}
	in := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 4096), maxConfigBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			end := strings.Index(line, "]")
			if end < 0 {
				return "", errUnsafe
			}
			header := strings.TrimSpace(line[1:end])
			name, sub, _ := strings.Cut(header, " ")
			in = strings.EqualFold(name, "remote") && strings.TrimSpace(sub) == `"origin"`
			line = strings.TrimSpace(line[end+1:])
			if line == "" {
				continue
			}
		}
		if !in {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "url") {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, `"`) {
			end := strings.Index(value[1:], `"`)
			if end < 0 {
				return "", errUnsafe
			}
			value = value[1 : end+1]
		} else if i := strings.IndexAny(value, "#;"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		return value, nil
	}
	return "", errNoValue
}
