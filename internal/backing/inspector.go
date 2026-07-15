// Package backing 负责校验 TeslaCam 镜像的数据盘布局，不执行格式化或目录创建。
package backing

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/wen-ryon/teslabox/internal/storage"
)

// State 表示 backing store 距离可供 Gadget 使用还缺少什么条件。
type State string

const (
	StateUnknown             State = "UNKNOWN"
	StateNeedsInitialization State = "NEEDS_INITIALIZATION"
	StateNeedsMount          State = "NEEDS_MOUNT"
	StateWrongMount          State = "WRONG_MOUNT"
	StateIncomplete          State = "INCOMPLETE"
	StateReady               State = "READY"
)

// Layout 定义所有高写入数据在外接盘中的相对位置。
type Layout struct {
	Root         string
	ImagePath    string
	SnapshotPath string
	IndexPath    string
	StatePath    string
}

// Status 是 backing store 的只读检查结果。
type Status struct {
	State        State    `json:"state"`
	Ready        bool     `json:"ready"`
	Root         string   `json:"root"`
	ImagePath    string   `json:"image_path"`
	SnapshotPath string   `json:"snapshot_path"`
	IndexPath    string   `json:"index_path"`
	StatePath    string   `json:"state_path"`
	Missing      []string `json:"missing,omitempty"`
	Reason       string   `json:"reason,omitempty"`
}

// Inspector 只观察已经存在的布局，避免常驻服务隐式初始化数据盘。
type Inspector struct {
	layout Layout
}

// NewInspector 创建固定布局检查器，路径安全由配置边界统一校验。
func NewInspector(layout Layout) *Inspector {
	return &Inspector{layout: layout}
}

// Inspect 校验文件系统、挂载点、目录和镜像，任何缺失都只报告而不修复。
func (i *Inspector) Inspect(device storage.Device) (Status, error) {
	status := i.baseStatus()
	if !strings.EqualFold(device.FSType, "xfs") {
		status.State = StateNeedsInitialization
		status.Reason = fmt.Sprintf("数据分区文件系统是 %q，需要显式初始化为支持 reflink 的 XFS", device.FSType)
		return status, nil
	}
	if len(device.MountPoints) == 0 {
		status.State = StateNeedsMount
		status.Reason = "数据分区尚未挂载"
		return status, nil
	}

	mountPoint := filepath.Clean(device.MountPoints[0])
	if mountPoint != filepath.Clean(i.layout.Root) {
		status.State = StateWrongMount
		status.Reason = fmt.Sprintf("数据分区挂载在 %q，期望 %q", mountPoint, i.layout.Root)
		return status, nil
	}
	if err := requireDirectory(i.layout.Root); err != nil {
		return status, fmt.Errorf("检查数据盘根目录: %w", err)
	}

	requiredDirectories := []string{
		filepath.Dir(status.ImagePath),
		status.SnapshotPath,
		status.IndexPath,
		status.StatePath,
	}
	for _, directory := range requiredDirectories {
		if err := requireDirectory(directory); err != nil {
			if os.IsNotExist(err) {
				status.Missing = append(status.Missing, directory)
				continue
			}
			return status, fmt.Errorf("检查 backing store 目录 %s: %w", directory, err)
		}
	}
	if err := requireRegularFile(status.ImagePath); err != nil {
		if os.IsNotExist(err) {
			status.Missing = append(status.Missing, status.ImagePath)
		} else {
			return status, fmt.Errorf("检查 TeslaCam 镜像: %w", err)
		}
	}

	slices.Sort(status.Missing)
	if len(status.Missing) > 0 {
		status.State = StateIncomplete
		status.Reason = "backing store 尚未完成显式初始化"
		return status, nil
	}
	status.State = StateReady
	status.Ready = true
	return status, nil
}

func (i *Inspector) baseStatus() Status {
	return Status{
		State:        StateUnknown,
		Root:         i.layout.Root,
		ImagePath:    filepath.Join(i.layout.Root, filepath.FromSlash(i.layout.ImagePath)),
		SnapshotPath: filepath.Join(i.layout.Root, filepath.FromSlash(i.layout.SnapshotPath)),
		IndexPath:    filepath.Join(i.layout.Root, filepath.FromSlash(i.layout.IndexPath)),
		StatePath:    filepath.Join(i.layout.Root, filepath.FromSlash(i.layout.StatePath)),
	}
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
