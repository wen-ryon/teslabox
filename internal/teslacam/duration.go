package teslacam

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

type mp4Box struct {
	typ         string
	payloadAt   int64
	payloadSize int64
}

func readMP4Duration(path string) (float64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	moov, err := findMP4Box(file, 0, info.Size(), "moov")
	if err != nil {
		return 0, err
	}
	mvhd, err := findMP4Box(file, moov.payloadAt, moov.payloadSize, "mvhd")
	if err != nil {
		return 0, err
	}
	return readMovieHeaderDuration(file, mvhd)
}

func findMP4Box(reader io.ReaderAt, start, length int64, target string) (mp4Box, error) {
	end := start + length
	for offset := start; offset+8 <= end; {
		header := make([]byte, 16)
		if _, err := reader.ReadAt(header[:8], offset); err != nil {
			return mp4Box{}, err
		}
		size := int64(binary.BigEndian.Uint32(header[:4]))
		headerSize := int64(8)
		if size == 1 {
			if _, err := reader.ReadAt(header[8:16], offset+8); err != nil {
				return mp4Box{}, err
			}
			size = int64(binary.BigEndian.Uint64(header[8:16]))
			headerSize = 16
		} else if size == 0 {
			size = end - offset
		}
		if size < headerSize || offset+size > end {
			return mp4Box{}, errors.New("MP4 box 大小无效")
		}
		box := mp4Box{
			typ: string(header[4:8]), payloadAt: offset + headerSize, payloadSize: size - headerSize,
		}
		if box.typ == target {
			return box, nil
		}
		offset += size
	}
	return mp4Box{}, fmt.Errorf("MP4 缺少 %s box", target)
}

func readMovieHeaderDuration(reader io.ReaderAt, box mp4Box) (float64, error) {
	payload := make([]byte, 32)
	readSize := min(int64(len(payload)), box.payloadSize)
	if readSize < 20 {
		return 0, errors.New("MP4 mvhd box 太短")
	}
	if _, err := reader.ReadAt(payload[:readSize], box.payloadAt); err != nil {
		return 0, err
	}
	var timescale uint32
	var duration uint64
	switch payload[0] {
	case 0:
		timescale = binary.BigEndian.Uint32(payload[12:16])
		duration = uint64(binary.BigEndian.Uint32(payload[16:20]))
	case 1:
		if readSize < 32 {
			return 0, errors.New("MP4 v1 mvhd box 太短")
		}
		timescale = binary.BigEndian.Uint32(payload[20:24])
		duration = binary.BigEndian.Uint64(payload[24:32])
	default:
		return 0, errors.New("MP4 mvhd 版本不支持")
	}
	if timescale == 0 {
		return 0, errors.New("MP4 timescale 为零")
	}
	return float64(duration) / float64(timescale), nil
}
