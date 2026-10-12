package updater

import (
	"errors"
	"os"
	"syscall"
)

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func safeFileMetadata(info os.FileInfo) error {
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("事务文件不能允许组或其他用户写入")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return errors.New("事务文件不允许硬链接")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return errors.New("事务文件必须属于安装进程用户")
	}
	return nil
}

func safeParentDirectory(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) {
		return errors.New("目录属于其他用户")
	}
	// /tmp 等粘滞目录可承载私有临时测试目录，普通可写父目录必须拒绝。
	if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
		return errors.New("父目录允许其他用户替换文件")
	}
	return nil
}
