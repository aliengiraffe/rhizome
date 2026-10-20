package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"rhizome/internal/channel"
	"rhizome/internal/config"
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
	fs := flag.NewFlagSet("service "+args[0], flag.ContinueOnError)
	if args[0] == "install" || args[0] == "run" {
		config.Bind(fs, "runtime-dir", "claude-root", "codex-root")
	} else {
		config.Bind(fs)
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
	resolved, err := config.FromFlags(fs)
	if err != nil {
		return err
	}
	c := service.FromConfig(resolved)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if args[0] == "run" {
		installed := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "config" {
				installed = true
			}
		})
		if installed {
			return service.Run(ctx, c)
		}
		return channel.RunObserver(ctx, c.Observer)
	}
	return service.Manage(ctx, args[0], c)
}
