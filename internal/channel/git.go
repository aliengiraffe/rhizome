package channel

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	pb "rhizome/proto"
)

// Git labels describe the repository a session works in: the normalized origin
// remote, the current branch and whether the project directory is the top
// level of a linked worktree. Like session names they are untrusted labels for
// relevance and filtering, never addresses: routing stays on exact session_id.
const (
	maxGitOrigin  = 180       // UTF-8 bytes; a longer origin is dropped, never cut
	maxGitBranch  = 96        // UTF-8 bytes; a longer branch is cut on a rune boundary
	maxGitPointer = 4096      // bytes read from .git files, HEAD and commondir
	maxGitConfig  = 256 << 10 // bytes read from the repository config
	maxGitParents = 64        // directories searched upward for .git
	maxGitRecords = 4096      // files kept under <runtime>/git
	// defaultGitRefresh is how often a bound node re-reads its git labels, so a
	// branch switch reaches peers without a new binding.
	defaultGitRefresh = 15 * time.Second
)

// gitInfo is the set of git labels of one session.
type gitInfo struct {
	Origin       string `json:"git_origin,omitempty"`
	Branch       string `json:"git_branch,omitempty"`
	WorktreeRoot bool   `json:"git_worktree_root,omitempty"`
}

// identityGit returns the git labels an identity carries.
func identityGit(i *pb.Identity) gitInfo {
	return gitInfo{Origin: i.GetGitOrigin(), Branch: i.GetGitBranch(), WorktreeRoot: i.GetGitWorktreeRoot()}
}

// apply copies the labels onto an identity.
func (g gitInfo) apply(i *pb.Identity) {
	i.GitOrigin, i.GitBranch, i.GitWorktreeRoot = g.Origin, g.Branch, g.WorktreeRoot
}

// validGitLabels bounds labels received from another process or host.
func validGitLabels(origin, branch string) bool {
	return len(origin) <= maxGitOrigin && len(branch) <= maxGitBranch && utf8.ValidString(origin) && utf8.ValidString(branch)
}

// gitLabel reports whether s can be advertised: valid UTF-8 without
// whitespace or control characters.
func gitLabel(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) })
}

// normalizeGitOrigin reduces a remote URL to `host/path`, so SCP-style, SSH and
// HTTPS remotes of one repository compare equal on every machine. The host is
// lowercased and the scheme, userinfo, port, query, fragment, surrounding
// slashes and a trailing ".git" are removed, so credentials embedded in a
// remote never leave the process. Local paths, unsupported schemes and results
// above maxGitOrigin yield "".
func normalizeGitOrigin(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !gitLabel(raw) {
		return ""
	}
	var host, path string
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		switch strings.ToLower(u.Scheme) {
		case "ssh", "git", "http", "https", "git+ssh", "ssh+git":
		default:
			return ""
		}
		host, path = u.Hostname(), u.Path
	} else {
		// SCP style: [user@]host:path, where the colon precedes any slash.
		colon, slash := strings.IndexByte(raw, ':'), strings.IndexByte(raw, '/')
		if colon <= 0 || slash >= 0 && slash < colon {
			return ""
		}
		host, path = raw[:colon], raw[colon+1:]
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			host = host[at+1:]
		}
		if len(host) < 2 {
			return "" // a Windows drive letter, not a host
		}
		if cut := strings.IndexAny(path, "?#"); cut >= 0 {
			path = path[:cut]
		}
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	path = strings.TrimRight(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/")
	if host == "" || path == "" || strings.ContainsFunc(host, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == ':' || r == '_')
	}) {
		return ""
	}
	out := host + "/" + path
	if len(out) > maxGitOrigin || !gitLabel(out) {
		return ""
	}
	return out
}

// readSmall reads a regular file of at most limit bytes. Anything else
// (missing, a FIFO, a device, an oversized file) is reported as unreadable.
func readSmall(path string, limit int64) (string, bool) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > limit {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return "", false
	}
	return string(raw), true
}

