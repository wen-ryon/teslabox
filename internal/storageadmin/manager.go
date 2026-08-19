// Package storageadmin 提供外接数据盘发现、显式初始化和设备登记能力。
package storageadmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/wen-ryon/teslabox/internal/driveinit"
	"github.com/wen-ryon/teslabox/internal/exfatreserve"
	"github.com/wen-ryon/teslabox/internal/fragmentation"
	"github.com/wen-ryon/teslabox/internal/imageinit"
	"github.com/wen-ryon/teslabox/internal/service"
	"github.com/wen-ryon/teslabox/internal/storage"
	"github.com/wen-ryon/teslabox/internal/storagebench"
	"github.com/wen-ryon/teslabox/internal/storagepolicy"
	"github.com/wen-ryon/teslabox/internal/system"
)

const (
	minimumDeviceSize          = 32 * 1000 * 1000 * 1000
	minimumReservedBytes       = 10 * 1000 * 1000 * 1000
	maximumSupportedDeviceSize = 2 * 1000 * 1000 * 1000 * 1000
	reservedCapacityDivisor    = 20
	defaultTeslaCamPercent     = 80
	minimumDefragmentFree      = 8 * 1024 * 1024 * 1024
	minimumDefragmentPct       = 5
	capacityLayoutTolerance    = 8 * 1024 * 1024
)

var (
	// ErrUnsafeState 表示当前 Gadget 或数据盘仍在使用，不能执行破坏性初始化。
	ErrUnsafeState = errors.New("当前状态不允许初始化存储设备")
)

// InputError 表示可以直接返回给管理页面的安全参数错误。
type InputError struct{ Message string }

// Error 返回面向用户的初始化校验说明。
func (e *InputError) Error() string { return e.Message }

// Candidate 是设置页面展示的可用数据整盘状态。
type Candidate struct {
	StablePath            string `json:"stable_path"`
	DevicePath            string `json:"device_path"`
	Model                 string `json:"model,omitempty"`
	Serial                string `json:"serial,omitempty"`
	SizeBytes             int64  `json:"size_bytes"`
	Transport             string `json:"transport"`
	State                 string `json:"state"`
	Initialized           bool   `json:"initialized"`
	Active                bool   `json:"active"`
	CanInitialize         bool   `json:"can_initialize"`
	CanReinitialize       bool   `json:"can_reinitialize"`
	CanResume             bool   `json:"can_resume"`
	CanCancel             bool   `json:"can_cancel"`
	PlannedImageSizeBytes int64  `json:"planned_image_size_bytes,omitempty"`
	PlannedWritableBytes  int64  `json:"planned_writable_bytes,omitempty"`
	Reason                string `json:"reason,omitempty"`
	LastError             string `json:"last_error,omitempty"`
}

// Status 是设备登记册和当前可初始化整盘的一致性视图。
type Status struct {
	ActiveUUID     string               `json:"active_uuid,omitempty"`
	Devices        []Candidate          `json:"devices"`
	Benchmark      *storagebench.Report `json:"benchmark,omitempty"`
	BenchmarkError string               `json:"benchmark_error,omitempty"`
}

// InitializeRequest 要求序列号和擦除确认同时匹配，避免误点直接清盘。
type InitializeRequest struct {
	StablePath             string `json:"stable_path"`
	ConfirmSerial          string `json:"confirm_serial"`
	ConfirmErase           bool   `json:"confirm_erase"`
	ConfirmOTGDisconnected bool   `json:"confirm_otg_disconnected"`
	Reinitialize           bool   `json:"reinitialize"`
	MusicEnabled           bool   `json:"music_enabled"`
	TeslaCamPercent        int    `json:"teslacam_percent"`
}

// CancelRequest 用序列号确认放弃未完成初始化并恢复原活动设备。
type CancelRequest struct {
	StablePath    string `json:"stable_path"`
	ConfirmSerial string `json:"confirm_serial"`
}

// DefragmentRequest 要求用户确认 OTG 已物理断开，避免把软件解绑误当成电气隔离。
type DefragmentRequest struct {
	ConfirmOTGDisconnected bool `json:"confirm_otg_disconnected"`
}

// DefragmentResult 保留整理前后证据和只读文件系统校验结果。
type DefragmentResult struct {
	Before               fragmentation.Report `json:"before"`
	After                fragmentation.Report `json:"after"`
	ImageSizeBytes       int64                `json:"image_size_bytes"`
	FileSystemChecked    bool                 `json:"file_system_checked"`
	COWExtentHintApplied bool                 `json:"cow_extent_hint_applied"`
	Changed              bool                 `json:"changed"`
	RequiresColdBoot     bool                 `json:"requires_cold_boot"`
}

// CapacityMigrationRequest 保护已有镜像的整盘容量迁移，分区和文件系统本身不会扩容。
type CapacityMigrationRequest struct {
	ConfirmOTGDisconnected bool `json:"confirm_otg_disconnected"`
}

// CapacityMigrationResult 明确区分旧整盘容量、新整盘容量和保持不变的可写分区容量。
type CapacityMigrationResult struct {
	PreviousAdvertisedBytes int64 `json:"previous_advertised_bytes"`
	AdvertisedBytes         int64 `json:"advertised_bytes"`
	WritableBytes           int64 `json:"writable_bytes"`
	Changed                 bool  `json:"changed"`
	FileSystemChecked       bool  `json:"file_system_checked"`
	RequiresColdBoot        bool  `json:"requires_cold_boot"`
}

// Controller 提供破坏性操作前的运行状态和完成后的即时收敛入口。
type Controller interface {
	Snapshot() service.Snapshot
	TriggerRefresh()
	BeginStorageMaintenance(context.Context) (func(), error)
	BeginMediaMaintenance(context.Context) (func(), error)
}

// DeviceLister 只列出有稳定 by-id 路径且不是系统盘的整盘。
type DeviceLister interface {
	List(context.Context) ([]driveinit.Device, error)
}

// DriveInitializer 执行经过序列号确认的数据盘分区和 XFS 初始化。
type DriveInitializer interface {
	Run(context.Context, driveinit.Options) (driveinit.Result, error)
}

