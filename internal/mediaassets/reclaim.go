package mediaassets

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

const (
	mbrSectorSize           = uint64(512)
	mbrPartitionTableOffset = 446
	mbrPartitionEntrySize   = 16
	exFATDirectoryEntrySize = 32
	exFATBitmapEntryType    = 0x81
	exFATEndOfChain         = 0xfffffff8
	exFATBadCluster         = 0xfffffff7
	maxAllocationBitmapSize = 64 << 20
	maxFreeExtentCount      = 1 << 20
)

type freeExtent struct {
	offset uint64
	length uint64
}

type exFATFreeSpace struct {
	clusterSize uint64
	freeBytes   uint64
	extents     []freeExtent
}

type exFATLayout struct {
	partitionOffset uint64
	partitionBytes  uint64
	sectorSize      uint64
	clusterSize     uint64
	fatOffset       uint64
	fatLength       uint64
	clusterHeap     uint64
	clusterCount    uint32
	rootCluster     uint32
	activeFAT       uint8
	numberOfFATs    uint8
}

type allocationBitmap struct {
	firstCluster uint32
	dataLength   uint64
}

func boundedReclaimLength(extentLength, clusterSize, remaining uint64) uint64 {
	if extentLength == 0 || clusterSize == 0 || remaining == 0 {
		return 0
	}
	if extentLength <= remaining {
		return extentLength
	}
	length := remaining / clusterSize * clusterSize
	if length == 0 {
		return min(extentLength, clusterSize)
	}
	return min(extentLength, length)
}

func scanExFATFreeSpace(ctx context.Context, reader io.ReaderAt, imageSize int64) (exFATFreeSpace, error) {
	if reader == nil || imageSize <= 0 {
		return exFATFreeSpace{}, errors.New("TeslaCam 镜像读取器无效")
	}
	if err := ctx.Err(); err != nil {
		return exFATFreeSpace{}, err
	}
	layout, err := readExFATLayout(reader, uint64(imageSize))
	if err != nil {
		return exFATFreeSpace{}, err
	}
	bitmap, rootClusters, err := findAllocationBitmap(ctx, reader, layout)
	if err != nil {
		return exFATFreeSpace{}, err
	}
	bitmapBytes, bitmapClusters, err := readAllocationBitmap(ctx, reader, layout, bitmap)
	if err != nil {
		return exFATFreeSpace{}, err
	}
	protected := make(map[uint32]struct{}, len(rootClusters)+len(bitmapClusters))
	for _, cluster := range rootClusters {
		protected[cluster] = struct{}{}
	}
	for _, cluster := range bitmapClusters {
		protected[cluster] = struct{}{}
	}
	for cluster := range protected {
		if !bitmapAllocated(bitmapBytes, cluster-2) {
			return exFATFreeSpace{}, fmt.Errorf("exFAT 分配位图把关键元数据簇 %d 标记为空闲，拒绝回收", cluster)
		}
	}

	space := exFATFreeSpace{clusterSize: layout.clusterSize}
	var current *freeExtent
	for index := uint32(0); index < layout.clusterCount; index++ {
		if index%65536 == 0 {
			if err := ctx.Err(); err != nil {
				return exFATFreeSpace{}, err
			}
		}
		if bitmapAllocated(bitmapBytes, index) {
			current = nil
			continue
		}
		offset, err := clusterOffset(layout, index+2)
		if err != nil {
			return exFATFreeSpace{}, err
		}
		space.freeBytes, err = checkedAddUint64(space.freeBytes, layout.clusterSize)
		if err != nil {
			return exFATFreeSpace{}, fmt.Errorf("计算 exFAT 空闲容量: %w", err)
		}
		currentEnd := uint64(0)
		if current != nil {
			currentEnd, err = checkedAddUint64(current.offset, current.length)
			if err != nil {
				return exFATFreeSpace{}, fmt.Errorf("合并 exFAT 空闲区间: %w", err)
			}
		}
		if current != nil && currentEnd == offset {
			current.length, err = checkedAddUint64(current.length, layout.clusterSize)
			if err != nil {
				return exFATFreeSpace{}, fmt.Errorf("扩展 exFAT 空闲区间: %w", err)
			}
			continue
		}
		if len(space.extents) >= maxFreeExtentCount {
			return exFATFreeSpace{}, errors.New("exFAT 空闲区间过于碎片化，拒绝在低内存设备上继续回收")
		}
		space.extents = append(space.extents, freeExtent{offset: offset, length: layout.clusterSize})
		current = &space.extents[len(space.extents)-1]
	}
	return space, nil
}

