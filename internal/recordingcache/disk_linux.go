//go:build linux

package recordingcache

import (
	"fmt"
	"syscall"
)

func ensureSystemDisk(path string) error {
	var root, target syscall.Stat_t
	if err := syscall.Stat("/", &root); err != nil {
		return err
	}
	if err := syscall.Stat(path, &target); err != nil {
		return err
	}
	if target.Dev != root.Dev {
		return fmt.Errorf("录像暂存目录必须位于系统盘")
	}
	return nil
}

func diskSpace(path string) (total, free uint64, err error) {
	var stat syscall.Statfs_t
	if err = syscall.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	return stat.Blocks * uint64(stat.Bsize), stat.Bavail * uint64(stat.Bsize), nil
}

func syncDirectory(path string) error {
	dir, err := openDirectory(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