// ImageInitializer 创建 Tesla 可识别的 exFAT 虚拟磁盘。
type ImageInitializer interface {
	Run(context.Context, imageinit.Options) (imageinit.Result, error)
}

// StorageBenchmark 只在新建空数据盘上运行受控临时文件测试。
type StorageBenchmark interface {
	Run(context.Context, string) (storagebench.Report, error)
}

// Config 保存初始化布局，不允许从请求体传入系统路径。
type Config struct {
	DataRoot   string
	ImagePath  string
	DataLabel  string
	ImageLabel string
	Policy     storagepolicy.SnapshotReservePolicy
}

type fileSystemSpace struct {
	TotalBytes uint64
	FreeBytes  uint64
}

// Manager 串行化存储初始化，避免两个浏览器同时操作同一块盘。
type Manager struct {
	config        Config
	runner        system.Runner
	registry      *storage.FileRegistry
	lister        DeviceLister
	driveInit     DriveInitializer
	imageInit     ImageInitializer
	controller    Controller
	fileSize      func(string) (int64, error)
	verifyReserve func(string) (exfatreserve.Status, error)
	resizeReserve func(string, int64) (exfatreserve.Status, error)
	benchmark     StorageBenchmark
	mu            sync.Mutex
}

// New 创建显式依赖的存储管理器，构造过程不会访问或修改块设备。
func New(config Config, runner system.Runner, registry *storage.FileRegistry, lister DeviceLister, driveInitializer DriveInitializer, imageInitializer ImageInitializer, controller Controller, benchmarks ...StorageBenchmark) *Manager {
	if config.Policy.MinFreeBytes() == 0 {
		config.Policy = storagepolicy.DefaultSnapshotReservePolicy()
	}
	manager := &Manager{
		config: config, runner: runner, registry: registry, lister: lister,
		driveInit: driveInitializer, imageInit: imageInitializer, controller: controller,
		fileSize: regularFileSize, verifyReserve: exfatreserve.Verify, resizeReserve: exfatreserve.Resize,
	}
	if len(benchmarks) == 1 {
		manager.benchmark = benchmarks[0]
	}
	return manager
}

// Status 读取登记册并分类当前可用的数据盘，不执行挂载或格式化。
func (m *Manager) Status(ctx context.Context) (Status, error) {
	devices, err := m.lister.List(ctx)
	if err != nil {
		return Status{}, err
	}
	records, err := m.registry.Records()
	if err != nil {
		return Status{}, err
	}
	active, hasActive, err := m.registry.Active()
	if err != nil {
		return Status{}, err
	}
	status := Status{Devices: make([]Candidate, 0, len(devices))}
	if hasActive {
		status.ActiveUUID = active.UUID
	}
	benchmark, benchmarkErr := readBenchmarkReport(filepath.Join(m.config.DataRoot, "state", "storage-benchmark.json"))
	if benchmarkErr != nil {
		status.BenchmarkError = benchmarkErr.Error()
	} else {
		status.Benchmark = benchmark
	}
	for _, device := range devices {
		status.Devices = append(status.Devices, classify(device, records, active, hasActive, m.config.DataRoot, m.config.DataLabel, m.config.Policy))
	}
	for _, record := range records {
		if record.Status == "ready" || recordAttached(record, devices) {
			continue
		}
		status.Devices = append(status.Devices, Candidate{
			StablePath: record.StablePath, Model: record.Model, Serial: record.Serial,
			SizeBytes: record.SizeBytes, State: record.Status,
			CanCancel: true, LastError: record.LastError,
			Reason: "初始化设备当前未插入；可以重新插入后继续，或取消任务以恢复原存储设备",
		})
	}
	return status, nil
}

// Fragmentation 只读取活动镜像的 XFS extent，不会暂停 Gadget 或触发自动整理。
func (m *Manager) Fragmentation(ctx context.Context) (fragmentation.Report, error) {
	state := m.controller.Snapshot()
	if !state.Backing.Ready {
		return fragmentation.Report{}, &InputError{Message: "TeslaCam 镜像尚未就绪"}
	}
	return fragmentation.New(m.runner).Inspect(ctx, m.config.ImagePath)
}

