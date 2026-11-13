package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"time"

	"rhizome/internal/config"
	"rhizome/internal/dream"
	"rhizome/internal/oai"
)

func dreamCommand(args []string) error {
	action := "run"
	if len(args) > 0 && (args[0] == "show" || args[0] == "log" || args[0] == "rollback") {
		action = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet("dream", flag.ContinueOnError)
	var dry bool
	var topic string
	if action == "run" {
		fs.BoolVar(&dry, "dry-run", false, "print proposed operations without writing memory")
	}
	if action == "show" {
		fs.StringVar(&topic, "topic", "", "only this topic")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if action != "rollback" && fs.NArg() != 0 || action == "rollback" && fs.NArg() != 1 {
		return errors.New("invalid dream arguments")
	}
	c, err := config.Resolve(nil, os.Getenv)
	if err != nil {
		return err
	}
	store := dream.Store{Runtime: c.RuntimeDir}
	if action == "rollback" {
		rev, err := strconv.Atoi(fs.Arg(0))
		if err != nil {
			return errors.New("dream rollback requires a revision number")
		}
		return store.Rollback(rev, time.Now())
	}
	if action == "show" || action == "log" {
		revs, err := store.Load()
		if err != nil {
			return err
		}
		if action == "show" {
			if len(revs) > 0 {
				fmt.Print(dream.Render(revs[len(revs)-1].Items, topic))
			}
		} else {
			for _, rev := range revs {
				fmt.Printf("%d %s added=%d updated=%d removed=%d\n", rev.Revision, rev.At.UTC().Format(time.RFC3339), rev.Added, rev.Updated, rev.Removed)
			}
		}
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return errors.New("cannot find home directory")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	return dream.Run(ctx, c, oai.New(filepath.Join(home, ".rhizome", "oai")), dry, os.Stdout, os.Stderr)
}