func readExFATLayout(reader io.ReaderAt, imageSize uint64) (exFATLayout, error) {
	mbr := make([]byte, mbrSectorSize)
	if err := readExactAt(reader, mbr, 0); err != nil {
		return exFATLayout{}, fmt.Errorf("读取 TeslaCam 镜像 MBR: %w", err)
	}
	if mbr[510] != 0x55 || mbr[511] != 0xaa {
		return exFATLayout{}, errors.New("TeslaCam 镜像 MBR 签名无效")
	}
	entry := mbr[mbrPartitionTableOffset : mbrPartitionTableOffset+mbrPartitionEntrySize]
	startLBA := binary.LittleEndian.Uint32(entry[8:12])
	sectorCount := binary.LittleEndian.Uint32(entry[12:16])
	if startLBA == 0 || sectorCount == 0 {
		return exFATLayout{}, errors.New("TeslaCam 镜像第一分区无效")
	}
	partitionOffset, err := checkedMultiplyUint64(uint64(startLBA), mbrSectorSize)
	if err != nil {
		return exFATLayout{}, fmt.Errorf("计算 TeslaCam 分区偏移: %w", err)
	}
	partitionBytes, err := checkedMultiplyUint64(uint64(sectorCount), mbrSectorSize)
	if err != nil {
		return exFATLayout{}, fmt.Errorf("计算 TeslaCam 分区容量: %w", err)
	}
	partitionEnd, err := checkedAddUint64(partitionOffset, partitionBytes)
	if err != nil || partitionEnd > imageSize {
		return exFATLayout{}, errors.New("TeslaCam 第一分区超出镜像边界")
	}

	boot := make([]byte, mbrSectorSize)
	if err := readExactAt(reader, boot, partitionOffset); err != nil {
		return exFATLayout{}, fmt.Errorf("读取 exFAT 启动扇区: %w", err)
	}
	if string(boot[3:11]) != "EXFAT   " || boot[510] != 0x55 || boot[511] != 0xaa {
		return exFATLayout{}, errors.New("TeslaCam 第一分区不是有效 exFAT")
	}
	sectorShift := boot[108]
	clusterShift := boot[109]
	if sectorShift < 9 || sectorShift > 12 || clusterShift > 25-sectorShift {
		return exFATLayout{}, errors.New("exFAT 扇区或簇大小超出规范")
	}
	sectorSize := uint64(1) << sectorShift
	sectorsPerCluster := uint64(1) << clusterShift
	clusterSize, err := checkedMultiplyUint64(sectorSize, sectorsPerCluster)
	if err != nil {
		return exFATLayout{}, fmt.Errorf("计算 exFAT 簇大小: %w", err)
	}
	volumeSectors := binary.LittleEndian.Uint64(boot[72:80])
	volumeBytes, err := checkedMultiplyUint64(volumeSectors, sectorSize)
	if err != nil || volumeBytes == 0 || volumeBytes > partitionBytes {
		return exFATLayout{}, errors.New("exFAT 卷容量超出第一分区边界")
	}
	fatOffset := uint64(binary.LittleEndian.Uint32(boot[80:84]))
	fatLength := uint64(binary.LittleEndian.Uint32(boot[84:88]))
	clusterHeap := uint64(binary.LittleEndian.Uint32(boot[88:92]))
	clusterCount := binary.LittleEndian.Uint32(boot[92:96])
	rootCluster := binary.LittleEndian.Uint32(boot[96:100])
	numberOfFATs := boot[110]
	activeFAT := uint8(binary.LittleEndian.Uint16(boot[106:108]) & 1)
	if fatOffset < 24 || fatLength == 0 || clusterCount == 0 || numberOfFATs < 1 || numberOfFATs > 2 {
		return exFATLayout{}, errors.New("exFAT 布局字段无效")
	}
	if activeFAT >= numberOfFATs || !validCluster(rootCluster, clusterCount) {
		return exFATLayout{}, errors.New("exFAT 活动 FAT 或根目录簇无效")
	}
	fatSectors, err := checkedMultiplyUint64(fatLength, uint64(numberOfFATs))
	fatEnd, addErr := checkedAddUint64(fatOffset, fatSectors)
	if err != nil || addErr != nil || fatEnd > clusterHeap {
		return exFATLayout{}, errors.New("exFAT FAT 区域与簇堆重叠")
	}
	heapSectors, err := checkedMultiplyUint64(uint64(clusterCount), sectorsPerCluster)
	if err != nil {
		return exFATLayout{}, fmt.Errorf("计算 exFAT 簇堆容量: %w", err)
	}
	heapEnd, err := checkedAddUint64(clusterHeap, heapSectors)
	if err != nil || heapEnd > volumeSectors {
		return exFATLayout{}, errors.New("exFAT 簇堆超出卷边界")
	}
	return exFATLayout{
		partitionOffset: partitionOffset,
		partitionBytes:  partitionBytes,
		sectorSize:      sectorSize,
		clusterSize:     clusterSize,
		fatOffset:       fatOffset,
		fatLength:       fatLength,
		clusterHeap:     clusterHeap,
		clusterCount:    clusterCount,
		rootCluster:     rootCluster,
		activeFAT:       activeFAT,
		numberOfFATs:    numberOfFATs,
	}, nil
}

