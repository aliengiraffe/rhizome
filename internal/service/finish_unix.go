//go:build !windows

package service

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

// startFinisher launches binary with args detached from this process. On
// Linux, when systemd-run is available, the finisher runs as a transient user
// unit so stopping the observer's cgroup does not kill it; elsewhere a new
// session suffices because launchd only tracks the job's process group.
func startFinisher(ctx context.Context, c Config, binary string, args []string) error {
	out, err := finisherOutput(c)
	if err != nil {
		return err
	}
	defer out.Close()
	var cmd *exec.Cmd
	if runtime.GOOS == "linux" {
		if path, err := exec.LookPath("systemd-run"); err == nil {
			unit := fmt.Sprintf("rhizome-update-finish-%d", time.Now().UnixNano())
			cmd = exec.Command(path, append([]string{"--user", "--collect", "--quiet", "--unit=" + unit, "--", binary}, args...)...)
			cmd.Stdout, cmd.Stderr = out, out
			if err := cmd.Run(); err == nil {
				return nil
			}
			// Fall back to a detached process; the observer's stop may end it.
		}
	}
	cmd = exec.Command(binary, args...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
