package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"rhizome/internal/config"
	"rhizome/internal/service"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

// This file is the counterpart of setup: `rhizome uninstall` removes what
// setup installed into an agent's user-scope configuration (the skill, the MCP
// registration, the hooks and the OpenCode plugins) and optionally stops the
// background service and removes its startup definition. It follows setup's
// rules: only Rhizome-owned entries are touched, a changed file is backed up
// and replaced atomically, malformed files and symlinked skill or plugin paths
// are reported and left alone, and nothing under ~/.rhizome is removed.

// Component names accepted by --only.
const (
	componentSkills  = "skills"
	componentMCP     = "mcp"
	componentHooks   = "hooks"
	componentPlugins = "plugins"
)

var allComponents = []string{componentSkills, componentMCP, componentHooks, componentPlugins}

// parseComponents resolves --only into the set of components to remove; an
// empty value selects every component.
func parseComponents(value string) (map[string]bool, error) {
	selected := map[string]bool{}
	if strings.TrimSpace(value) == "" {
		for _, c := range allComponents {
			selected[c] = true
		}
		return selected, nil
	}
	for _, name := range strings.Split(value, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		known := false
		for _, c := range allComponents {
			known = known || c == name
		}
		if !known {
			return nil, fmt.Errorf("unknown component %q (known: %s)", name, strings.Join(allComponents, ", "))
		}
		selected[name] = true
	}
	if len(selected) == 0 {
		return nil, errors.New("--only selects no component")
	}
	return selected, nil
}

// removalStatus classifies what uninstall found for one component.
type removalStatus int

const (
	removalAbsent    removalStatus = iota // nothing of Rhizome's at the path
	removalUnchanged                      // the file exists and holds no Rhizome entry
	removalRemove                         // Rhizome content is (or would be) removed
	removalSkipped                        // a symlink Rhizome never follows
	removalError
)

// removal reports one component of one agent: what uninstall would do, did,
// or could not do.
type removal struct {
	Label  string // skill, mcp, hooks, plugin or tui
	What   string // text between the status and the path, e.g. "rhizome server in "
	Path   string
	Status removalStatus
	Note   string // symlink target for skipped
	Backup string
	Err    error
}

// describe renders the status for one mode: "list" reports presence, "plan"
// what a run would do, and "apply" what it did.
func (r removal) describe(mode string) string {
	var status string
	switch r.Status {
	case removalRemove:
		status = map[string]string{"list": "installed", "plan": "would remove", "apply": "removed"}[mode]
	case removalUnchanged:
		if mode == "list" {
			return "absent " + r.What + r.Path
		}
		return "unchanged (no " + r.What + r.Path + ")"
	case removalAbsent:
		status = "absent"
	case removalSkipped:
		status = "skipped (symlink to " + r.Note + ")"
	case removalError:
		return "error: " + r.Err.Error()
	}
	s := status + " " + r.What + r.Path
	if r.Backup != "" {
		s += " (backup: " + r.Backup + ")"
	}
	return s
}

// agentUninstaller removes the selected components of an agent. With apply
// false it only inspects, so the same code renders --list, --dry-run and the
// confirmation plan.
type agentUninstaller struct {
	only  map[string]bool
	apply bool
	now   func() time.Time
}

// remove inspects or removes every selected component of a, in setup's order,
// and reports each one. A failing component does not stop the others.
func (u agentUninstaller) remove(a agent) []removal {
	var out []removal
	if u.only[componentSkills] {
		for _, name := range skillNames() {
			out = append(out, u.removeTree("skill", filepath.Join(a.Skills, name)))
		}
	}
	if a.Name == "opencode" {
		if u.only[componentPlugins] {
			for _, p := range a.plugins() {
				out = append(out, u.removeFile(p.Label, p.Dest))
			}
		}
		return out
	}
	if u.only[componentMCP] {
		edit := jsonEdit(stripJSONServer)
		if a.Name == "codex" {
			edit = stripCodexServer
		}
		out = append(out, u.stripEntry("mcp", serverName+" server in ", a.MCP, edit))
	}
	if u.only[componentHooks] {
		out = append(out, u.stripEntry("hooks", serverName+" hooks in ", a.Hooks, jsonEdit(func(doc map[string]any) error {
			_, err := stripHooks(doc)
			return err
		})))
	}
	return out
}

