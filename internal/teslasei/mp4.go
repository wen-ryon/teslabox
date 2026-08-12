package teslasei

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"time"
)

const maxVideoSamples = 200_000

type mp4Box struct {
	typ         string
	payloadAt   int64
	payloadSize int64
}

type videoSample struct {
	offset int64
	size   int64
	pts    time.Duration
}

type sampleToChunkEntry struct {
	firstChunk      uint32
	samplesPerChunk uint32
}

// readVideoSamples 从视频轨 sample table 还原每个压缩帧的文件位置和 PTS。
// TeslaCam 的 SEI 并不保证从文件首帧开始出现，只有使用真实 PTS 才能与画面严格对齐。
func readVideoSamples(reader io.ReaderAt, fileSize int64) ([]videoSample, error) {
	moov, err := findBox(reader, 0, fileSize, "moov")
	if err != nil {
		return nil, err
	}
	tracks, err := findBoxes(reader, moov.payloadAt, moov.payloadSize, "trak")
	if err != nil {
		return nil, err
	}
	for _, track := range tracks {
		media, err := findBox(reader, track.payloadAt, track.payloadSize, "mdia")
		if err != nil {
			continue
		}
		handler, err := findBox(reader, media.payloadAt, media.payloadSize, "hdlr")
		if err != nil || !isVideoTrack(reader, handler) {
			continue
		}
		return readTrackSamples(reader, media, fileSize)
	}
	return nil, errors.New("MP4 缺少视频轨 sample table")
}

func readTrackSamples(reader io.ReaderAt, media mp4Box, fileSize int64) ([]videoSample, error) {
	header, err := findBox(reader, media.payloadAt, media.payloadSize, "mdhd")
	if err != nil {
		return nil, err
	}
	timescale, err := readMediaTimescale(reader, header)
	if err != nil {
		return nil, err
	}
	mediaInfo, err := findBox(reader, media.payloadAt, media.payloadSize, "minf")
	if err != nil {
		return nil, err
	}
	table, err := findBox(reader, mediaInfo.payloadAt, mediaInfo.payloadSize, "stbl")
	if err != nil {
		return nil, err
	}
	sizeBox, err := findBox(reader, table.payloadAt, table.payloadSize, "stsz")
	if err != nil {
		return nil, err
	}
	sizes, err := readSampleSizes(reader, sizeBox)
	if err != nil {
		return nil, err
	}
	chunkOffsets, err := readChunkOffsets(reader, table)
	if err != nil {
		return nil, err
	}
	chunkMapBox, err := findBox(reader, table.payloadAt, table.payloadSize, "stsc")
	if err != nil {
		return nil, err
	}
	chunkMap, err := readSampleToChunk(reader, chunkMapBox)
	if err != nil {
		return nil, err
	}
	samples, err := mapSamplePositions(sizes, chunkOffsets, chunkMap, fileSize)
	if err != nil {
		return nil, err
	}
	timingBox, err := findBox(reader, table.payloadAt, table.payloadSize, "stts")
	if err != nil {
		return nil, err
	}
	decodeTimes, err := readDecodeTimes(reader, timingBox, len(samples))
	if err != nil {
		return nil, err
	}
	compositionOffsets := make([]int64, len(samples))
	if compositionBox, findErr := findBox(reader, table.payloadAt, table.payloadSize, "ctts"); findErr == nil {
		compositionOffsets, err = readCompositionOffsets(reader, compositionBox, len(samples))
		if err != nil {
			return nil, err
		}
	}
	for index := range samples {
		pts := decodeTimes[index] + compositionOffsets[index]
		if pts < 0 {
			pts = 0
		}
		samples[index].pts = ticksToDuration(pts, timescale)
	}
	return samples, nil
}

