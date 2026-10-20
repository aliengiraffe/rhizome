package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"rhizome/internal/channel"
	"rhizome/internal/service"
	"syscall"
)

func serviceCommand(args []string) error {
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Println("usage: rhizome service install|start|stop|status|uninstall|run [flags]")
		return nil
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: rhizome service install|start|stop|status|uninstall|run [flags]")
	}
	c, err := service.Defaults()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("service "+args[0], flag.ContinueOnError)
	config := ""
	if args[0] == "install" || args[0] == "run" {
		fs.StringVar(&c.Observer.Runtime, "runtime-dir", c.Observer.Runtime, "private Rhizome runtime directory")
		fs.StringVar(&c.Observer.ClaudeRoot, "claude-root", c.Observer.ClaudeRoot, "Claude projects log directory")
		fs.StringVar(&c.Observer.CodexRoot, "codex-root", c.Observer.CodexRoot, "Codex sessions log directory")
	}
	if args[0] == "run" {
		fs.StringVar(&config, "config", "", "installed service configuration file")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected service arguments")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if args[0] == "run" {
		if config != "" {
			c, err = service.Load(config)
			if err != nil {
				return err
			}
		}
		for _, p := range []string{c.Observer.ClaudeRoot, c.Observer.CodexRoot} {
			if p == "" {
				return fmt.Errorf("log roots must be configured")
			}
		}
		if config != "" {
			return service.Run(ctx, c)
		}
		return channel.RunObserver(ctx, c.Observer)
	}
	return service.Manage(ctx, args[0], c)
}