// removeTree removes the Rhizome-owned directory at path (a skill). A symlink
// is reported and never followed; a missing path is absent.
func (u agentUninstaller) removeTree(label, path string) removal {
	r := removal{Label: label, Path: path}
	st, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return r
	case err != nil:
		r.Status, r.Err = removalError, err
		return r
	case st.Mode()&os.ModeSymlink != 0:
		r.Status = removalSkipped
		r.Note, _ = os.Readlink(path)
		return r
	case !st.IsDir():
		r.Status, r.Err = removalError, fmt.Errorf("%s exists and is not a directory", path)
		return r
	}
	r.Status = removalRemove
	if u.apply {
		if err := os.RemoveAll(path); err != nil {
			r.Status, r.Err = removalError, err
		}
	}
	return r
}

// removeFile removes one Rhizome-owned plugin file. A symlink is reported and
// never followed; a missing path is absent. The TUI plugin's own directory is
// removed afterwards when it is a real, now empty directory.
func (u agentUninstaller) removeFile(label, path string) removal {
	r := removal{Label: label, Path: path}
	st, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return r
	case err != nil:
		r.Status, r.Err = removalError, err
		return r
	case st.Mode()&os.ModeSymlink != 0:
		r.Status = removalSkipped
		r.Note, _ = os.Readlink(path)
		return r
	case !st.Mode().IsRegular():
		r.Status, r.Err = removalError, fmt.Errorf("%s is not a regular file", path)
		return r
	}
	r.Status = removalRemove
	if !u.apply {
		return r
	}
	if err := os.Remove(path); err != nil {
		r.Status, r.Err = removalError, err
		return r
	}
	if label == "tui" {
		dir := filepath.Dir(path)
		if st, err := os.Lstat(dir); err == nil && st.IsDir() {
			_ = os.Remove(dir) // only succeeds when empty
		}
	}
	return r
}

// stripEntry removes Rhizome's entry from the configuration file at path
// through edit, with editFile's backup and atomic replacement. A missing file
// is absent and never created; a file without the entry is unchanged and not
// rewritten; a malformed file is reported and left untouched.
func (u agentUninstaller) stripEntry(label, what, path string, edit func([]byte) ([]byte, error)) removal {
	r := removal{Label: label, What: what, Path: path}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return r
	}
	var res fileResult
	var err error
	if u.apply {
		res, err = editFile(path, edit, u.now())
	} else {
		var f fileEdit
		f, err = previewFile(path, edit)
		res = f.fileResult
	}
	if res.Path != "" {
		r.Path = res.Path
	}
	if err != nil {
		r.Status, r.Err = removalError, err
		return r
	}
	r.Backup = res.Backup
	r.Status = removalUnchanged
	if res.Status != "unchanged" {
		r.Status = removalRemove
	}
	return r
}

// uninstallNotes lists what remains for the user after Rhizome was removed
// from agentName.
func uninstallNotes(agentName string) []string {
	switch agentName {
	case "claude":
		return []string{
			"restart Claude Code so it stops launching the removed rhizome server and hooks; MCP processes it already started end with it.",
			"project-scope entries are not edited: remove a rhizome entry from a project .mcp.json and Rhizome hooks from a project .claude/settings.json yourself.",
		}
	case "codex":
		return []string{
			"start a fresh Codex session so the removed hooks and server are no longer offered; remove any Rhizome hooks or [mcp_servers.rhizome] you keep at project scope yourself.",
		}
	case "opencode":
		return []string{
			"remove the hand-added rhizome MCP entry from your OpenCode config yourself (uninstall does not edit it, as setup never wrote it), then restart OpenCode so the removed plugins unload.",
		}
	}
	return nil
}

// serviceRemoval reports what the service step found and did.
type serviceRemoval struct {
	Found   bool   // a startup definition existed before the step
	Running bool   // the observer held its runtime lock before the step
	Runtime string // effective runtime directory from the configuration file
}

// serviceUninstaller stops the observer and removes its startup definition
// for the configuration at path, or only inspects it with apply false. The
// command takes it as a value so tests never touch a real service manager.
type serviceUninstaller func(ctx context.Context, path string, apply bool) (serviceRemoval, error)

