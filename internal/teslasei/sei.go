// Package teslasei 解析 Tesla 行车记录 MP4 内的 H.264 SEI 遥测数据。
package teslasei

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"
)

var (
	// ErrNoMetadata 表示文件有效但不含当前已知的 Tesla FSD 遥测 SEI。
	ErrNoMetadata = errors.New("录像不包含 Tesla SEI 信息")
)

const (
	maxSEINALBytes = 128 * 1024
	maxMetadata    = 20_000
	readBlockBytes = 256 * 1024
)

var teslaMagic = []byte{'B', 'B', 'B', 'i'}

// Metadata 是 Tesla 通过 SEI 写入单帧的车辆状态。
type Metadata struct {
	Version             uint32  `json:"version"`
	Gear                string  `json:"gear"`
	FrameSequence       uint64  `json:"frame_sequence"`
	SpeedMPS            float32 `json:"speed_mps"`
	AcceleratorPosition float32 `json:"accelerator_position"`
	SteeringAngle       float32 `json:"steering_angle"`
	LeftBlinker         bool    `json:"left_blinker"`
	RightBlinker        bool    `json:"right_blinker"`
	BrakeApplied        bool    `json:"brake_applied"`
	Autopilot           string  `json:"autopilot"`
	Latitude            float64 `json:"latitude"`
	Longitude           float64 `json:"longitude"`
	Heading             float64 `json:"heading"`
	AccelerationX       float64 `json:"acceleration_x"`
	AccelerationY       float64 `json:"acceleration_y"`
	AccelerationZ       float64 `json:"acceleration_z"`
}

// Sample 是用于页面时间轴或导出字幕的一条定时遥测信息。
type Sample struct {
	OffsetMS int64    `json:"offset_ms"`
	Metadata Metadata `json:"metadata"`
	Text     string   `json:"text"`
}

// Read 从一个受信任的录像路径读取每个可识别的 Tesla SEI 元数据包。
func Read(ctx context.Context, path string) ([]Metadata, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开 SEI 录像: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("读取 SEI 录像大小: %w", err)
	}
	if info.Size() < 8 {
		return nil, ErrNoMetadata
	}
	reader := newBlockReaderAt(file, info.Size())
	metadata := make([]Metadata, 0)
	for offset := int64(0); offset+8 <= info.Size(); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		atomSize, atomType, headerSize, err := readAtomHeader(reader, offset, info.Size())
		if err != nil {
			return nil, err
		}
		atomEnd := offset + atomSize
		if atomType == "mdat" {
			items, err := readMdat(ctx, reader, offset+headerSize, atomEnd)
			if err != nil {
				return nil, err
			}
			metadata = append(metadata, items...)
			if len(metadata) > maxMetadata {
				return nil, errors.New("SEI 元数据数量超过安全上限")
			}
		}
		offset = atomEnd
	}
	if len(metadata) == 0 {
		return nil, ErrNoMetadata
	}
	return metadata, nil
}

// Timeline 将帧级 SEI 降采样为固定上限的播放时间线，避免页面传输整分钟全部帧信息。
func Timeline(metadata []Metadata, duration time.Duration, maximum int) []Sample {
	if len(metadata) == 0 || maximum <= 0 {
		return nil
	}
	count := min(len(metadata), maximum)
	timeline := make([]Sample, 0, count)
	for index := range count {
		metadataIndex := index
		if count > 1 && len(metadata) > count {
			metadataIndex = int(math.Round(float64(index) * float64(len(metadata)-1) / float64(count-1)))
		}
		offset := sampleOffset(metadataIndex, len(metadata), duration)
		item := metadata[metadataIndex]
		timeline = append(timeline, Sample{OffsetMS: offset.Milliseconds(), Metadata: item, Text: Display(item)})
	}
	return timeline
}