// Defragment 在 Gadget 和媒体租约全部释放后在线整理外层 XFS 文件，结束后保持 Gadget 暂停等待冷启动。
func (m *Manager) Defragment(ctx context.Context, request DefragmentRequest) (result DefragmentResult, resultErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.controller.Snapshot()
	if !state.Backing.Ready {
		return DefragmentResult{}, &InputError{Message: "TeslaCam 镜像尚未就绪"}
	}
	inspector := fragmentation.New(m.runner)
	before, err := inspector.Inspect(ctx, m.config.ImagePath)
	if err != nil {
		return DefragmentResult{}, err
	}
	info, err := os.Stat(m.config.ImagePath)
	if err != nil {
		return DefragmentResult{}, fmt.Errorf("读取整理前镜像大小: %w", err)
	}
	result = DefragmentResult{Before: before, After: before, ImageSizeBytes: info.Size(), RequiresColdBoot: true}
	writableBytes, err := m.readPartitionEnd(ctx)
	if err != nil {
		return result, err
	}
	if writableBytes > result.ImageSizeBytes {
		return result, errors.New("现有分区边界超出镜像容量，拒绝整理")
	}
	allocationNeeded := max(int64(0), writableBytes-before.AllocatedBytes)
	needsDefragment := before.Severity != fragmentation.SeverityHealthy
	if !needsDefragment && allocationNeeded == 0 {
		// 健康镜像不进入维护状态，避免用户误点整理后无意义地断开 Gadget。
		result.RequiresColdBoot = false
		return result, nil
	}
	if !request.ConfirmOTGDisconnected {
		return result, &InputError{Message: "必须先物理拔掉车机或电脑 OTG 线"}
	}
	// COW 分配提示是元数据级保护，不需要复制镜像。必须在完整整理的空间预检前完成，
	// 否则旧镜像会因为无法容纳 xfs_fsr 临时副本而永远得不到防碎片迁移。
	if _, err := m.runner.Run(ctx, "/usr/sbin/xfs_io", "-c", "cowextsize 1m", m.config.ImagePath); err != nil {
		return result, fmt.Errorf("设置镜像 COW extent 保护: %w", err)
	}
	result.COWExtentHintApplied = true
	copyBytes := int64(0)
	if needsDefragment {
		// xfs_fsr 会在同一 XFS 上创建临时副本；预检必须覆盖预分配增量、完整副本和安全余量。
		copyBytes = before.AllocatedBytes + allocationNeeded
	}
	if needsDefragment || allocationNeeded > 0 {
		if err := m.requireDefragmentSpace(ctx, allocationNeeded, copyBytes); err != nil {
			return result, &InputError{Message: err.Error() + "；已启用后续 COW extent 保护，现有 extent 暂不合并也可以继续使用，无需仅因此重新初始化存储设备"}
		}
	}
	endMaintenance, err := m.controller.BeginMediaMaintenance(ctx)
	if err != nil {
		return DefragmentResult{}, errors.Join(ErrUnsafeState, err)
	}
	defer endMaintenance()

	// 进入维护状态后复核一次，避免预检后被其他任务占用空间。
	if err := m.requireDefragmentSpace(ctx, allocationNeeded, copyBytes); err != nil {
		return result, fmt.Errorf("整理开始前空间复核失败: %w", err)
	}
	if _, err := m.runner.Run(ctx, "sync"); err != nil {
		return result, fmt.Errorf("整理前同步数据盘: %w", err)
	}
	if _, err := m.runner.Run(ctx, "fallocate", "--keep-size", "--length", strconv.FormatInt(writableBytes, 10), "--", m.config.ImagePath); err != nil {
		return result, fmt.Errorf("预分配现有镜像可写区域: %w", err)
	}
	if needsDefragment {
		if _, err := m.runner.Run(ctx, "/usr/sbin/xfs_fsr", "-v", m.config.ImagePath); err != nil {
			return result, fmt.Errorf("整理 TeslaCam 镜像: %w", err)
		}
	}
	if _, err := m.runner.Run(ctx, "sync"); err != nil {
		return result, fmt.Errorf("整理后同步数据盘: %w", err)
	}
	afterInfo, err := os.Stat(m.config.ImagePath)
	if err != nil {
		return result, fmt.Errorf("读取整理后镜像大小: %w", err)
	}
	if afterInfo.Size() != result.ImageSizeBytes {
		return result, fmt.Errorf("整理后镜像大小发生变化：%d -> %d", result.ImageSizeBytes, afterInfo.Size())
	}
	after, err := inspector.Inspect(ctx, m.config.ImagePath)
	if err != nil {
		return result, err
	}
	result.After = after
	result.Changed = after.ExtentCount < before.ExtentCount || after.AllocatedBytes > before.AllocatedBytes
	if needsDefragment && after.ExtentCount >= before.ExtentCount {
		return result, errors.New("XFS 整理未减少镜像 extent，已停止且不会自动重试")
	}
	if err := m.verifyImageReadOnly(ctx); err != nil {
		return result, err
	}
	result.FileSystemChecked = true
	return result, nil
}

// MigrateAdvertisedCapacity 检查现有卷是否具备完整容量和安全保留区；旧卷不能无损原地扩容。
func (m *Manager) MigrateAdvertisedCapacity(ctx context.Context, request CapacityMigrationRequest) (CapacityMigrationResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !request.ConfirmOTGDisconnected {
		return CapacityMigrationResult{}, &InputError{Message: "必须先物理拔掉车机或电脑 OTG 线"}
	}
	status, err := m.Status(ctx)
	if err != nil {
		return CapacityMigrationResult{}, err
	}
	var active Candidate
	for _, candidate := range status.Devices {
		if candidate.Active {
			active = candidate
			break
		}
	}
	if !active.Active {
		return CapacityMigrationResult{}, &InputError{Message: "未找到当前活动数据盘"}
	}
	advertisedBytes, err := chooseAdvertisedSize(active.SizeBytes)
	if err != nil {
		return CapacityMigrationResult{}, err
	}
	endMaintenance, err := m.controller.BeginMediaMaintenance(ctx)
	if err != nil {
		return CapacityMigrationResult{}, errors.Join(ErrUnsafeState, err)
	}
	defer endMaintenance()
	previousSize, err := m.fileSize(m.config.ImagePath)
	if err != nil {
		return CapacityMigrationResult{}, fmt.Errorf("读取现有镜像容量: %w", err)
	}
	partitionEnd, err := m.readPartitionEnd(ctx)
	if err != nil {
		return CapacityMigrationResult{}, err
	}
	result := CapacityMigrationResult{
		PreviousAdvertisedBytes: previousSize, AdvertisedBytes: advertisedBytes,
		WritableBytes: partitionEnd, RequiresColdBoot: true,
	}
	if previousSize > advertisedBytes {
		return result, errors.New("现有镜像已经大于物理设备容量，拒绝缩小或覆盖")
	}
	if partitionEnd > previousSize || partitionEnd > advertisedBytes {
		return result, errors.New("现有分区边界超出镜像或物理设备容量，拒绝迁移")
	}
	if advertisedBytes-partitionEnd > capacityLayoutTolerance {
		return result, &InputError{Message: "现有 TeslaCam 分区仍是旧式安全容量，exFAT 不支持无损原地扩容；请先备份录像，再重新初始化存储设备"}
	}
	reserve, err := m.verifyReserve(m.config.ImagePath)
	if err != nil {
		return result, &InputError{Message: "完整 TeslaCam 卷的安全容量保留区无效；请先备份录像，再重新初始化存储设备"}
	}
	if reserve.Legacy || reserve.ReservedBytes >= reserve.VolumeBytes {
		return result, &InputError{Message: "现有 TeslaCam 卷不是完整容量保护布局；请先备份录像，再重新初始化存储设备"}
	}
	currentWritable := int64(reserve.VolumeBytes - reserve.ReservedBytes)
	result.WritableBytes = currentWritable
	targetWritable, err := chooseImageSize(active.SizeBytes)
	if err != nil {
		return result, err
	}
	if targetWritable <= currentWritable+capacityLayoutTolerance {
		result.RequiresColdBoot = false
		if err := m.verifyImageReadOnly(ctx); err != nil {
			return result, err
		}
		result.FileSystemChecked = true
		return result, nil
	}
	expansionBytes := targetWritable - currentWritable
	if err := m.requireDefragmentSpace(ctx, expansionBytes, 0); err != nil {
		return result, &InputError{Message: strings.Replace(err.Error(), "安全整理", "安全扩容", 1)}
	}
	if _, err := m.runner.Run(ctx, "fallocate", "--keep-size", "--length", strconv.FormatInt(targetWritable, 10), "--", m.config.ImagePath); err != nil {
		return result, fmt.Errorf("预分配新增 TeslaCam 可写空间: %w", err)
	}
	if _, err := m.runner.Run(ctx, "sync"); err != nil {
		return result, fmt.Errorf("同步新增 TeslaCam 可写空间: %w", err)
	}
	resized, err := m.resizeReserve(m.config.ImagePath, targetWritable)
	if err != nil {
		return result, fmt.Errorf("缩小 TeslaCam 安全保留区: %w", err)
	}
	result.WritableBytes = int64(resized.VolumeBytes - resized.ReservedBytes)
	result.Changed = result.WritableBytes > currentWritable
	if err := m.verifyImageReadOnly(ctx); err != nil {
		return result, err
	}
	result.FileSystemChecked = true
	return result, nil
}

