//go:build linux

package runtimecomponents

import (
	"errors"
	"os"
	"syscall"
)

func trustedDirectory(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) {
		return errors.New("组件目录所有者不受信任")
	}
	if info.Mode().Perm()&0022 != 0 && !(info.Mode()&os.ModeSticky != 0 && stat.Uid == 0) {
		return errors.New("组件目录可由其他用户写入")
	}
	return nil
}
func trustedFile(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return errors.New("组件文件不能含硬链接")
	}
	return nil
}
func syncDirectory(p string) error {
	d, err := os.Open(p)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func replaceArchive(source, target string) error { return os.Rename(source, target) }
