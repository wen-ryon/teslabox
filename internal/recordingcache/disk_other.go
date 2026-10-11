//go:build !linux && !windows

package recordingcache

import "fmt"

func ensureSystemDisk(string) error {
	return fmt.Errorf("当前系统不支持录像暂存空间检查")
}

func diskSpace(string) (uint64, uint64, error) {
	return 0, 0, fmt.Errorf("当前系统不支持录像暂存空间检查")
}

func syncDirectory(string) error { return nil }
