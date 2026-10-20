package service

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"rhizome/internal/channel"
	"rhizome/internal/config"
	"rhizome/internal/localstate"
	"strings"
	"testing"
)

func TestNativeDefinitions(t *testing.T) {
	for _, platform := range []string{"darwin", "linux", "windows"} {
		t.Run(platform, func(t *testing.T) {
			m := manager{platform: platform, configPath: `/home/test/a b&c/config.toml`, definition: "/service", binary: `/opt/a b/rhizome`}
			c := Config{Observer: channel.ObserverOptions{Runtime: "/runtime"}}
			definition := m.render(c)
			if !strings.Contains(definition, "service") || !strings.Contains(definition, "run") {
				t.Fatal(definition)
			}
			if platform != "linux" {
				d := xml.NewDecoder(strings.NewReader(definition))
				for {
					_, err := d.Token()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, action := range []string{"install", "start", "stop", "status", "uninstall"} {
				if len(m.commands(action)) == 0 {
					t.Fatal(action)
				}
			}
			if platform == "windows" && (!strings.Contains(definition, "InteractiveToken") || !strings.Contains(definition, "IgnoreNew")) {
				t.Fatal("not user scoped")
			}
			if platform == "linux" && !strings.Contains(definition, `ExecStart="/opt/a b/rhizome"`) {
				t.Fatal("unquoted binary")
			}
		})
	}
}
func TestEscaping(t *testing.T) {
	if got := unitArg(`/a%u/"x`); got != `"/a%%u/\"x"` {
		t.Fatal(got)
	}
	if got := windowsQuote(`C:\a b\`); got != `"C:\a b\\"` {
		t.Fatal(got)
	}
}

func TestFileResolutionAndUninstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := fmt.Sprintf("runtime_dir = %q\nclaude.projects_dir = %q\ncodex.sessions_dir = %q\n", filepath.Join(dir, "run"), filepath.Join(dir, "claude"), filepath.Join(dir, "codex"))
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	resolved, err := config.Resolve(map[string]string{"config": path}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	c := FromConfig(resolved)
	if c.Observer.Runtime != filepath.Join(dir, "run") || c.Observer.ClaudeRoot != filepath.Join(dir, "claude") || c.Observer.CodexRoot != filepath.Join(dir, "codex") {
		t.Fatalf("%+v", c)
	}
	m := manager{platform: "linux", configPath: path, binary: "/bin/rhizome", definition: filepath.Join(dir, "service.unit"), run: func(context.Context, string, ...string) error { return nil }}
	if err := os.WriteFile(m.definition, []byte(m.render(c)), 0600); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.render(c), "service run --config "+unitArg(path)) {
		t.Fatal(m.render(c))
	}
	if err := m.manage(context.Background(), "uninstall", c); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != body {
		t.Fatalf("config changed: %v", err)
	}
	if _, err := os.Stat(m.definition); !os.IsNotExist(err) {
		t.Fatal("definition left behind")
	}
}

func TestInstallRefusesRunningRuntimeChange(t *testing.T) {
	dir := t.TempDir()
	oldRuntime, newRuntime := filepath.Join(dir, "old"), filepath.Join(dir, "new")
	if err := localstate.SecureDir(oldRuntime); err != nil {
		t.Fatal(err)
	}
	lock, err := localstate.Lock(filepath.Join(oldRuntime, "observer.lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("runtime_dir = %q", newRuntime)), 0644); err != nil {
		t.Fatal(err)
	}
	resolved, err := config.Resolve(map[string]string{"config": path}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	c := FromConfig(resolved)
	m := manager{platform: "linux", configPath: path, binary: "/bin/rhizome", definition: filepath.Join(dir, "service.unit")}
	old := c
	old.Observer.Runtime = oldRuntime
	if err := os.WriteFile(m.definition, []byte(m.render(old)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.manage(context.Background(), "install", c); err == nil || !strings.Contains(err.Error(), "observer is running") {
		t.Fatalf("got %v", err)
	}
	c.Observer.Runtime = filepath.Join(dir, "override")
	if err := m.manage(context.Background(), "install", c); err == nil || !strings.Contains(err.Error(), "overrides differ") {
		t.Fatalf("got %v", err)
	}
}

func TestInstallWritesOnlyDefinition(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := fmt.Sprintf("runtime_dir = %q", filepath.Join(dir, "run"))
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	resolved, err := config.Resolve(map[string]string{"config": path}, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	c := FromConfig(resolved)
	m := manager{platform: "linux", configPath: path, binary: "/bin/rhizome", definition: filepath.Join(dir, "service.unit")}
	var lock *os.File
	m.run = func(context.Context, string, ...string) error {
		if lock == nil {
			var err error
			lock, err = localstate.Lock(filepath.Join(c.Observer.Runtime, "observer.lock"), false)
			return err
		}
		return nil
	}
	defer func() {
		if lock != nil {
			lock.Close()
		}
	}()
	if err := m.manage(context.Background(), "install", c); err != nil {
		t.Fatal(err)
	}
	if err := m.manage(context.Background(), "install", c); err != nil {
		t.Fatalf("identical reinstall: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != body {
		t.Fatal("config overwritten", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("unexpected install files: %v", entries)
	}
}

func TestHostnameFromConfig(t *testing.T) {
	c := config.Config{Hostname: "Work laptop"}
	if got := FromConfig(c).Observer.Hostname; got != c.Hostname {
		t.Fatalf("hostname %q", got)
	}
}
