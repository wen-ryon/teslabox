// Package datamount 负责按稳定标识挂载已经初始化的数据盘，不承担格式化或修复。
package datamount

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wen-ryon/teslabox/internal/storage"
	"github.com/wen-ryon/teslabox/internal/system"
)

// Manager 只挂载已经由存储发现器确认身份的 XFS 数据分区。
type Manager struct {
	runner    system.Runner
	mountPath string
	uuid      string
	label     string
	source    storage.IdentitySource
}

// NewRegistered 创建使用系统盘活动设备登记的动态挂载管理器。
func NewRegistered(runner system.Runner, mountPath string, source storage.IdentitySource, uuid, label string) *Manager {
	manager := New(runner, mountPath, uuid, label)
	manager.source = source
	return manager
}

// New 创建数据盘挂载管理器，UUID 配置后不会回退到卷标。
func New(runner system.Runner, mountPath, uuid, label string) *Manager {
	return &Manager{
		runner:    runner,
		mountPath: filepath.Clean(mountPath),
		uuid:      strings.TrimSpace(uuid),
		label:     strings.TrimSpace(label),
	}
}

// Mount 按 UUID 或卷标挂载已初始化分区，不使用可能变化的 /dev/sdX 名称。
func (m *Manager) Mount(ctx context.Context, device storage.Device) error {
	uuid, label, err := m.identity()
	if err != nil {
		return err
	}
	if err := m.validate(device, uuid, label); err != nil {
		return err
	}

	args := []string{"--options", "noatime,nosuid,nodev"}
	if uuid != "" {
		args = append(args, "--uuid", uuid)
	} else {
		args = append(args, "--label", label)
	}
	args = append(args, m.mountPath)
	if _, err := m.runner.Run(ctx, "mount", args...); err != nil {
		return fmt.Errorf("挂载 TeslaBox 数据盘: %w", err)
	}
	return nil
}

func (m *Manager) identity() (string, string, error) {
	uuid, label := m.uuid, m.label
	if m.source != nil {
		record, exists, err := m.source.Active()
		if err != nil {
			return "", "", fmt.Errorf("读取活动数据盘登记: %w", err)
		}
		if exists {
			uuid, label = record.UUID, record.Label
		} else {
			uuid, label = "", ""
		}
	}
	return uuid, label, nil
}

func (m *Manager) validate(device storage.Device, uuid, label string) error {
	if !strings.EqualFold(device.FSType, "xfs") {
		return fmt.Errorf("拒绝自动挂载文件系统 %q，期望 xfs", device.FSType)
	}
	if len(device.MountPoints) != 0 {
		return fmt.Errorf("数据盘已经挂载到 %v，拒绝自动改挂载点", device.MountPoints)
	}
	if uuid != "" {
		if !strings.EqualFold(device.UUID, uuid) {
			return errors.New("数据盘 UUID 与配置不一致")
		}
	} else if label == "" || !strings.EqualFold(device.Label, label) {
		return errors.New("数据盘卷标与配置不一致")
	}

	info, err := os.Lstat(m.mountPath)
	if err != nil {
		return fmt.Errorf("检查数据盘挂载目录: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("数据盘挂载目录不是可信目录")
	}
	return nil
}