func regularFileSize(name string) (int64, error) {
	info, err := os.Stat(name)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (m *Manager) readPartitionEnd(ctx context.Context) (int64, error) {
	output, err := m.runner.Run(ctx, "parted", "--machine", "--script", m.config.ImagePath, "unit", "B", "print")
	if err != nil {
		return 0, fmt.Errorf("读取现有镜像分区边界: %w", err)
	}
	var maximum int64
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ":")
		if len(fields) < 3 {
			continue
		}
		if _, err := strconv.Atoi(fields[0]); err != nil {
			continue
		}
		end, err := strconv.ParseInt(strings.TrimSuffix(fields[2], "B"), 10, 64)
		if err != nil || end <= 0 {
			return 0, fmt.Errorf("解析镜像分区边界失败: %q", strings.TrimSpace(line))
		}
		maximum = max(maximum, end)
	}
	if maximum == 0 {
		return 0, errors.New("镜像没有可识别的分区边界")
	}
	return maximum, nil
}

func (m *Manager) requireDefragmentSpace(ctx context.Context, allocationNeeded, copyBytes int64) error {
	output, err := m.runner.Run(ctx, "stat", "--file-system", "--format=%S:%b:%a", m.config.DataRoot)
	if err != nil {
		return fmt.Errorf("读取整理可用空间: %w", err)
	}
	parts := strings.Split(strings.TrimSpace(string(output)), ":")
	if len(parts) != 3 {
		return fmt.Errorf("解析整理可用空间: %q", strings.TrimSpace(string(output)))
	}
	values := make([]uint64, 3)
	for index, part := range parts {
		value, parseErr := strconv.ParseUint(part, 10, 64)
		if parseErr != nil || value == 0 {
			return fmt.Errorf("解析整理空间数值 %q", part)
		}
		values[index] = value
	}
	total := values[0] * values[1]
	free := values[0] * values[2]
	required := uint64(minimumDefragmentFree)
	if allocationNeeded > 0 {
		required += uint64(allocationNeeded)
	}
	if copyBytes > 0 {
		required += uint64(copyBytes)
	}
	if free < required || total == 0 || free*100/total < minimumDefragmentPct {
		return fmt.Errorf("数据盘可用空间不足以安全整理：可用 %d 字节，至少需要 %d 字节且不低于 %d%%", free, required, minimumDefragmentPct)
	}
	return nil
}

func (m *Manager) verifyImageReadOnly(ctx context.Context) (resultErr error) {
	if _, err := m.runner.Run(ctx, "parted", "--script", m.config.ImagePath, "print"); err != nil {
		return fmt.Errorf("核对整理后分区表: %w", err)
	}
	output, err := m.runner.Run(ctx, "losetup", "--find", "--show", "--read-only", "--partscan", m.config.ImagePath)
	if err != nil {
		return fmt.Errorf("建立整理校验 loop 设备: %w", err)
	}
	loopDevice := strings.TrimSpace(string(output))
	if !strings.HasPrefix(loopDevice, "/dev/loop") || strings.ContainsAny(loopDevice, " \t\r\n") {
		return fmt.Errorf("losetup 返回了异常设备路径 %q", loopDevice)
	}
	defer func() {
		if _, detachErr := m.runner.Run(context.WithoutCancel(ctx), "losetup", "--detach", loopDevice); detachErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("解除整理校验 loop 设备: %w", detachErr))
		}
	}()
	if _, err := m.runner.Run(ctx, "udevadm", "settle"); err != nil {
		return fmt.Errorf("等待整理校验分区设备: %w", err)
	}
	if _, err := m.runner.Run(ctx, "fsck.exfat", "-n", loopDevice+"p1"); err != nil {
		return fmt.Errorf("只读检查 TeslaCam 文件系统: %w", err)
	}
	return nil
}

