package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The subprocess uses the real entry point so failures must obey CLI framing.
func TestConfigProcess(t *testing.T) {
	if os.Getenv("RHIZOME_CONFIG_TEST_PROCESS") != "1" {
		return
	}
	for i, a := range os.Args {
		if a == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			break
		}
	}
	if err := entry(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func configProcess(t *testing.T, home string, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestConfigProcess$", "--"}, args...)...)
	for _, e := range os.Environ() {
		key, _, _ := strings.Cut(e, "=")
		switch key {
		case "HOME", "RHIZOME_CONFIG", "RHIZOME_RUNTIME_DIR", "CODEX_HOME", "CLAUDE_CONFIG_DIR", "RHIZOME_CONFIG_TEST_PROCESS":
			continue
		}
		cmd.Env = append(cmd.Env, e)
	}
	cmd.Env = append(cmd.Env, "HOME="+home, "RHIZOME_CONFIG_TEST_PROCESS=1")
	return cmd
}

func TestConfigCLIFailures(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, "config.toml")
	for _, body := range []string{"unknown = 1", "mcp.agent = 1", `runtime_dir = "relative"`} {
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"mcp"}, {"setup", "--list"}, {"service", "run"}, {"service", "status"}, {"config"}} {
			cmd := configProcess(t, home, append(args, "--config", p)...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err == nil || stdout.Len() != 0 || !strings.Contains(stderr.String(), p) {
				t.Fatalf("%v stdout=%s stderr=%s err=%v", args, &stdout, &stderr, err)
			}
		}
	}
}

func TestConfigSetupAndMCP(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, "config.toml")
	rt := filepath.Join(home, "runtime")
	claude, codex := filepath.Join(home, "claude"), filepath.Join(home, "codex")
	for _, dir := range []string{claude, codex} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	body := fmt.Sprintf("runtime_dir = %q\nagents.claude_config_dir = %q\nagents.codex_home = %q\nmcp.agent = 'codex'\nmcp.codex_bin = 'missing-codex-executable-for-test'\n", rt, claude, codex)
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	cmd := configProcess(t, home, "setup", "--list", "--config", p)
	out, err := cmd.Output()
	if err != nil || !bytes.Contains(out, []byte(filepath.Join(claude, "skills"))) || !bytes.Contains(out, []byte(filepath.Join(codex, "skills"))) {
		t.Fatalf("%s %v", out, err)
	}
	cmd = configProcess(t, home, "mcp", "--config", p)
	out, err = cmd.CombinedOutput()
	if err == nil || !bytes.Contains(out, []byte("missing-codex-executable-for-test")) {
		t.Fatalf("Codex default ignored: %s %v", out, err)
	}
	for _, envOverride := range []bool{false, true} {
		want := rt
		cmd = configProcess(t, home, "mcp", "--config", p, "--agent", "claude")
		if envOverride {
			want = filepath.Join(home, "env-runtime")
			cmd.Env = append(cmd.Env, "RHIZOME_RUNTIME_DIR="+want)
		}
		in, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintln(in, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"config-test","version":"1"}}}`)
		line, err := bufio.NewReader(out).ReadString('\n')
		if err != nil || !strings.Contains(line, `"result"`) {
			in.Close()
			cmd.Wait()
			t.Fatalf("%s %v %s", line, err, &stderr)
		}
		registrations, _ := filepath.Glob(filepath.Join(want, "*.json"))
		st, statErr := os.Stat(filepath.Join(want, "token"))
		in.Close()
		if err := cmd.Wait(); err != nil {
			t.Fatalf("%v %s", err, &stderr)
		}
		if len(registrations) == 0 {
			t.Fatal("no node registration in configured runtime")
		}
		if statErr != nil || st.Mode().Perm() != 0600 {
			t.Fatalf("token: %v %v", st, statErr)
		}
	}
}

func TestConfigObserverRuntime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("graceful subprocess signal requires Unix")
	}
	for _, envOverride := range []bool{false, true} {
		home := t.TempDir()
		p := filepath.Join(home, "config.toml")
		rt := filepath.Join(home, "runtime")
		claude, codex := filepath.Join(home, "claude"), filepath.Join(home, "codex")
		for _, dir := range []string{claude, codex} {
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
		}
		body := fmt.Sprintf("runtime_dir = %q\nservice.claude_root = %q\nservice.codex_root = %q\n", rt, claude, codex)
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		cmd := configProcess(t, home, "service", "run", "--config", p)
		if envOverride {
			rt = filepath.Join(home, "env-runtime")
			cmd.Env = append(cmd.Env, "RHIZOME_RUNTIME_DIR="+rt)
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		deadline := time.NewTimer(10 * time.Second)
		tick := time.NewTicker(10 * time.Millisecond)
		ready := false
		for !ready {
			select {
			case err := <-done:
				tick.Stop()
				deadline.Stop()
				t.Fatalf("observer exited: %v %s", err, &stderr)
			case <-deadline.C:
				cmd.Process.Kill()
				<-done
				tick.Stop()
				t.Fatalf("observer not ready: %s", &stderr)
			case <-tick.C:
				_, err := os.Stat(filepath.Join(rt, "observer-health.json"))
				ready = err == nil
			}
		}
		tick.Stop()
		deadline.Stop()
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			cmd.Process.Kill()
			<-done
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatalf("observer shutdown: %v %s", err, &stderr)
		}
		if stdout.Len() != 0 {
			t.Fatal(stdout.String())
		}
	}
}
