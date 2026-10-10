//go:build linux

package automation

import (
	"errors"
	"os"
	"syscall"
)

func validatePrivateStateFile(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != 0o600 {
		return errors.New("CarPlay 恢复记录必须由服务账号持有且权限为 0600")
	}
	return nil
}

func syncStateDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
