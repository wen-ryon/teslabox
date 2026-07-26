//go:build linux

package mediaassets

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"

	"golang.org/x/sys/unix"
)

func platformReclaimSpace(ctx context.Context, imagePath string) (SpaceReclaimResult, error) {
	image, err := os.OpenFile(imagePath, os.O_RDWR, 0)
	if err != nil {
		return SpaceReclaimResult{}, fmt.Errorf("打开 TeslaCam 镜像: %w", err)
	}
	defer image.Close()
	info, err := image.Stat()
	if err != nil {
		return SpaceReclaimResult{}, fmt.Errorf("读取 TeslaCam 镜像信息: %w", err)
	}
	if !info.Mode().IsRegular() {
		return SpaceReclaimResult{}, errors.New("TeslaCam 镜像不是普通文件")
	}
	space, err := scanExFATFreeSpace(ctx, image, info.Size())
	if err != nil {
		return SpaceReclaimResult{}, err
	}
	before, err := allocatedBytes(image)
	if err != nil {
		return SpaceReclaimResult{}, err
	}
	result := SpaceReclaimResult{
		CandidateBytes: space.freeBytes,
		FreeClusters:   space.freeBytes / space.clusterSize,
		ClusterSize:    space.clusterSize,
		RangeCount:     len(space.extents),
	}
	for _, extent := range space.extents {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if extent.offset > math.MaxInt64 || extent.length > math.MaxInt64 {
			return result, errors.New("exFAT 空闲区间超出系统打洞限制")
		}
		err := unix.Fallocate(
			int(image.Fd()),
			unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE,
			int64(extent.offset),
			int64(extent.length),
		)
		if err != nil {
			return result, fmt.Errorf("回收 exFAT 空闲簇: %w", err)
		}
		result.ProcessedRanges++
	}
	if err := image.Sync(); err != nil {
		return result, fmt.Errorf("同步 TeslaCam 镜像回收结果: %w", err)
	}
	after, err := allocatedBytes(image)
	if err != nil {
		return result, err
	}
	if before > after {
		result.ReclaimedBytes = before - after
	}
	return result, nil
}

func allocatedBytes(file *os.File) (uint64, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return 0, fmt.Errorf("读取 TeslaCam 镜像物理占用: %w", err)
	}
	if stat.Blocks < 0 {
		return 0, errors.New("TeslaCam 镜像物理块数量无效")
	}
	if uint64(stat.Blocks) > math.MaxUint64/512 {
		return 0, errors.New("TeslaCam 镜像物理占用超出统计上限")
	}
	return uint64(stat.Blocks) * 512, nil
}
