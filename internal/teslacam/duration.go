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
	duration, _, err := readMP4DurationProof(path)
	return duration, err
}

func readMP4DurationProof(path string) (float64, *durationProof, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, nil, err
	}
	reader := &proofReader{source: &durationPageReader{source: file, size: info.Size(), pages: make(map[int64][]byte)}, proof: durationProof{size: info.Size()}}
	duration, err := readMP4DurationFrom(reader, info.Size())
	if err != nil || reader.overflow {
		return duration, nil, err
	}
	return duration, &reader.proof, nil
}

func readMP4DurationFrom(file io.ReaderAt, size int64) (float64, error) {
	moov, err := findMP4Box(file, 0, size, "moov")
	if err != nil {
		return 0, err
	}
	if duration, err := readVideoTrackDuration(file, moov); err == nil && duration > 0 {
		return duration, nil
	}
	// 少数旧文件缺少完整视频轨结构，只能退回容器时长以保持可索引性。
	mvhd, err := findMP4Box(file, moov.payloadAt, moov.payloadSize, "mvhd")
	if err != nil {
		return 0, err
	}
	return readMovieHeaderDuration(file, mvhd)
}

func readVideoTrackDuration(reader io.ReaderAt, moov mp4Box) (float64, error) {
	tracks, err := findMP4Boxes(reader, moov.payloadAt, moov.payloadSize, "trak")
	if err != nil {
		return 0, err
	}
	for _, track := range tracks {
		media, err := findMP4Box(reader, track.payloadAt, track.payloadSize, "mdia")
		if err != nil {
			continue
		}
		handler, err := findMP4Box(reader, media.payloadAt, media.payloadSize, "hdlr")
		if err != nil || !isVideoHandler(reader, handler) {
			continue
		}
		header, err := findMP4Box(reader, media.payloadAt, media.payloadSize, "mdhd")
		if err != nil {
			continue
		}
		return readMediaHeaderDuration(reader, header)
	}
	return 0, errors.New("MP4 缺少视频轨时长")
}

func isVideoHandler(reader io.ReaderAt, box mp4Box) bool {
	if box.payloadSize < 12 {
		return false
	}
	payload := make([]byte, 12)
	if _, err := reader.ReadAt(payload, box.payloadAt); err != nil {
		return false
	}
	return string(payload[8:12]) == "vide"
}

func findMP4Box(reader io.ReaderAt, start, length int64, target string) (mp4Box, error) {
	boxes, err := findMP4Boxes(reader, start, length, target)
	if err != nil {
		return mp4Box{}, err
	}
	if len(boxes) > 0 {
		return boxes[0], nil
	}
	return mp4Box{}, fmt.Errorf("MP4 缺少 %s box", target)
}

func findMP4Boxes(reader io.ReaderAt, start, length int64, target string) ([]mp4Box, error) {
	end := start + length
	boxes := make([]mp4Box, 0, 2)
	for offset := start; offset+8 <= end; {
		header := make([]byte, 16)
		if _, err := reader.ReadAt(header[:8], offset); err != nil {
			return nil, err
		}
		size := int64(binary.BigEndian.Uint32(header[:4]))
		headerSize := int64(8)
		if size == 1 {
			if _, err := reader.ReadAt(header[8:16], offset+8); err != nil {
				return nil, err
			}
			size = int64(binary.BigEndian.Uint64(header[8:16]))
			headerSize = 16
		} else if size == 0 {
			size = end - offset
		}
		if size < headerSize || offset+size > end {
			return nil, errors.New("MP4 box 大小无效")
		}
		box := mp4Box{
			typ: string(header[4:8]), payloadAt: offset + headerSize, payloadSize: size - headerSize,
		}
		if box.typ == target {
			boxes = append(boxes, box)
		}
		offset += size
	}
	return boxes, nil
}

func readMovieHeaderDuration(reader io.ReaderAt, box mp4Box) (float64, error) {
	return readHeaderDuration(reader, box, "mvhd")
}

func readMediaHeaderDuration(reader io.ReaderAt, box mp4Box) (float64, error) {
	return readHeaderDuration(reader, box, "mdhd")
}

func readHeaderDuration(reader io.ReaderAt, box mp4Box, boxType string) (float64, error) {
	payload := make([]byte, 32)
	readSize := min(int64(len(payload)), box.payloadSize)
	if readSize < 20 {
		return 0, fmt.Errorf("MP4 %s box 太短", boxType)
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
			return 0, fmt.Errorf("MP4 v1 %s box 太短", boxType)
		}
		timescale = binary.BigEndian.Uint32(payload[20:24])
		duration = binary.BigEndian.Uint64(payload[24:32])
	default:
		return 0, fmt.Errorf("MP4 %s 版本不支持", boxType)
	}
	if timescale == 0 {
		return 0, errors.New("MP4 timescale 为零")
	}
	return float64(duration) / float64(timescale), nil
}