func readTimedSamples(ctx context.Context, reader io.ReaderAt, samples []videoSample) ([]Metadata, error) {
	metadata := make([]Metadata, 0)
	var sizeBuffer [4]byte
	for _, sample := range samples {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := sample.offset + sample.size
		for offset := sample.offset; offset+4 <= end; {
			if _, err := reader.ReadAt(sizeBuffer[:], offset); err != nil {
				return nil, fmt.Errorf("读取 MP4 sample NAL 长度: %w", err)
			}
			nalSize := int64(binary.BigEndian.Uint32(sizeBuffer[:]))
			offset += 4
			if nalSize == 0 {
				continue
			}
			if nalSize > end-offset {
				return nil, errors.New("MP4 sample NAL 长度超出帧边界")
			}
			var prefix [2]byte
			if nalSize >= 2 {
				if _, err := reader.ReadAt(prefix[:], offset); err != nil {
					return nil, fmt.Errorf("读取 MP4 sample NAL 头: %w", err)
				}
			}
			if nalSize >= 2 && prefix[0]&0x1f == 6 && prefix[1] == 5 && nalSize <= maxSEINALBytes {
				nal := make([]byte, nalSize)
				if _, err := reader.ReadAt(nal, offset); err != nil {
					return nil, fmt.Errorf("读取 MP4 sample SEI NAL: %w", err)
				}
				if payload, found := protoPayload(nal); found {
					item, decodeErr := decodeMetadata(payload)
					if decodeErr == nil {
						item.mediaOffset = sample.pts
						item.hasMediaOffset = true
						metadata = append(metadata, item)
						if len(metadata) > maxMetadata {
							return nil, errors.New("SEI 元数据数量超过安全上限")
						}
					}
				}
			}
			offset += nalSize
		}
	}
	sort.SliceStable(metadata, func(left, right int) bool {
		return metadata[left].mediaOffset < metadata[right].mediaOffset
	})
	return metadata, nil
}

func findBox(reader io.ReaderAt, start, length int64, target string) (mp4Box, error) {
	boxes, err := findBoxes(reader, start, length, target)
	if err != nil {
		return mp4Box{}, err
	}
	if len(boxes) == 0 {
		return mp4Box{}, fmt.Errorf("MP4 缺少 %s box", target)
	}
	return boxes[0], nil
}

func findBoxes(reader io.ReaderAt, start, length int64, target string) ([]mp4Box, error) {
	if start < 0 || length < 0 || start > math.MaxInt64-length {
		return nil, errors.New("MP4 box 范围无效")
	}
	end := start + length
	boxes := make([]mp4Box, 0, 2)
	for offset := start; offset+8 <= end; {
		size, typ, headerSize, err := readAtomHeader(reader, offset, end)
		if err != nil {
			return nil, err
		}
		box := mp4Box{typ: typ, payloadAt: offset + headerSize, payloadSize: size - headerSize}
		if typ == target {
			boxes = append(boxes, box)
		}
		offset += size
	}
	return boxes, nil
}

func isVideoTrack(reader io.ReaderAt, box mp4Box) bool {
	if box.payloadSize < 12 {
		return false
	}
	var payload [12]byte
	if _, err := reader.ReadAt(payload[:], box.payloadAt); err != nil {
		return false
	}
	return string(payload[8:12]) == "vide"
}

func readMediaTimescale(reader io.ReaderAt, box mp4Box) (uint32, error) {
	var payload [24]byte
	readSize := min(int64(len(payload)), box.payloadSize)
	if readSize < 16 {
		return 0, errors.New("MP4 mdhd box 太短")
	}
	if _, err := reader.ReadAt(payload[:readSize], box.payloadAt); err != nil {
		return 0, err
	}
	var timescale uint32
	switch payload[0] {
	case 0:
		timescale = binary.BigEndian.Uint32(payload[12:16])
	case 1:
		if readSize < 24 {
			return 0, errors.New("MP4 v1 mdhd box 太短")
		}
		timescale = binary.BigEndian.Uint32(payload[20:24])
	default:
		return 0, errors.New("MP4 mdhd 版本不支持")
	}
	if timescale == 0 {
		return 0, errors.New("MP4 timescale 为零")
	}
	return timescale, nil
}

