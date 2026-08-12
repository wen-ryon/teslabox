package exfatreserve

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testImageBytes = 64 * 1024 * 1024

func TestInstallAndVerifyCapacityReserve(t *testing.T) {
	t.Parallel()
	imagePath := filepath.Join(t.TempDir(), "teslacam.img")
	createTestImage(t, imagePath, testImageBytes-1024*1024)

	status, err := Install(imagePath, 48*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	if status.Legacy || status.ReservedBytes == 0 || status.FirstCluster < 2 {
		t.Fatalf("Install() status = %#v", status)
	}
	verified, err := Verify(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	if verified != status {
		t.Fatalf("Verify() = %#v, want %#v", verified, status)
	}
	file, err := os.Open(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fs, err := readLayout(file, testImageBytes)
	if err != nil {
		t.Fatal(err)
	}
	reserveOffset, err := clusterOffset(fs, status.FirstCluster)
	if err != nil {
		t.Fatal(err)
	}
	const writableBoundary = 48 * 1024 * 1024
	if reserveOffset > writableBoundary || reserveOffset+fs.clusterSize <= writableBoundary {
		t.Fatalf("reserve starts at %d with cluster size %d, boundary %d is not protected", reserveOffset, fs.clusterSize, writableBoundary)
	}
}

func TestResizeCapacityReserveReleasesOnlyNewWritableClusters(t *testing.T) {
	t.Parallel()
	imagePath := filepath.Join(t.TempDir(), "teslacam.img")
	createTestImage(t, imagePath, testImageBytes-1024*1024)
	original, err := Install(imagePath, 48*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	resized, err := Resize(imagePath, 56*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	if resized.FirstCluster <= original.FirstCluster || resized.ReservedBytes >= original.ReservedBytes {
		t.Fatalf("Resize() = %#v, original = %#v", resized, original)
	}
	verified, err := Verify(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	if verified != resized {
		t.Fatalf("Verify() = %#v, want %#v", verified, resized)
	}
	file, err := os.Open(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fs, err := readLayout(file, testImageBytes)
	if err != nil {
		t.Fatal(err)
	}
	bitmapFile, _, err := readRootMetadata(file, fs)
	if err != nil {
		t.Fatal(err)
	}
	bitmap, _, err := readSystemFile(file, fs, bitmapFile)
	if err != nil {
		t.Fatal(err)
	}
	if bitmapAllocated(bitmap, original.FirstCluster-2) {
		t.Fatal("原保留区起始簇仍被占用")
	}
	if !bitmapAllocated(bitmap, resized.FirstCluster-2) {
		t.Fatal("新保留区起始簇未保持占用")
	}
}

func TestResizeCapacityReserveRejectsReducingWritableCapacity(t *testing.T) {
	t.Parallel()
	imagePath := filepath.Join(t.TempDir(), "teslacam.img")
	createTestImage(t, imagePath, testImageBytes-1024*1024)
	if _, err := Install(imagePath, 48*1024*1024); err != nil {
		t.Fatal(err)
	}
	if _, err := Resize(imagePath, 40*1024*1024); err == nil || !strings.Contains(err.Error(), "拒绝扩大") {
		t.Fatalf("Resize() error = %v", err)
	}
}

func TestVerifyAcceptsLegacyShortPartition(t *testing.T) {
	t.Parallel()
	imagePath := filepath.Join(t.TempDir(), "teslacam.img")
	createTestImage(t, imagePath, 32*1024*1024)

	status, err := Verify(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Legacy || status.ReservedBytes != 0 {
		t.Fatalf("Verify() status = %#v", status)
	}
}

func TestVerifyAcceptsLegacyFullPartitionWithoutMarker(t *testing.T) {
	t.Parallel()
	imagePath := filepath.Join(t.TempDir(), "teslacam.img")
	createTestImage(t, imagePath, testImageBytes-1024*1024)

	status, err := Verify(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Legacy {
		t.Fatalf("Verify() status = %#v", status)
	}
}

func TestVerifyRejectsInvalidOuterMarker(t *testing.T) {
	t.Parallel()
	imagePath := filepath.Join(t.TempDir(), "teslacam.img")
	createTestImage(t, imagePath, testImageBytes-1024*1024)
	if err := os.WriteFile(MarkerPath(imagePath), []byte("invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Verify(imagePath)
	if err == nil || !strings.Contains(err.Error(), "标记内容无效") {
		t.Fatalf("Verify() error = %v", err)
	}
}

func TestVerifyRejectsModifiedReserveEntry(t *testing.T) {
	t.Parallel()
	imagePath := filepath.Join(t.TempDir(), "teslacam.img")
	createTestImage(t, imagePath, testImageBytes-1024*1024)
	if _, err := Install(imagePath, 48*1024*1024); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(imagePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	rootOffset := uint64(1024*1024 + 128*512)
	if _, err := file.WriteAt([]byte{0}, int64(rootOffset+directoryEntrySize+4)); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Verify(imagePath)
	if err == nil || !strings.Contains(err.Error(), "校验失败") {
		t.Fatalf("Verify() error = %v", err)
	}
}

func TestInstallRejectsOccupiedTailCluster(t *testing.T) {
	t.Parallel()
	imagePath := filepath.Join(t.TempDir(), "teslacam.img")
	clusterCount := createTestImage(t, imagePath, testImageBytes-1024*1024)
	file, err := os.OpenFile(imagePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	bitmapOffset := int64(1024*1024 + 128*512 + 4096)
	lastIndex := clusterCount - 1
	byteOffset := bitmapOffset + int64(lastIndex/8)
	value := []byte{0}
	if _, err := file.ReadAt(value, byteOffset); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	value[0] |= 1 << uint(lastIndex%8)
	if _, err := file.WriteAt(value, byteOffset); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Install(imagePath, 48*1024*1024)
	if err == nil || !strings.Contains(err.Error(), "已被占用") {
		t.Fatalf("Install() error = %v", err)
	}
}

func createTestImage(t *testing.T, imagePath string, partitionBytes int64) uint32 {
	t.Helper()
	file, err := os.Create(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.Truncate(testImageBytes); err != nil {
		t.Fatal(err)
	}
	const partitionOffset = 1024 * 1024
	partitionSectors := uint32(partitionBytes / 512)
	mbr := make([]byte, 512)
	entry := mbr[partitionTableOffset : partitionTableOffset+partitionEntrySize]
	entry[4] = 0x07
	binary.LittleEndian.PutUint32(entry[8:12], partitionOffset/512)
	binary.LittleEndian.PutUint32(entry[12:16], partitionSectors)
	mbr[510], mbr[511] = 0x55, 0xaa
	if _, err := file.WriteAt(mbr, 0); err != nil {
		t.Fatal(err)
	}

	const clusterHeapSectors = 128
	const sectorsPerCluster = 8
	clusterCount := uint32((uint64(partitionSectors) - clusterHeapSectors) / sectorsPerCluster)
	boot := make([]byte, 512)
	boot[0] = 0xeb
	copy(boot[3:11], "EXFAT   ")
	binary.LittleEndian.PutUint64(boot[72:80], uint64(partitionSectors))
	binary.LittleEndian.PutUint32(boot[80:84], 24)
	binary.LittleEndian.PutUint32(boot[84:88], 64)
	binary.LittleEndian.PutUint32(boot[88:92], clusterHeapSectors)
	binary.LittleEndian.PutUint32(boot[92:96], clusterCount)
	binary.LittleEndian.PutUint32(boot[96:100], 2)
	boot[108], boot[109], boot[110] = 9, 3, 1
	boot[510], boot[511] = 0x55, 0xaa
	if _, err := file.WriteAt(boot, partitionOffset); err != nil {
		t.Fatal(err)
	}

	rootOffset := int64(partitionOffset + clusterHeapSectors*512)
	root := make([]byte, 4096)
	root[0] = bitmapEntryType
	root[1] = 0 // 主分配位图标识；该字段不是普通流扩展标志。
	binary.LittleEndian.PutUint32(root[20:24], 3)
	binary.LittleEndian.PutUint64(root[24:32], (uint64(clusterCount)+7)/8)
	if _, err := file.WriteAt(root, rootOffset); err != nil {
		t.Fatal(err)
	}
	bitmap := make([]byte, 4096)
	bitmap[0] = 0x03 // 根目录簇 2 和分配位图簇 3。
	if _, err := file.WriteAt(bitmap, rootOffset+4096); err != nil {
		t.Fatal(err)
	}
	return clusterCount
}
