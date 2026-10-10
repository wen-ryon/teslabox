package certdelivery

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func checkDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("证书分发目录必须使用绝对路径")
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() {
			return errors.New("证书分发目录包含无效目录或符号链接")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
			return errors.New("证书分发目录所有者无效")
		}
		if current == path {
			if stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm() != 0o700 {
				return errors.New("证书分发私有目录权限必须为 0700")
			}
		} else if info.Mode().Perm()&0o022 != 0 && !(stat.Uid == 0 && info.Mode()&os.ModeSticky != 0) {
			return errors.New("证书分发上级目录允许其他用户写入")
		}
		if filepath.Dir(current) == current {
			return nil
		}
	}
}

func checkFile(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || info.Mode().Perm() != 0o600 {
		return errors.New("证书分发私有文件权限必须为 0600")
	}
	return nil
}

func syncDirectory(file *os.File) error { return file.Sync() }