func readSampleSizes(reader io.ReaderAt, box mp4Box) ([]uint32, error) {
	var header [12]byte
	if box.payloadSize < int64(len(header)) {
		return nil, errors.New("MP4 stsz box 太短")
	}
	if _, err := reader.ReadAt(header[:], box.payloadAt); err != nil {
		return nil, err
	}
	defaultSize := binary.BigEndian.Uint32(header[4:8])
	count := binary.BigEndian.Uint32(header[8:12])
	if count == 0 || count > maxVideoSamples {
		return nil, errors.New("MP4 sample 数量无效")
	}
	sizes := make([]uint32, int(count))
	if defaultSize != 0 {
		for index := range sizes {
			sizes[index] = defaultSize
		}
		return sizes, nil
	}
	bytesNeeded := int64(count) * 4
	if box.payloadSize-12 < bytesNeeded {
		return nil, errors.New("MP4 stsz sample 列表截断")
	}
	payload := make([]byte, bytesNeeded)
	if _, err := reader.ReadAt(payload, box.payloadAt+12); err != nil {
		return nil, err
	}
	for index := range sizes {
		sizes[index] = binary.BigEndian.Uint32(payload[index*4 : index*4+4])
		if sizes[index] == 0 {
			return nil, errors.New("MP4 sample 大小为零")
		}
	}
	return sizes, nil
}

func readChunkOffsets(reader io.ReaderAt, table mp4Box) ([]int64, error) {
	box, err := findBox(reader, table.payloadAt, table.payloadSize, "stco")
	wide := false
	if err != nil {
		box, err = findBox(reader, table.payloadAt, table.payloadSize, "co64")
		wide = true
	}
	if err != nil {
		return nil, err
	}
	var header [8]byte
	if box.payloadSize < int64(len(header)) {
		return nil, errors.New("MP4 chunk offset box 太短")
	}
	if _, err := reader.ReadAt(header[:], box.payloadAt); err != nil {
		return nil, err
	}
	count := binary.BigEndian.Uint32(header[4:8])
	if count == 0 || count > maxVideoSamples {
		return nil, errors.New("MP4 chunk 数量无效")
	}
	width := int64(4)
	if wide {
		width = 8
	}
	bytesNeeded := int64(count) * width
	if box.payloadSize-8 < bytesNeeded {
		return nil, errors.New("MP4 chunk offset 列表截断")
	}
	payload := make([]byte, bytesNeeded)
	if _, err := reader.ReadAt(payload, box.payloadAt+8); err != nil {
		return nil, err
	}
	offsets := make([]int64, int(count))
	for index := range offsets {
		if wide {
			value := binary.BigEndian.Uint64(payload[index*8 : index*8+8])
			if value > math.MaxInt64 {
				return nil, errors.New("MP4 chunk offset 超出支持范围")
			}
			offsets[index] = int64(value)
		} else {
			offsets[index] = int64(binary.BigEndian.Uint32(payload[index*4 : index*4+4]))
		}
	}
	return offsets, nil
}

func readSampleToChunk(reader io.ReaderAt, box mp4Box) ([]sampleToChunkEntry, error) {
	var header [8]byte
	if box.payloadSize < int64(len(header)) {
		return nil, errors.New("MP4 stsc box 太短")
	}
	if _, err := reader.ReadAt(header[:], box.payloadAt); err != nil {
		return nil, err
	}
	count := binary.BigEndian.Uint32(header[4:8])
	if count == 0 || count > maxVideoSamples || box.payloadSize-8 < int64(count)*12 {
		return nil, errors.New("MP4 stsc 条目数量无效")
	}
	payload := make([]byte, int64(count)*12)
	if _, err := reader.ReadAt(payload, box.payloadAt+8); err != nil {
		return nil, err
	}
	entries := make([]sampleToChunkEntry, int(count))
	for index := range entries {
		start := index * 12
		entries[index] = sampleToChunkEntry{
			firstChunk:      binary.BigEndian.Uint32(payload[start : start+4]),
			samplesPerChunk: binary.BigEndian.Uint32(payload[start+4 : start+8]),
		}
		if entries[index].firstChunk == 0 || entries[index].samplesPerChunk == 0 ||
			(index > 0 && entries[index].firstChunk <= entries[index-1].firstChunk) {
			return nil, errors.New("MP4 stsc 映射无效")
		}
	}
	if entries[0].firstChunk != 1 {
		return nil, errors.New("MP4 stsc 未从首个 chunk 开始")
	}
	return entries, nil
}

