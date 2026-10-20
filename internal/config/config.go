// Package config resolves the single per-user configuration before startup.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"rhizome/internal/localstate"
)

// Config contains effective settings and their provenance. It never holds tokens.
// Settings are grouped by the agent they describe; which agent a process serves
// is a per-launch choice (`rhizome mcp --agent`) and is never configured here.
type Config struct {
	RuntimeDir string `toml:"runtime_dir"`
	Claude     struct {
		ConfigDir   string `toml:"config_dir"`   // CLAUDE_CONFIG_DIR; skills and transcripts live under it.
		ProjectsDir string `toml:"projects_dir"` // Transcript root the observer watches; default <config_dir>/projects.
	} `toml:"claude"`
	Codex struct {
		Bin         string `toml:"bin"`          // Codex CLI executable name or path.
		Home        string `toml:"home"`         // CODEX_HOME; App Server socket and skills live under it.
		SessionsDir string `toml:"sessions_dir"` // Session log root the observer watches; default <home>/sessions.
	} `toml:"codex"`
	Path    string            `toml:"-"`
	Found   bool              `toml:"-"`
	Sources map[string]string `toml:"-"`
}

type setting struct {
	key, env, flag string
	value          *string
	path           bool
}

func (c *Config) settings() []setting {
	return []setting{
		{"runtime_dir", "RHIZOME_RUNTIME_DIR", "runtime-dir", &c.RuntimeDir, true},
		{"claude.config_dir", "CLAUDE_CONFIG_DIR", "", &c.Claude.ConfigDir, true},
		{"claude.projects_dir", "", "claude-root", &c.Claude.ProjectsDir, true},
		{"codex.bin", "", "codex-bin", &c.Codex.Bin, false},
		{"codex.home", "CODEX_HOME", "", &c.Codex.Home, true},
		{"codex.sessions_dir", "", "codex-root", &c.Codex.SessionsDir, true},
	}
}

// Bind adds configuration selection and the requested override flags.
func Bind(fs *flag.FlagSet, names ...string) {
	fs.String("config", "", "configuration TOML path (default ~/.rhizome/config.toml)")
	var c Config
	for _, s := range c.settings() {
		for _, name := range names {
			if s.flag == name {
				fs.String(name, "", "override "+s.key)
			}
		}
	}
}

// FromFlags resolves explicitly visited flags over environment, file and defaults.
func FromFlags(fs *flag.FlagSet) (Config, error) {
	flags := map[string]string{}
	fs.Visit(func(f *flag.Flag) { flags[f.Name] = f.Value.String() })
	c, err := Resolve(flags, os.Getenv)
	if err == nil {
		slog.Info("configuration", "path", c.Path, "found", c.Found)
	}
	return c, err
}

