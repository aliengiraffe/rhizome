package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"rhizome/internal/config"
	"rhizome/internal/service"
	"rhizome/internal/update"
	"syscall"
)

// updateCommand implements `rhizome update`: by default it reports whether a
// newer verified release exists; --apply installs it over the running
// executable and restarts an installed, running observer. It works whether
// or not background updates are enabled, since it is an explicit request.
// --finish is the internal step the observer launches after installing an
// update onto itself.
func updateCommand(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	check := fs.Bool("check", false, "report the newest eligible release without installing (default)")
	apply := fs.Bool("apply", false, "download, verify and install the newest eligible release, then restart the service")
	finish := fs.Bool("finish", false, "internal: complete a restart started by the observer after it updated itself")
	config.Bind(fs)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected update arguments")
	}
	if *apply && *finish || *check && *finish {
		return fmt.Errorf("--finish cannot be combined with --check or --apply")
	}
	resolved, err := config.FromFlags(fs)
	if err != nil {
		return err
	}
	c := service.FromConfig(resolved)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *finish {
		return service.Finish(ctx, c)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	u, err := service.NewUpdater(c, log)
	if err != nil {
		return err
	}
	version, _ := service.Build()
	if version == "" {
		version = "dev"
	}
	candidate, res, err := u.Check(ctx)
	if err != nil {
		return fmt.Errorf("update check failed (%s): %w", res.Outcome, err)
	}
	switch res.Outcome {
	case update.OutcomeDevBuild:
		fmt.Printf("running %s: development builds are not updated\n", version)
		return nil
	case update.OutcomeNoRelease:
		fmt.Printf("running %s: no published release found\n", version)
		return nil
	case update.OutcomeUpToDate:
		fmt.Printf("running %s: up to date\n", version)
		return nil
	}
	fmt.Printf("running %s: release %s is available\n", version, res.Available)
	if !*apply {
		fmt.Println("run `rhizome update --apply` to install it")
		return nil
	}
	res, err = u.Apply(ctx, candidate)
	if err != nil {
		if res.Outcome == update.OutcomeNotWritable {
			fmt.Println(res.Message)
			return errors.New("update not installed: " + res.Outcome)
		}
		return fmt.Errorf("update not installed (%s): %w", res.Outcome, err)
	}
	fmt.Println(res.Message)
	switch res.Outcome {
	case update.OutcomeUpdated:
		fmt.Println("MCP processes keep running the old binary until their agent client restarts them; run `rhizome setup` if the release notes mention skill or hook changes.")
	case update.OutcomeInstalled:
		fmt.Println("No installed, running observer was found; start the service to run the new version. MCP processes pick it up when their agent client restarts them.")
	}
	return nil
}
