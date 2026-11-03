package service

import (
	"context"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// startFinisher launches binary with args detached from this process and its
// job so ending the scheduled task does not end the finisher.
func startFinisher(ctx context.Context, c Config, binary string, args []string) error {
	out, err := finisherOutput(c)
	if err != nil {
		return err
	}
	defer out.Close()
	for _, flags := range []uint32{
		windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS | windows.CREATE_BREAKAWAY_FROM_JOB,
		windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
	} {
		cmd := exec.Command(binary, args...)
		cmd.Stdout, cmd.Stderr = out, out
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
		if err = cmd.Start(); err == nil {
			return cmd.Process.Release()
		}
	}
	return err
}
