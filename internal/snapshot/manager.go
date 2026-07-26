// Package snapshot 负责从仍在写入的 TeslaCam 镜像创建独立副本并只读挂载。
package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wen-ryon/teslabox/internal/system"
)

var (
	loopDevicePattern    = regexp.MustCompile(`^/dev/loop[0-9]+$`)
	snapshotIDPattern    = regexp.MustCompile(`^\d{8}T\d{6}\.\d{9}Z$`)
	snapshotImagePattern = regexp.MustCompile(`^teslacam-(\d{8}T\d{6}\.\d{9}Z)\.img(?:\.partial)?$`)
)

// ErrInsufficientSpace 表示为了优先保证车端录像，当前不应创建新快照。
var ErrInsufficientSpace = errors.New("快照空间不足")

// SpaceStatus 描述快照创建前的数据盘容量门槛。
type SpaceStatus struct {
	TotalBytes     uint64  `json:"total_bytes"`
	FreeBytes      uint64  `json:"free_bytes"`
	FreePercent    float64 `json:"free_percent"`
	MinFreeBytes   uint64  `json:"min_free_bytes"`
	MinFreePercent uint64  `json:"min_free_percent"`
	Ready          bool    `json:"ready"`
	Reason         string  `json:"reason,omitempty"`
}

// VolumeSpace 描述车机实际使用的 exFAT 虚拟盘容量，避免与承载镜像的外层 XFS 容量混淆。
type VolumeSpace struct {
	TotalBytes  uint64  `json:"total_bytes"`
	FreeBytes   uint64  `json:"free_bytes"`
	FreePercent float64 `json:"free_percent"`
}

// InsufficientSpaceError 保留实际容量，便于 API 和日志解释降级原因。
type InsufficientSpaceError struct {
	Status SpaceStatus
}

// Error 返回稳定的中文容量说明。
func (e *InsufficientSpaceError) Error() string {
	return e.Status.Reason
}

// Unwrap 允许调用方使用 errors.Is 判断空间不足。
func (e *InsufficientSpaceError) Unwrap() error {
	return ErrInsufficientSpace
}

// View 描述一个已经检查并只读挂载的 TeslaCam 快照。
type View struct {
	ID            string      `json:"id"`
	ImagePath     string      `json:"image_path"`
	MountPath     string      `json:"mount_path"`
	LoopDevice    string      `json:"loop_device"`
	CreatedAt     time.Time   `json:"created_at"`
	RepairApplied bool        `json:"repair_applied"`
	VolumeSpace   VolumeSpace `json:"volume_space"`
}

// Config 固定原始镜像、快照目录和运行时只读挂载根目录。
type Config struct {
	ImagePath      string
	SnapshotPath   string
	MountRoot      string
	MinFreeBytes   uint64
	MinFreePercent uint64
}

// Manager 串行创建快照，避免多个 fsck 和 loop 挂载同时操作同一镜像族。
type Manager struct {
	runner system.Runner
	config Config
	now    func() time.Time
	space  func(string) (uint64, uint64, error)
	mu     sync.Mutex
}

// New 创建快照管理器，目录本身必须由显式初始化或 systemd 准备完成。
func New(runner system.Runner, config Config) *Manager {
	return &Manager{runner: runner, config: config, now: time.Now, space: platformSpace}
}

// Space 检查快照目录所在文件系统是否仍为车端录像保留了安全余量。
func (m *Manager) Space() (SpaceStatus, error) {
	if err := requireDirectory(m.config.SnapshotPath); err != nil {
		return SpaceStatus{}, fmt.Errorf("检查快照目录容量: %w", err)
	}
	total, free, err := m.space(m.config.SnapshotPath)
	if err != nil {
		return SpaceStatus{}, fmt.Errorf("读取快照目录容量: %w", err)
	}
	status := SpaceStatus{
		TotalBytes: total, FreeBytes: free,
		MinFreeBytes: m.config.MinFreeBytes, MinFreePercent: m.config.MinFreePercent,
		Ready: true,
	}
	if total > 0 {
		status.FreePercent = float64(free) * 100 / float64(total)
	}
	if free < m.config.MinFreeBytes || status.FreePercent < float64(m.config.MinFreePercent) {
		status.Ready = false
		status.Reason = fmt.Sprintf("快照空间不足：可用 %d 字节（%.2f%%），至少保留 %d 字节且不低于 %d%%", free, status.FreePercent, m.config.MinFreeBytes, m.config.MinFreePercent)
	}
	return status, nil
}

