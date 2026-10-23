package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"rhizome/internal/update"
)

// Status is the local definition and observer state, without parsing native CLI output.
type Status struct {
	Installed bool
	Running   bool
	Health    json.RawMessage
	Update    json.RawMessage // last auto-update state, when recorded
}

// Inspect reads available service and observer status. Health may be stale when stopped.
func Inspect(ctx context.Context, c Config) (Status, error) {
	var s Status
	if err := ctx.Err(); err != nil {
		return s, err
	}
	m, err := newManager(c.ConfigPath)
	if err != nil {
		return s, err
	}
	_, err = os.Stat(m.definition)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return s, err
	}
	s.Installed = err == nil
	// Avoid creating the lock file merely to display status.
	if _, err := os.Stat(filepath.Join(c.Observer.Runtime, "observer.lock")); err == nil {
		s.Running = running(c.Observer.Runtime)
	}
	if s.Update, err = readJSON(filepath.Join(c.Observer.Runtime, update.StateFile), "update state"); err != nil {
		return s, err
	}
	if s.Health, err = readJSON(filepath.Join(c.Observer.Runtime, "observer-health.json"), "observer health"); err != nil {
		return s, err
	}
	return s, nil
}

// readJSON returns a bounded, valid JSON document, nil when the file is absent.
func readJSON(path, what string) (json.RawMessage, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64*1024+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 64*1024 || !json.Valid(b) {
		return nil, errors.New("invalid " + what + " data")
	}
	return b, nil
}

// ManageQuiet uses the same lifecycle logic without writing into a terminal UI.
func ManageQuiet(ctx context.Context, action string, c Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if action == "status" {
		return errors.New("use Inspect for structured status")
	}
	m, err := newManager(c.ConfigPath)
	if err != nil {
		return err
	}
	m.run = func(ctx context.Context, name string, args ...string) error {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...)
		output := &boundedOutput{}
		cmd.Stdout, cmd.Stderr = output, output
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w: %s", name, err, output.String())
		}
		return nil
	}
	return m.manage(ctx, action, c)
}

// boundedOutput drains native command diagnostics without unbounded buffering.
type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 4096 - b.Len(); remaining > 0 {
		_, _ = b.Buffer.Write(p[:min(n, remaining)])
	}
	return n, nil
}
