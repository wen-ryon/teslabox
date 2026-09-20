package service

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

type indexVolume struct {
	Offset int64
	Length int64
	Serial uint32
}

// readIndexVolume 与镜像初始化及挂载约定保持一致，只识别 MBR 第一分区。
// 身份检查不是文件系统修复，不扫描其他分区或猜测启动扇区位置。
func readIndexVolume(reader io.ReaderAt, imageSize int64) (indexVolume, error) {
	var mbr [512]byte
	if imageSize < int64(len(mbr)) {
		return indexVolume{}, errors.New("录像镜像不足一个 MBR 扇区")
	}
	if _, err := reader.ReadAt(mbr[:], 0); err != nil {
		return indexVolume{}, fmt.Errorf("读取录像镜像 MBR: %w", err)
	}
	if mbr[510] != 0x55 || mbr[511] != 0xaa {
		return indexVolume{}, errors.New("录像镜像 MBR 签名无效")
	}
	entry := mbr[446:462]
	if entry[4] != 0x07 {
		return indexVolume{}, errors.New("录像镜像第一分区不是支持的 MBR exFAT 分区类型")
	}
	// 提升到 int64 后计算，避免 32 位 LBA 在乘法中溢出。
	offset := int64(binary.LittleEndian.Uint32(entry[8:12])) * 512
	length := int64(binary.LittleEndian.Uint32(entry[12:16])) * 512
	if offset < 512 || length < 512 || offset > imageSize || length > imageSize-offset {
		return indexVolume{}, errors.New("录像镜像第一分区为空或超出镜像边界")
	}
	var boot [512]byte
	if _, err := reader.ReadAt(boot[:], offset); err != nil {
		return indexVolume{}, fmt.Errorf("读取第一分区 exFAT 启动扇区: %w", err)
	}
	if string(boot[3:11]) != "EXFAT   " || boot[510] != 0x55 || boot[511] != 0xaa {
		return indexVolume{}, errors.New("录像镜像第一分区不是有效 exFAT")
	}
	return indexVolume{Offset: offset, Length: length, Serial: binary.LittleEndian.Uint32(boot[100:104])}, nil
}
