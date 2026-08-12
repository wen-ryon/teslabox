// Package storagebench 在新建的 XFS 数据盘上模拟 TeslaCam 六路分钟文件落盘。
package storagebench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const (
	ProfileStable  = "stable"
	ProfileLimited = "limited"
	ProfileUnsafe  = "unsafe"
)

// Config 控制初始化体检的数据量；默认约写入 1.7 GiB，足以观察短时缓存耗尽后的波动。
type Config struct {
	CameraCount  int
	BurstCount   int
	BytesPerFile int64
	ChunkBytes   int
	StableMBps   float64
	LimitedMBps  float64
	StableStall  time.Duration
	LimitedStall time.Duration
	StableFsync  time.Duration
	LimitedFsync time.Duration
}

// DefaultConfig 返回与六路 TeslaCam 分钟写入接近的有界测试参数。
func DefaultConfig() Config {
	return Config{
		CameraCount: 6, BurstCount: 6, BytesPerFile: 48 * 1024 * 1024, ChunkBytes: 1024 * 1024,
		StableMBps: 30, LimitedMBps: 12, StableStall: time.Second, LimitedStall: 5 * time.Second,
		StableFsync: 2 * time.Second, LimitedFsync: 8 * time.Second,
	}
}

// Burst 记录一次六路分钟文件落盘的实际速度和同步尾延迟。
type Burst struct {
	Index          int     `json:"index"`
	Bytes          int64   `json:"bytes"`
	DurationMS     int64   `json:"duration_ms"`
	ThroughputMBps float64 `json:"throughput_mbps"`
	FsyncMaxMS     int64   `json:"fsync_max_ms"`
}

// Report 是初始化体检的可持久化结果，不包含自动修改系统参数的指令。
type Report struct {
	DeviceModel         string    `json:"device_model,omitempty"`
	DeviceSerial        string    `json:"device_serial,omitempty"`
	DeviceSizeBytes     int64     `json:"device_size_bytes,omitempty"`
	CompletedAt         time.Time `json:"completed_at"`
	BytesWritten        int64     `json:"bytes_written"`
	DurationMS          int64     `json:"duration_ms"`
	AverageMBps         float64   `json:"average_mbps"`
	MinimumBurstMBps    float64   `json:"minimum_burst_mbps"`
	MaximumWriteStallMS int64     `json:"maximum_write_stall_ms"`
	FsyncP95MS          int64     `json:"fsync_p95_ms"`
	FsyncMaxMS          int64     `json:"fsync_max_ms"`
	Profile             string    `json:"profile"`
	Reason              string    `json:"reason"`
	Bursts              []Burst   `json:"bursts"`
}

// Benchmark 使用普通文件和 fsync 走与 backing image 相同的 XFS/USB 路径。
type Benchmark struct {
	config Config
	now    func() time.Time
}

// New 创建有界测试器；无效参数会在 Run 前失败，不会创建临时文件。
func New(config Config) *Benchmark {
	return &Benchmark{config: config, now: time.Now}
}

// Run 在目标根目录创建可信临时目录，所有结果同步完成后删除测试文件。
func (b *Benchmark) Run(ctx context.Context, root string) (report Report, resultErr error) {
	if err := validateConfig(b.config); err != nil {
		return Report{}, err
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return Report{}, fmt.Errorf("检查性能体检目录: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return Report{}, errors.New("性能体检目录不是可信目录")
	}
	temporaryRoot, err := os.MkdirTemp(root, ".teslabox-benchmark-")
	if err != nil {
		return Report{}, fmt.Errorf("创建性能体检临时目录: %w", err)
	}
	defer func() {
		if removeErr := os.RemoveAll(temporaryRoot); removeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("清理性能体检临时文件: %w", removeErr))
		}
	}()

	buffer := make([]byte, b.config.ChunkBytes)
	for index := range buffer {
		buffer[index] = byte(index*31 + 17)
	}
	startedAt := b.now()
	report = Report{Bursts: make([]Burst, 0, b.config.BurstCount)}
	fsyncSamples := make([]time.Duration, 0, b.config.CameraCount*b.config.BurstCount)
	var maximumWriteStall time.Duration
	for burstIndex := 0; burstIndex < b.config.BurstCount; burstIndex++ {
		burst, stalls, fsyncs, err := b.runBurst(ctx, temporaryRoot, burstIndex+1, buffer)
		if err != nil {
			return Report{}, err
		}
		report.Bursts = append(report.Bursts, burst)
		for _, stall := range stalls {
			maximumWriteStall = max(maximumWriteStall, stall)
		}
		fsyncSamples = append(fsyncSamples, fsyncs...)
		report.BytesWritten += burst.Bytes
	}
	duration := b.now().Sub(startedAt)
	if duration <= 0 {
		duration = time.Nanosecond
	}
	report.CompletedAt = b.now()
	report.DurationMS = duration.Milliseconds()
	report.AverageMBps = bytesPerSecond(report.BytesWritten, duration) / 1_000_000
	report.MinimumBurstMBps = report.Bursts[0].ThroughputMBps
	for _, burst := range report.Bursts[1:] {
		report.MinimumBurstMBps = min(report.MinimumBurstMBps, burst.ThroughputMBps)
	}
	report.MaximumWriteStallMS = maximumWriteStall.Milliseconds()
	slices.Sort(fsyncSamples)
	if len(fsyncSamples) > 0 {
		report.FsyncMaxMS = fsyncSamples[len(fsyncSamples)-1].Milliseconds()
		index := (len(fsyncSamples)*95 + 99) / 100
		report.FsyncP95MS = fsyncSamples[max(0, index-1)].Milliseconds()
	}
	classify(&report, b.config)
	return report, nil
}

