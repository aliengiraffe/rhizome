package service

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"rhizome/internal/channel"
	"rhizome/internal/localstate"
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
	slog.SetDefault(slog.New(slog.NewJSONHandler(&rotatingLog{path: filepath.Join(c.Observer.Runtime, "observer.log")}, nil)))
	return RunForeground(ctx, c)
}

// RunForeground owns observer and gateway lifecycles under one cancellation scope.
func RunForeground(ctx context.Context, c Config) error {
	if c.Gateway.Mode == "" || c.Gateway.Mode == "off" {
		return channel.RunObserver(ctx, c.Observer)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, 2)
	go func() { results <- channel.RunObserver(ctx, c.Observer) }()
	go func() {
		results <- channel.RunGateway(ctx, c.Observer.Runtime, c.Gateway, channel.GatewayHistory{ClaudeRoot: c.Observer.ClaudeRoot, CodexRoot: c.Observer.CodexRoot, OpenCodeRoot: c.Observer.OpenCodeRoot, Policy: c.PeerHistory, RecentWindow: c.RecentWindow, Hostname: c.Observer.Hostname})
	}()
	err := <-results
	cancel()
	other := <-results
	if err != nil {
		return err
	}
	return other
}
