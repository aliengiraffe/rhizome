package service

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"rhizome/internal/channel"
	"rhizome/internal/localstate"
	"rhizome/internal/update"
	"sync"
)

type rotatingLog struct {
	mu   sync.Mutex
	path string
}

func (w *rotatingLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if st, err := os.Stat(w.path); err == nil && st.Size()+int64(len(p)) > 2*1024*1024 {
		if err := os.Remove(w.path + ".1"); err != nil && !os.IsNotExist(err) {
			return 0, err
		}
		if err := os.Rename(w.path, w.path+".1"); err != nil {
			return 0, err
		}
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.Write(p)
}

// Run starts the installed observer with bounded diagnostics in its runtime dir.
func Run(ctx context.Context, c Config) error {
	if err := validate(&c); err != nil {
		return err
	}
	if err := localstate.SecureDir(c.Observer.Runtime); err != nil {
		return err
	}
	previous := slog.Default()
	defer slog.SetDefault(previous)
	// slog.SetDefault also routes the log package through the handler, and
	// restoring the previous default does not undo that; put the final error
	// back on stderr for the service manager's log.
	defer log.SetOutput(os.Stderr)
	defer log.SetFlags(log.Flags())
	slog.SetDefault(slog.New(slog.NewJSONHandler(&rotatingLog{path: filepath.Join(c.Observer.Runtime, "observer.log")}, channel.LogOptions(c.Debug))))
	return RunForeground(ctx, c)
}

// RunForeground owns the observer, the optional gateway and the optional
// background updater under one cancellation scope. It returns
// update.ErrRestartRequired when a verified update was installed and this
// process must exit so its supervisor restarts it on the new binary.
func RunForeground(ctx context.Context, c Config) error {
	if err := update.Confirm(c.Observer.Runtime, build.version, nil); err != nil {
		slog.Warn("update_confirm", "error", err)
	}
	tasks := []func(context.Context) error{func(ctx context.Context) error { return channel.RunObserver(ctx, c.Observer) }}
	if c.Gateway.Mode != "" && c.Gateway.Mode != "off" {
		tasks = append(tasks, func(ctx context.Context) error {
			return channel.RunGateway(ctx, c.Observer.Runtime, c.Gateway, channel.GatewayHistory{ClaudeRoot: c.Observer.ClaudeRoot, CodexRoot: c.Observer.CodexRoot, OpenCodeRoot: c.Observer.OpenCodeRoot, Policy: c.PeerHistory, RecentWindow: c.RecentWindow, Hostname: c.Observer.Hostname})
		})
	}
	if c.AutoUpdate.Enabled {
		u, err := newUpdater(c, &selfRestarter{c: c}, slog.Default())
		if err != nil {
			return err
		}
		// A finished loop (development build) must not stop the observer; only
		// a required restart ends the process.
		tasks = append(tasks, func(ctx context.Context) error {
			err := u.Run(ctx)
			if errors.Is(err, update.ErrRestartRequired) {
				return err
			}
			<-ctx.Done()
			return nil
		})
	} else {
		slog.Info("update_disabled", "reason", "configuration")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(tasks))
	for _, task := range tasks {
		go func() { results <- task(ctx) }()
	}
	first := <-results
	cancel()
	for i := 1; i < len(tasks); i++ {
		if err := <-results; first == nil {
			first = err
		}
	}
	return first
}