// uninstallService resolves the configuration file at path exactly as the
// service commands do, reports a missing definition as not found without
// calling the service manager, and otherwise runs the uninstall lifecycle
// through manage (stop the observer, remove the native definition) and
// confirms the definition is gone. With apply false it only inspects.
func uninstallService(ctx context.Context, path string, apply bool, manage func(context.Context, string, service.Config) error) (serviceRemoval, error) {
	c, err := config.Resolve(map[string]string{"config": path}, os.Getenv)
	if err != nil {
		// Installation requires the file, so without it and without a
		// definition there is nothing to remove; with a definition the
		// resolution error is the actionable report.
		if _, statErr := os.Lstat(path); errors.Is(statErr, fs.ErrNotExist) {
			if status, inspectErr := service.Inspect(ctx, service.Config{ConfigPath: path}); inspectErr == nil && !status.Installed {
				return serviceRemoval{}, nil
			}
		}
		return serviceRemoval{}, err
	}
	sc := service.FromConfig(c)
	out := serviceRemoval{Runtime: sc.Observer.Runtime}
	status, err := service.Inspect(ctx, sc)
	if err != nil {
		return out, err
	}
	out.Found, out.Running = status.Installed, status.Running
	if !out.Found || !apply {
		return out, nil
	}
	manageErr := manage(ctx, "uninstall", sc)
	// Inspect reads local files only; report the final state even after cancellation.
	status, inspectErr := service.Inspect(context.Background(), sc)
	switch {
	case manageErr != nil:
		return out, fmt.Errorf("service not uninstalled: %w", manageErr)
	case inspectErr != nil:
		return out, fmt.Errorf("service uninstalled but its status could not be read: %w", inspectErr)
	case status.Installed:
		return out, errors.New("service manager reported success but the startup definition is still present")
	}
	return out, nil
}

// plainServiceUninstaller lets native command output reach the terminal and
// Ctrl-C cancel the service manager call.
func plainServiceUninstaller(ctx context.Context, path string, apply bool) (serviceRemoval, error) {
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return uninstallService(ctx, path, apply, service.Manage)
}

// uninstaller is one `rhizome uninstall` run over the selected agents and,
// optionally, the service.
type uninstaller struct {
	agents  []agent
	checked []bool
	service bool
	path    string // configuration file the service step resolves
	only    map[string]bool
	now     func() time.Time
	out     io.Writer
	svc     serviceUninstaller
}

// run performs the selected removals with apply true or reports the plan with
// apply false: the service first, then the agents in setup's order. Every
// component is reported; the first error does not stop the rest, and a run
// with any failed component returns an error after the complete report.
func (u uninstaller) run(apply bool) error {
	mode := "plan"
	if apply {
		mode = "apply"
	}
	failed, changed, backups := false, false, false
	if u.service {
		fmt.Fprintln(u.out, "Background service:")
		outcome, err := u.svc(context.Background(), u.path, apply)
		switch {
		case err != nil:
			failed = true
			fmt.Fprintf(u.out, "  service: error: %v\n", err)
		case !outcome.Found && outcome.Running:
			fmt.Fprintln(u.out, "  service: not installed (an observer runs outside the service; stop that process yourself)")
		case !outcome.Found:
			fmt.Fprintln(u.out, "  service: not installed")
		case !apply:
			fmt.Fprintf(u.out, "  service: would stop the observer and remove the startup definition (configuration %s)\n", u.path)
		default:
			changed = true
			fmt.Fprintf(u.out, "  service: removed the startup definition; observer stopped (configuration %s)\n", u.path)
		}
	}
	a := agentUninstaller{only: u.only, apply: apply, now: u.now}
	for i, ag := range u.agents {
		if !u.checked[i] {
			continue
		}
		fmt.Fprintf(u.out, "%s:\n", ag.Label)
		removed := false
		for _, r := range a.remove(ag) {
			fmt.Fprintf(u.out, "  %-6s %s\n", r.Label+":", r.describe(mode))
			failed = failed || r.Status == removalError
			removed = removed || r.Status == removalRemove
			backups = backups || r.Backup != ""
		}
		changed = changed || removed
		if apply && removed {
			for _, note := range uninstallNotes(ag.Name) {
				fmt.Fprintf(u.out, "  next:  %s\n", note)
			}
		}
	}
	if apply && changed {
		if backups {
			fmt.Fprintln(u.out, "Backups named above are kept; delete them once you no longer need them.")
		}
		fmt.Fprintln(u.out, "Everything under ~/.rhizome (configuration, identity, pairings, history) is kept; remove that directory yourself to discard it.")
	}
	if failed {
		return errors.New("uninstall finished with errors; see the report above")
	}
	return nil
}