// Initialize 显式初始化或继续一块中断的设备，成功前不会切换活动 UUID。
func (m *Manager) Initialize(ctx context.Context, request InitializeRequest) (result Candidate, resultErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !request.ConfirmErase {
		return Candidate{}, &InputError{Message: "必须明确确认将擦除目标存储设备"}
	}
	if !request.ConfirmOTGDisconnected {
		return Candidate{}, &InputError{Message: "必须先物理拔掉车机或电脑 OTG 线"}
	}
	if !driveinit.IsWholeDiskStablePath(request.StablePath) {
		return Candidate{}, &InputError{Message: "目标必须是 /dev/disk/by-id/ 下的整盘稳定路径"}
	}
	status, err := m.Status(ctx)
	if err != nil {
		return Candidate{}, err
	}
	candidate, found := candidateByPath(status.Devices, request.StablePath)
	if !found {
		return Candidate{}, &InputError{Message: "未找到目标存储设备，请重新扫描"}
	}
	if candidate.Serial == "" || candidate.Serial != request.ConfirmSerial {
		return Candidate{}, &InputError{Message: "输入的设备序列号不匹配"}
	}
	if err := validateSupportedDeviceSize(candidate.SizeBytes); err != nil {
		return Candidate{}, &InputError{Message: err.Error()}
	}
	if candidate.Initialized && !request.Reinitialize && !candidate.CanResume {
		return Candidate{}, &InputError{Message: "目标设备已经完成初始化"}
	}
	if request.Reinitialize && !candidate.CanReinitialize {
		return Candidate{}, &InputError{Message: "当前设备无法安全重新初始化，请先停止使用并重新扫描"}
	}
	if !request.Reinitialize && !candidate.CanInitialize && !candidate.CanResume {
		return Candidate{}, &InputError{Message: candidate.Reason}
	}
	writableBytes, err := chooseImageSize(candidate.SizeBytes)
	if err != nil {
		return Candidate{}, err
	}
	advertisedBytes, err := chooseAdvertisedSize(candidate.SizeBytes)
	if err != nil {
		return Candidate{}, err
	}
	teslaCamPercent := request.TeslaCamPercent
	if teslaCamPercent == 0 {
		teslaCamPercent = defaultTeslaCamPercent
	}
	if _, err := imageinit.PlanLayout(writableBytes, request.MusicEnabled, teslaCamPercent); err != nil {
		return Candidate{}, &InputError{Message: err.Error()}
	}
	endMaintenance, err := m.controller.BeginStorageMaintenance(ctx)
	if err != nil {
		return Candidate{}, errors.Join(ErrUnsafeState, err)
	}
	defer endMaintenance()

	record := storage.DeviceRecord{
		StablePath: candidate.StablePath, Serial: candidate.Serial, Model: candidate.Model,
		SizeBytes: candidate.SizeBytes, Label: m.config.DataLabel,
	}
	if err := m.registry.Begin(record); err != nil {
		return Candidate{}, err
	}
	defer func() {
		if resultErr != nil {
			if registryErr := m.registry.Fail(record, resultErr); registryErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("记录初始化失败状态: %w", registryErr))
			}
		}
	}()

	partition := ""
	if candidate.CanResume {
		partition, resultErr = m.resumeMount(ctx, request.StablePath)
	} else {
		var driveResult driveinit.Result
		driveResult, resultErr = m.driveInit.Run(ctx, driveinit.Options{
			Device: request.StablePath, ConfirmSerial: request.ConfirmSerial,
			Label: m.config.DataLabel, MountPath: m.config.DataRoot, Execute: true,
		})
		partition = driveResult.Partition
	}
	if resultErr != nil {
		return Candidate{}, resultErr
	}

	uuidOutput, err := m.runner.Run(ctx, "blkid", "-s", "UUID", "-o", "value", partition)
	if err != nil {
		return Candidate{}, fmt.Errorf("读取初始化后数据盘 UUID: %w", err)
	}
	record.UUID = strings.TrimSpace(string(uuidOutput))
	if record.UUID == "" {
		return Candidate{}, errors.New("初始化后数据盘 UUID 为空")
	}
	if err := m.registry.Begin(record); err != nil {
		return Candidate{}, err
	}

	if err := m.ensureLayoutDirectories(); err != nil {
		return Candidate{}, err
	}
	imageExists, err := regularFileExists(m.config.ImagePath)
	if err != nil {
		return Candidate{}, err
	}
	if imageExists {
		if err := m.verifyExistingImage(ctx); err != nil {
			return Candidate{}, err
		}
	} else {
		if m.benchmark != nil {
			benchmark, benchmarkErr := m.benchmark.Run(ctx, m.config.DataRoot)
			if benchmarkErr != nil {
				return Candidate{}, fmt.Errorf("运行存储性能体检: %w", benchmarkErr)
			}
			benchmark.DeviceModel = candidate.Model
			benchmark.DeviceSerial = candidate.Serial
			benchmark.DeviceSizeBytes = candidate.SizeBytes
			if benchmarkErr := writeBenchmarkReport(filepath.Join(m.config.DataRoot, "state", "storage-benchmark.json"), benchmark); benchmarkErr != nil {
				return Candidate{}, benchmarkErr
			}
			if _, benchmarkErr := m.runner.Run(ctx, "sync"); benchmarkErr != nil {
				return Candidate{}, fmt.Errorf("同步存储性能体检清理结果: %w", benchmarkErr)
			}
		}
		writableBytes, err = m.planWritableImageSize(ctx, candidate.SizeBytes)
		if err != nil {
			return Candidate{}, err
		}
		if _, err := imageinit.PlanLayout(writableBytes, request.MusicEnabled, teslaCamPercent); err != nil {
			return Candidate{}, &InputError{Message: err.Error()}
		}
		_, err = m.imageInit.Run(ctx, imageinit.Options{
			DataRoot: m.config.DataRoot, ImagePath: m.config.ImagePath,
			SizeBytes: advertisedBytes, WritableBytes: writableBytes, Label: m.config.ImageLabel,
			MusicEnabled: request.MusicEnabled, TeslaCamPercent: teslaCamPercent, MusicLabel: "MUSIC",
			Execute: true,
		})
		if err != nil {
			return Candidate{}, fmt.Errorf("创建 TeslaCam 虚拟磁盘: %w", err)
		}
	}
	if err := m.verifySnapshotReserve(ctx); err != nil {
		return Candidate{}, err
	}
	if err := writeDeviceMarker(filepath.Join(m.config.DataRoot, "state", "device.json"), record); err != nil {
		return Candidate{}, err
	}
	if err := m.registry.Complete(record); err != nil {
		return Candidate{}, err
	}
	m.controller.TriggerRefresh()
	result = candidate
	result.Initialized = true
	result.Active = true
	result.CanInitialize = false
	result.CanReinitialize = true
	result.CanResume = false
	result.State = "ready"
	result.PlannedImageSizeBytes = advertisedBytes
	result.PlannedWritableBytes = writableBytes
	result.Reason = "初始化完成；请保持 OTG 已插入并执行温重启恢复"
	return result, nil
}

