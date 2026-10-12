package updater

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

var errInstallationBusy = errors.New("另一个安装或恢复事务正在运行")

func installationActive(dir string) bool {
	path := filepath.Join(dir, "install.lock")
	if err := plainPath(path); err != nil {
		return true
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	defer f.Close()
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

func lockInstallation(dir string) (func(), error) {
	if err := plainPath(dir); err != nil {
		return nil, err
	}
	if err := durableMkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "install.lock")
	if err := plainPath(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil {
		err = safeFileMetadata(info)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errInstallationBusy
		}
		return nil, err
	}
	// 锁文件不删除，否则另一个进程可能锁住新 inode，造成并发安装。
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
