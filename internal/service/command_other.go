//go:build !linux

package service

import (
	"context"
	"os/exec"
)

func managerCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}