// list prints which Rhizome components are installed for every agent.
func (u uninstaller) list() {
	a := agentUninstaller{only: u.only, now: u.now}
	for _, ag := range u.agents {
		fmt.Fprintf(u.out, "%-8s %s\n", ag.Name, ag.Label)
		for _, r := range a.remove(ag) {
			fmt.Fprintf(u.out, "  %-6s %s\n", r.Label+":", r.describe("list"))
		}
	}
}

// present reports whether any selected component of a holds Rhizome content.
func (u uninstaller) present(a agent) bool {
	for _, r := range (agentUninstaller{only: u.only, now: u.now}).remove(a) {
		if r.Status == removalRemove {
			return true
		}
	}
	return false
}

func uninstall(args []string) error {
	opts := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	agentsFlag := opts.String("agents", "", "comma-separated agents to remove Rhizome from (claude, codex, opencode) or \"all\"; omit for an interactive picker")
	svc := opts.Bool("service", false, "stop the background observer and remove its startup definition")
	all := opts.Bool("all", false, "shorthand for --agents all --service")
	only := opts.String("only", "", "comma-separated components to remove: "+strings.Join(allComponents, ", ")+" (default: every component)")
	dryRun := opts.Bool("dry-run", false, "print every file that would change and every path that would be deleted, then exit without writing")
	yes := opts.Bool("yes", false, "skip the confirmation prompt (required without a terminal unless --dry-run)")
	list := opts.Bool("list", false, "print which Rhizome components are installed for each agent and exit")
	config.Bind(opts)
	if err := opts.Parse(args); err != nil {
		return err
	}
	if opts.NArg() != 0 {
		return fmt.Errorf("unexpected uninstall arguments")
	}
	visited := map[string]bool{}
	opts.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	if *all && visited["agents"] {
		return errors.New("--all already selects every agent; drop --agents")
	}
	if *all {
		*agentsFlag, *svc = "all", true
	}
	components, err := parseComponents(*only)
	if err != nil {
		return err
	}
	c, path, _, err := resolveForSetup(opts, false)
	if err != nil {
		return err
	}
	u := uninstaller{agents: detectAgents(c, exec.LookPath), service: *svc, path: path, only: components, now: time.Now, out: os.Stdout, svc: plainServiceUninstaller}
	if *list {
		u.list()
		return nil
	}
	tty := usableTerminal()
	switch {
	case *agentsFlag != "":
		if u.checked, err = parseAgentFlag(*agentsFlag, u.agents); err != nil {
			return err
		}
	case *svc:
		u.checked = make([]bool, len(u.agents))
	case tty:
		// Preselect the agents where Rhizome is currently installed.
		picker := append([]agent(nil), u.agents...)
		u.checked = make([]bool, len(u.agents))
		for i := range picker {
			u.checked[i] = u.present(picker[i])
			picker[i].Detected = u.checked[i]
		}
		state, err := term.MakeRaw(int(os.Stdin.Fd()))
		if err != nil {
			return err
		}
		u.checked, err = pickAgentsTitled(os.Stdin, os.Stdout, "Remove Rhizome (MCP server, hooks, plugins and the skill) from which agents?", "(nothing installed)", picker, u.checked)
		_ = term.Restore(int(os.Stdin.Fd()), state)
		if err != nil {
			return err
		}
	default:
		return errors.New("stdin is not a terminal; pass --agents claude,codex,opencode, --agents all, --all or --service")
	}
	if !tty && !*yes && !*dryRun {
		return errors.New("stdin is not a terminal; pass --yes to remove without confirmation, or --dry-run")
	}
	selected := false
	for _, ok := range u.checked {
		selected = selected || ok
	}
	if !selected && !u.service {
		fmt.Fprintln(u.out, "nothing selected; nothing removed")
		return nil
	}
	if *dryRun {
		return u.run(false)
	}
	if !*yes {
		if err := u.run(false); err != nil {
			return err
		}
		accept, err := prompter{in: bufio.NewReader(os.Stdin), out: os.Stdout}.yesNo("Remove the components listed above", false)
		if err != nil {
			return err
		}
		if !accept {
			fmt.Fprintln(u.out, "cancelled; nothing removed")
			return nil
		}
	}
	return u.run(true)
}