// Cancel 放弃未完成初始化；若目标仍挂载则先卸载，避免恢复旧盘时覆盖挂载点。
func (m *Manager) Cancel(ctx context.Context, request CancelRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	status, err := m.Status(ctx)
	if err != nil {
		return err
	}
	candidate, found := candidateByPath(status.Devices, request.StablePath)
	if !found || !candidate.CanCancel {
		return &InputError{Message: "未找到可放弃的初始化任务"}
	}
	if candidate.Serial == "" || candidate.Serial != request.ConfirmSerial {
		return &InputError{Message: "输入的设备序列号不匹配"}
	}
	devices, err := m.lister.List(ctx)
	if err != nil {
		return err
	}
	for _, device := range devices {
		if device.RequestedPath != request.StablePath {
			continue
		}
		for _, child := range device.Children {
			if filepath.Clean(child.MountPoint) == filepath.Clean(m.config.DataRoot) {
				if _, err := m.runner.Run(ctx, "umount", "--", m.config.DataRoot); err != nil {
					return fmt.Errorf("卸载未完成数据盘: %w", err)
				}
			}
		}
	}
	if err := m.registry.CancelPending(request.StablePath, request.ConfirmSerial); err != nil {
		return err
	}
	m.controller.TriggerRefresh()
	return nil
}

func (m *Manager) resumeMount(ctx context.Context, stablePath string) (string, error) {
	devices, err := m.lister.List(ctx)
	if err != nil {
		return "", err
	}
	for _, device := range devices {
		if device.RequestedPath != stablePath {
			continue
		}
		for _, child := range device.Children {
			if !strings.EqualFold(child.FileSystem, "xfs") || !strings.EqualFold(child.Label, m.config.DataLabel) {
				continue
			}
			if child.MountPoint == "" {
				if _, err := m.runner.Run(ctx, "mount", "-o", "noatime,nosuid,nodev", child.ResolvedPath, m.config.DataRoot); err != nil {
					return "", fmt.Errorf("挂载未完成数据盘: %w", err)
				}
			} else if filepath.Clean(child.MountPoint) != filepath.Clean(m.config.DataRoot) {
				return "", fmt.Errorf("未完成数据盘已挂载到其他位置: %s", child.MountPoint)
			}
			return child.ResolvedPath, nil
		}
	}
	return "", errors.New("没有找到可继续初始化的 XFS 数据分区")
}

func (m *Manager) ensureLayoutDirectories() error {
	paths := []string{
		filepath.Dir(m.config.ImagePath),
		filepath.Join(m.config.DataRoot, "snapshots"),
		filepath.Join(m.config.DataRoot, "index"),
		filepath.Join(m.config.DataRoot, "state"),
	}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(path, 0o750); err != nil {
				return fmt.Errorf("创建数据盘目录 %s: %w", filepath.Base(path), err)
			}
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("数据盘路径不是可信目录: %s", filepath.Base(path))
		}
	}
	return nil
}

func regularFileExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, errors.New("TeslaCam 镜像不是可信普通文件")
	}
	if info.Size() < minimumDeviceSize {
		return false, errors.New("已有 TeslaCam 镜像容量异常，拒绝自动复用")
	}
	return true, nil
}

func (m *Manager) verifyExistingImage(ctx context.Context) (resultErr error) {
	output, err := m.runner.Run(ctx, "losetup", "--find", "--show", "--partscan", m.config.ImagePath)
	if err != nil {
		return fmt.Errorf("核验已有 TeslaCam 镜像: %w", err)
	}
	loopDevice := strings.TrimSpace(string(output))
	if !strings.HasPrefix(loopDevice, "/dev/loop") || strings.ContainsAny(loopDevice, " \t\r\n") {
		return fmt.Errorf("核验镜像时获得异常 loop 设备: %q", loopDevice)
	}
	defer func() {
		if _, detachErr := m.runner.Run(context.WithoutCancel(ctx), "losetup", "--detach", loopDevice); detachErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("解除镜像核验 loop 设备: %w", detachErr))
		}
	}()
	if _, err := m.runner.Run(ctx, "udevadm", "settle"); err != nil {
		return fmt.Errorf("等待镜像分区设备: %w", err)
	}
	partition := loopDevice + "p1"
	typeOutput, err := m.runner.Run(ctx, "blkid", "-s", "TYPE", "-o", "value", partition)
	if err != nil || !strings.EqualFold(strings.TrimSpace(string(typeOutput)), "exfat") {
		return errors.New("已有 TeslaCam 镜像没有有效的 exFAT 分区")
	}
	if _, err := m.runner.Run(ctx, "fsck.exfat", "-n", partition); err != nil {
		return fmt.Errorf("只读检查已有 TeslaCam 镜像: %w", err)
	}
	return nil
}

func chooseImageSize(deviceSize int64) (int64, error) {
	if err := validateSupportedDeviceSize(deviceSize); err != nil {
		return 0, err
	}
	reserve := deviceSize / reservedCapacityDivisor
	if reserve < minimumReservedBytes {
		reserve = minimumReservedBytes
	}
	available := deviceSize - reserve
	available = available / 1_000_000_000 * 1_000_000_000
	if available < minimumDeviceSize {
		return 0, errors.New("设备容量不足以同时保存 TeslaCam 镜像和快照预留空间")
	}
	return available, nil
}

