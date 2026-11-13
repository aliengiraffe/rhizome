package channel

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"rhizome/internal/localstate"
	pb "rhizome/proto"
)

func TestNormalizeGitOrigin(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"scp", "git@github.com:org/repo.git", "github.com/org/repo"},
		{"scp-no-user", "github.com:org/repo", "github.com/org/repo"},
		{"ssh", "ssh://git@github.com/org/repo", "github.com/org/repo"},
		{"ssh-port", "ssh://git@github.com:22/org/repo.git", "github.com/org/repo"},
		{"https", "https://github.com/org/repo", "github.com/org/repo"},
		{"https-git", "https://github.com/org/repo.git", "github.com/org/repo"},
		{"trailing-slash", "https://github.com/org/repo.git/", "github.com/org/repo"},
		{"uppercase-host", "https://GitHub.COM/Org/Repo.git", "github.com/Org/Repo"},
		{"uppercase-scp", "git@GITHUB.com:Org/Repo.git", "github.com/Org/Repo"},
		{"userinfo", "https://alice:s3cr3t-token@github.com/org/repo.git", "github.com/org/repo"},
		{"token-user", "https://ghp_abcdef0123456789@github.com/org/repo", "github.com/org/repo"},
		{"query-token", "https://github.com/org/repo.git?access_token=s3cr3t#frag", "github.com/org/repo"},
		{"scp-query", "git@github.com:org/repo.git?token=s3cr3t", "github.com/org/repo"},
		{"non-github", "git@gitlab.example.com:group/sub/repo.git", "gitlab.example.com/group/sub/repo"},
		{"non-github-https", "https://git.example.org:8443/scm/team/repo.git", "git.example.org/scm/team/repo"},
		{"git-scheme", "git://example.org/repo.git", "example.org/repo"},
		{"whitespace", "  https://github.com/org/repo.git\n", "github.com/org/repo"},
		{"empty", "", ""},
		{"local-path", "/srv/git/repo.git", ""},
		{"relative-path", "../repo.git", ""},
		{"file-scheme", "file:///srv/git/repo.git", ""},
		{"windows-drive", `C:\repos\repo.git`, ""},
		{"no-path", "https://github.com/", ""},
		{"control", "https://github.com/org/re\x00po", ""},
		{"inner-space", "https://github.com/org/re po", ""},
		{"escaped-control", "https://github.com/org/re%0Apo", ""},
		{"bad-url", "https://[::1/org/repo", ""},
		{"too-long", "https://github.com/" + strings.Repeat("a", maxGitOrigin), ""},
	} {
		got := normalizeGitOrigin(tc.in)
		if got != tc.want {
			t.Errorf("%s: normalizeGitOrigin(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
		if strings.Contains(got, "s3cr3t") || strings.Contains(got, "ghp_") || strings.Contains(got, "@") {
			t.Errorf("%s: credential material in %q", tc.name, got)
		}
		// Normalization is idempotent through the search filter.
		if got != "" && searchOrigin(got) != got {
			t.Errorf("%s: searchOrigin(%q) = %q", tc.name, got, searchOrigin(got))
		}
	}
	if len(normalizeGitOrigin("https://github.com/"+strings.Repeat("a", maxGitOrigin-len("github.com/")))) != maxGitOrigin {
		t.Error("origin at the bound must be kept")
	}
}

// gitSecret is embedded in every fixture remote; it must never be advertised.
const gitSecret = "s3cr3t-fixture-token"

func writeGitFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// gitFixture writes repository metadata by hand, so the cases need no git
// binary. It returns the session working directory.
func gitFixture(t *testing.T, kind string) string {
	t.Helper()
	root := shortDir(t)
	main := filepath.Join(root, "main")
	config := "[core]\n\tbare = false\n[remote \"upstream\"]\n\turl = https://example.org/other/fork.git\n[remote \"origin\"]\n\turl = https://bot:" + gitSecret + "@GitHub.com/Org/Repo.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n[branch \"main\"]\n\tremote = origin\n"
	switch kind {
	case "no-repo":
		if err := os.MkdirAll(main, 0700); err != nil {
			t.Fatal(err)
		}
		return main
	case "no-origin":
		config = "[core]\n\tbare = false\n[remote \"upstream\"]\n\turl = https://example.org/other/fork.git\n"
	}
	head := "ref: refs/heads/main\n"
	if kind == "detached" {
		head = "0123456789abcdef0123456789abcdef01234567\n"
	}
	writeGitFile(t, filepath.Join(main, ".git", "HEAD"), head)
	writeGitFile(t, filepath.Join(main, ".git", "config"), config)
	switch kind {
	case "worktree", "worktree-subdir":
		tree := filepath.Join(root, "trees", "issue-128")
		private := filepath.Join(main, ".git", "worktrees", "issue-128")
		writeGitFile(t, filepath.Join(tree, ".git"), "gitdir: "+private+"\n")
		writeGitFile(t, filepath.Join(private, "HEAD"), "ref: refs/heads/feature/issue-128\n")
		writeGitFile(t, filepath.Join(private, "commondir"), "../..\n")
		if kind == "worktree-subdir" {
			tree = filepath.Join(tree, "internal", "pkg")
			if err := os.MkdirAll(tree, 0700); err != nil {
				t.Fatal(err)
			}
		}
		return tree
	case "subdir":
		sub := filepath.Join(main, "internal", "pkg")
		if err := os.MkdirAll(sub, 0700); err != nil {
			t.Fatal(err)
		}
		return sub
	case "submodule":
		// A submodule's .git is a file too, but it has no commondir.
		module := filepath.Join(main, "vendor", "lib")
		private := filepath.Join(main, ".git", "modules", "lib")
		writeGitFile(t, filepath.Join(module, ".git"), "gitdir: ../../.git/modules/lib\n")
		writeGitFile(t, filepath.Join(private, "HEAD"), "ref: refs/heads/stable\n")
		writeGitFile(t, filepath.Join(private, "config"), "[remote \"origin\"]\n\turl = git@gitlab.example.com:group/lib.git\n")
		return module
	}
	return main
}

// gitCases are the repository shapes every agent must report identically.
var gitCases = []struct {
	kind string
	want gitInfo
}{
	{"repo", gitInfo{Origin: "github.com/Org/Repo", Branch: "main"}},
	{"worktree", gitInfo{Origin: "github.com/Org/Repo", Branch: "feature/issue-128", WorktreeRoot: true}},
	{"detached", gitInfo{Origin: "github.com/Org/Repo"}},
	{"no-repo", gitInfo{}},
	{"no-origin", gitInfo{Branch: "main"}},
}

func TestDetectGit(t *testing.T) {
	for _, tc := range gitCases {
		if got := detectGit(gitFixture(t, tc.kind)); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.kind, got, tc.want)
		}
	}
	for _, tc := range []struct {
		kind string
		want gitInfo
	}{
		{"subdir", gitInfo{Origin: "github.com/Org/Repo", Branch: "main"}},
		// Only the top level of a linked worktree sets the flag.
		{"worktree-subdir", gitInfo{Origin: "github.com/Org/Repo", Branch: "feature/issue-128"}},
		{"submodule", gitInfo{Origin: "gitlab.example.com/group/lib", Branch: "stable"}},
	} {
		if got := detectGit(gitFixture(t, tc.kind)); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.kind, got, tc.want)
		}
	}
	if got := detectGit("relative/dir"); got != (gitInfo{}) {
		t.Errorf("relative directory: %+v", got)
	}

	// Malformed or hostile metadata yields empty labels, never an error.
	dir := gitFixture(t, "repo")
	writeGitFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/"+strings.Repeat("ü", 60)+"\n")
	if got := detectGit(dir).Branch; len(got) != maxGitBranch || got != strings.Repeat("ü", 48) {
		t.Errorf("long branch not cut on a rune boundary: %d bytes", len(got))
	}
	writeGitFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/.invalid\n")
	if got := detectGit(dir).Branch; got != "" {
		t.Errorf("reftable placeholder reported as branch %q", got)
	}
	writeGitFile(t, filepath.Join(dir, ".git", "config"), "[remote \"origin\"]\n\turl = "+strings.Repeat("x", maxGitConfig)+"\n")
	if got := detectGit(dir).Origin; got != "" {
		t.Errorf("oversized config read: %q", got)
	}
	if err := os.Remove(filepath.Join(dir, ".git", "HEAD")); err != nil {
		t.Fatal(err)
	}
	if got := detectGit(dir); got != (gitInfo{}) {
		t.Errorf("directory without HEAD is not a repository: %+v", got)
	}
	stray := shortDir(t)
	writeGitFile(t, filepath.Join(stray, ".git"), "not a pointer\n")
	if got := detectGit(stray); got != (gitInfo{}) {
		t.Errorf("stray .git file: %+v", got)
	}
}

