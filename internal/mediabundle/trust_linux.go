//go:build linux

package mediabundle

import (
	"errors"
	"os"
	"syscall"
)

func trustedMediaMember(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) {
		return errors.New("媒体版本成员所有者不受信任")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if info.Mode().Perm()&0022 != 0 && !(info.Mode()&os.ModeSticky != 0 && stat.Uid == 0) {
		return errors.New("媒体版本成员可由其他用户写入")
	}
	if info.Mode().IsRegular() && stat.Nlink != 1 {
		return errors.New("媒体版本成员不能含硬链接")
	}
	return nil
}