func (m *Manager) planWritableImageSize(ctx context.Context, deviceSize int64) (int64, error) {
	planned, err := chooseImageSize(deviceSize)
	if err != nil {
		return 0, err
	}
	space, err := m.readFileSystemSpace(ctx)
	if err != nil {
		return 0, fmt.Errorf("读取初始化后数据盘空间: %w", err)
	}
	evaluation, err := m.config.Policy.EvaluateInitialization(space.TotalBytes, space.FreeBytes)
	if err != nil {
		return 0, &InputError{Message: fmt.Sprintf("空间策略不可用：%v", err)}
	}
	if evaluation.ReasonCode == storagepolicy.ReasonPolicyUnreachable {
		return 0, &InputError{Message: fmt.Sprintf("policy_unreachable：数据盘总容量 %d 字节无法满足当前快照保留策略", space.TotalBytes)}
	}
	required := evaluation.RequiredBytes
	if space.FreeBytes <= required {
		return 0, fmt.Errorf("初始化后数据盘空间不足：可用 %d 字节，快照和初始化余量至少需要 %d 字节", space.FreeBytes, required)
	}
	available := space.FreeBytes - required
	if available > uint64(math.MaxInt64) {
		available = uint64(math.MaxInt64)
	}
	actual := int64(available) / 1_000_000_000 * 1_000_000_000
	if actual > planned {
		actual = planned
	}
	if actual < minimumDeviceSize {
		return 0, errors.New("设备容量不足以同时创建 TeslaCam 镜像并保留快照安全空间")
	}
	return actual, nil
}

func (m *Manager) verifySnapshotReserve(ctx context.Context) error {
	space, err := m.readFileSystemSpace(ctx)
	if err != nil {
		return fmt.Errorf("校验初始化后快照空间: %w", err)
	}
	evaluation, err := m.config.Policy.EvaluateInitialization(space.TotalBytes, space.FreeBytes)
	if err != nil {
		return fmt.Errorf("校验初始化空间策略: %w", err)
	}
	if !evaluation.Ready {
		return fmt.Errorf("初始化后快照空间不足：可用 %d 字节，至少需要 %d 字节", space.FreeBytes, evaluation.RequiredBytes)
	}
	return nil
}

func (m *Manager) readFileSystemSpace(ctx context.Context) (fileSystemSpace, error) {
	output, err := m.runner.Run(ctx, "stat", "--file-system", "--format=%S:%b:%a", m.config.DataRoot)
	if err != nil {
		return fileSystemSpace{}, err
	}
	parts := strings.Split(strings.TrimSpace(string(output)), ":")
	if len(parts) != 3 {
		return fileSystemSpace{}, fmt.Errorf("解析文件系统空间: %q", strings.TrimSpace(string(output)))
	}
	values := make([]uint64, 3)
	for index, part := range parts {
		value, parseErr := strconv.ParseUint(part, 10, 64)
		if parseErr != nil {
			return fileSystemSpace{}, fmt.Errorf("解析文件系统空间字段 %q: %w", part, parseErr)
		}
		values[index] = value
	}
	if values[0] == 0 || values[1] > math.MaxUint64/values[0] || values[2] > math.MaxUint64/values[0] {
		return fileSystemSpace{}, errors.New("文件系统空间统计超出有效范围")
	}
	return fileSystemSpace{TotalBytes: values[0] * values[1], FreeBytes: values[0] * values[2]}, nil
}

func chooseAdvertisedSize(deviceSize int64) (int64, error) {
	if err := validateSupportedDeviceSize(deviceSize); err != nil {
		return 0, err
	}
	if deviceSize < minimumDeviceSize {
		return 0, errors.New("设备容量不足以创建 TeslaCam 虚拟整盘")
	}
	return deviceSize / 512 * 512, nil
}

func validateSupportedDeviceSize(deviceSize int64) error {
	if deviceSize > maximumSupportedDeviceSize {
		return errors.New("设备容量超过 2TB，当前版本不支持大于 2TB 的存储设备")
	}
	return nil
}

func classify(device driveinit.Device, records []storage.DeviceRecord, active storage.DeviceRecord, hasActive bool, dataRoot, dataLabel string, policy storagepolicy.SnapshotReservePolicy) Candidate {
	candidate := Candidate{
		StablePath: device.RequestedPath, DevicePath: device.ResolvedPath, Model: device.Model,
		Serial: device.Serial, SizeBytes: device.Size, Transport: device.Transport, State: "uninitialized",
	}
	if err := validateSupportedDeviceSize(device.Size); err != nil {
		candidate.State = "unsupported"
		candidate.Reason = err.Error()
		return candidate
	}
	if writable, err := chooseImageSize(device.Size); err == nil {
		candidate.PlannedWritableBytes = writable
		candidate.PlannedImageSizeBytes, _ = chooseAdvertisedSize(device.Size)
	}
	record, registered := matchingRecord(device, records)
	if registered {
		candidate.LastError = record.LastError
		candidate.Initialized = record.Status == "ready"
		candidate.Active = hasActive && sameDeviceRecord(record, active)
		if record.Status == "failed" || record.Status == "initializing" {
			candidate.State = record.Status
			candidate.CanResume = hasUsableXFS(device, dataRoot, dataLabel)
			candidate.CanInitialize = !candidate.CanResume && safeForErase(device)
			candidate.CanCancel = true
			candidate.Reason = "上次初始化未完成，可以继续恢复"
			return candidate
		}
		candidate.State = "ready"
		candidate.Reason = "设备已完成 TeslaBox 初始化"
		candidate.CanReinitialize = safeForManagedReinitialize(device, dataRoot, dataLabel)
		return candidate
	}
	if device.Serial == "" {
		candidate.State = "unsafe"
		candidate.Reason = "设备没有稳定序列号，无法防止误认盘"
		return candidate
	}
	required, policyErr := policy.InitializationRequiredBytes(uint64(max(device.Size, 0)))
	if policyErr != nil || device.Size <= 0 || required >= uint64(device.Size) || uint64(device.Size)-required < uint64(minimumDeviceSize) {
		candidate.State = "unsupported"
		candidate.Reason = "设备容量不足以同时保留最低录像镜像、快照空间和初始化余量"
		return candidate
	}
	if !safeForErase(device) {
		candidate.State = "unsafe"
		candidate.Reason = "设备或其分区正在挂载或作为 swap 使用"
		return candidate
	}
	candidate.CanInitialize = true
	candidate.Reason = "尚未初始化；继续操作会擦除整块设备"
	return candidate
}

func safeForManagedReinitialize(device driveinit.Device, dataRoot, dataLabel string) bool {
	if device.MountPoint != "" || strings.EqualFold(device.FileSystem, "swap") {
		return false
	}
	for _, child := range device.Children {
		if strings.EqualFold(child.FileSystem, "swap") {
			return false
		}
		if child.MountPoint == "" {
			continue
		}
		if !strings.EqualFold(child.FileSystem, "xfs") || !strings.EqualFold(child.Label, dataLabel) ||
			filepath.Clean(child.MountPoint) != filepath.Clean(dataRoot) {
			return false
		}
	}
	return true
}

