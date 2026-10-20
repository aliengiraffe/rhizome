package main

import (
	"bufio"
	"bytes"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"rhizome/internal/config"
	"sort"
	"strings"

	"golang.org/x/term"
)

// skillFS embeds the collaboration skill so `rhizome setup` can install it
// without a checkout. Add further skill files under skills/rhizome-collab and
// they are copied as well.
//
//go:embed skills/rhizome-collab
var skillFS embed.FS

const skillName = "rhizome-collab"

// agent is a coding agent Rhizome knows how to serve and where it loads skills.
type agent struct {
	Name     string // CLI/flag identifier: claude, codex
	Label    string // human-readable name
	Skills   string // directory that holds <skill>/SKILL.md
	Detected bool
	Reason   string // why it was (not) detected
}

// detectAgents reports the agents Rhizome can integrate with. Detection uses the
// same configuration directories the MCP integration documents, plus the agent
// binary on PATH. Undetected agents are still returned so --agents can force them.
func detectAgents(c config.Config, lookPath func(string) (string, error)) []agent {
	claudeDir, codexDir := c.Agents.ClaudeConfigDir, c.Agents.CodexHome
	agents := []agent{
		{Name: "claude", Label: "Claude Code", Skills: filepath.Join(claudeDir, "skills")},
		{Name: "codex", Label: "Codex", Skills: filepath.Join(codexDir, "skills")},
	}
	for i := range agents {
		a := &agents[i]
		dir := filepath.Dir(a.Skills)
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			a.Detected, a.Reason = true, "found "+dir
			continue
		}
		if p, err := lookPath(a.Name); err == nil {
			a.Detected, a.Reason = true, "found "+p+" on PATH"
			continue
		}
		a.Reason = "no " + dir + " and no " + a.Name + " on PATH"
	}
	return agents
}

// installSkill writes the embedded skill into <skillsDir>/<skillName>. It returns
// "installed", "updated", "unchanged" or "skipped". An existing symlink is left
// alone: it already points at a checkout the user manages.
func installSkill(skillsDir string) (string, error) {
	dest := filepath.Join(skillsDir, skillName)
	if st, err := os.Lstat(dest); err == nil && st.Mode()&os.ModeSymlink != 0 {
		target, _ := os.Readlink(dest)
		return "skipped (symlink to " + target + ")", nil
	} else if err == nil && !st.IsDir() {
		return "", fmt.Errorf("%s exists and is not a directory", dest)
	}
	root := "skills/" + skillName
	status := "unchanged"
	err := fs.WalkDir(skillFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.FromSlash(p))
		out := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		want, err := skillFS.ReadFile(p)
		if err != nil {
			return err
		}
		have, readErr := os.ReadFile(out)
		switch {
		case readErr == nil && bytes.Equal(have, want):
			return nil
		case readErr == nil:
			status = "updated"
		case errors.Is(readErr, fs.ErrNotExist):
			if status == "unchanged" {
				status = "installed"
			}
		default:
			return readErr
		}
		return os.WriteFile(out, want, 0o644)
	})
	if err != nil {
		return "", err
	}
	return status, nil
}

// pickAgents is a minimal checkbox prompt. in must deliver raw key bytes (the
// caller puts the terminal in raw mode). Keys: up/down or j/k move, space
// toggles, a toggles all, enter confirms, q or Esc or Ctrl-C aborts.
func pickAgents(in io.Reader, out io.Writer, agents []agent, checked []bool) ([]bool, error) {
	n := len(agents)
	cur := 0
	draw := func() {
		for i, a := range agents {
			cursor, box := "  ", "[ ]"
			if i == cur {
				cursor = "> "
			}
			if checked[i] {
				box = "[x]"
			}
			note := ""
			if !a.Detected {
				note = "  (not detected)"
			}
			fmt.Fprintf(out, "\r\x1b[2K%s%s %s  %s%s\r\n", cursor, box, a.Label, a.Skills, note)
		}
	}
	fmt.Fprint(out, "Install the /rhizome-collab skill for which agents?\r\n")
	fmt.Fprint(out, "space: toggle  a: all/none  enter: confirm  q: cancel\r\n")
	draw()
	keys := bufio.NewReader(in)
	for {
		key, err := readKey(keys)
		if err != nil {
			return nil, err
		}
		switch key {
		case "up", "k":
			cur = (cur + n - 1) % n
		case "down", "j":
			cur = (cur + 1) % n
		case " ":
			checked[cur] = !checked[cur]
		case "a":
			all := true
			for _, c := range checked {
				all = all && c
			}
			for i := range checked {
				checked[i] = !all
			}
		case "\r", "\n":
			return checked, nil
		case "q", "esc", "\x03":
			return nil, errors.New("cancelled")
		default:
			continue
		}
		fmt.Fprintf(out, "\x1b[%dA", n)
		draw()
	}
}

