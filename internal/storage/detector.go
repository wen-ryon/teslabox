// Package storage 负责识别 TeslaBox 的固定数据盘，不承担格式化或挂载副作用。
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/wen-ryon/teslabox/internal/system"
)

var (
	// ErrDeviceNotFound 表示稳定标识未匹配到任何数据分区。
	ErrDeviceNotFound = errors.New("未发现 TeslaBox 数据盘")
	// ErrAmbiguousDevice 表示稳定标识匹配到多个分区，继续运行可能选错磁盘。
	ErrAmbiguousDevice = errors.New("TeslaBox 数据盘标识不唯一")
)

// Device 是只读发现得到的数据盘分区信息。
type Device struct {
	Path        string   `json:"path"`
	UUID        string   `json:"uuid"`
	Label       string   `json:"label"`
	FSType      string   `json:"fs_type"`
	MountPoints []string `json:"mount_points"`
	Removable   bool     `json:"removable"`
	Transport   string   `json:"transport"`
	SizeBytes   int64    `json:"size_bytes"`
	Model       string   `json:"model,omitempty"`
	Serial      string   `json:"serial,omitempty"`
}

// StableID 返回用于日志和状态 API 的稳定标识，UUID 的可信度高于卷标。
func (d Device) StableID() string {
	if d.UUID != "" {
		return "uuid:" + d.UUID
	}
	return "label:" + d.Label
}

// Detector 隔离 lsblk，业务状态机只依赖稳定的数据盘模型。
type Detector interface {
	Detect(ctx context.Context) (Device, error)
}

// LSBLKDetector 通过 util-linux JSON 输出按 UUID 或卷标发现数据分区。
type LSBLKDetector struct {
	runner system.Runner
	uuid   string
	label  string
	source IdentitySource
}

// NewRegisteredLSBLKDetector 优先使用系统盘登记册中的活动 UUID，并保留旧配置回退。
func NewRegisteredLSBLKDetector(runner system.Runner, source IdentitySource, uuid, label string) *LSBLKDetector {
	detector := NewLSBLKDetector(runner, uuid, label)
	detector.source = source
	return detector
}

type lsblkOutput struct {
	BlockDevices []lsblkDevice `json:"blockdevices"`
}

type lsblkDevice struct {
	Path       string        `json:"path"`
	UUID       string        `json:"uuid"`
	Label      string        `json:"label"`
	FSType     string        `json:"fstype"`
	Type       string        `json:"type"`
	MountPoint any           `json:"mountpoint"`
	Removable  any           `json:"rm"`
	Transport  string        `json:"tran"`
	Size       any           `json:"size"`
	Model      string        `json:"model"`
	Serial     string        `json:"serial"`
	Children   []lsblkDevice `json:"children"`
}

type inheritedFields struct {
	removable any
	transport string
	model     string
	serial    string
}

// NewLSBLKDetector 创建只读发现器；配置 UUID 后不会回退到卷标，避免误选另一块盘。
func NewLSBLKDetector(runner system.Runner, uuid, label string) *LSBLKDetector {
	return &LSBLKDetector{
		runner: runner,
		uuid:   strings.TrimSpace(uuid),
		label:  strings.TrimSpace(label),
	}
}

// Detect 返回唯一匹配的数据分区，重复卷标会失败而不是猜测设备。
func (d *LSBLKDetector) Detect(ctx context.Context) (Device, error) {
	uuid, label := d.uuid, d.label
	if d.source != nil {
		record, exists, err := d.source.Active()
		if err != nil {
			return Device{}, fmt.Errorf("读取活动数据盘登记: %w", err)
		}
		if exists {
			uuid, label = record.UUID, record.Label
		} else {
			uuid, label = "", ""
		}
	}
	output, err := d.runner.Run(
		ctx,
		"lsblk",
		"--json",
		"--tree",
		"--bytes",
		"--output",
		"PATH,UUID,LABEL,FSTYPE,TYPE,MOUNTPOINT,RM,TRAN,SIZE,MODEL,SERIAL",
	)
	if err != nil {
		return Device{}, fmt.Errorf("读取块设备: %w", err)
	}

	decoder := json.NewDecoder(strings.NewReader(string(output)))
	decoder.UseNumber()
	var payload lsblkOutput
	if err := decoder.Decode(&payload); err != nil {
		return Device{}, fmt.Errorf("解析 lsblk 输出: %w", err)
	}

	matches := make([]Device, 0, 1)
	for _, block := range payload.BlockDevices {
		d.collect(block, inheritedFields{}, uuid, label, &matches)
	}
	if len(matches) == 0 {
		return Device{}, ErrDeviceNotFound
	}
	if len(matches) > 1 {
		return Device{}, fmt.Errorf("%w: 匹配到 %d 个分区", ErrAmbiguousDevice, len(matches))
	}
	return matches[0], nil
}

func (d *LSBLKDetector) collect(block lsblkDevice, parent inheritedFields, uuid, label string, matches *[]Device) {
	fields := inheritedFields{
		removable: firstNonNil(block.Removable, parent.removable),
		transport: firstNonEmpty(block.Transport, parent.transport),
		model:     firstNonEmpty(block.Model, parent.model),
		serial:    firstNonEmpty(block.Serial, parent.serial),
	}
	if block.Type == "part" && matchesIdentity(block, uuid, label) {
		*matches = append(*matches, Device{
			Path:        block.Path,
			UUID:        block.UUID,
			Label:       block.Label,
			FSType:      block.FSType,
			MountPoints: normalizeMountPoint(block.MountPoint),
			Removable:   parseBoolish(fields.removable),
			Transport:   fields.transport,
			SizeBytes:   parseInt64(block.Size),
			Model:       strings.TrimSpace(fields.model),
			Serial:      strings.TrimSpace(fields.serial),
		})
	}
	for _, child := range block.Children {
		d.collect(child, fields, uuid, label, matches)
	}
}

func matchesIdentity(block lsblkDevice, uuid, label string) bool {
	if uuid != "" {
		return strings.EqualFold(block.UUID, uuid)
	}
	return label != "" && strings.EqualFold(block.Label, label)
}

func normalizeMountPoint(value any) []string {
	mountPoint, ok := value.(string)
	if !ok || strings.TrimSpace(mountPoint) == "" {
		return nil
	}
	return []string{strings.TrimSpace(mountPoint)}
}

func parseBoolish(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case json.Number:
		return typed.String() == "1"
	case string:
		return typed == "1" || strings.EqualFold(typed, "true")
	default:
		return false
	}
}

func parseInt64(value any) int64 {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Int64()
		if err == nil {
			return parsed
		}
	case string:
		parsed, err := strconv.ParseInt(typed, 10, 64)
		if err == nil {
			return parsed
		}
	}
	return 0
}

func firstNonNil(value, fallback any) any {
	if value != nil {
		return value
	}
	return fallback
}

func firstNonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}
