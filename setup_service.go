package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"rhizome/internal/config"
	"rhizome/internal/service"
	"syscall"
)

// serviceOutcome reports what the setup service step achieved so callers can
// distinguish a written definition from a running observer.
type serviceOutcome struct {
	Installed bool   // a native definition exists after the attempt
	Running   bool   // the observer holds its runtime lock
	Runtime   string // effective runtime directory from the configuration file
}

// serviceInstaller installs and starts the background service for the absolute
// configuration path and confirms it is running. Setup takes it as a value so
// tests never touch a real service manager.
type serviceInstaller func(ctx context.Context, path string) (serviceOutcome, error)

// installService resolves the configuration file at path exactly as the
// service commands do, runs the install lifecycle through manage (which
// installs a missing definition, starts a stopped compatible service, treats a
// running one as success, and waits for the observer lock), then confirms the
// observer is running before reporting success. On failure the definition and
// logs are left in place for diagnosis and the error says whether the
// definition was installed.
func installService(ctx context.Context, path string, manage func(context.Context, string, service.Config) error) (serviceOutcome, error) {
	c, err := config.Resolve(map[string]string{"config": path}, os.Getenv)
	if err != nil {
		return serviceOutcome{}, err
	}
	sc := service.FromConfig(c)
	out := serviceOutcome{Runtime: sc.Observer.Runtime}
	installErr := manage(ctx, "install", sc)
	// Inspect reads local files only; report the final state even after cancellation.
	status, inspectErr := service.Inspect(context.Background(), sc)
	out.Installed, out.Running = status.Installed, status.Running
	switch {
	case installErr != nil && out.Installed && !out.Running:
		return out, fmt.Errorf("service definition installed but the observer is not running (definition and logs kept for diagnosis): %w", installErr)
	case installErr != nil:
		return out, fmt.Errorf("service not installed: %w", installErr)
	case inspectErr != nil:
		return out, fmt.Errorf("service installed but its status could not be read: %w", inspectErr)
	case !out.Running:
		return out, errors.New("service manager reported success but the observer is not running; inspect service logs")
	}
	return out, nil
}

// plainServiceInstaller is the installer for the line-based prompts: native
// command output goes to the terminal and Ctrl-C cancels the startup wait.
func plainServiceInstaller(ctx context.Context, path string) (serviceOutcome, error) {
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return installService(ctx, path, service.Manage)
}

// quietServiceInstaller is the installer for the terminal UI, which must not
// let native commands write into the screen.
func quietServiceInstaller(ctx context.Context, path string) (serviceOutcome, error) {
	return installService(ctx, path, service.ManageQuiet)
}