func matchingRecord(device driveinit.Device, records []storage.DeviceRecord) (storage.DeviceRecord, bool) {
	for _, record := range records {
		for _, child := range device.Children {
			if record.UUID != "" && strings.EqualFold(record.UUID, child.UUID) {
				return record, true
			}
			if record.UUID == "" && record.Serial == "" && record.Label != "" && strings.EqualFold(record.Label, child.Label) && strings.EqualFold(child.FileSystem, "xfs") {
				return record, true
			}
		}
		if record.Serial != "" && record.Serial == device.Serial && record.SizeBytes == device.Size {
			return record, true
		}
	}
	return storage.DeviceRecord{}, false
}

func recordAttached(record storage.DeviceRecord, devices []driveinit.Device) bool {
	for _, device := range devices {
		matched, found := matchingRecord(device, []storage.DeviceRecord{record})
		if found && sameDeviceRecord(matched, record) {
			return true
		}
	}
	return false
}

func sameDeviceRecord(left, right storage.DeviceRecord) bool {
	if left.UUID != "" && right.UUID != "" {
		return strings.EqualFold(left.UUID, right.UUID)
	}
	return left.Serial != "" && left.Serial == right.Serial && left.SizeBytes == right.SizeBytes
}

func safeForErase(device driveinit.Device) bool {
	if device.MountPoint != "" || strings.EqualFold(device.FileSystem, "swap") {
		return false
	}
	for _, child := range device.Children {
		if !safeForErase(child) {
			return false
		}
	}
	return true
}

func hasUsableXFS(device driveinit.Device, dataRoot, dataLabel string) bool {
	for _, child := range device.Children {
		if strings.EqualFold(child.FileSystem, "xfs") && strings.EqualFold(child.Label, dataLabel) &&
			(child.MountPoint == "" || filepath.Clean(child.MountPoint) == filepath.Clean(dataRoot)) {
			return true
		}
	}
	return false
}

func candidateByPath(devices []Candidate, path string) (Candidate, bool) {
	for _, device := range devices {
		if device.StablePath == path {
			return device, true
		}
	}
	return Candidate{}, false
}

func writeDeviceMarker(path string, record storage.DeviceRecord) (resultErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("创建设备标记目录: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".device-marker-*")
	if err != nil {
		return fmt.Errorf("创建设备标记: %w", err)
	}
	temporaryName := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			resultErr = errors.Join(resultErr, temporary.Close())
		}
		if resultErr != nil {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o640); err != nil {
		return err
	}
	if err := json.NewEncoder(temporary).Encode(record); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	closed = true
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("替换设备标记: %w", err)
	}
	return nil
}

func writeBenchmarkReport(name string, report storagebench.Report) error {
	content, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("编码存储性能体检结果: %w", err)
	}
	content = append(content, '\n')
	if err := os.WriteFile(name, content, 0o640); err != nil {
		return fmt.Errorf("保存存储性能体检结果: %w", err)
	}
	return nil
}

func readBenchmarkReport(name string) (*storagebench.Report, error) {
	content, err := os.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取存储性能体检结果: %w", err)
	}
	var report storagebench.Report
	if err := json.Unmarshal(content, &report); err != nil {
		return nil, fmt.Errorf("解析存储性能体检结果: %w", err)
	}
	return &report, nil
}

// OSDeviceLister 从 /dev/disk/by-id 枚举整盘并通过 lsblk 核实真实设备树。
type OSDeviceLister struct {
	inspector   driveinit.Inspector
	stablePaths func() ([]string, error)
}

// NewOSDeviceLister 创建只读整盘枚举器，支持 USB、NVMe 和其他有稳定身份的设备。
func NewOSDeviceLister(inspector driveinit.Inspector) *OSDeviceLister {
	return &OSDeviceLister{
		inspector: inspector,
		stablePaths: func() ([]string, error) {
			return filepath.Glob("/dev/disk/by-id/*")
		},
	}
}

// List 去重同一整盘的多个稳定链接，不返回系统盘或分区链接。
func (l *OSDeviceLister) List(ctx context.Context) ([]driveinit.Device, error) {
	if l == nil || l.inspector == nil || l.stablePaths == nil {
		return nil, errors.New("存储设备枚举器未初始化")
	}
	paths, err := l.stablePaths()
	if err != nil {
		return nil, fmt.Errorf("枚举存储稳定路径: %w", err)
	}
	slices.SortFunc(paths, func(left, right string) int {
		leftRank := stablePathRank(left)
		rightRank := stablePathRank(right)
		if leftRank != rightRank {
			return leftRank - rightRank
		}
		return strings.Compare(left, right)
	})
	devices := make([]driveinit.Device, 0, len(paths))
	resolved := make(map[string]bool, len(paths))
	for _, path := range paths {
		if !driveinit.IsWholeDiskStablePath(path) {
			continue
		}
		device, err := l.inspector.Inspect(ctx, path)
		if err != nil {
			continue
		}
		if device.Type != "disk" || systemDisk(device) || resolved[device.ResolvedPath] {
			continue
		}
		resolved[device.ResolvedPath] = true
		devices = append(devices, device)
	}
	return devices, nil
}

func stablePathRank(name string) int {
	base := path.Base(name)
	switch {
	case strings.HasPrefix(base, "usb-"):
		return 0
	case strings.HasPrefix(base, "nvme-"):
		return 1
	case strings.HasPrefix(base, "wwn-"), strings.HasPrefix(base, "ata-"), strings.HasPrefix(base, "scsi-"):
		return 2
	default:
		return 3
	}
}

func systemDisk(device driveinit.Device) bool {
	if path.Clean(device.MountPoint) == "/" {
		return true
	}
	for _, child := range device.Children {
		if systemDisk(child) {
			return true
		}
	}
	return false
}

var _ DeviceLister = (*OSDeviceLister)(nil)
