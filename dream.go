package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"rhizome/internal/config"
	"rhizome/internal/dream"
	"rhizome/internal/oai"
)

func dreamCommand(args []string) error {
	if len(args) > 0 && args[0] == "digest" {
		return dreamDigestCommand(args[1:], os.Stdout)
	}
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

// dreamDigestCommand reviews local digests and lists those received from
// paired hosts. It never calls a model or the network.
func dreamDigestCommand(args []string, out io.Writer) error {
	if len(args) == 0 {
		return dream.ErrDigestUsage
	}
	action, args := args[0], args[1:]
	fs := flag.NewFlagSet("dream digest "+action, flag.ContinueOnError)
	var peers bool
	var entries string
	switch action {
	case "list":
		fs.BoolVar(&peers, "peers", false, "list untrusted digests received from paired hosts, grouped by project")
	case "approve":
		fs.StringVar(&entries, "entries", "", "comma-separated 1-based entry numbers to publish (default all)")
	case "show", "reject":
	default:
		return dream.ErrDigestUsage
	}
	// Accept the digest ID before or after flags.
	var id string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if id == "" && fs.NArg() > 0 {
		id = fs.Arg(0)
	} else if fs.NArg() > 0 {
		return dream.ErrDigestUsage
	}
	if (action == "list") != (id == "") {
		return dream.ErrDigestUsage
	}
	c, err := config.Resolve(nil, os.Getenv)
	if err != nil {
		return err
	}
	r := dream.Review{Runtime: c.RuntimeDir}
	switch action {
	case "list":
		if peers {
			return r.Peers(out)
		}
		return r.List(out)
	case "show":
		return r.Show(out, id)
	case "approve":
		if err := r.Approve(id, entries); err != nil {
			return err
		}
		if !c.Gateway.Dream.Enabled() {
			fmt.Fprintln(os.Stderr, "dream: digest approved; [gateway.dream].mode is off, so nothing is shared until it is enabled and the service restarts")
		}
		return nil
	default:
		return r.Reject(id)
	}
}
