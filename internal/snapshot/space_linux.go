//go:build linux

package snapshot

import (
	"fmt"
	"math"
	"syscall"
)

func platformSpace(path string) (uint64, uint64, error) {
	return readPlatformSpace(path, false)
}

func platformXFSSpace(path string) (uint64, uint64, error) {
	return readPlatformSpace(path, true)
}

func readPlatformSpace(path string, requireXFS bool) (uint64, uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	// 数据盘掉线后不能把同路径下系统盘的空闲容量误认为 XFS 已恢复。
	if requireXFS && stat.Type != 0x58465342 {
		return 0, 0, fmt.Errorf("%s 不在 XFS 数据盘上，拒绝使用替代文件系统容量", path)
	}
	blockSize := uint64(stat.Bsize)
	return saturatingMultiply(stat.Blocks, blockSize), saturatingMultiply(stat.Bavail, blockSize), nil
}

func saturatingMultiply(left, right uint64) uint64 {
	if right != 0 && left > math.MaxUint64/right {
		return math.MaxUint64
	}
	return left * right
}
