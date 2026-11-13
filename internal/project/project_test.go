package project

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNormalizeRemote(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
	}{
		{"git@github.com:Acme/App.git", "github.com/Acme/App"},
		{"https://github.com/Acme/App.git", "github.com/Acme/App"},
		{"https://github.com/Acme/App", "github.com/Acme/App"},
		{"https://GitHub.COM/Acme/App/", "github.com/Acme/App"},
		{"ssh://git@github.com:22/Acme/App.git", "github.com/Acme/App"},
		{"ssh://git@github.com/Acme/App.git", "github.com/Acme/App"},
		{"https://user:s3cr3t-token@github.com/Acme/App.git", "github.com/Acme/App"},
		{"https://ghp_abcdef0123456789@github.com:443/Acme/App.git?x=1#frag", "github.com/Acme/App"},
		{"git://example.org/group/sub/repo.git", "example.org/group/sub/repo"},
		{"git+ssh://git@gitlab.example.com:2222/group/repo", "gitlab.example.com/group/repo"},
		{"deploy@[::1]:Acme/App", ""}, // scp form with a bracketed host is not a valid key
		{"/srv/git/app.git", ""},
		{"../app.git", ""},
		{"file:///srv/git/app.git", ""},
		{"https://github.com/onlyowner", ""},
		{"https://github.com/Acme/../App", ""},
		{"", ""},
		{"https://host/a b/c", ""},
		{"ftp://github.com/Acme/App", ""},
	} {
		got, ok := NormalizeRemote(tc.raw)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("NormalizeRemote(%q) = %q, %v; want %q", tc.raw, got, ok, tc.want)
		}
		if strings.Contains(got, "@") || strings.Contains(got, "s3cr3t") || strings.Contains(got, "ghp_") {
			t.Errorf("NormalizeRemote(%q) leaked credentials: %q", tc.raw, got)
		}
	}
	ssh, _ := NormalizeRemote("git@github.com:acme/app.git")
	https, _ := NormalizeRemote("https://github.com/acme/app.git")
	if ssh != https {
		t.Fatalf("ssh %q and https %q differ", ssh, https)
	}
}

func writeRepo(t *testing.T, dir, config string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestKey(t *testing.T) {
	root := t.TempDir()
	cfg := "[core]\n\tbare = false\n[remote \"upstream\"]\n\turl = https://github.com/other/thing.git\n[remote \"origin\"]\n\turl = https://bot:hunter2@github.com/acme/app.git ; comment\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
	repo := filepath.Join(root, "repo")
	writeRepo(t, repo, cfg)
	if got := Key(repo); got != "github.com/acme/app" {
		t.Fatalf("Key(repo) = %q", got)
	}
	sub := filepath.Join(repo, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := Key(sub); got != "github.com/acme/app" {
		t.Fatalf("Key(subdir) = %q", got)
	}

	// Quoted values.
	quoted := filepath.Join(root, "quoted")
	writeRepo(t, quoted, "[remote \"origin\"]\n\turl = \"git@github.com:acme/app.git\"\n")
	if got := Key(quoted); got != "github.com/acme/app" {
		t.Fatalf("Key(quoted) = %q", got)
	}

	// No remote and an unparseable remote fall back to the local key.
	bare := filepath.Join(root, "noremote")
	writeRepo(t, bare, "[core]\n\tbare = false\n")
	if got := Key(bare); got != LocalKey(bare) || !Valid(got) || !strings.HasPrefix(got, "local:") {
		t.Fatalf("Key(no remote) = %q", got)
	}
	localRemote := filepath.Join(root, "localremote")
	writeRepo(t, localRemote, "[remote \"origin\"]\n\turl = /srv/git/app.git\n")
	if got := Key(localRemote); got != LocalKey(localRemote) {
		t.Fatalf("Key(local remote) = %q", got)
	}
	plain := filepath.Join(root, "plain")
	if err := os.MkdirAll(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := Key(plain); got != LocalKey(plain) {
		t.Fatalf("Key(plain) = %q", got)
	}
	if LocalKey("/a") == LocalKey("/b") {
		t.Fatal("local keys collide")
	}

	// Oversized config fails closed to the local key.
	big := filepath.Join(root, "big")
	writeRepo(t, big, "[remote \"origin\"]\n\turl = https://github.com/acme/app.git\n"+strings.Repeat("#", maxConfigBytes))
	if got := Key(big); got != LocalKey(big) {
		t.Fatalf("Key(big) = %q", got)
	}
}

func TestKeyLinkedWorktree(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "main")
	writeRepo(t, main, "[remote \"origin\"]\n\turl = git@github.com:acme/app.git\n")
	wtGit := filepath.Join(main, ".git", "worktrees", "feature")
	if err := os.MkdirAll(wtGit, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtGit, "commondir"), []byte("../..\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(root, "feature")
	if err := os.MkdirAll(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+wtGit+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Key(wt); got != "github.com/acme/app" {
		t.Fatalf("Key(worktree) = %q", got)
	}
}

func TestKeyRefusesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	root := t.TempDir()
	real := filepath.Join(root, "real")
	writeRepo(t, real, "[remote \"origin\"]\n\turl = https://github.com/acme/app.git\n")

	linkedGit := filepath.Join(root, "linkedgit")
	if err := os.MkdirAll(linkedGit, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, ".git"), filepath.Join(linkedGit, ".git")); err != nil {
		t.Fatal(err)
	}
	if got := Key(linkedGit); got != LocalKey(linkedGit) {
		t.Fatalf("symlinked .git = %q", got)
	}

	linkedConfig := filepath.Join(root, "linkedconfig")
	if err := os.MkdirAll(filepath.Join(linkedConfig, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, ".git", "config"), filepath.Join(linkedConfig, ".git", "config")); err != nil {
		t.Fatal(err)
	}
	if got := Key(linkedConfig); got != LocalKey(linkedConfig) {
		t.Fatalf("symlinked config = %q", got)
	}
}

func TestValid(t *testing.T) {
	for key, want := range map[string]bool{
		"github.com/acme/app":        true,
		"example.org/group/sub/repo": true,
		"local:0123456789ab":         true,
		"local:0123456789AB":         false,
		"local:short":                false,
		"github.com/acme":            false,
		"user@github.com/acme/app":   false,
		"github.com/acme/../app":     false,
		"":                           false,
		strings.Repeat("a", 300):     false,
	} {
		if Valid(key) != want {
			t.Errorf("Valid(%q) = %v", key, !want)
		}
	}
}

func FuzzNormalizeRemote(f *testing.F) {
	for _, s := range []string{"git@github.com:a/b.git", "https://u:p@h.io:1/a/b", "ssh://[::1]:22/a/b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		key, ok := NormalizeRemote(raw)
		if ok && (!Valid(key) || strings.Contains(key, "@") || strings.Contains(key, ":")) {
			t.Fatalf("NormalizeRemote(%q) = %q", raw, key)
		}
	})
}