func mapSamplePositions(sizes []uint32, chunkOffsets []int64, entries []sampleToChunkEntry, fileSize int64) ([]videoSample, error) {
	samples := make([]videoSample, len(sizes))
	sampleIndex := 0
	entryIndex := 0
	for chunkIndex, chunkOffset := range chunkOffsets {
		chunkNumber := uint32(chunkIndex + 1)
		for entryIndex+1 < len(entries) && entries[entryIndex+1].firstChunk <= chunkNumber {
			entryIndex++
		}
		position := chunkOffset
		for range entries[entryIndex].samplesPerChunk {
			if sampleIndex >= len(samples) {
				return nil, errors.New("MP4 chunk 映射包含过多 sample")
			}
			size := int64(sizes[sampleIndex])
			if position < 0 || size <= 0 || position > fileSize-size {
				return nil, errors.New("MP4 sample 文件范围无效")
			}
			samples[sampleIndex] = videoSample{offset: position, size: size}
			position += size
			sampleIndex++
		}
	}
	if sampleIndex != len(samples) {
		return nil, errors.New("MP4 chunk 映射未覆盖全部 sample")
	}
	return samples, nil
}

func readDecodeTimes(reader io.ReaderAt, box mp4Box, sampleCount int) ([]int64, error) {
	entries, err := readTimeEntries(reader, box, false)
	if err != nil {
		return nil, err
	}
	result := make([]int64, sampleCount)
	index := 0
	current := int64(0)
	for _, entry := range entries {
		for range entry.count {
			if index >= len(result) {
				return nil, errors.New("MP4 stts 包含过多 sample")
			}
			result[index] = current
			current += entry.value
			index++
		}
	}
	if index != len(result) {
		return nil, errors.New("MP4 stts 未覆盖全部 sample")
	}
	return result, nil
}

func readCompositionOffsets(reader io.ReaderAt, box mp4Box, sampleCount int) ([]int64, error) {
	entries, err := readTimeEntries(reader, box, true)
	if err != nil {
		return nil, err
	}
	result := make([]int64, sampleCount)
	index := 0
	for _, entry := range entries {
		for range entry.count {
			if index >= len(result) {
				return nil, errors.New("MP4 ctts 包含过多 sample")
			}
			result[index] = entry.value
			index++
		}
	}
	if index != len(result) {
		return nil, errors.New("MP4 ctts 未覆盖全部 sample")
	}
	return result, nil
}

type timeEntry struct {
	count uint32
	value int64
}

func readTimeEntries(reader io.ReaderAt, box mp4Box, signed bool) ([]timeEntry, error) {
	var header [8]byte
	if box.payloadSize < int64(len(header)) {
		return nil, errors.New("MP4 时间表 box 太短")
	}
	if _, err := reader.ReadAt(header[:], box.payloadAt); err != nil {
		return nil, err
	}
	version := header[0]
	count := binary.BigEndian.Uint32(header[4:8])
	if count == 0 || count > maxVideoSamples || box.payloadSize-8 < int64(count)*8 {
		return nil, errors.New("MP4 时间表条目数量无效")
	}
	payload := make([]byte, int64(count)*8)
	if _, err := reader.ReadAt(payload, box.payloadAt+8); err != nil {
		return nil, err
	}
	entries := make([]timeEntry, int(count))
	for index := range entries {
		start := index * 8
		entries[index].count = binary.BigEndian.Uint32(payload[start : start+4])
		value := binary.BigEndian.Uint32(payload[start+4 : start+8])
		if signed && version == 1 {
			entries[index].value = int64(int32(value))
		} else {
			entries[index].value = int64(value)
		}
		if entries[index].count == 0 {
			return nil, errors.New("MP4 时间表包含空条目")
		}
	}
	return entries, nil
}

func ticksToDuration(ticks int64, timescale uint32) time.Duration {
	seconds := ticks / int64(timescale)
	remainder := ticks % int64(timescale)
	return time.Duration(seconds)*time.Second + time.Duration(remainder)*time.Second/time.Duration(timescale)
}
