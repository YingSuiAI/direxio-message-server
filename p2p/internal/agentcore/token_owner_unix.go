//go:build !windows

package agentcore

import (
	"errors"
	"os"
	"syscall"
)

func validateTokenFileInfo(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return errors.New("token file is not a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("token file permissions are not protected")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint32(stat.Uid) != uint32(os.Geteuid()) {
		return errors.New("token file is not owned by the server user")
	}
	return nil
}