// At 返回最接近播放位置的帧级 SEI 信息。
func At(metadata []Metadata, duration, offset time.Duration) (Metadata, bool) {
	if len(metadata) == 0 || duration <= 0 {
		return Metadata{}, false
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= duration {
		return metadata[len(metadata)-1], true
	}
	index := int(float64(offset) / float64(duration) * float64(len(metadata)))
	index = min(max(index, 0), len(metadata)-1)
	return metadata[index], true
}

// Display 返回适合叠加在播放器或导出画面右上角的简洁中文文本。
func Display(metadata Metadata) string {
	parts := make([]string, 0, 5)
	if metadata.Gear != "" {
		parts = append(parts, "档位 "+metadata.Gear)
	}
	parts = append(parts, fmt.Sprintf("时速 %.1f km/h", float64(metadata.SpeedMPS)*3.6))
	if metadata.Autopilot != "" {
		parts = append(parts, "辅助驾驶 "+metadata.Autopilot)
	}
	if metadata.SteeringAngle != 0 {
		parts = append(parts, fmt.Sprintf("方向 %.1f°", metadata.SteeringAngle))
	}
	if metadata.BrakeApplied {
		parts = append(parts, "制动")
	}
	if metadata.LeftBlinker || metadata.RightBlinker {
		signal := "左"
		if metadata.RightBlinker {
			signal = "右"
		}
		if metadata.LeftBlinker && metadata.RightBlinker {
			signal = "双"
		}
		parts = append(parts, signal+"转向灯")
	}
	if metadata.Latitude != 0 || metadata.Longitude != 0 {
		parts = append(parts, fmt.Sprintf("GPS %.5f, %.5f", metadata.Latitude, metadata.Longitude))
	}
	return joinLines(parts)
}

func readAtomHeader(file io.ReaderAt, offset, fileSize int64) (int64, string, int64, error) {
	var header [16]byte
	if _, err := file.ReadAt(header[:8], offset); err != nil {
		return 0, "", 0, fmt.Errorf("读取 MP4 原子头: %w", err)
	}
	atomSize := int64(binary.BigEndian.Uint32(header[:4]))
	headerSize := int64(8)
	if atomSize == 1 {
		if _, err := file.ReadAt(header[8:], offset+8); err != nil {
			return 0, "", 0, fmt.Errorf("读取扩展 MP4 原子头: %w", err)
		}
		value := binary.BigEndian.Uint64(header[8:])
		if value > math.MaxInt64 {
			return 0, "", 0, errors.New("MP4 原子大小超出支持范围")
		}
		atomSize = int64(value)
		headerSize = 16
	} else if atomSize == 0 {
		atomSize = fileSize - offset
	}
	if atomSize < headerSize || atomSize > fileSize-offset {
		return 0, "", 0, errors.New("MP4 原子大小无效")
	}
	return atomSize, string(header[4:8]), headerSize, nil
}

func readMdat(ctx context.Context, file io.ReaderAt, start, end int64) ([]Metadata, error) {
	metadata := make([]Metadata, 0)
	var sizeBuffer [4]byte
	for offset := start; offset+4 <= end; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := file.ReadAt(sizeBuffer[:], offset); err != nil {
			return nil, fmt.Errorf("读取 H.264 NAL 长度: %w", err)
		}
		nalSize := int64(binary.BigEndian.Uint32(sizeBuffer[:]))
		offset += 4
		if nalSize == 0 {
			continue
		}
		if nalSize > end-offset {
			return nil, errors.New("H.264 NAL 长度超出 MP4 数据范围")
		}
		var prefix [2]byte
		if nalSize >= 2 {
			if _, err := file.ReadAt(prefix[:], offset); err != nil {
				return nil, fmt.Errorf("读取 H.264 NAL 头: %w", err)
			}
		}
		if nalSize >= 2 && prefix[0]&0x1f == 6 && prefix[1] == 5 && nalSize <= maxSEINALBytes {
			nal := make([]byte, nalSize)
			if _, err := file.ReadAt(nal, offset); err != nil {
				return nil, fmt.Errorf("读取 H.264 SEI NAL: %w", err)
			}
			if payload, found := protoPayload(nal); found {
				item, err := decodeMetadata(payload)
				if err != nil {
					// 同一文件可包含厂商未公开或损坏的 SEI；跳过单包，避免一帧异常使整段录像无法播放或导出。
					continue
				}
				metadata = append(metadata, item)
			}
		}
		offset += nalSize
	}
	return metadata, nil
}

// blockReaderAt 合并顺序推进的细碎读取，避免快照文件系统为每个 NAL 头执行一次底层 I/O。
type blockReaderAt struct {
	source     io.ReaderAt
	size       int64
	buffer     []byte
	blockStart int64
	blockEnd   int64
}

func newBlockReaderAt(source io.ReaderAt, size int64) *blockReaderAt {
	return &blockReaderAt{source: source, size: size, buffer: make([]byte, readBlockBytes), blockStart: -1}
}

func (reader *blockReaderAt) ReadAt(destination []byte, offset int64) (int, error) {
	requested := len(destination)
	written := 0
	for len(destination) > 0 && offset < reader.size {
		if offset < reader.blockStart || offset >= reader.blockEnd {
			reader.blockStart = offset / readBlockBytes * readBlockBytes
			blockLength := min(int64(len(reader.buffer)), reader.size-reader.blockStart)
			count, err := reader.source.ReadAt(reader.buffer[:blockLength], reader.blockStart)
			reader.blockEnd = reader.blockStart + int64(count)
			if err != nil && !errors.Is(err, io.EOF) {
				return written, err
			}
			if count == 0 {
				break
			}
		}
		available := int(reader.blockEnd - offset)
		count := min(len(destination), available)
		start := int(offset - reader.blockStart)
		copy(destination[:count], reader.buffer[start:start+count])
		destination = destination[count:]
		offset += int64(count)
		written += count
	}
	if written != requested {
		return written, io.EOF
	}
	return written, nil
}