// Recover 只回收命名和 backing path 都属于 TeslaBox 的异常断电遗留资源。
func (m *Manager) Recover(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.validateRecovery(); err != nil {
		return err
	}

	mountEntries, err := os.ReadDir(m.config.MountRoot)
	if err != nil {
		return fmt.Errorf("读取快照挂载目录: %w", err)
	}
	for _, entry := range mountEntries {
		if !snapshotIDPattern.MatchString(entry.Name()) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return fmt.Errorf("检查遗留快照挂载目录 %s: %w", entry.Name(), infoErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("拒绝清理非普通快照挂载目录 %s", entry.Name())
		}
		mountPath := filepath.Join(m.config.MountRoot, entry.Name())
		if _, unmountErr := m.runner.Run(ctx, "umount", "--", mountPath); unmountErr != nil && !isNotMountedError(unmountErr) {
			return fmt.Errorf("卸载遗留 TeslaCam 快照 %s: %w", entry.Name(), unmountErr)
		}
	}

	output, err := m.runner.Run(ctx, "losetup", "--list", "--noheadings", "--output", "NAME,BACK-FILE")
	if err != nil {
		return fmt.Errorf("列出 loop 设备: %w", err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		device, imagePath, ok := parseLoopLine(line)
		if !ok || !m.isManagedImage(imagePath) {
			continue
		}
		if err := m.detachLoop(ctx, device); err != nil {
			return fmt.Errorf("回收遗留快照 %s: %w", filepath.Base(imagePath), err)
		}
	}

	imageEntries, err := os.ReadDir(m.config.SnapshotPath)
	if err != nil {
		return fmt.Errorf("读取快照目录: %w", err)
	}
	for _, entry := range imageEntries {
		if !snapshotImagePattern.MatchString(entry.Name()) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return fmt.Errorf("检查遗留快照 %s: %w", entry.Name(), infoErr)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("拒绝删除非普通快照文件 %s", entry.Name())
		}
		if err := os.Remove(filepath.Join(m.config.SnapshotPath, entry.Name())); err != nil {
			return fmt.Errorf("删除遗留快照 %s: %w", entry.Name(), err)
		}
	}
	for _, entry := range mountEntries {
		if snapshotIDPattern.MatchString(entry.Name()) {
			if err := os.Remove(filepath.Join(m.config.MountRoot, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("删除遗留快照挂载目录 %s: %w", entry.Name(), err)
			}
		}
	}
	return nil
}

// Create 使用 XFS reflink 克隆原镜像，只修复副本并以只读 loop 方式挂载。
func (m *Manager) Create(ctx context.Context) (View, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return View{}, err
	}
	if err := m.validate(); err != nil {
		return View{}, err
	}
	space, err := m.Space()
	if err != nil {
		return View{}, err
	}
	if !space.Ready {
		return View{}, &InsufficientSpaceError{Status: space}
	}

	createdAt := m.now().UTC()
	id := createdAt.Format("20060102T150405.000000000Z")
	finalImage := filepath.Join(m.config.SnapshotPath, "teslacam-"+id+".img")
	temporaryImage := finalImage + ".partial"
	mountPath := filepath.Join(m.config.MountRoot, id)
	if err := os.Mkdir(mountPath, 0o750); err != nil {
		return View{}, fmt.Errorf("创建快照挂载目录: %w", err)
	}

	cleanup := true
	loopDevice := ""
	defer func() {
		if cleanup {
			canRemoveImage := true
			if loopDevice != "" {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				_, detachErr := m.runner.Run(cleanupCtx, "losetup", "--detach", loopDevice)
				cancel()
				canRemoveImage = detachErr == nil
			}
			if canRemoveImage {
				_ = os.Remove(temporaryImage)
				_ = os.Remove(finalImage)
			}
			_ = os.Remove(mountPath)
		}
	}()

	if _, err := m.runner.Run(ctx, "cp", "--reflink=always", "--sparse=auto", "--", m.config.ImagePath, temporaryImage); err != nil {
		return View{}, fmt.Errorf("创建 TeslaCam reflink 快照: %w", err)
	}
	if err := requireRegularFile(temporaryImage); err != nil {
		return View{}, fmt.Errorf("检查临时快照: %w", err)
	}
	if err := os.Rename(temporaryImage, finalImage); err != nil {
		return View{}, fmt.Errorf("发布 TeslaCam 快照: %w", err)
	}
	loopDevice, err = m.attachLoop(ctx, finalImage, true)
	if err != nil {
		return View{}, err
	}
	repairApplied := false
	firstMountErr := m.mountReadOnly(ctx, loopDevice+"p1", mountPath)
	if firstMountErr != nil {
		// 正常 reflink 副本通常可直接只读挂载；只有捕获到写入中间态时才运行全盘 fsck，
		// 避免每 30 秒扫描都与车机的音乐读取和录像写入争抢底层闪存。
		if err := m.detachLoop(ctx, loopDevice); err != nil {
			return View{}, errors.Join(firstMountErr, err)
		}
		loopDevice = ""
		loopDevice, err = m.attachLoop(ctx, finalImage, false)
		if err != nil {
			return View{}, errors.Join(firstMountErr, err)
		}
		if _, err := m.runner.Run(ctx, "fsck.exfat", "-p", loopDevice+"p1"); err != nil {
			commandError, ok := errors.AsType[*system.CommandError](err)
			if !ok || commandError.Code != 1 {
				return View{}, errors.Join(firstMountErr, fmt.Errorf("检查并修复 TeslaCam 快照: %w", err))
			}
		}
		repairApplied = true
		if err := m.detachLoop(ctx, loopDevice); err != nil {
			return View{}, errors.Join(firstMountErr, err)
		}
		loopDevice = ""
		loopDevice, err = m.attachLoop(ctx, finalImage, true)
		if err != nil {
			return View{}, errors.Join(firstMountErr, err)
		}
		if retryErr := m.mountReadOnly(ctx, loopDevice+"p1", mountPath); retryErr != nil {
			return View{}, errors.Join(
				fmt.Errorf("首次只读挂载 TeslaCam 快照: %w", firstMountErr),
				fmt.Errorf("修复后只读挂载 TeslaCam 快照: %w", retryErr),
			)
		}
	}

	totalBytes, freeBytes, err := platformSpace(mountPath)
	if err != nil {
		return View{}, fmt.Errorf("读取 TeslaCam 虚拟盘容量: %w", err)
	}
	freePercent := float64(0)
	if totalBytes > 0 {
		freePercent = float64(freeBytes) * 100 / float64(totalBytes)
	}

	cleanup = false
	return View{
		ID:            id,
		ImagePath:     finalImage,
		MountPath:     mountPath,
		LoopDevice:    loopDevice,
		CreatedAt:     createdAt,
		RepairApplied: repairApplied,
		VolumeSpace: VolumeSpace{
			TotalBytes:  totalBytes,
			FreeBytes:   freeBytes,
			FreePercent: freePercent,
		},
	}, nil
}

