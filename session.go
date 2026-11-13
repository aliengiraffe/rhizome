package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"

	"rhizome/internal/channel"
	"rhizome/internal/config"
)

func sessionCommand(args []string) error {
	if len(args) == 0 || args[0] != "visibility" {
		return errors.New("usage: rhizome session visibility [--config FILE] [--runtime-dir DIR] <exact-local-session-id> [public|private]")
	}
	fs := flag.NewFlagSet("session visibility", flag.ContinueOnError)
	config.Bind(fs, "runtime-dir")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return errors.New("visibility requires an exact local session ID and optional public or private")
	}
	c, err := config.FromFlags(fs)
	if err != nil {
		return err
	}
	value := ""
	if fs.NArg() == 2 {
		value = fs.Arg(1)
	}
	result, err := channel.SessionVisibility(context.Background(), c.RuntimeDir, fs.Arg(0), value)
	if encodeErr := json.NewEncoder(os.Stdout).Encode(result); err == nil {
		err = encodeErr
	}
	return err
}