func TestOriginURL(t *testing.T) {
	for _, tc := range []struct{ name, config, want string }{
		{"plain", "[remote \"origin\"]\n\turl = a:b/c\n", "a:b/c"},
		{"first-url-wins", "[remote \"origin\"]\n\turl = a:b/c\n\turl = d:e/f\n", "a:b/c"},
		{"quoted", "[remote \"origin\"]\n\turl = \"a:b/c\" ; note\n", "a:b/c"},
		{"comment", "[remote \"origin\"]\n\turl = a:b/c # note\n", "a:b/c"},
		{"case", "[REMOTE \"origin\"]\n\tURL=a:b/c\n", "a:b/c"},
		{"legacy-section", "[remote.origin]\n\turl = a:b/c\n", "a:b/c"},
		{"other-remote", "[remote \"Origin\"]\n\turl = a:b/c\n[remote \"origin2\"]\n\turl = x:y/z\n", ""},
		{"pushurl-only", "[remote \"origin\"]\n\tpushurl = a:b/c\n", ""},
		{"section-ends", "[remote \"origin\"]\n[core]\n\turl = a:b/c\n", ""},
		{"commented-out", "[remote \"origin\"]\n\t# url = a:b/c\n", ""},
	} {
		if got := originURL(tc.config); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestDetectGitMatchesGit checks the hand-read metadata layout against a real
// repository with a linked worktree, when a git binary is available.
func TestDetectGitMatchesGit(t *testing.T) {
	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	root := shortDir(t)
	if root, err = filepath.EvalSymlinks(root); err != nil {
		t.Fatal(err)
	}
	main, tree := filepath.Join(root, "main"), filepath.Join(root, "tree")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.org", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.org"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(main, 0700); err != nil {
		t.Fatal(err)
	}
	git(main, "init", "-q", "-b", "main")
	git(main, "remote", "add", "origin", "https://bot:"+gitSecret+"@github.com/org/repo.git")
	git(main, "commit", "-q", "--allow-empty", "-m", "init")
	git(main, "worktree", "add", "-q", "-b", "topic", tree)
	if got, want := detectGit(main), (gitInfo{Origin: "github.com/org/repo", Branch: "main"}); got != want {
		t.Errorf("main checkout: %+v, want %+v", got, want)
	}
	if got, want := detectGit(tree), (gitInfo{Origin: "github.com/org/repo", Branch: "topic", WorktreeRoot: true}); got != want {
		t.Errorf("linked worktree: %+v, want %+v", got, want)
	}
	git(tree, "checkout", "-q", "--detach")
	if got, want := detectGit(tree), (gitInfo{Origin: "github.com/org/repo", WorktreeRoot: true}); got != want {
		t.Errorf("detached worktree: %+v, want %+v", got, want)
	}
}

// gitSettled waits for the bind-time detection of n's current generation.
func gitSettled(t *testing.T, n *Node, dir string) *pb.Identity {
	t.Helper()
	eventually(t, func() bool {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.identity.SessionId != "" && n.cwd == dir && n.gitSynced == n.identity.Generation
	})
	return n.local()
}

// checkGitIdentity asserts the labels, the stored record and that no fixture
// credential reaches any peer-visible form of the identity.
func checkGitIdentity(t *testing.T, agent, kind string, n *Node, dir string, want gitInfo) {
	t.Helper()
	id := gitSettled(t, n, dir)
	if id.AgentType != agent {
		t.Fatalf("%s/%s: agent type %q", agent, kind, id.AgentType)
	}
	if got := identityGit(id); got != want {
		t.Errorf("%s/%s: identity %+v, want %+v", agent, kind, got, want)
	}
	if got := loadGit(n.runtime, id.SessionId); got != want {
		t.Errorf("%s/%s: stored record %+v, want %+v", agent, kind, got, want)
	}
	listed, _ := json.Marshal(n.listSessions())
	wire, _ := json.Marshal(id)
	record, _ := os.ReadFile(gitPath(n.runtime, id.SessionId))
	for _, visible := range [][]byte{listed, wire, n.NodeMeta(512), record} {
		if strings.Contains(string(visible), gitSecret) || strings.Contains(string(visible), "bot:") {
			t.Fatalf("%s/%s: credential leaked: %s", agent, kind, visible)
		}
	}
	if want.Origin != "" && !strings.Contains(string(listed), `"git_origin":"`+want.Origin+`"`) {
		t.Errorf("%s/%s: list_sessions lacks git_origin: %s", agent, kind, listed)
	}
	if !validGatewayIdentity(&pb.Identity{Protocol: 1, GatewayVersion: 1, NodeId: id.NodeId, SessionId: id.SessionId, Generation: id.Generation, AgentType: agent, GitOrigin: id.GitOrigin, GitBranch: id.GitBranch, GitWorktreeRoot: id.GitWorktreeRoot}, true) {
		t.Errorf("%s/%s: labels rejected by gateway bounds", agent, kind)
	}
}

func TestGitIdentityClaude(t *testing.T) {
	for _, tc := range gitCases {
		n := node(t)
		if err := n.start(Options{Runtime: filepath.Join(t.TempDir(), "runtime")}); err != nil {
			t.Fatal(err)
		}
		dir := gitFixture(t, tc.kind)
		bind(t, n, "claude-"+tc.kind, dir)
		checkGitIdentity(t, "claude", tc.kind, n, dir, tc.want)
	}
}

func TestGitIdentityCodex(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := &codexNodes{ctx: ctx, nodes: map[string]*Node{}, opts: Options{Runtime: filepath.Join(t.TempDir(), "run")}, binary: "/bin/false", socket: "/nonexistent.sock"}
	defer manager.close()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := makeServer(manager.resolve, agentCodex, func() {}, nil).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "codex-test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	call := func(thread, name string, args any) {
		t.Helper()
		r, err := cs.CallTool(ctx, &mcp.CallToolParams{Meta: mcp.Meta{"threadId": thread}, Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		if r.IsError {
			t.Fatal(r.Content[0].(*mcp.TextContent).Text)
		}
	}
	for i, tc := range gitCases {
		thread := strings.Replace(threadA, "1111", string(rune('a'+i))+"000", 1)
		dir := gitFixture(t, tc.kind)
		call(thread, "list_sessions", map[string]any{})
		manager.nodes[thread].codex.reader = &fakeCodex{thread: thread, status: "idle", items: map[string][]codexItem{}}
		call(thread, "observe_session", map[string]any{"session_id": thread, "cwd": dir, "hook_event_name": "UserPromptSubmit", "transcript_path": nil})
		checkGitIdentity(t, "codex", tc.kind, manager.nodes[thread], dir, tc.want)
	}
}

func TestGitIdentityOpenCode(t *testing.T) {
	m, _ := openCodeManager(t)
	for _, tc := range gitCases {
		dir := gitFixture(t, tc.kind)
		session := "ses_" + strings.ReplaceAll(tc.kind, "-", "")
		m.handle(bindFrame(session, "", dir, ""))
		n, err := m.resolve(callerReq(mcp.Meta{openCodeMetaKey: session}))
		if err != nil {
			t.Fatal(err)
		}
		checkGitIdentity(t, "opencode", tc.kind, n, dir, tc.want)
	}
}

func TestGitIdentityPi(t *testing.T) {
	for _, tc := range gitCases {
		s, path := startPi(t)
		dir := gitFixture(t, tc.kind)
		p := dialPlugin(t, path, true)
		p.send(t, bridgeFrame{Type: "bind", Nonce: testPiNonce, SessionID: "01a184cd-c0f1-704a-998b-dbcef11fd346", Directory: dir})
		checkGitIdentity(t, "pi", tc.kind, waitBound(t, s), dir, tc.want)
	}
}

// TestGitLabelsNeverRoute proves the labels are not part of any routing key:
// sessions sharing an origin stay distinct, a branch switch reaches peers
// without touching generation or streams, and the registry stays label-free.
func TestGitLabelsNeverRoute(t *testing.T) {
	a, b := node(t), node(t)
	runtime := filepath.Join(t.TempDir(), "runtime")
	for _, n := range []*Node{a, b} {
		if err := n.start(Options{Runtime: runtime}); err != nil {
			t.Fatal(err)
		}
	}
	dir := gitFixture(t, "repo")
	bind(t, a, "session-a", dir)
	bind(t, b, "session-b", dir)
	if a.local().GitOrigin != "github.com/Org/Repo" || b.local().GitOrigin != a.local().GitOrigin {
		t.Fatalf("origins %q %q", a.local().GitOrigin, b.local().GitOrigin)
	}
	eventually(t, func() bool { _, err := b.resolve(context.Background(), "session-a"); return err == nil })
	for _, label := range []string{"github.com/Org/Repo", "main", dir} {
		if _, err := b.resolve(context.Background(), label); err == nil {
			t.Fatalf("label %q resolved as an address", label)
		}
	}
	before := a.local()
	a.mu.Lock()
	stream := a.stream
	a.mu.Unlock()
	result, err := b.send(context.Background(), "session-a", "hello", "")
	if err != nil || !result.Accepted {
		t.Fatalf("send: %v %v", result, err)
	}

	writeGitFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/topic\n")
	if !a.updateGit() {
		t.Fatal("branch switch not detected")
	}
	a.advertise()
	if a.updateGit() {
		t.Fatal("unchanged labels reported as a change")
	}
	after := a.local()
	a.mu.Lock()
	sameStream := a.stream == stream
	a.mu.Unlock()
	if after.GitBranch != "topic" || !matches(before, after) || after.Endpoint != before.Endpoint || after.GossipEndpoint != before.GossipEndpoint || !sameStream {
		t.Fatalf("label change touched routing identity: %+v -> %+v", before, after)
	}
	// A changed remote reaches the peer through discovery, without a rebind.
	writeGitFile(t, filepath.Join(dir, ".git", "config"), "[remote \"origin\"]\n\turl = git@example.org:team/moved.git\n")
	if !a.updateGit() {
		t.Fatal("remote change not detected")
	}
	a.advertise()
	eventually(t, func() bool {
		for _, p := range b.peers() {
			if p.SessionId == "session-a" && p.GitOrigin == "example.org/team/moved" {
				return true
			}
		}
		return false
	})
	after = a.local()
	if !matches(before, after) {
		t.Fatalf("label change touched routing identity: %+v -> %+v", before, after)
	}
	target, err := b.resolve(context.Background(), "session-a")
	if err != nil || target.NodeId != before.NodeId || target.Generation != before.Generation {
		t.Fatalf("resolve after label change: %+v %v", target, err)
	}
	raw, _ := json.Marshal(registrationRecord(after))
	if strings.Contains(string(raw), "git_") {
		t.Fatalf("registry carries git labels: %s", raw)
	}

	// Leaving the repository clears the labels and the stored record.
	if err := a.observe(Hook{SessionID: "session-a", TranscriptPath: filepath.Join(dir, "session-a.jsonl"), Cwd: gitFixture(t, "no-repo"), Event: "UserPromptSubmit"}); err != nil {
		t.Fatal(err)
	}
	if got := identityGit(a.local()); got != (gitInfo{}) {
		t.Fatalf("labels kept outside a repository: %+v", got)
	}
	if _, err := os.Stat(gitPath(runtime, "session-a")); !os.IsNotExist(err) {
		t.Fatalf("stale git record: %v", err)
	}
}

func TestGitPeriodicRefresh(t *testing.T) {
	n := node(t)
	n.gitRefresh = 20 * time.Millisecond
	if err := n.start(Options{Runtime: filepath.Join(t.TempDir(), "runtime")}); err != nil {
		t.Fatal(err)
	}
	dir := gitFixture(t, "repo")
	bind(t, n, "refresh", dir)
	writeGitFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/later\n")
	eventually(t, func() bool { return n.local().GitBranch == "later" })
	if n.local().Generation != 1 {
		t.Fatalf("refresh changed generation: %d", n.local().Generation)
	}
}

// An observer publisher never reads the project directory: it adopts the
// record written by the session's own process.
func TestObservedNodeAdoptsGitRecord(t *testing.T) {
	n := node(t)
	n.identity.SessionId = "observed"
	n.identity.Generation = 1
	n.identity.AgentType = "codex"
	n.identity.ObservationOnly = true
	n.cwd = gitFixture(t, "repo")
	if err := n.start(Options{Runtime: filepath.Join(t.TempDir(), "runtime")}); err != nil {
		t.Fatal(err)
	}
	if n.updateGit() || identityGit(n.local()) != (gitInfo{}) {
		t.Fatalf("observer read the project directory: %+v", identityGit(n.local()))
	}
	want := gitInfo{Origin: "example.org/team/app", Branch: "main", WorktreeRoot: true}
	if err := storeGit(n.runtime, "observed", want); err != nil {
		t.Fatal(err)
	}
	if !n.updateGit() || identityGit(n.local()) != want || n.local().Generation != 1 {
		t.Fatalf("observed node labels %+v", identityGit(n.local()))
	}
	if err := storeGit(n.runtime, "observed", gitInfo{}); err != nil {
		t.Fatal(err)
	}
	if !n.updateGit() || identityGit(n.local()) != (gitInfo{}) {
		t.Fatalf("cleared record kept: %+v", identityGit(n.local()))
	}
}

func TestGitRecordStore(t *testing.T) {
	runtime := t.TempDir()
	want := gitInfo{Origin: "example.org/team/app", Branch: "main"}
	if err := storeGit(runtime, "s1", want); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(gitPath(runtime, "s1")); err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("record mode: %v %v", st, err)
	}
	if got := loadGit(runtime, "s1"); got != want {
		t.Fatalf("loaded %+v", got)
	}
	if got := loadGit(runtime, "s2"); got != (gitInfo{}) {
		t.Fatalf("unknown session %+v", got)
	}
	if got := knownGit(runtime, "s1", gitRecords(runtime)); got != want {
		t.Fatalf("listed record %+v", got)
	}
	// A record naming another session, or one outside the bounds, is ignored.
	for _, raw := range []string{
		`{"session_id":"other","git_origin":"example.org/a/b"}`,
		`{"session_id":"s1","git_origin":"` + strings.Repeat("a", maxGitOrigin+1) + `"}`,
		`{"session_id":"s1","git_branch":"` + strings.Repeat("b", maxGitBranch+1) + `"}`,
		`{"session_id":"s1","git_origin":"example.org/a b"}`,
		`not json`,
	} {
		if err := os.WriteFile(gitPath(runtime, "s1"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if got := loadGit(runtime, "s1"); got != (gitInfo{}) {
			t.Fatalf("accepted %s: %+v", raw, got)
		}
	}
	if err := storeGit(runtime, "s1", gitInfo{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(gitPath(runtime, "s1")); !os.IsNotExist(err) {
		t.Fatalf("empty labels kept a record: %v", err)
	}
	if err := storeGit("", "s1", want); err == nil {
		t.Fatal("stored without a runtime directory")
	}

	// The directory keeps only the newest maxGitRecords files.
	dir := filepath.Join(runtime, "git")
	old := time.Now().Add(-time.Hour)
	for i := 0; i < maxGitRecords+3; i++ {
		path := filepath.Join(dir, "old-"+strings.Repeat("0", 4)+string(rune('a'+i%26))+strings.Repeat("x", i/26))
		if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := storeGit(runtime, "newest", want); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != maxGitRecords {
		t.Fatalf("records after prune: %d %v", len(entries), err)
	}
	if got := loadGit(runtime, "newest"); got != want {
		t.Fatalf("newest record pruned: %+v", got)
	}
}

func TestNodeMetaBudgetWithGitLabels(t *testing.T) {
	n := node(t)
	n.identity.SessionId = strings.Repeat("s", 128)
	n.identity.Generation = 7
	n.identity.Endpoint = "127.0.0.1:65535"
	n.identity.GossipEndpoint = "127.0.0.1:65535"
	n.identity.Host = "laptop"
	n.identity.DisplayName = "rhizome"
	n.identity.Project = "/home/dev/git/rhizome"
	n.identity.SessionId = "0199c0de-0000-7000-8000-000000000000"
	n.identity.GitOrigin = "github.com/aliengiraffe/rhizome"
	n.identity.GitBranch = "main"
	n.identity.GitWorktreeRoot = true
	n.resolveNameLocked()
	var got pb.Identity
	if err := json.Unmarshal(n.NodeMeta(512), &got); err != nil {
		t.Fatal(err)
	}
	if identityGit(&got) != identityGit(n.identity) || got.Project != n.identity.Project {
		t.Fatalf("typical labels trimmed: %+v", &got)
	}

	// Maximum labels: the branch goes first, then the other labels, and the
	// routing fields always survive.
	n.identity.SessionId = strings.Repeat("s", 128)
	n.identity.DisplayName = strings.Repeat("d", 40)
	n.identity.Project = "/" + strings.Repeat("p", 179)
	n.identity.GitOrigin = "example.org/" + strings.Repeat("o", maxGitOrigin-len("example.org/"))
	n.identity.GitBranch = strings.Repeat("b", maxGitBranch)
	n.names.explicit = strings.Repeat("n", maxSessionName)
	n.resolveNameLocked()
	meta := n.NodeMeta(512)
	if meta == nil || len(meta) > 512 {
		t.Fatalf("advertisement dropped or oversized: %d", len(meta))
	}
	got = pb.Identity{}
	if err := json.Unmarshal(meta, &got); err != nil {
		t.Fatal(err)
	}
	if got.NodeId != n.identity.NodeId || got.SessionId != n.identity.SessionId || got.Generation != 7 || got.Endpoint != "127.0.0.1:65535" || got.GossipEndpoint != "127.0.0.1:65535" {
		t.Fatal("routing metadata trimmed")
	}
	if got.GitBranch != "" || got.GitOrigin != "" && got.GitOrigin != n.identity.GitOrigin {
		t.Fatalf("git labels must be dropped whole, never cut: %q %q", got.GitBranch, got.GitOrigin)
	}
}

func TestGatewayGitBounds(t *testing.T) {
	base := func() *pb.Identity {
		return &pb.Identity{Protocol: 1, GatewayVersion: 1, NodeId: id(), SessionId: "s", Generation: 1, AgentType: "claude"}
	}
	// Identities from peers that predate the fields stay valid.
	if !validGatewayIdentity(base(), false) {
		t.Fatal("identity without git labels rejected")
	}
	for _, tc := range []struct {
		name           string
		origin, branch string
		ok             bool
	}{
		{"typical", "github.com/org/repo", "main", true},
		{"at-bounds", strings.Repeat("o", maxGitOrigin), strings.Repeat("b", maxGitBranch), true},
		{"origin-over", strings.Repeat("o", maxGitOrigin+1), "main", false},
		{"branch-over", "github.com/org/repo", strings.Repeat("b", maxGitBranch+1), false},
		{"origin-invalid-utf8", "github.com/org/\xff", "main", false},
		{"branch-invalid-utf8", "github.com/org/repo", "\xff", false},
	} {
		p := base()
		p.GitOrigin, p.GitBranch, p.GitWorktreeRoot = tc.origin, tc.branch, true
		if validGatewayIdentity(p, false) != tc.ok {
			t.Errorf("%s: valid = %v, want %v", tc.name, !tc.ok, tc.ok)
		}
		r := recentSession{SessionID: "s", AgentType: "claude", LastActivityAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), gitInfo: gitInfo{Origin: tc.origin, Branch: tc.branch}}
		if validRecentSession(r, false) != tc.ok {
			t.Errorf("%s: recent row valid = %v, want %v", tc.name, !tc.ok, tc.ok)
		}
	}
	if maxGitOrigin != 180 || maxGitBranch != 96 {
		t.Fatal("documented bounds changed")
	}
}

// legacyIdentity and legacyRecent are the JSON shapes a peer released before
// the git labels decodes gateway frames into.
type legacyIdentity struct {
	Protocol        uint32 `json:"protocol,omitempty"`
	NodeID          string `json:"node_id,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	Generation      uint64 `json:"generation,omitempty"`
	DisplayName     string `json:"display_name,omitempty"`
	Project         string `json:"project,omitempty"`
	AgentType       string `json:"agent_type,omitempty"`
	SessionName     string `json:"session_name,omitempty"`
	Host            string `json:"host,omitempty"`
	GatewayVersion  uint32 `json:"gateway_version,omitempty"`
	ObservationOnly bool   `json:"observation_only,omitempty"`
}
type legacyRecent struct {
	SessionID      string    `json:"session_id"`
	AgentType      string    `json:"agent_type"`
	Project        string    `json:"project,omitempty"`
	LastActivityAt time.Time `json:"last_activity_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}
type legacyFrame struct {
	Type     string           `json:"type"`
	Sessions []legacyIdentity `json:"sessions,omitempty"`
	Recent   []legacyRecent   `json:"recent,omitempty"`
}

// TestGatewayGitCompatibility exchanges snapshot frames with the wire shape of
// a peer that predates the labels, in both directions.
func TestGatewayGitCompatibility(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	labels := gitInfo{Origin: "github.com/org/repo", Branch: "main", WorktreeRoot: true}
	current := &pb.Identity{Protocol: 1, GatewayVersion: 1, NodeId: id(), SessionId: "new", Generation: 3, AgentType: "codex", Project: "/work/app"}
	labels.apply(current)
	raw, err := json.Marshal(gatewayFrame{Type: "snapshot", Sessions: []*pb.Identity{current}, Recent: []recentSession{{SessionID: "r", AgentType: "claude", Project: "/work/app", gitInfo: labels, LastActivityAt: now, ExpiresAt: now.Add(time.Hour)}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"git_origin":"github.com/org/repo"`) || strings.Count(string(raw), `"git_worktree_root":true`) != 2 {
		t.Fatalf("new frame lacks labels: %s", raw)
	}
	// New to old: the unknown keys are ignored and the known fields survive.
	var old legacyFrame
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatalf("legacy peer rejects a new frame: %v", err)
	}
	if len(old.Sessions) != 1 || old.Sessions[0].SessionID != "new" || old.Sessions[0].Generation != 3 || old.Sessions[0].Project != "/work/app" || len(old.Recent) != 1 || old.Recent[0].SessionID != "r" {
		t.Fatalf("legacy peer decoded %+v", old)
	}
	// Old to new: frames without the keys decode to valid, label-free rows.
	raw, err = json.Marshal(legacyFrame{Type: "snapshot", Sessions: []legacyIdentity{{Protocol: 1, GatewayVersion: 1, NodeID: id(), SessionID: "old", Generation: 1, AgentType: "claude", Project: "/work/app"}}, Recent: []legacyRecent{{SessionID: "r", AgentType: "claude", LastActivityAt: now, ExpiresAt: now.Add(time.Hour)}}})
	if err != nil {
		t.Fatal(err)
	}
	var f gatewayFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Sessions) != 1 || !validGatewayIdentity(f.Sessions[0], false) || identityGit(f.Sessions[0]) != (gitInfo{}) {
		t.Fatalf("legacy identity %+v", f.Sessions)
	}
	if len(f.Recent) != 1 || !validRecentSession(f.Recent[0], false) || f.Recent[0].gitInfo != (gitInfo{}) {
		t.Fatalf("legacy recent row %+v", f.Recent)
	}
}

// TestGatewayCarriesGitLabels pairs two hosts and follows the labels of a
// session across the link, including a later branch switch.
func TestGatewayCarriesGitLabels(t *testing.T) {
	a, b := startGatewayHarness(t), startGatewayHarness(t)
	na := node(t)
	if err := na.start(Options{Runtime: a.runtime}); err != nil {
		t.Fatal(err)
	}
	dir := gitFixture(t, "worktree")
	bind(t, na, "gateway-git-a", dir)
	nb := gatewayTestNode(t, b, "gateway-git-b")
	b.command(t, GatewayCommand{Action: "join", Code: a.invite(t)})
	remote := func() *pb.Identity {
		for _, p := range nb.peers() {
			if p.SessionId == "gateway-git-a" && p.GatewayHost != "" {
				return p
			}
		}
		return nil
	}
	want := gitInfo{Origin: "github.com/Org/Repo", Branch: "feature/issue-128", WorktreeRoot: true}
	// The proxy's gossip budget may drop the branch, never cut it; the origin
	// and the worktree flag outlive the other labels.
	arrived := func(p *pb.Identity, origin string) bool {
		return p != nil && p.GitOrigin == origin && p.GitWorktreeRoot && (p.GitBranch == "" || p.GitBranch == want.Branch)
	}
	gatewayUntil(t, func() bool { return arrived(remote(), want.Origin) })
	// The peer without a repository arrives label-free on the other side.
	gatewayUntil(t, func() bool {
		for _, p := range na.peers() {
			if p.SessionId == "gateway-git-b" && p.GatewayHost != "" {
				return identityGit(p) == gitInfo{}
			}
		}
		return false
	})
	generation := remote().Generation
	writeGitFile(t, filepath.Join(filepath.Dir(filepath.Dir(dir)), "main", ".git", "config"), "[remote \"origin\"]\n\turl = git@example.org:team/moved.git\n")
	if !na.updateGit() {
		t.Fatal("remote change not detected")
	}
	na.advertise()
	// A gossip broadcast can be missed and is then repaired by memberlist's
	// periodic full sync (15 s), so this wait outlasts two of them.
	deadline := time.Now().Add(45 * time.Second)
	for !arrived(remote(), "example.org/team/moved") {
		if time.Now().After(deadline) {
			t.Fatalf("label update never crossed the link: %+v", identityGit(remote()))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if p := remote(); p.Generation != generation {
		t.Fatalf("label update replaced the proxy: %+v", p)
	}
	result, err := nb.send(context.Background(), "gateway-git-a", "across the link", "")
	if err != nil || !result.Accepted {
		t.Fatalf("send: %v %v", result, err)
	}
}

func TestHistorySearchGitOrigin(t *testing.T) {
	hh := newHistoryHarness(t)
	base := time.Now().UTC().Truncate(time.Second)
	ts := func(d time.Duration) string { return base.Add(d).Format(time.RFC3339Nano) }
	proj := filepath.Join(hh.o.opts.ClaudeRoot, "p")
	if err := os.MkdirAll(proj, 0700); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"main-checkout", "worktree", "other-repo", "no-record"} {
		appendLog(t, filepath.Join(proj, id+".jsonl"), assistantText(id, "/work/"+id, "a", ts(-time.Duration(i+1)*time.Minute), "x"))
	}
	hh.index()
	hh.h.clock = func() time.Time { return base }
	lock, err := localstate.Lock(filepath.Join(hh.scope.Runtime, "observer.lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	records := map[string]gitInfo{
		"main-checkout": {Origin: "github.com/org/repo", Branch: "main"},
		"worktree":      {Origin: "github.com/org/repo", Branch: "feature/x", WorktreeRoot: true},
		"other-repo":    {Origin: "gitlab.example.com/group/app", Branch: "main"},
	}
	for session, g := range records {
		if err := storeGit(hh.scope.Runtime, session, g); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(m map[string]any) string {
		var out []string
		for _, r := range sessionsOf(m) {
			out = append(out, r["session_id"].(string))
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct{ name, filter, want string }{
		{"normalized", "github.com/org/repo", "main-checkout,worktree"},
		{"scp", "git@github.com:org/repo.git", "main-checkout,worktree"},
		{"ssh", "ssh://git@github.com/org/repo", "main-checkout,worktree"},
		{"https-credentials", "https://user:token@GitHub.com/org/repo.git", "main-checkout,worktree"},
		{"other", "https://gitlab.example.com/group/app", "other-repo"},
		{"path-case-sensitive", "github.com/Org/repo", ""},
		{"no-prefix-match", "github.com/org", ""},
		{"unknown", "github.com/org/missing", ""},
	} {
		args, _ := json.Marshal(map[string]string{"git_origin": tc.filter})
		out, herr := hh.search(string(args))
		if herr != nil {
			t.Fatalf("%s: %v", tc.name, herr)
		}
		if got := ids(out); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	out, herr := hh.search(`{"git_origin":"github.com/org/repo","agent_type":"codex"}`)
	if herr != nil || ids(out) != "" {
		t.Fatalf("filters must combine: %v %v", ids(out), herr)
	}
	for _, args := range []string{`{"git_origin":""}`, `{"git_origin":"/srv/git/repo.git"}`, `{"git_origin":"not a remote"}`, `{"git_origin":5}`, `{"git_origin":"https://github.com/` + strings.Repeat("a", 2048) + `"}`} {
		if _, herr := hh.search(args); herr == nil || herr.Code != "INVALID_ARGUMENT" {
			t.Fatalf("%s accepted: %v", args, herr)
		}
	}

	// Every row carries the labels of its record; rows without one omit them.
	out, herr = hh.search(`{}`)
	if herr != nil {
		t.Fatal(herr)
	}
	for _, row := range sessionsOf(out) {
		session := row["session_id"].(string)
		want, got := records[session], gitInfo{}
		got.Origin, _ = row["git_origin"].(string)
		got.Branch, _ = row["git_branch"].(string)
		got.WorktreeRoot, _ = row["git_worktree_root"].(bool)
		if got != want {
			t.Errorf("%s: row labels %+v, want %+v", session, got, want)
		}
		if _, present := row["git_origin"]; present != (want.Origin != "") {
			t.Errorf("%s: git_origin presence %v", session, present)
		}
	}
	// A cursor is bound to the filter it was issued for.
	page, herr := hh.search(`{"git_origin":"github.com/org/repo","limit":1}`)
	if herr != nil || ids(page) != "main-checkout" || cursorOf(page) == "" {
		t.Fatalf("first page %v %v", ids(page), herr)
	}
	if _, herr := hh.search(`{"git_origin":"gitlab.example.com/group/app","limit":1,"cursor":"` + cursorOf(page) + `"}`); herr == nil {
		t.Fatal("cursor reused with another git_origin")
	}
	next, herr := hh.search(`{"git_origin":"git@github.com:org/repo.git","limit":1,"cursor":"` + cursorOf(page) + `"}`)
	if herr != nil || ids(next) != "worktree" {
		t.Fatalf("second page %v %v", ids(next), herr)
	}

	// Recent rows in list_sessions carry the same labels.
	a := hh.n
	if err := a.start(Options{Runtime: hh.scope.Runtime}); err != nil {
		t.Fatal(err)
	}
	a.archive = hh.h
	a.recentWindow = 24 * time.Hour
	bind(t, a, "caller", t.TempDir())
	seen := 0
	for _, raw := range roundTrip(t, a.listSessions())["sessions"].([]any) {
		identity := raw.(map[string]any)["identity"].(map[string]any)
		session := identity["session_id"].(string)
		if session == "caller" {
			continue
		}
		want, got := records[session], gitInfo{}
		got.Origin, _ = identity["git_origin"].(string)
		got.Branch, _ = identity["git_branch"].(string)
		got.WorktreeRoot, _ = identity["git_worktree_root"].(bool)
		if got != want {
			t.Errorf("recent %s: labels %+v, want %+v", session, got, want)
		}
		seen++
	}
	if seen != 4 {
		t.Fatalf("recent rows listed: %d", seen)
	}
}