// Remove 先卸载只读视图再删除快照，不会触碰正在向 Tesla 暴露的原始镜像。
func (m *Manager) Remove(ctx context.Context, view View) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.validateView(view); err != nil {
		return err
	}
	if _, err := m.runner.Run(ctx, "umount", "--", view.MountPath); err != nil && !isNotMountedError(err) {
		return fmt.Errorf("卸载 TeslaCam 快照: %w", err)
	}
	if err := m.detachLoop(ctx, view.LoopDevice); err != nil {
		return err
	}
	var result error
	if err := os.Remove(view.MountPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		result = errors.Join(result, fmt.Errorf("删除快照挂载目录: %w", err))
	}
	if err := os.Remove(view.ImagePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		result = errors.Join(result, fmt.Errorf("删除快照镜像: %w", err))
	}
	return result
}

func isNotMountedError(err error) bool {
	commandError, ok := errors.AsType[*system.CommandError](err)
	return ok && strings.Contains(strings.ToLower(commandError.Output), "not mounted")
}

func (m *Manager) validate() error {
	if err := requireRegularFile(m.config.ImagePath); err != nil {
		return fmt.Errorf("检查 TeslaCam 原始镜像: %w", err)
	}
	for name, directory := range map[string]string{
		"快照目录":    m.config.SnapshotPath,
		"快照挂载根目录": m.config.MountRoot,
	} {
		if err := requireDirectory(directory); err != nil {
			return fmt.Errorf("检查%s: %w", name, err)
		}
	}
	return nil
}

