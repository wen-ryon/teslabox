//go:build linux

package carplaybundle

import (
	"errors"
	"os"
	"syscall"
)

func trustedParent(info os.FileInfo, allowSticky bool) error {
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 && int(owner.Uid) != os.Geteuid() {
		return errors.New("CarPlay 安装目录所有者不安全")
	}
	// 仅 root 拥有的 /tmp 祖先可以例外，实际安装根和版本树不能借用粘滞位放行。
	if info.Mode().Perm()&0022 != 0 && !(allowSticky && owner.Uid == 0 && info.Mode()&os.ModeSticky != 0) {
		return errors.New("CarPlay 安装目录不能由其他用户写入")
	}
	return nil
}

func trustedMember(info os.FileInfo) error {
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Geteuid() || owner.Nlink != 1 {
		return errors.New("CarPlay 版本成员所有者或链接数不安全")
	}
	return nil
}
