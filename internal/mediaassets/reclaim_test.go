package mediaassets

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestBoundedReclaimLength(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		extent    uint64
		cluster   uint64
		remaining uint64
		want      uint64
	}{
		{name: "whole extent below target", extent: 4096, cluster: 4096, remaining: 8192, want: 4096},
		{name: "rounds down to cluster", extent: 16384, cluster: 4096, remaining: 9000, want: 8192},
		{name: "one cluster minimum", extent: 16384, cluster: 4096, remaining: 1, want: 4096},
		{name: "target reached", extent: 16384, cluster: 4096, remaining: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := boundedReclaimLength(test.extent, test.cluster, test.remaining); got != test.want {
				t.Fatalf("boundedReclaimLength() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestScanExFATFreeSpaceUsesAllocationBitmap(t *testing.T) {
	t.Parallel()
	image := testExFATImage()
	space, err := scanExFATFreeSpace(t.Context(), bytes.NewReader(image), int64(len(image)))
	if err != nil {
		t.Fatalf("scanExFATFreeSpace() error = %v", err)
	}
	if space.clusterSize != 512 || space.freeBytes != 12*512 {
		t.Fatalf("space = %#v", space)
	}
	if len(space.extents) != 2 {
		t.Fatalf("extents = %#v, want 2 ranges", space.extents)
	}
	partitionOffset := uint64(8 * 512)
	heapOffset := uint64(25 * 512)
	want := []freeExtent{
		{offset: partitionOffset + heapOffset + 2*512, length: 512},
		{offset: partitionOffset + heapOffset + 4*512, length: 11 * 512},
	}
	for index := range want {
		if space.extents[index] != want[index] {
			t.Fatalf("extent[%d] = %#v, want %#v", index, space.extents[index], want[index])
		}
	}
}

func TestScanExFATFreeSpaceRejectsMetadataMarkedFree(t *testing.T) {
	t.Parallel()
	image := testExFATImage()
	bitmapOffset := (8 + 25 + 1) * 512
	image[bitmapOffset] &^= 1
	_, err := scanExFATFreeSpace(t.Context(), bytes.NewReader(image), int64(len(image)))
	if err == nil || !strings.Contains(err.Error(), "关键元数据簇") {
		t.Fatalf("scanExFATFreeSpace() error = %v", err)
	}
}

func TestScanExFATFreeSpaceRejectsInvalidMBR(t *testing.T) {
	t.Parallel()
	image := testExFATImage()
	image[510] = 0
	if _, err := scanExFATFreeSpace(t.Context(), bytes.NewReader(image), int64(len(image))); err == nil {
		t.Fatal("scanExFATFreeSpace() error = nil, want invalid MBR")
	}
}

func TestReadAllocationBitmapRejectsUnsafeMemorySize(t *testing.T) {
	t.Parallel()
	layout := exFATLayout{
		clusterCount: uint32((maxAllocationBitmapSize + 1) * 8),
	}
	_, _, err := readAllocationBitmap(
		t.Context(),
		bytes.NewReader(nil),
		layout,
		allocationBitmap{firstCluster: 2},
	)
	if err == nil || !strings.Contains(err.Error(), "安全内存上限") {
		t.Fatalf("readAllocationBitmap() error = %v", err)
	}
}

func testExFATImage() []byte {
	const (
		imageSectors     = 64
		partitionStart   = 8
		partitionSectors = 40
		fatOffset        = 24
		clusterHeap      = 25
		clusterCount     = 15
	)
	image := make([]byte, imageSectors*512)
	image[510] = 0x55
	image[511] = 0xaa
	partition := image[mbrPartitionTableOffset : mbrPartitionTableOffset+mbrPartitionEntrySize]
	partition[4] = 0x07
	binary.LittleEndian.PutUint32(partition[8:12], partitionStart)
	binary.LittleEndian.PutUint32(partition[12:16], partitionSectors)

	bootOffset := partitionStart * 512
	boot := image[bootOffset : bootOffset+512]
	copy(boot[3:11], "EXFAT   ")
	binary.LittleEndian.PutUint64(boot[72:80], partitionSectors)
	binary.LittleEndian.PutUint32(boot[80:84], fatOffset)
	binary.LittleEndian.PutUint32(boot[84:88], 1)
	binary.LittleEndian.PutUint32(boot[88:92], clusterHeap)
	binary.LittleEndian.PutUint32(boot[92:96], clusterCount)
	binary.LittleEndian.PutUint32(boot[96:100], 2)
	boot[108] = 9
	boot[109] = 0
	boot[110] = 1
	boot[510] = 0x55
	boot[511] = 0xaa

	fat := image[(partitionStart+fatOffset)*512 : (partitionStart+fatOffset+1)*512]
	binary.LittleEndian.PutUint32(fat[2*4:2*4+4], 0xffffffff)
	binary.LittleEndian.PutUint32(fat[3*4:3*4+4], 0xffffffff)

	root := image[(partitionStart+clusterHeap)*512 : (partitionStart+clusterHeap+1)*512]
	root[0] = exFATBitmapEntryType
	binary.LittleEndian.PutUint32(root[20:24], 3)
	binary.LittleEndian.PutUint64(root[24:32], 2)

	bitmap := image[(partitionStart+clusterHeap+1)*512 : (partitionStart+clusterHeap+2)*512]
	bitmap[0] = 0b00001011
	return image
}
