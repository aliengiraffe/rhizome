package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func managerCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	if name == "systemctl" {
		uid := os.Geteuid()
		cmd.Env = systemdUserEnv(fmt.Sprintf("/run/user/%d", uid), uid)
	}
	return cmd
}

// A multiplexer or SSH shell can lose the login environment while the user manager is still running.
func systemdUserEnv(dir string, uid int) []string {
	if os.Getenv("XDG_RUNTIME_DIR") != "" || os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
		return nil
	}
	var st syscall.Stat_t
	if err := syscall.Lstat(dir, &st); err != nil || st.Uid != uint32(uid) ||
		st.Mode&syscall.S_IFMT != syscall.S_IFDIR || st.Mode&0777 != 0700 {
		return nil
	}
	return append(os.Environ(), "XDG_RUNTIME_DIR="+dir)
}
