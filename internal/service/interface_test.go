package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"rhizome/internal/channel"
)

func TestInspectDoesNotCreateRuntime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	c := Config{Observer: channel.ObserverOptions{Runtime: filepath.Join(home, "run")}}
	s, err := Inspect(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if s.Running || s.Installed || len(s.Health) != 0 {
		t.Fatal(s)
	}
	if _, err := os.Stat(c.Observer.Runtime); !os.IsNotExist(err) {
		t.Fatal("status created runtime", err)
	}
	if err := os.MkdirAll(c.Observer.Runtime, 0700); err != nil {
		t.Fatal(err)
	}
	health := filepath.Join(c.Observer.Runtime, "observer-health.json")
	if err := os.WriteFile(health, []byte(`{"observed_sessions":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	s, err = Inspect(context.Background(), c)
	if err != nil || len(s.Health) == 0 || s.Running {
		t.Fatal(s, err)
	}
	if err := os.WriteFile(health, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), c); err == nil {
		t.Fatal("invalid health accepted")
	}
}