func (m *Manager) validateRecovery() error {
	for name, directory := range map[string]string{
		"快照目录":    m.config.SnapshotPath,
		"快照挂载根目录": m.config.MountRoot,
	} {
		if err := requireDirectory(directory); err != nil {
			return fmt.Errorf("检查%s: %w", name, err)
		}
	}
	return nil
}

func (m *Manager) isManagedImage(name string) bool {
	cleaned := strings.TrimSuffix(strings.TrimSpace(name), " (deleted)")
	return filepath.Clean(filepath.Dir(cleaned)) == filepath.Clean(m.config.SnapshotPath) && snapshotImagePattern.MatchString(filepath.Base(cleaned))
}

func parseLoopLine(line string) (string, string, bool) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 || !loopDevicePattern.MatchString(fields[0]) {
		return "", "", false
	}
	imagePath := strings.Join(fields[1:], " ")
	if strings.HasSuffix(imagePath, " (deleted)") {
		imagePath = strings.TrimSuffix(imagePath, " (deleted)")
	}
	if _, err := strconv.Unquote(imagePath); err == nil && strings.HasPrefix(imagePath, "\"") {
		imagePath, _ = strconv.Unquote(imagePath)
	}
	return fields[0], imagePath, true
}

func (m *Manager) validateView(view View) error {
	if strings.TrimSpace(view.ID) == "" {
		return errors.New("快照 ID 不能为空")
	}
	if !isWithin(m.config.SnapshotPath, view.ImagePath) || !isWithin(m.config.MountRoot, view.MountPath) {
		return errors.New("拒绝清理快照目录之外的路径")
	}
	if !loopDevicePattern.MatchString(view.LoopDevice) {
		return errors.New("快照 loop 设备无效")
	}
	return nil
}

func (m *Manager) attachLoop(ctx context.Context, imagePath string, readOnly bool) (string, error) {
	args := []string{"--find", "--show"}
	if readOnly {
		args = append(args, "--read-only")
	}
	args = append(args, "--partscan", "--", imagePath)
	output, err := m.runner.Run(ctx, "losetup", args...)
	if err != nil {
		return "", fmt.Errorf("建立 TeslaCam 快照 loop 设备: %w", err)
	}
	device := strings.TrimSpace(string(output))
	if !loopDevicePattern.MatchString(device) {
		return "", fmt.Errorf("losetup 返回了无效设备 %q", device)
	}
	return device, nil
}

func (m *Manager) detachLoop(ctx context.Context, device string) error {
	if !loopDevicePattern.MatchString(device) {
		return errors.New("拒绝解绑无效的 loop 设备")
	}
	if _, err := m.runner.Run(ctx, "losetup", "--detach", device); err != nil {
		return fmt.Errorf("解绑 TeslaCam 快照 loop 设备: %w", err)
	}
	return nil
}

func (m *Manager) mountReadOnly(ctx context.Context, device, mountPath string) error {
	_, err := m.runner.Run(ctx, "mount", "--options", "ro,nosuid,nodev,noexec", "--", device, mountPath)
	if err == nil {
		return nil
	}
	commandError, ok := errors.AsType[*system.CommandError](err)
	if !ok || !strings.Contains(strings.ToLower(commandError.Output), "unknown filesystem type 'exfat'") {
		return fmt.Errorf("只读挂载 TeslaCam 快照: %w", err)
	}
	if _, fallbackErr := m.runner.Run(ctx, "mount.exfat-fuse", "-o", "ro,nosuid,nodev,noexec", device, mountPath); fallbackErr != nil {
		return errors.Join(
			fmt.Errorf("内核只读挂载 TeslaCam 快照: %w", err),
			fmt.Errorf("exfat-fuse 只读挂载 TeslaCam 快照: %w", fallbackErr),
		)
	}
	return nil
}

func isWithin(root, name string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(name))
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func requireDirectory(name string) error {
	info, err := os.Lstat(name)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s 不是可信目录", name)
	}
	return nil
}

func requireRegularFile(name string) error {
	info, err := os.Lstat(name)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%s 不是可信普通文件", name)
	}
	return nil
}
