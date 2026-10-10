package webui

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func privateCarPlayToken(path string, info os.FileInfo) error {
	value, ok := info.Sys().(*syscall.Stat_t)
	if !ok || value.Uid != uint32(os.Geteuid()) || value.Nlink != 1 || info.Mode().Perm() != 0o600 {
		return errors.New("CarPlay 令牌权限无效")
	}
	directory, err := os.Lstat(filepath.Dir(path))
	if err != nil || !directory.IsDir() || directory.Mode().Perm() != 0o700 {
		return errors.New("CarPlay 令牌目录权限无效")
	}
	owner, ok := directory.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return errors.New("CarPlay 令牌目录所有者无效")
	}
	return nil
}
