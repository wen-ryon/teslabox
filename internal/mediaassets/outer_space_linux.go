//go:build linux

package mediaassets

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

func platformOuterSpace(path string) (uint64, uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, 0, fmt.Errorf("读取外层文件系统空间: %w", err)
	}
	blockSize := uint64(stat.Bsize)
	if stat.Blocks > math.MaxUint64/blockSize || stat.Bavail > math.MaxUint64/blockSize {
		return 0, 0, fmt.Errorf("外层文件系统空间统计溢出")
	}
	return stat.Blocks * blockSize, stat.Bavail * blockSize, nil
}