func protoPayload(nal []byte) ([]byte, bool) {
	if len(nal) < 7 || nal[0]&0x1f != 6 || nal[1] != 5 {
		return nil, false
	}
	index := bytes.Index(nal[2:], teslaMagic)
	if index < 0 {
		return nil, false
	}
	start := 2 + index + len(teslaMagic)
	if start >= len(nal) {
		return nil, false
	}
	payload := nal[start:]
	if payload[len(payload)-1] == 0x80 {
		payload = payload[:len(payload)-1]
	}
	payload = removeEmulationPrevention(payload)
	if len(payload) == 0 {
		return nil, false
	}
	return payload, true
}

func removeEmulationPrevention(payload []byte) []byte {
	result := make([]byte, 0, len(payload))
	zeroes := 0
	for _, value := range payload {
		if zeroes >= 2 && value == 0x03 {
			zeroes = 0
			continue
		}
		result = append(result, value)
		if value == 0 {
			zeroes++
		} else {
			zeroes = 0
		}
	}
	return result
}

func decodeMetadata(payload []byte) (Metadata, error) {
	var metadata Metadata
	for offset := 0; offset < len(payload); {
		key, next, err := readVarint(payload, offset)
		if err != nil {
			return Metadata{}, err
		}
		offset = next
		field, wireType := int(key>>3), int(key&0x7)
		if field == 0 {
			return Metadata{}, errors.New("protobuf 字段编号无效")
		}
		switch field {
		case 1, 2, 3, 7, 8, 9, 10:
			if wireType != 0 {
				return Metadata{}, errors.New("protobuf 整数字段编码无效")
			}
			value, next, err := readVarint(payload, offset)
			if err != nil {
				return Metadata{}, err
			}
			offset = next
			switch field {
			case 1:
				metadata.Version = uint32(value)
			case 2:
				metadata.Gear = gearName(value)
			case 3:
				metadata.FrameSequence = value
			case 7:
				metadata.LeftBlinker = value != 0
			case 8:
				metadata.RightBlinker = value != 0
			case 9:
				metadata.BrakeApplied = value != 0
			case 10:
				metadata.Autopilot = autopilotName(value)
			}
		case 4, 5, 6:
			if wireType != 5 || offset+4 > len(payload) {
				return Metadata{}, errors.New("protobuf 浮点字段编码无效")
			}
			value := math.Float32frombits(binary.LittleEndian.Uint32(payload[offset : offset+4]))
			offset += 4
			if field == 4 {
				metadata.SpeedMPS = value
			} else if field == 5 {
				metadata.AcceleratorPosition = value
			} else {
				metadata.SteeringAngle = value
			}
		case 11, 12, 13, 14, 15, 16:
			if wireType != 1 || offset+8 > len(payload) {
				return Metadata{}, errors.New("protobuf 双精度字段编码无效")
			}
			value := math.Float64frombits(binary.LittleEndian.Uint64(payload[offset : offset+8]))
			offset += 8
			switch field {
			case 11:
				metadata.Latitude = value
			case 12:
				metadata.Longitude = value
			case 13:
				metadata.Heading = value
			case 14:
				metadata.AccelerationX = value
			case 15:
				metadata.AccelerationY = value
			case 16:
				metadata.AccelerationZ = value
			}
		default:
			next, err := skipField(payload, offset, wireType)
			if err != nil {
				return Metadata{}, err
			}
			offset = next
		}
	}
	return metadata, nil
}

func readVarint(payload []byte, offset int) (uint64, int, error) {
	var value uint64
	for shift := uint(0); shift < 64; shift += 7 {
		if offset >= len(payload) {
			return 0, 0, errors.New("protobuf 变长整数截断")
		}
		item := payload[offset]
		offset++
		value |= uint64(item&0x7f) << shift
		if item&0x80 == 0 {
			return value, offset, nil
		}
	}
	return 0, 0, errors.New("protobuf 变长整数过长")
}

func skipField(payload []byte, offset, wireType int) (int, error) {
	switch wireType {
	case 0:
		_, next, err := readVarint(payload, offset)
		return next, err
	case 1:
		offset += 8
	case 2:
		length, next, err := readVarint(payload, offset)
		if err != nil {
			return 0, err
		}
		if length > uint64(len(payload)-next) {
			return 0, errors.New("protobuf 长度字段截断")
		}
		offset = next + int(length)
	case 5:
		offset += 4
	default:
		return 0, errors.New("protobuf 包含不支持的字段编码")
	}
	if offset > len(payload) {
		return 0, errors.New("protobuf 字段截断")
	}
	return offset, nil
}

func sampleOffset(index, total int, duration time.Duration) time.Duration {
	if total <= 1 || duration <= 0 {
		return 0
	}
	return time.Duration(float64(duration) * float64(index) / float64(total-1))
}

func gearName(value uint64) string {
	switch value {
	case 0:
		return "P"
	case 1:
		return "D"
	case 2:
		return "R"
	case 3:
		return "N"
	default:
		return ""
	}
}

func autopilotName(value uint64) string {
	switch value {
	case 0:
		return "无"
	case 1:
		return "FSD"
	case 2:
		return "自动辅助转向"
	case 3:
		return "TACC"
	default:
		return ""
	}
}

func joinLines(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	result := parts[0]
	for _, part := range parts[1:] {
		result += "\n" + part
	}
	return result
}
