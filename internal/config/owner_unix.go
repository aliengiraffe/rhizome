//go:build !windows

package config

import (
	"fmt"
	"os"
	"syscall"
)

func checkOwner(_ *os.File, st os.FileInfo) error {
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || st.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("file must be owned by current user and not group- or world-writable")
	}
	return nil
}