// readKey returns one key press: a single byte as a string, or "up", "down" or
// "esc" for the arrow and escape keys. Other CSI sequences are returned verbatim
// and ignored by the caller. A lone Esc is only recognized when no bytes follow
// it in the buffer, which is how terminals deliver it in raw mode.
func readKey(r *bufio.Reader) (string, error) {
	b, err := r.ReadByte()
	if err != nil {
		return "", err
	}
	if b != '\x1b' {
		return string(b), nil
	}
	if r.Buffered() == 0 {
		return "esc", nil
	}
	next, err := r.ReadByte()
	if err != nil {
		return "", err
	}
	if next != '[' {
		return "esc", nil
	}
	final, err := r.ReadByte()
	if err != nil {
		return "", err
	}
	switch final {
	case 'A':
		return "up", nil
	case 'B':
		return "down", nil
	}
	return "\x1b[" + string(final), nil
}

// parseAgentFlag resolves --agents into a selection over agents.
func parseAgentFlag(value string, agents []agent) ([]bool, error) {
	checked := make([]bool, len(agents))
	if value == "all" {
		for i := range checked {
			checked[i] = true
		}
		return checked, nil
	}
	known := map[string]int{}
	for i, a := range agents {
		known[a.Name] = i
	}
	for _, name := range strings.Split(value, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		i, ok := known[name]
		if !ok {
			names := make([]string, 0, len(known))
			for k := range known {
				names = append(names, k)
			}
			sort.Strings(names)
			return nil, fmt.Errorf("unknown agent %q (known: %s, all)", name, strings.Join(names, ", "))
		}
		checked[i] = true
	}
	return checked, nil
}

func setup(args []string) error {
	opts := flag.NewFlagSet("setup", flag.ContinueOnError)
	agentsFlag := opts.String("agents", "", "comma-separated agents to install for (claude, codex) or \"all\"; omit for an interactive picker")
	config.Bind(opts)
	list := opts.Bool("list", false, "print detected agents and exit")
	if err := opts.Parse(args); err != nil {
		return err
	}
	if opts.NArg() != 0 {
		return fmt.Errorf("unexpected setup arguments")
	}
	c, err := config.FromFlags(opts)
	if err != nil {
		return err
	}
	agents := detectAgents(c, exec.LookPath)
	if *list {
		for _, a := range agents {
			state := "not detected"
			if a.Detected {
				state = "detected"
			}
			fmt.Printf("%-7s %-12s %s (%s)\n", a.Name, state, a.Skills, a.Reason)
		}
		return nil
	}
	var checked []bool
	switch {
	case *agentsFlag != "":
		if checked, err = parseAgentFlag(*agentsFlag, agents); err != nil {
			return err
		}
	case term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())):
		checked = make([]bool, len(agents))
		for i, a := range agents {
			checked[i] = a.Detected
		}
		state, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			return err
		}
		checked, err = pickAgents(os.Stdin, os.Stdout, agents, checked)
		_ = term.Restore(int(os.Stdin.Fd()), state)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("stdin is not a terminal; pass --agents claude,codex or --agents all")
	}
	any := false
	for i, a := range agents {
		if !checked[i] {
			continue
		}
		any = true
		status, err := installSkill(a.Skills)
		if err != nil {
			return fmt.Errorf("%s: %w", a.Label, err)
		}
		fmt.Printf("%s: %s %s\n", a.Label, status, filepath.Join(a.Skills, skillName))
	}
	if !any {
		fmt.Println("nothing selected; no changes made")
	}
	return nil
}
