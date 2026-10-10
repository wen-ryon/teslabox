package webtls

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func checkPrivateDirectory(directory string) error {
	if !filepath.IsAbs(directory) {
		return errors.New("HTTPS 证书目录必须使用绝对路径")
	}
	for current := directory; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() {
			return errors.New("HTTPS 证书目录不可用或包含符号链接")
		}
		value, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (value.Uid != 0 && value.Uid != uint32(os.Geteuid())) {
			return errors.New("HTTPS 证书目录所有者无效")
		}
		if current == directory {
			if value.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != 0o700 {
				return errors.New("HTTPS 私有证书目录权限必须为 0700")
			}
		} else if info.Mode().Perm()&0o022 != 0 && !(value.Uid == 0 && info.Mode()&os.ModeSticky != 0) {
			return errors.New("HTTPS 证书上级目录允许非所有者写入")
		}
		if filepath.Dir(current) == current {
			return nil
		}
	}
}

func checkPrivateBundle(info os.FileInfo) error {
	value, ok := info.Sys().(*syscall.Stat_t)
	if !ok || value.Uid != uint32(os.Geteuid()) || value.Nlink != 1 || info.Mode().Perm() != 0o600 {
		return errors.New("HTTPS 私钥文件必须归当前用户所有且权限为 0600")
	}
	return nil
}
