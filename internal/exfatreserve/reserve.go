// Package exfatreserve 在完整 exFAT 卷尾部登记不占外层物理空间的安全保留文件。
package exfatreserve

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"
	"unicode/utf16"
)

const (
	sectorSize             = uint64(512)
	partitionTableOffset   = 446
	partitionEntrySize     = 16
	directoryEntrySize     = 32
	bitmapEntryType        = 0x81
	fileEntryType          = 0x85
	streamEntryType        = 0xc0
	fileNameEntryType      = 0xc1
	allocationPossibleFlag = 0x01
	noFATChainFlag         = 0x02
	reservedAttributes     = 0x0007 // 只读、隐藏、系统。
	endOfChain             = 0xfffffff8
	fullVolumeTolerance    = 8 * 1024 * 1024
	maxAllocationBitmap    = 64 * 1024 * 1024
	reserveName            = "TBRESERVE.SYS"
	markerContent          = "teslabox-capacity-reserve-v1\n"
)

// Status 描述容量保护布局；Legacy 表示分区本身没有铺满镜像，无需保留文件。
type Status struct {
	Legacy        bool
	VolumeBytes   uint64
	ReservedBytes uint64
	FirstCluster  uint32
}

type layout struct {
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

type systemFile struct {
	flags        uint8
	firstCluster uint32
	dataLength   uint64
}

type reserveEntry struct {
	attributes   uint16
	flags        uint8
	firstCluster uint32
	dataLength   uint64
	entryOffset  int
	entryCount   int
}

// Install 把 writableBoundary 之后的完整簇登记为保留文件；调用方必须保证该边界之前已预分配。
func Install(imagePath string, writableBoundary int64) (Status, error) {
	if writableBoundary <= 0 {
		return Status{}, errors.New("exfat reserve: writable boundary must be positive")
	}
	file, err := os.OpenFile(imagePath, os.O_RDWR, 0)
	if err != nil {
		return Status{}, fmt.Errorf("打开 TeslaCam 镜像: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Status{}, fmt.Errorf("读取 TeslaCam 镜像大小: %w", err)
	}
	fs, err := readLayout(file, uint64(info.Size()))
	if err != nil {
		return Status{}, err
	}
	if !isFullVolume(fs, uint64(info.Size())) {
		return Status{}, errors.New("exFAT 分区未覆盖完整镜像，拒绝安装容量保留区")
	}
	bitmapFile, root, err := readRootMetadata(file, fs)
	if err != nil {
		return Status{}, err
	}
	bitmap, bitmapClusters, err := readSystemFile(file, fs, bitmapFile)
	if err != nil {
		return Status{}, fmt.Errorf("读取 exFAT 分配位图: %w", err)
	}
	if existing, ok, findErr := findReserve(root); findErr != nil {
		return Status{}, findErr
	} else if ok {
		status, verifyErr := verifyReserve(fs, bitmap, existing)
		if verifyErr != nil {
			return Status{}, verifyErr
		}
		if markerErr := writeMarker(imagePath); markerErr != nil {
			return Status{}, markerErr
		}
		return status, nil
	}

	heapOffset, err := clusterOffset(fs, 2)
	if err != nil {
		return Status{}, err
	}
	boundary := uint64(writableBoundary)
	if boundary <= heapOffset || boundary >= heapOffset+uint64(fs.clusterCount)*fs.clusterSize {
		return Status{}, errors.New("安全可写边界不在 exFAT 数据簇范围内")
	}
	// 把跨越安全边界的簇也纳入保留区，确保任何可写簇都完整落在外层预分配范围内。
	firstIndex := (boundary - heapOffset) / fs.clusterSize
	firstCluster := uint32(firstIndex) + 2
	reservedClusters := uint64(fs.clusterCount) - firstIndex
	if reservedClusters == 0 || reservedClusters > math.MaxUint32 {
		return Status{}, errors.New("exFAT 容量保留区大小无效")
	}
	for index := firstIndex; index < uint64(fs.clusterCount); index++ {
		if bitmapAllocated(bitmap, uint32(index)) {
			return Status{}, fmt.Errorf("exFAT 卷尾簇 %d 已被占用，拒绝覆盖", index+2)
		}
		setBitmapAllocated(bitmap, uint32(index))
	}
	entryOffset, err := reserveEntryOffset(root)
	if err != nil {
		return Status{}, err
	}
	reservedBytes := reservedClusters * fs.clusterSize
	entrySet := buildReserveEntry(firstCluster, reservedBytes)
	copy(root[entryOffset:entryOffset+len(entrySet)], entrySet)
	if entryOffset+len(entrySet) < len(root) {
		root[entryOffset+len(entrySet)] = 0
	}
	if err := writeSystemFile(file, fs, bitmapFile, bitmapClusters, bitmap); err != nil {
		return Status{}, fmt.Errorf("写入 exFAT 分配位图: %w", err)
	}
	rootOffset, err := clusterOffset(fs, fs.rootCluster)
	if err != nil {
		return Status{}, err
	}
	if err := writeExactAt(file, root, rootOffset); err != nil {
		return Status{}, fmt.Errorf("写入 exFAT 根目录: %w", err)
	}
	if err := writePercentInUse(file, fs, bitmap); err != nil {
		return Status{}, fmt.Errorf("更新 exFAT 已用容量比例: %w", err)
	}
	if err := file.Sync(); err != nil {
		return Status{}, fmt.Errorf("同步 exFAT 容量保留区: %w", err)
	}
	status, err := verifyOpenFile(file, fs)
	if err != nil {
		return Status{}, err
	}
	if err := writeMarker(imagePath); err != nil {
		return Status{}, err
	}
	return status, nil
}

// Resize 缩小现有卷尾保留区，从而增加已经在外层 XFS 完成预分配的安全可写容量。
// 调用方必须先预分配到 writableBoundary；本函数不允许扩大保留区或回收已有可写簇。
func Resize(imagePath string, writableBoundary int64) (Status, error) {
	if writableBoundary <= 0 {
		return Status{}, errors.New("exfat reserve: writable boundary must be positive")
	}
	file, err := os.OpenFile(imagePath, os.O_RDWR, 0)
	if err != nil {
		return Status{}, fmt.Errorf("打开 TeslaCam 镜像: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Status{}, fmt.Errorf("读取 TeslaCam 镜像大小: %w", err)
	}
	fs, err := readLayout(file, uint64(info.Size()))
	if err != nil {
		return Status{}, err
	}
	if !isFullVolume(fs, uint64(info.Size())) {
		return Status{}, errors.New("exFAT 分区未覆盖完整镜像，拒绝调整容量保留区")
	}
	bitmapFile, root, err := readRootMetadata(file, fs)
	if err != nil {
		return Status{}, err
	}
	bitmap, bitmapClusters, err := readSystemFile(file, fs, bitmapFile)
	if err != nil {
		return Status{}, fmt.Errorf("读取 exFAT 分配位图: %w", err)
	}
	existing, ok, err := findReserve(root)
	if err != nil {
		return Status{}, err
	}
	if !ok {
		return Status{}, errors.New("TeslaBox 安全容量保留文件不存在")
	}
	if _, err := verifyReserve(fs, bitmap, existing); err != nil {
		return Status{}, err
	}
	heapOffset, err := clusterOffset(fs, 2)
	if err != nil {
		return Status{}, err
	}
	boundary := uint64(writableBoundary)
	if boundary <= heapOffset || boundary >= heapOffset+uint64(fs.clusterCount)*fs.clusterSize {
		return Status{}, errors.New("安全可写边界不在 exFAT 数据簇范围内")
	}
	firstIndex := (boundary - heapOffset) / fs.clusterSize
	firstCluster := uint32(firstIndex) + 2
	if firstCluster < existing.firstCluster {
		return Status{}, errors.New("拒绝扩大 TeslaBox 容量保留区")
	}
	if firstCluster == existing.firstCluster {
		return verifyReserve(fs, bitmap, existing)
	}
	reservedClusters := uint64(fs.clusterCount) - firstIndex
	if reservedClusters == 0 || reservedClusters > math.MaxUint32 {
		return Status{}, errors.New("exFAT 容量保留区大小无效")
	}
	reservedBytes := reservedClusters * fs.clusterSize

	// 先缩短目录项，再释放旧保留簇。中途断电最多造成空间暂时仍被占用，
	// 不会让目录项继续引用已经标记为空闲的簇。
	entrySet := buildReserveEntry(firstCluster, reservedBytes)
	copy(root[existing.entryOffset:existing.entryOffset+len(entrySet)], entrySet)
	rootOffset, err := clusterOffset(fs, fs.rootCluster)
	if err != nil {
		return Status{}, err
	}
	if err := writeExactAt(file, root, rootOffset); err != nil {
		return Status{}, fmt.Errorf("更新 exFAT 容量保留目录项: %w", err)
	}
	if err := file.Sync(); err != nil {
		return Status{}, fmt.Errorf("同步 exFAT 容量保留目录项: %w", err)
	}
	for cluster := existing.firstCluster; cluster < firstCluster; cluster++ {
		setBitmapFree(bitmap, cluster-2)
	}
	if err := writeSystemFile(file, fs, bitmapFile, bitmapClusters, bitmap); err != nil {
		return Status{}, fmt.Errorf("释放 exFAT 容量保留簇: %w", err)
	}
	if err := writePercentInUse(file, fs, bitmap); err != nil {
		return Status{}, fmt.Errorf("更新 exFAT 已用容量比例: %w", err)
	}
	if err := file.Sync(); err != nil {
		return Status{}, fmt.Errorf("同步 exFAT 容量保留区: %w", err)
	}
	return verifyOpenFile(file, fs)
}

// Verify 检查完整卷必须保留卷尾空间；旧式小分区镜像保持兼容。
func Verify(imagePath string) (Status, error) {
	content, err := os.ReadFile(MarkerPath(imagePath))
	if errors.Is(err, os.ErrNotExist) {
		return Status{Legacy: true}, nil
	}
	if err != nil {
		return Status{}, fmt.Errorf("读取 TeslaCam 容量保护标记: %w", err)
	}
	if string(content) != markerContent {
		return Status{}, errors.New("TeslaCam 容量保护标记内容无效")
	}
	file, err := os.Open(imagePath)
	if err != nil {
		return Status{}, fmt.Errorf("打开 TeslaCam 镜像: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Status{}, fmt.Errorf("读取 TeslaCam 镜像大小: %w", err)
	}
	fs, err := readLayout(file, uint64(info.Size()))
	if err != nil {
		return Status{}, err
	}
	if !isFullVolume(fs, uint64(info.Size())) {
		return Status{Legacy: true, VolumeBytes: fs.partitionBytes}, nil
	}
	return verifyOpenFile(file, fs)
}

// MarkerPath 返回车机不可访问的外层容量保护标记路径。
func MarkerPath(imagePath string) string {
	return imagePath + ".capacity-reserve"
}

func writeMarker(imagePath string) error {
	markerPath := MarkerPath(imagePath)
	temporaryPath := markerPath + ".tmp"
	file, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("写入 TeslaCam 容量保护临时标记: %w", err)
	}
	defer os.Remove(temporaryPath)
	if _, err := file.WriteString(markerContent); err != nil {
		_ = file.Close()
		return fmt.Errorf("写入 TeslaCam 容量保护临时标记: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("同步 TeslaCam 容量保护临时标记: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭 TeslaCam 容量保护临时标记: %w", err)
	}
	if err := os.Rename(temporaryPath, markerPath); err != nil {
		return fmt.Errorf("发布 TeslaCam 容量保护标记: %w", err)
	}
	if err := syncMarkerDirectory(markerPath); err != nil {
		return err
	}
	return nil
}

func verifyOpenFile(reader io.ReaderAt, fs layout) (Status, error) {
	bitmapFile, root, err := readRootMetadata(reader, fs)
	if err != nil {
		return Status{}, err
	}
	bitmap, _, err := readSystemFile(reader, fs, bitmapFile)
	if err != nil {
		return Status{}, fmt.Errorf("读取 exFAT 分配位图: %w", err)
	}
	entry, ok, err := findReserve(root)
	if err != nil {
		return Status{}, err
	}
	if !ok {
		return Status{}, errors.New("完整 exFAT 卷缺少 TeslaBox 安全容量保留区，拒绝连接 Gadget")
	}
	return verifyReserve(fs, bitmap, entry)
}

func verifyReserve(fs layout, bitmap []byte, entry reserveEntry) (Status, error) {
	if entry.attributes&reservedAttributes != reservedAttributes {
		return Status{}, errors.New("TeslaBox 安全容量保留文件属性已被修改")
	}
	if entry.flags&(allocationPossibleFlag|noFATChainFlag) != allocationPossibleFlag|noFATChainFlag {
		return Status{}, errors.New("TeslaBox 安全容量保留文件不是连续分配")
	}
	if entry.dataLength == 0 || entry.dataLength%fs.clusterSize != 0 {
		return Status{}, errors.New("TeslaBox 安全容量保留文件大小无效")
	}
	clusterCount := entry.dataLength / fs.clusterSize
	lastExclusive := uint64(entry.firstCluster) + clusterCount
	if entry.firstCluster < 2 || lastExclusive != uint64(fs.clusterCount)+2 {
		return Status{}, errors.New("TeslaBox 安全容量保留文件未覆盖卷尾")
	}
	for cluster := uint64(entry.firstCluster); cluster < lastExclusive; cluster++ {
		if !bitmapAllocated(bitmap, uint32(cluster-2)) {
			return Status{}, fmt.Errorf("TeslaBox 安全容量保留簇 %d 已被释放", cluster)
		}
	}
	return Status{VolumeBytes: fs.partitionBytes, ReservedBytes: entry.dataLength, FirstCluster: entry.firstCluster}, nil
}

func readLayout(reader io.ReaderAt, imageSize uint64) (layout, error) {
	mbr := make([]byte, sectorSize)
	if err := readExactAt(reader, mbr, 0); err != nil {
		return layout{}, fmt.Errorf("读取 TeslaCam 镜像 MBR: %w", err)
	}
	if mbr[510] != 0x55 || mbr[511] != 0xaa {
		return layout{}, errors.New("TeslaCam 镜像 MBR 签名无效")
	}
	entry := mbr[partitionTableOffset : partitionTableOffset+partitionEntrySize]
	startLBA := binary.LittleEndian.Uint32(entry[8:12])
	sectorCount := binary.LittleEndian.Uint32(entry[12:16])
	if startLBA == 0 || sectorCount == 0 {
		return layout{}, errors.New("TeslaCam 镜像第一分区无效")
	}
	partitionOffset := uint64(startLBA) * sectorSize
	partitionBytes := uint64(sectorCount) * sectorSize
	if partitionOffset > imageSize || partitionBytes > imageSize-partitionOffset {
		return layout{}, errors.New("TeslaCam 第一分区超出镜像边界")
	}
	boot := make([]byte, sectorSize)
	if err := readExactAt(reader, boot, partitionOffset); err != nil {
		return layout{}, fmt.Errorf("读取 exFAT 启动扇区: %w", err)
	}
	if string(boot[3:11]) != "EXFAT   " || boot[510] != 0x55 || boot[511] != 0xaa {
		return layout{}, errors.New("TeslaCam 第一分区不是有效 exFAT")
	}
	sectorShift, clusterShift := boot[108], boot[109]
	if sectorShift < 9 || sectorShift > 12 || clusterShift > 25-sectorShift {
		return layout{}, errors.New("exFAT 扇区或簇大小超出规范")
	}
	fs := layout{
		partitionOffset: partitionOffset,
		partitionBytes:  partitionBytes,
		sectorSize:      uint64(1) << sectorShift,
		clusterSize:     uint64(1) << (sectorShift + clusterShift),
		fatOffset:       uint64(binary.LittleEndian.Uint32(boot[80:84])),
		fatLength:       uint64(binary.LittleEndian.Uint32(boot[84:88])),
		clusterHeap:     uint64(binary.LittleEndian.Uint32(boot[88:92])),
		clusterCount:    binary.LittleEndian.Uint32(boot[92:96]),
		rootCluster:     binary.LittleEndian.Uint32(boot[96:100]),
		numberOfFATs:    boot[110],
		activeFAT:       uint8(binary.LittleEndian.Uint16(boot[106:108]) & 1),
	}
	if fs.fatOffset < 24 || fs.fatLength == 0 || fs.clusterCount == 0 || fs.numberOfFATs == 0 || fs.numberOfFATs > 2 || fs.activeFAT >= fs.numberOfFATs || !validCluster(fs.rootCluster, fs.clusterCount) {
		return layout{}, errors.New("exFAT 布局字段无效")
	}
	heapEnd := fs.clusterHeap*fs.sectorSize + uint64(fs.clusterCount)*fs.clusterSize
	volumeBytes := binary.LittleEndian.Uint64(boot[72:80]) * fs.sectorSize
	if heapEnd > volumeBytes || volumeBytes > fs.partitionBytes {
		return layout{}, errors.New("exFAT 数据簇超出卷边界")
	}
	return fs, nil
}

func isFullVolume(fs layout, imageSize uint64) bool {
	partitionEnd := fs.partitionOffset + fs.partitionBytes
	return partitionEnd <= imageSize && imageSize-partitionEnd <= fullVolumeTolerance
}

func readRootMetadata(reader io.ReaderAt, fs layout) (systemFile, []byte, error) {
	root, err := readCluster(reader, fs, fs.rootCluster)
	if err != nil {
		return systemFile{}, nil, fmt.Errorf("读取 exFAT 根目录: %w", err)
	}
	for offset := 0; offset+directoryEntrySize <= len(root); offset += directoryEntrySize {
		entry := root[offset : offset+directoryEntrySize]
		if entry[0] == 0 {
			break
		}
		if entry[0] == bitmapEntryType && entry[1]&1 == fs.activeFAT {
			bitmap := systemFile{flags: entry[1], firstCluster: binary.LittleEndian.Uint32(entry[20:24]), dataLength: binary.LittleEndian.Uint64(entry[24:32])}
			if !validCluster(bitmap.firstCluster, fs.clusterCount) || bitmap.dataLength < (uint64(fs.clusterCount)+7)/8 {
				return systemFile{}, nil, errors.New("exFAT 分配位图入口无效")
			}
			return bitmap, root, nil
		}
	}
	return systemFile{}, nil, errors.New("exFAT 根目录缺少活动分配位图")
}

func readSystemFile(reader io.ReaderAt, fs layout, file systemFile) ([]byte, []uint32, error) {
	required := (uint64(fs.clusterCount) + 7) / 8
	if required > math.MaxInt || required > maxAllocationBitmap {
		return nil, nil, errors.New("exFAT 分配位图超过内存上限")
	}
	clusters, err := fileClusters(reader, fs, file)
	if err != nil {
		return nil, nil, err
	}
	content := make([]byte, int(required))
	written := uint64(0)
	for _, cluster := range clusters {
		data, readErr := readCluster(reader, fs, cluster)
		if readErr != nil {
			return nil, nil, readErr
		}
		length := min(uint64(len(data)), required-written)
		copy(content[int(written):int(written+length)], data[:int(length)])
		written += length
		if written == required {
			break
		}
	}
	if written != required {
		return nil, nil, errors.New("exFAT 分配位图簇链提前结束")
	}
	return content, clusters, nil
}

func writeSystemFile(writer io.WriterAt, fs layout, file systemFile, clusters []uint32, content []byte) error {
	written := 0
	for _, cluster := range clusters {
		if written >= len(content) {
			break
		}
		offset, err := clusterOffset(fs, cluster)
		if err != nil {
			return err
		}
		length := min(int(fs.clusterSize), len(content)-written)
		if err := writeExactAt(writer, content[written:written+length], offset); err != nil {
			return err
		}
		written += length
	}
	if written != len(content) {
		return errors.New("exFAT 分配位图写入不完整")
	}
	return nil
}

func fileClusters(reader io.ReaderAt, fs layout, file systemFile) ([]uint32, error) {
	count := (file.dataLength + fs.clusterSize - 1) / fs.clusterSize
	if count == 0 || count > uint64(fs.clusterCount) {
		return nil, errors.New("exFAT 系统文件簇数量无效")
	}
	clusters := make([]uint32, 0, int(count))
	cluster := file.firstCluster
	for index := uint64(0); index < count; index++ {
		if !validCluster(cluster, fs.clusterCount) {
			return nil, errors.New("exFAT 系统文件簇超出边界")
		}
		clusters = append(clusters, cluster)
		if index+1 == count {
			break
		}
		if file.flags&noFATChainFlag != 0 {
			cluster++
			continue
		}
		next, err := nextCluster(reader, fs, cluster)
		if err != nil {
			return nil, err
		}
		if next >= endOfChain {
			return nil, errors.New("exFAT 系统文件簇链提前结束")
		}
		cluster = next
	}
	return clusters, nil
}

func findReserve(root []byte) (reserveEntry, bool, error) {
	for offset := 0; offset+directoryEntrySize <= len(root); {
		if root[offset] == 0 {
			return reserveEntry{}, false, nil
		}
		if root[offset] != fileEntryType {
			offset += directoryEntrySize
			continue
		}
		secondaryCount := int(root[offset+1])
		setLength := (secondaryCount + 1) * directoryEntrySize
		if secondaryCount < 2 || offset+setLength > len(root) {
			return reserveEntry{}, false, errors.New("exFAT 根目录文件入口损坏")
		}
		set := root[offset : offset+setLength]
		if entrySetChecksum(set) != binary.LittleEndian.Uint16(set[2:4]) {
			return reserveEntry{}, false, errors.New("exFAT 根目录文件入口校验失败")
		}
		stream := set[directoryEntrySize : 2*directoryEntrySize]
		if stream[0] != streamEntryType {
			return reserveEntry{}, false, errors.New("exFAT 文件缺少流扩展入口")
		}
		nameLength := int(stream[3])
		name := decodeName(set[2*directoryEntrySize:], nameLength)
		if name == reserveName {
			return reserveEntry{
				attributes:   binary.LittleEndian.Uint16(set[4:6]),
				flags:        stream[1],
				firstCluster: binary.LittleEndian.Uint32(stream[20:24]),
				dataLength:   binary.LittleEndian.Uint64(stream[24:32]),
				entryOffset:  offset,
				entryCount:   secondaryCount + 1,
			}, true, nil
		}
		offset += setLength
	}
	return reserveEntry{}, false, nil
}

func reserveEntryOffset(root []byte) (int, error) {
	for offset := 0; offset+3*directoryEntrySize <= len(root); offset += directoryEntrySize {
		if root[offset] == 0 {
			return offset, nil
		}
	}
	return 0, errors.New("exFAT 根目录没有足够空间写入容量保留文件")
}

func buildReserveEntry(firstCluster uint32, dataLength uint64) []byte {
	set := make([]byte, 3*directoryEntrySize)
	set[0] = fileEntryType
	set[1] = 2
	binary.LittleEndian.PutUint16(set[4:6], reservedAttributes)
	date, clock, milliseconds := exFATTimestamp(time.Now().UTC())
	binary.LittleEndian.PutUint16(set[8:10], clock)
	binary.LittleEndian.PutUint16(set[10:12], date)
	binary.LittleEndian.PutUint16(set[12:14], clock)
	binary.LittleEndian.PutUint16(set[14:16], date)
	binary.LittleEndian.PutUint16(set[16:18], clock)
	binary.LittleEndian.PutUint16(set[18:20], date)
	set[20], set[21] = milliseconds, milliseconds
	set[22], set[23], set[24] = 0x80, 0x80, 0x80
	stream := set[directoryEntrySize : 2*directoryEntrySize]
	stream[0] = streamEntryType
	stream[1] = allocationPossibleFlag | noFATChainFlag
	stream[3] = byte(len(reserveName))
	binary.LittleEndian.PutUint16(stream[4:6], nameHash(reserveName))
	binary.LittleEndian.PutUint64(stream[8:16], dataLength)
	binary.LittleEndian.PutUint32(stream[20:24], firstCluster)
	binary.LittleEndian.PutUint64(stream[24:32], dataLength)
	nameEntry := set[2*directoryEntrySize:]
	nameEntry[0] = fileNameEntryType
	for index, value := range utf16.Encode([]rune(reserveName)) {
		binary.LittleEndian.PutUint16(nameEntry[2+index*2:4+index*2], value)
	}
	binary.LittleEndian.PutUint16(set[2:4], entrySetChecksum(set))
	return set
}

func exFATTimestamp(value time.Time) (uint16, uint16, byte) {
	year := min(max(value.Year(), 1980), 2107)
	date := uint16(year-1980)<<9 | uint16(value.Month())<<5 | uint16(value.Day())
	clock := uint16(value.Hour())<<11 | uint16(value.Minute())<<5 | uint16(value.Second()/2)
	milliseconds := byte((value.Second()%2)*100 + value.Nanosecond()/10_000_000)
	return date, clock, milliseconds
}

func writePercentInUse(writer io.WriterAt, fs layout, bitmap []byte) error {
	var allocated uint64
	for index := uint32(0); index < fs.clusterCount; index++ {
		if bitmapAllocated(bitmap, index) {
			allocated++
		}
	}
	percentage := byte((allocated*100 + uint64(fs.clusterCount) - 1) / uint64(fs.clusterCount))
	for _, bootOffset := range []uint64{fs.partitionOffset, fs.partitionOffset + 12*fs.sectorSize} {
		if err := writeExactAt(writer, []byte{percentage}, bootOffset+112); err != nil {
			return err
		}
	}
	return nil
}

func decodeName(entries []byte, length int) string {
	if length <= 0 || length > 255 {
		return ""
	}
	values := make([]uint16, 0, length)
	for offset := 0; offset+directoryEntrySize <= len(entries) && len(values) < length; offset += directoryEntrySize {
		entry := entries[offset : offset+directoryEntrySize]
		if entry[0] != fileNameEntryType {
			break
		}
		for index := 0; index < 15 && len(values) < length; index++ {
			values = append(values, binary.LittleEndian.Uint16(entry[2+index*2:4+index*2]))
		}
	}
	return string(utf16.Decode(values))
}

func nameHash(name string) uint16 {
	var hash uint16
	for _, value := range utf16.Encode([]rune(name)) {
		hash = rotateAdd(hash, byte(value))
		hash = rotateAdd(hash, byte(value>>8))
	}
	return hash
}

func entrySetChecksum(set []byte) uint16 {
	var checksum uint16
	for index, value := range set {
		if index == 2 || index == 3 {
			continue
		}
		checksum = rotateAdd(checksum, value)
	}
	return checksum
}

func rotateAdd(value uint16, next byte) uint16 {
	return (value>>1 | value<<15) + uint16(next)
}

func readCluster(reader io.ReaderAt, fs layout, cluster uint32) ([]byte, error) {
	offset, err := clusterOffset(fs, cluster)
	if err != nil {
		return nil, err
	}
	if fs.clusterSize > math.MaxInt {
		return nil, errors.New("exFAT 簇大小超过内存上限")
	}
	data := make([]byte, int(fs.clusterSize))
	if err := readExactAt(reader, data, offset); err != nil {
		return nil, err
	}
	return data, nil
}

func clusterOffset(fs layout, cluster uint32) (uint64, error) {
	if !validCluster(cluster, fs.clusterCount) {
		return 0, errors.New("exFAT 簇编号超出边界")
	}
	relative := fs.clusterHeap*fs.sectorSize + uint64(cluster-2)*fs.clusterSize
	if relative > fs.partitionBytes || fs.clusterSize > fs.partitionBytes-relative {
		return 0, errors.New("exFAT 簇超出分区边界")
	}
	return fs.partitionOffset + relative, nil
}

func nextCluster(reader io.ReaderAt, fs layout, cluster uint32) (uint32, error) {
	fatSector := fs.fatOffset + uint64(fs.activeFAT)*fs.fatLength
	offset := fs.partitionOffset + fatSector*fs.sectorSize + uint64(cluster)*4
	entry := make([]byte, 4)
	if err := readExactAt(reader, entry, offset); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(entry), nil
}

func bitmapAllocated(bitmap []byte, index uint32) bool {
	return bitmap[index/8]&(1<<uint(index%8)) != 0
}

func setBitmapAllocated(bitmap []byte, index uint32) {
	bitmap[index/8] |= 1 << uint(index%8)
}

func setBitmapFree(bitmap []byte, index uint32) {
	bitmap[index/8] &^= 1 << uint(index%8)
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

func writeExactAt(writer io.WriterAt, buffer []byte, offset uint64) error {
	if offset > math.MaxInt64 {
		return errors.New("写入偏移超出系统限制")
	}
	n, err := writer.WriteAt(buffer, int64(offset))
	if err != nil {
		return err
	}
	if n != len(buffer) {
		return io.ErrShortWrite
	}
	return nil
}