// detectGit reads the git labels of dir from repository metadata. It never
// runs git, so no repository-configured program, hook or credential helper
// executes, and every read is bounded. A missing repository, a missing origin
// or unreadable metadata yields empty labels, never an error.
func detectGit(dir string) gitInfo {
	if !filepath.IsAbs(dir) {
		return gitInfo{}
	}
	dir = filepath.Clean(dir)
	top := dir
	for range maxGitParents {
		dot := filepath.Join(top, ".git")
		if st, err := os.Stat(dot); err == nil {
			return readGit(top, dot, st.IsDir(), top == dir)
		}
		parent := filepath.Dir(top)
		if parent == top {
			break
		}
		top = parent
	}
	return gitInfo{}
}

// readGit resolves the git directory behind top/.git and reads HEAD and the
// origin remote. A linked worktree's .git is a file naming a private git
// directory whose commondir file points at the main repository.
func readGit(top, dot string, isDir, atProject bool) gitInfo {
	gitDir := dot
	if !isDir {
		raw, ok := readSmall(dot, maxGitPointer)
		line, _, _ := strings.Cut(raw, "\n")
		target, found := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
		if target = strings.TrimSpace(target); !ok || !found || target == "" {
			return gitInfo{}
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(top, target)
		}
		gitDir = filepath.Clean(target)
	}
	head, ok := readSmall(filepath.Join(gitDir, "HEAD"), maxGitPointer)
	if !ok {
		return gitInfo{}
	}
	common, linked := gitDir, false
	if raw, ok := readSmall(filepath.Join(gitDir, "commondir"), maxGitPointer); ok {
		if target := strings.TrimSpace(raw); target != "" {
			if !filepath.IsAbs(target) {
				target = filepath.Join(gitDir, target)
			}
			common = filepath.Clean(target)
			linked = common != gitDir
		}
	}
	info := gitInfo{Branch: headBranch(head), WorktreeRoot: linked && !isDir && atProject}
	if config, ok := readSmall(filepath.Join(common, "config"), maxGitConfig); ok {
		info.Origin = normalizeGitOrigin(originURL(config))
	}
	return info
}

// headBranch returns the branch a symbolic HEAD names, or "" when detached.
func headBranch(head string) string {
	line, _, _ := strings.Cut(head, "\n")
	branch, ok := strings.CutPrefix(strings.TrimSpace(line), "ref: refs/heads/")
	// Reftable repositories keep a placeholder in HEAD; the branch is unknown.
	if !ok || branch == "" || branch == ".invalid" || !gitLabel(branch) {
		return ""
	}
	for len(branch) > maxGitBranch {
		branch = trimRune(branch)
	}
	return branch
}

// originURL returns the first url of [remote "origin"] in a git config file.
// Includes and url.*.insteadOf rewrites are not followed.
func originURL(config string) string {
	origin := false
	lines := bufio.NewScanner(strings.NewReader(config))
	lines.Buffer(make([]byte, 0, 4096), maxGitConfig)
	for lines.Scan() {
		line := strings.TrimSpace(lines.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				origin = false
				continue
			}
			name, sub, _ := strings.Cut(strings.TrimSpace(line[1:end]), " ")
			sub = strings.TrimSpace(sub)
			origin = strings.EqualFold(name, "remote") && sub == `"origin"` || strings.EqualFold(name, "remote.origin") && sub == ""
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !origin || !ok || !strings.EqualFold(strings.TrimSpace(key), "url") {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, `"`) {
			value, _, _ = strings.Cut(value[1:], `"`)
		} else if cut := strings.IndexAny(value, "#;"); cut >= 0 {
			value = strings.TrimSpace(value[:cut])
		}
		return value
	}
	return ""
}

// Git records live in <runtime>/git, one private file per exact session ID,
// written by the process bound to the session. The observer, the history
// tools and recent rows read them instead of opening project directories.
func gitPath(runtime, session string) string {
	sum := sha256.Sum256([]byte(session))
	return filepath.Join(runtime, "git", hex.EncodeToString(sum[:16]))
}

