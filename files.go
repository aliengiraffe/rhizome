package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"rhizome/internal/channel"
	"rhizome/internal/config"
)

func filesCommand(args []string) error {
	const usage = "usage: rhizome files send|list|accept|cancel --session <local-session-id> [--to <remote-session-id>] [path|transfer-id]"
	if len(args) == 0 {
		return errors.New(usage)
	}
	if args[0] == "--help" || args[0] == "-h" {
		fmt.Println(usage)
		return nil
	}
	action := args[0]
	if action != "send" && action != "list" && action != "accept" && action != "cancel" {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet("files "+action, flag.ContinueOnError)
	config.Bind(fs, "runtime-dir")
	session := fs.String("session", "", "exact local session ID")
	target := fs.String("to", "", "exact paired recipient session ID")
	if err := fs.Parse(args[1:]); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if *session == "" {
		return errors.New(usage)
	}
	cmd := channel.GatewayCommand{Action: "files-" + action, Session: *session, Target: *target}
	switch action {
	case "send":
		if fs.NArg() != 1 || *target == "" {
			return errors.New(usage)
		}
		path, err := filepath.Abs(fs.Arg(0))
		if err != nil {
			return err
		}
		cmd.Path = path
	case "list":
		if fs.NArg() != 0 || *target != "" {
			return errors.New(usage)
		}
	default:
		if fs.NArg() != 1 || *target != "" {
			return errors.New(usage)
		}
		cmd.ID = fs.Arg(0)
	}
	c, err := config.FromFlags(fs)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	out, err := channel.GatewayAdmin(ctx, c.RuntimeDir, cmd)
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}
