//go:build !windows

package config

import (
	"os"
	"syscall"
	"testing"
)

type foreignInfo struct{ os.FileInfo }

func (f foreignInfo) Sys() any { return &syscall.Stat_t{Uid: uint32(os.Getuid() + 1)} }
func TestForeignOwner(t *testing.T) {
	p := fixture(t, "")
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkOwner(nil, foreignInfo{st}); err == nil {
		t.Fatal("foreign owner accepted")
	}
}