func (b *Benchmark) runBurst(ctx context.Context, root string, index int, buffer []byte) (Burst, []time.Duration, []time.Duration, error) {
	files := make([]*os.File, 0, b.config.CameraCount)
	for camera := 0; camera < b.config.CameraCount; camera++ {
		name := filepath.Join(root, fmt.Sprintf("burst-%02d-camera-%02d.bin", index, camera+1))
		file, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			closeFiles(files)
			return Burst{}, nil, nil, fmt.Errorf("创建性能体检文件: %w", err)
		}
		files = append(files, file)
	}
	defer closeFiles(files)
	startedAt := b.now()
	remaining := b.config.BytesPerFile
	stalls := make([]time.Duration, 0)
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return Burst{}, nil, nil, err
		}
		writeBytes := min(int64(len(buffer)), remaining)
		for _, file := range files {
			writeStarted := b.now()
			if _, err := file.Write(buffer[:writeBytes]); err != nil {
				return Burst{}, nil, nil, fmt.Errorf("写入性能体检文件: %w", err)
			}
			stalls = append(stalls, b.now().Sub(writeStarted))
		}
		remaining -= writeBytes
	}
	fsyncs := make([]time.Duration, 0, len(files))
	var fsyncMaximum time.Duration
	for _, file := range files {
		syncStarted := b.now()
		if err := file.Sync(); err != nil {
			return Burst{}, nil, nil, fmt.Errorf("同步性能体检文件: %w", err)
		}
		elapsed := b.now().Sub(syncStarted)
		fsyncs = append(fsyncs, elapsed)
		fsyncMaximum = max(fsyncMaximum, elapsed)
	}
	duration := b.now().Sub(startedAt)
	bytes := int64(b.config.CameraCount) * b.config.BytesPerFile
	return Burst{Index: index, Bytes: bytes, DurationMS: duration.Milliseconds(), ThroughputMBps: bytesPerSecond(bytes, duration) / 1_000_000, FsyncMaxMS: fsyncMaximum.Milliseconds()}, stalls, fsyncs, nil
}

func closeFiles(files []*os.File) {
	for _, file := range files {
		_ = file.Close()
	}
}

func bytesPerSecond(bytes int64, duration time.Duration) float64 {
	if duration <= 0 {
		return 0
	}
	return float64(bytes) / duration.Seconds()
}

func validateConfig(config Config) error {
	if config.CameraCount <= 0 || config.BurstCount <= 0 || config.BytesPerFile <= 0 || config.ChunkBytes <= 0 || int64(config.ChunkBytes) > config.BytesPerFile {
		return errors.New("存储性能体检参数无效")
	}
	return nil
}

func classify(report *Report, config Config) {
	stall := time.Duration(report.MaximumWriteStallMS) * time.Millisecond
	fsync := time.Duration(report.FsyncP95MS) * time.Millisecond
	switch {
	case report.MinimumBurstMBps >= config.StableMBps && stall <= config.StableStall && fsync <= config.StableFsync:
		report.Profile = ProfileStable
		report.Reason = "持续写入和同步尾延迟满足六路录像稳定档"
	case report.MinimumBurstMBps >= config.LimitedMBps && stall <= config.LimitedStall && fsync <= config.LimitedFsync:
		report.Profile = ProfileLimited
		report.Reason = "可用于录像，但写入波动较大；建议避免同时导出或整理"
	default:
		report.Profile = ProfileUnsafe
		report.Reason = "最低突发速度或同步尾延迟不足，不建议作为持续六路录像数据盘"
	}
}