func findAllocationBitmap(ctx context.Context, reader io.ReaderAt, layout exFATLayout) (allocationBitmap, []uint32, error) {
	cluster := layout.rootCluster
	visited := make(map[uint32]struct{})
	for {
		if err := ctx.Err(); err != nil {
			return allocationBitmap{}, nil, err
		}
		if _, exists := visited[cluster]; exists {
			return allocationBitmap{}, nil, errors.New("exFAT 根目录簇链存在循环")
		}
		visited[cluster] = struct{}{}
		content, err := readCluster(reader, layout, cluster)
		if err != nil {
			return allocationBitmap{}, nil, fmt.Errorf("读取 exFAT 根目录簇: %w", err)
		}
		for offset := 0; offset+exFATDirectoryEntrySize <= len(content); offset += exFATDirectoryEntrySize {
			entry := content[offset : offset+exFATDirectoryEntrySize]
			if entry[0] == 0 {
				return allocationBitmap{}, mapKeys(visited), errors.New("exFAT 根目录缺少活动分配位图")
			}
			if entry[0] != exFATBitmapEntryType || entry[1]&1 != layout.activeFAT {
				continue
			}
			bitmap := allocationBitmap{
				firstCluster: binary.LittleEndian.Uint32(entry[20:24]),
				dataLength:   binary.LittleEndian.Uint64(entry[24:32]),
			}
			minimumLength := (uint64(layout.clusterCount) + 7) / 8
			if !validCluster(bitmap.firstCluster, layout.clusterCount) || bitmap.dataLength < minimumLength {
				return allocationBitmap{}, nil, errors.New("exFAT 分配位图入口无效")
			}
			return bitmap, mapKeys(visited), nil
		}
		next, err := nextCluster(reader, layout, cluster)
		if err != nil {
			return allocationBitmap{}, nil, fmt.Errorf("读取 exFAT 根目录簇链: %w", err)
		}
		if next >= exFATEndOfChain {
			break
		}
		cluster = next
	}
	return allocationBitmap{}, mapKeys(visited), errors.New("exFAT 根目录缺少活动分配位图")
}

func readAllocationBitmap(ctx context.Context, reader io.ReaderAt, layout exFATLayout, bitmap allocationBitmap) ([]byte, []uint32, error) {
	required := (uint64(layout.clusterCount) + 7) / 8
	if required > uint64(math.MaxInt) || required > maxAllocationBitmapSize {
		return nil, nil, errors.New("exFAT 分配位图超过安全内存上限")
	}
	content := make([]byte, int(required))
	cluster := bitmap.firstCluster
	visited := make(map[uint32]struct{})
	written := uint64(0)
	for written < required {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if _, exists := visited[cluster]; exists {
			return nil, nil, errors.New("exFAT 分配位图簇链存在循环")
		}
		visited[cluster] = struct{}{}
		clusterData, err := readCluster(reader, layout, cluster)
		if err != nil {
			return nil, nil, fmt.Errorf("读取 exFAT 分配位图簇: %w", err)
		}
		remaining := required - written
		copyLength := uint64(len(clusterData))
		if copyLength > remaining {
			copyLength = remaining
		}
		copy(content[int(written):int(written+copyLength)], clusterData[:int(copyLength)])
		written += copyLength
		if written >= required {
			break
		}
		next, err := nextCluster(reader, layout, cluster)
		if err != nil {
			return nil, nil, fmt.Errorf("读取 exFAT 分配位图簇链: %w", err)
		}
		if next >= exFATEndOfChain {
			return nil, nil, errors.New("exFAT 分配位图簇链提前结束")
		}
		cluster = next
	}
	return content, mapKeys(visited), nil
}

