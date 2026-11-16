package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"time"

	"rhizome/internal/oai"
)

func oaiCommand(mode string, args []string) error {
	if len(args) == 0 || args[0] != "oai" {
		return errors.New("expected oai subcommand")
	}
	args = args[1:]
	action := ""
	model, prompt := "", "Reply with a one-sentence hello from Rhizome."
	if mode == "auth" {
		if len(args) > 1 {
			return errors.New("usage: rhizome auth oai [status|logout]")
		}
		if len(args) == 1 {
			action = args[0]
			if action != "status" && action != "logout" {
				return errors.New("usage: rhizome auth oai [status|logout]")
			}
		}
	} else {
		fs := flag.NewFlagSet("test oai", flag.ContinueOnError)
		fs.StringVar(&model, "model", "", "model slug (default: first visible model)")
		fs.StringVar(&prompt, "prompt", prompt, "prompt text")
		if e := fs.Parse(args); e != nil {
			return e
		}
		if fs.NArg() != 0 {
			return errors.New("unexpected test oai arguments")
		}
	}
	home, e := os.UserHomeDir()
	if e != nil {
		return errors.New("cannot find home directory")
	}
	client := oai.New(filepath.Join(home, ".rhizome", "oai"))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if mode == "test" {
		reply, e := client.Test(ctx, model, prompt)
		if e != nil {
			return e
		}
		fmt.Println(reply)
		return nil
	}
	switch action {
	case "status":
		s, e := client.Status()
		if e != nil {
			return e
		}
		printOAIStatus(s)
		return nil
	case "logout":
		confirmed, e := client.Logout(ctx)
		if e != nil {
			return e
		}
		fmt.Println("Signed out; registration retained.")
		if !confirmed {
			fmt.Fprintln(os.Stderr, "Remote revocation was not confirmed; disconnect the app in ChatGPT settings.")
		}
		return nil
	default:
		s, e := client.SignIn(ctx, func(u string) error {
			fmt.Fprintln(os.Stderr, "Continue with ChatGPT in your browser.")
			if openOAIBrowser(ctx, u) != nil {
				fmt.Fprintln(os.Stderr, "Open this sign-in URL in a local browser:", u)
			}
			return nil
		})
		if e != nil {
			return e
		}
		printOAIStatus(s)
		return nil
	}
}
func printOAIStatus(s oai.State) {
	if !s.SignedIn() {
		fmt.Println("Not signed in (or verification required). Run rhizome auth oai.")
		return
	}
	fmt.Printf("Signed in: %q\nChatGPT plan usage granted: %t\nToken expires: %s\n", s.Account, s.PlanUsage(), s.Expires.UTC().Format(time.RFC3339))
}
func openOAIBrowser(ctx context.Context, u string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "/usr/bin/open", u)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", u)
	}
	return cmd.Run()
}
