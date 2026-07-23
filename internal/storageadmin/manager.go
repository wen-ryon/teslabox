// Package storageadmin 提供外接数据盘发现、显式初始化和设备登记能力。
package storageadmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/wen-ryon/teslabox/internal/driveinit"
	"github.com/wen-ryon/teslabox/internal/imageinit"
	"github.com/wen-ryon/teslabox/internal/service"
	"github.com/wen-ryon/teslabox/internal/storage"
	"github.com/wen-ryon/teslabox/internal/system"
)

const (
	minimumDeviceSize       = 32 * 1000 * 1000 * 1000
	minimumReservedBytes    = 20 * 1000 * 1000 * 1000
	maximumMBRImageSize     = 2 * 1000 * 1000 * 1000 * 1000
	reservedCapacityDivisor = 20
	defaultTeslaCamPercent  = 80
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
	Reason                string `json:"reason,omitempty"`
	LastError             string `json:"last_error,omitempty"`
}

// Status 是设备登记册和当前可初始化整盘的一致性视图。
type Status struct {
	ActiveUUID string      `json:"active_uuid,omitempty"`
	Devices    []Candidate `json:"devices"`
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

// Controller 提供破坏性操作前的运行状态和完成后的即时收敛入口。
type Controller interface {
	Snapshot() service.Snapshot
	TriggerRefresh()
	BeginStorageMaintenance(context.Context) (func(), error)
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

// Config 保存初始化布局，不允许从请求体传入系统路径。
type Config struct {
	DataRoot   string
	ImagePath  string
	DataLabel  string
	ImageLabel string
}

// Manager 串行化存储初始化，避免两个浏览器同时操作同一块盘。
type Manager struct {
	config     Config
	runner     system.Runner
	registry   *storage.FileRegistry
	lister     DeviceLister
	driveInit  DriveInitializer
	imageInit  ImageInitializer
	controller Controller
	mu         sync.Mutex
}

// New 创建显式依赖的存储管理器，构造过程不会访问或修改块设备。
func New(config Config, runner system.Runner, registry *storage.FileRegistry, lister DeviceLister, driveInitializer DriveInitializer, imageInitializer ImageInitializer, controller Controller) *Manager {
	return &Manager{config: config, runner: runner, registry: registry, lister: lister, driveInit: driveInitializer, imageInit: imageInitializer, controller: controller}
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
	for _, device := range devices {
		status.Devices = append(status.Devices, classify(device, records, active, hasActive, m.config.DataRoot, m.config.DataLabel))
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
	if candidate.Initialized && !request.Reinitialize && !candidate.CanResume {
		return Candidate{}, &InputError{Message: "目标设备已经完成初始化"}
	}
	if request.Reinitialize && !candidate.CanReinitialize {
		return Candidate{}, &InputError{Message: "当前设备无法安全重新初始化，请先停止使用并重新扫描"}
	}
	if !request.Reinitialize && !candidate.CanInitialize && !candidate.CanResume {
		return Candidate{}, &InputError{Message: candidate.Reason}
	}
	imageSize, err := chooseImageSize(candidate.SizeBytes)
	if err != nil {
		return Candidate{}, err
	}
	teslaCamPercent := request.TeslaCamPercent
	if teslaCamPercent == 0 {
		teslaCamPercent = defaultTeslaCamPercent
	}
	if _, err := imageinit.PlanLayout(imageSize, request.MusicEnabled, teslaCamPercent); err != nil {
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
		_, err = m.imageInit.Run(ctx, imageinit.Options{
			DataRoot: m.config.DataRoot, ImagePath: m.config.ImagePath,
			SizeBytes: imageSize, Label: m.config.ImageLabel,
			MusicEnabled: request.MusicEnabled, TeslaCamPercent: teslaCamPercent, MusicLabel: "MUSIC",
			Execute: true,
		})
		if err != nil {
			return Candidate{}, fmt.Errorf("创建 TeslaCam 虚拟磁盘: %w", err)
		}
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
	result.PlannedImageSizeBytes = imageSize
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
	reserve := deviceSize / reservedCapacityDivisor
	if reserve < minimumReservedBytes {
		reserve = minimumReservedBytes
	}
	available := deviceSize - reserve
	if available > maximumMBRImageSize {
		available = maximumMBRImageSize
	}
	available = available / 1_000_000_000 * 1_000_000_000
	if available < minimumDeviceSize {
		return 0, errors.New("设备容量不足以同时保存 TeslaCam 镜像和快照预留空间")
	}
	return available, nil
}

func classify(device driveinit.Device, records []storage.DeviceRecord, active storage.DeviceRecord, hasActive bool, dataRoot, dataLabel string) Candidate {
	candidate := Candidate{
		StablePath: device.RequestedPath, DevicePath: device.ResolvedPath, Model: device.Model,
		Serial: device.Serial, SizeBytes: device.Size, Transport: device.Transport, State: "uninitialized",
	}
	if planned, err := chooseImageSize(device.Size); err == nil {
		candidate.PlannedImageSizeBytes = planned
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
	if device.Size < minimumDeviceSize+minimumReservedBytes {
		candidate.State = "unsupported"
		candidate.Reason = "设备容量不足 52GB，无法同时保留录像镜像和快照空间"
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
