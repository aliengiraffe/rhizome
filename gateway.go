package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"rhizome/internal/channel"
	"rhizome/internal/config"
)

func gatewayCommand(command string, args []string) error {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	config.Bind(fs, "runtime-dir")
	expires := fs.String("expires", "10m", "invitation validity, 1m to 24h")
	stdin := fs.Bool("stdin", false, "read invitation from stdin")
	action := command
	if command == "peers" {
		if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
			fmt.Println("usage: rhizome peers list|revoke|invitations|revoke-invite [flags] [id] (list --tui: interactive paired hosts)")
			return nil
		}
		if len(args) == 0 {
			return errors.New("usage: rhizome peers list|revoke|invitations|revoke-invite [flags] [id] (list --tui: interactive paired hosts)")
		}
		action = args[0]
		args = args[1:]
		if action == "list" {
			action = "peers"
		}
		if action != "peers" && action != "revoke" && action != "invitations" && action != "revoke-invite" {
			return errors.New("unknown peers action")
		}
	}
	var tui bool
	if command == "peers" && action == "peers" {
		fs.BoolVar(&tui, "tui", false, "browse paired hosts interactively (requires a terminal)")
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if tui {
		if err := requireTerminal(); err != nil {
			return err
		}
	}
	c, err := config.FromFlags(fs)
	if err != nil {
		return err
	}
	cmd := channel.GatewayCommand{Action: action, Expires: *expires}
	switch action {
	case "join":
		if *stdin {
			if fs.NArg() != 0 {
				return errors.New("--stdin takes no invitation argument")
			}
			b, e := io.ReadAll(io.LimitReader(os.Stdin, 2049))
			if e != nil {
				return e
			}
			if len(b) > 2048 {
				return errors.New("invitation too long")
			}
			cmd.Code = strings.TrimSpace(string(b))
		} else {
			if fs.NArg() != 1 {
				return errors.New("usage: rhizome join [flags] <invitation>, or rhizome join --stdin")
			}
			cmd.Code = fs.Arg(0)
		}
	case "revoke", "revoke-invite":
		if fs.NArg() != 1 {
			return errors.New("exact peer or invitation ID required")
		}
		cmd.ID = fs.Arg(0)
	default:
		if fs.NArg() != 0 {
			return errors.New("unexpected arguments")
		}
	}
	if tui {
		return runPanel(peersPanel(c))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	result, err := channel.GatewayAdmin(ctx, c.RuntimeDir, cmd)
	if err != nil {
		return err
	}
	if action == "invite" {
		var out map[string]string
		if err := json.Unmarshal(result, &out); err != nil {
			return err
		}
		fmt.Println("rhizome join " + out["invitation"])
		return nil
	}
	fmt.Println(strings.TrimSpace(string(result)))
	return nil
}