// Resolve reads settings once. getenv may be replaced to resolve without an environment.
func Resolve(flags map[string]string, getenv func(string) string) (Config, error) {
	var c Config
	home, err := os.UserHomeDir()
	if err != nil {
		return c, err
	}
	path := filepath.Join(home, ".rhizome", "config.toml")
	explicit := false
	if p := getenv("RHIZOME_CONFIG"); p != "" {
		path, explicit = p, true
	}
	if p, ok := flags["config"]; ok {
		path, explicit = p, true
	}
	path, err = expand(path, home)
	if err != nil {
		return c, fmt.Errorf("config path %q: %w", path, err)
	}
	c.Path = path
	c.Sources = map[string]string{}
	file, found, err := read(path)
	if err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	if !found && explicit {
		return c, fmt.Errorf("config %s: %w", path, os.ErrNotExist)
	}
	c.Found = found
	var raw Config
	md, err := toml.Decode(string(file), &raw)
	if err != nil {
		return c, fmt.Errorf("config %s: %w", path, err)
	}
	if keys := md.Undecoded(); len(keys) > 0 {
		return c, fmt.Errorf("config %s: unknown key %s", path, keys[0])
	}
	// TOML keys are case-sensitive even though the decoder can match Go fields
	// case-insensitively. Reject alternate spellings instead of losing their source.
	known := map[string]bool{"claude": true, "codex": true}
	for _, s := range raw.settings() {
		known[s.key] = true
	}
	for _, key := range md.Keys() {
		if !known[key.String()] {
			return c, fmt.Errorf("config %s: unknown key %s", path, key)
		}
	}
	// Validate file paths even if an override would hide the invalid value.
	for _, s := range raw.settings() {
		if (s.path || s.key == "codex.bin" && binaryPath(*s.value)) && *s.value != "" {
			if _, err := expand(*s.value, home); err != nil {
				return c, fmt.Errorf("config %s: %s: %w", path, s.key, err)
			}
		}
	}
	c.RuntimeDir = filepath.Join(home, ".rhizome", "run")
	c.Codex.Bin = "codex"
	c.Claude.ConfigDir, c.Codex.Home = filepath.Join(home, ".claude"), filepath.Join(home, ".codex")
	rawSettings := raw.settings()
	for i, s := range c.settings() {
		source := "default"
		if md.IsDefined(strings.Split(s.key, ".")...) && (*rawSettings[i].value != "" || s.key == "runtime_dir" || s.key == "codex.bin") {
			*s.value, source = *rawSettings[i].value, "file"
		}
		if s.env != "" {
			if v := getenv(s.env); v != "" {
				*s.value, source = v, "env"
			}
		}
		if v, ok := flags[s.flag]; s.flag != "" && ok {
			*s.value, source = v, "flag"
		}
		if source == "default" {
			switch s.key {
			case "claude.projects_dir":
				*s.value = filepath.Join(c.Claude.ConfigDir, "projects")
			case "codex.sessions_dir":
				*s.value = filepath.Join(c.Codex.Home, "sessions")
			}
		}
		if *s.value == "" {
			return c, fmt.Errorf("config %s: %s must not be empty (%s)", path, s.key, source)
		}
		if s.path || s.key == "codex.bin" && binaryPath(*s.value) {
			*s.value, err = expand(*s.value, home)
			if err != nil {
				return c, fmt.Errorf("config %s: %s (%s): %w", path, s.key, source, err)
			}
		}
		c.Sources[s.key] = source
	}
	c.RuntimeDir, err = localstate.Runtime(c.RuntimeDir)
	if err != nil {
		return c, err
	}
	if c.Claude.ProjectsDir == c.Codex.SessionsDir {
		return c, fmt.Errorf("config %s: claude.projects_dir and codex.sessions_dir must differ", path)
	}
	return c, nil
}

func binaryPath(value string) bool {
	return strings.ContainsAny(value, `/\`) || strings.HasPrefix(value, "~") || strings.HasPrefix(value, "$HOME") || strings.HasPrefix(value, "${HOME}")
}

func expand(path, home string) (string, error) {
	if path == "~" {
		path = home
	} else if strings.HasPrefix(path, "~/") {
		path = filepath.Join(home, path[2:])
	}
	path = strings.ReplaceAll(strings.ReplaceAll(path, "${HOME}", home), "$HOME", home)
	if !filepath.IsAbs(path) {
		return path, errors.New("path must be absolute")
	}
	return filepath.Clean(path), nil
}

func read(path string) ([]byte, bool, error) {
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !st.Mode().IsRegular() {
		return nil, false, errors.New("file must be regular and not a symlink")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !os.SameFile(st, opened) {
		return nil, false, errors.New("file changed while opening")
	}
	if err := checkOwner(f, opened); err != nil {
		return nil, false, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
	if len(b) > 64*1024 {
		return nil, false, errors.New("file exceeds 64 KiB")
	}
	return b, true, err
}

// Write prints all effective values and sources as annotated TOML.
func (c Config) Write(w io.Writer) error {
	state := "found"
	if !c.Found {
		state = "not found"
	}
	if _, err := fmt.Fprintf(w, "# config: %s (%s)\n", c.Path, state); err != nil {
		return err
	}
	for _, s := range c.settings() {
		if _, err := fmt.Fprintf(w, "%s = %q # source: %s\n", s.key, *s.value, c.Sources[s.key]); err != nil {
			return err
		}
	}
	return nil
}