type gitRecord struct {
	SessionID string `json:"session_id"`
	gitInfo
}

// storeGit persists the labels of session; empty labels remove the record.
func storeGit(runtime, session string, g gitInfo) error {
	if runtime == "" {
		return errors.New("runtime directory not initialized")
	}
	path := gitPath(runtime, session)
	if g == (gitInfo{}) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := secureDir(filepath.Dir(path)); err != nil {
		return err
	}
	_, err := os.Lstat(path)
	created := os.IsNotExist(err)
	raw, _ := json.Marshal(gitRecord{SessionID: session, gitInfo: g})
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if created {
		pruneGit(filepath.Dir(path))
	}
	return nil
}

// pruneGit keeps the newest maxGitRecords records.
func pruneGit(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) <= maxGitRecords {
		return
	}
	type aged struct {
		name string
		at   time.Time
	}
	files := make([]aged, 0, len(entries))
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			files = append(files, aged{e.Name(), info.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].at.Before(files[j].at) })
	for i := 0; i < len(files)-maxGitRecords; i++ {
		_ = os.Remove(filepath.Join(dir, files[i].name))
	}
}

// loadGit returns the persisted labels of session, or empty labels when none
// are stored, the record names another session, or it exceeds the bounds.
func loadGit(runtime, session string) gitInfo {
	if runtime == "" {
		return gitInfo{}
	}
	raw, ok := readSmall(gitPath(runtime, session), maxGitPointer)
	if !ok {
		return gitInfo{}
	}
	var r gitRecord
	if json.Unmarshal([]byte(raw), &r) != nil || r.SessionID != session || !validGitLabels(r.Origin, r.Branch) || !gitLabel(r.Origin) || !gitLabel(r.Branch) {
		return gitInfo{}
	}
	return r.gitInfo
}

// gitRecords lists the stored record file names once per query, so rows
// without a record cost no read.
func gitRecords(runtime string) map[string]bool {
	entries, err := os.ReadDir(filepath.Join(runtime, "git"))
	if err != nil {
		return nil
	}
	out := make(map[string]bool, len(entries))
	for _, e := range entries {
		out[e.Name()] = true
	}
	return out
}

// knownGit loads the labels of session when records lists one for it.
func knownGit(runtime, session string, records map[string]bool) gitInfo {
	if !records[filepath.Base(gitPath(runtime, session))] {
		return gitInfo{}
	}
	return loadGit(runtime, session)
}

// updateGit refreshes the git labels of the bound session and reports whether
// the advertised identity changed; the caller re-advertises. A process bound
// to the session reads the working directory and persists the result; a
// passive observer only reads that record, and gateway proxies keep what the
// paired host sent. Labels never touch generation, streams, subscriptions,
// cursors or queues. Must be called without n.mu held.
func (n *Node) updateGit() bool {
	if n.bridge != nil {
		return false
	}
	n.gitMu.Lock()
	defer n.gitMu.Unlock()
	n.mu.Lock()
	session, gen, cwd, runtime, observed := n.identity.SessionId, n.identity.Generation, n.cwd, n.runtime, n.identity.ObservationOnly
	n.mu.Unlock()
	if session == "" {
		return false
	}
	var g gitInfo
	if observed {
		g = loadGit(runtime, session)
	} else if cwd != "" {
		g = detectGit(cwd)
	}
	n.mu.Lock()
	if n.identity.SessionId != session || n.identity.Generation != gen || n.cwd != cwd {
		n.mu.Unlock()
		return false
	}
	changed := identityGit(n.identity) != g
	g.apply(n.identity)
	store := !observed && runtime != "" && (changed || n.gitSynced != gen)
	n.gitSynced = gen
	n.mu.Unlock()
	if store {
		if err := storeGit(runtime, session, g); err != nil {
			slog.Debug("git_record", "error", err)
		}
	}
	return changed
}