func readCluster(reader io.ReaderAt, layout exFATLayout, cluster uint32) ([]byte, error) {
	offset, err := clusterOffset(layout, cluster)
	if err != nil {
		return nil, err
	}
	if layout.clusterSize > uint64(math.MaxInt) {
		return nil, errors.New("exFAT 簇大小无法分配")
	}
	content := make([]byte, int(layout.clusterSize))
	if err := readExactAt(reader, content, offset); err != nil {
		return nil, err
	}
	return content, nil
}

func clusterOffset(layout exFATLayout, cluster uint32) (uint64, error) {
	if !validCluster(cluster, layout.clusterCount) {
		return 0, fmt.Errorf("exFAT 簇编号 %d 超出边界", cluster)
	}
	clusterBytes, err := checkedMultiplyUint64(uint64(cluster-2), layout.clusterSize)
	if err != nil {
		return 0, err
	}
	heapBytes, err := checkedMultiplyUint64(layout.clusterHeap, layout.sectorSize)
	if err != nil {
		return 0, err
	}
	relative, err := checkedAddUint64(heapBytes, clusterBytes)
	if err != nil {
		return 0, err
	}
	offset, err := checkedAddUint64(layout.partitionOffset, relative)
	if err != nil {
		return 0, err
	}
	end, err := checkedAddUint64(offset, layout.clusterSize)
	partitionEnd, partitionErr := checkedAddUint64(layout.partitionOffset, layout.partitionBytes)
	if err != nil || partitionErr != nil || end > partitionEnd {
		return 0, errors.New("exFAT 簇超出第一分区边界")
	}
	return offset, nil
}

func nextCluster(reader io.ReaderAt, layout exFATLayout, cluster uint32) (uint32, error) {
	if !validCluster(cluster, layout.clusterCount) {
		return 0, errors.New("exFAT FAT 查询簇超出边界")
	}
	activeOffset, err := checkedMultiplyUint64(uint64(layout.activeFAT), layout.fatLength)
	if err != nil {
		return 0, err
	}
	fatSector, err := checkedAddUint64(layout.fatOffset, activeOffset)
	if err != nil {
		return 0, err
	}
	fatBase, err := checkedMultiplyUint64(fatSector, layout.sectorSize)
	if err != nil {
		return 0, err
	}
	fatBytes, err := checkedMultiplyUint64(layout.fatLength, layout.sectorSize)
	if err != nil {
		return 0, err
	}
	entryOffset, err := checkedMultiplyUint64(uint64(cluster), 4)
	entryEnd, addErr := checkedAddUint64(entryOffset, 4)
	if err != nil || addErr != nil || entryEnd > fatBytes {
		return 0, errors.New("exFAT FAT 入口超出边界")
	}
	absoluteFAT, err := checkedAddUint64(layout.partitionOffset, fatBase)
	if err != nil {
		return 0, err
	}
	offset, err := checkedAddUint64(absoluteFAT, entryOffset)
	if err != nil {
		return 0, err
	}
	entry := make([]byte, 4)
	if err := readExactAt(reader, entry, offset); err != nil {
		return 0, err
	}
	next := binary.LittleEndian.Uint32(entry)
	if next == exFATBadCluster || (next < exFATEndOfChain && !validCluster(next, layout.clusterCount)) {
		return 0, fmt.Errorf("exFAT FAT 簇链包含无效入口 %#x", next)
	}
	return next, nil
}

func bitmapAllocated(bitmap []byte, index uint32) bool {
	return bitmap[index/8]&(1<<uint(index%8)) != 0
}

func validCluster(cluster, count uint32) bool {
	return cluster >= 2 && uint64(cluster) < uint64(count)+2
}

func readExactAt(reader io.ReaderAt, buffer []byte, offset uint64) error {
	if offset > math.MaxInt64 {
		return errors.New("读取偏移超出系统限制")
	}
	n, err := reader.ReadAt(buffer, int64(offset))
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if n != len(buffer) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func checkedMultiplyUint64(left, right uint64) (uint64, error) {
	if right != 0 && left > math.MaxUint64/right {
		return 0, errors.New("整数乘法溢出")
	}
	return left * right, nil
}

func checkedAddUint64(left, right uint64) (uint64, error) {
	if left > math.MaxUint64-right {
		return 0, errors.New("整数加法溢出")
	}
	return left + right, nil
}

func mapKeys(values map[uint32]struct{}) []uint32 {
	keys := make([]uint32, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
