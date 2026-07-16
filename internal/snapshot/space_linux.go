//go:build linux

package snapshot

import (
	"fmt"
	"math"
	"syscall"
)

func platformSpace(path string) (uint64, uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, fmt.Errorf("statfs %s: %w", path, err)
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
