package hostmigration

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"os"

	"github.com/wen-ryon/teslabox/internal/system"
)

var (
	//go:embed assets/teslabox-fan-early.sh assets/teslabox-fan-early.service
	managedFanAssets embed.FS
)

// FanComponentPaths 固定主程序允许安装的早期风扇脚本和 systemd unit。
type FanComponentPaths struct {
	Script string
	Unit   string
}

// EnsureFanEarlyComponents 为在线更新用户补齐早期风扇控制组件。
// 已存在但内容不同的管理员自定义文件不会被覆盖。
func EnsureFanEarlyComponents(ctx context.Context, paths FanComponentPaths, runner system.Runner) (bool, error) {
	if runner == nil {
		return false, errors.New("风扇组件安装命令执行器为空")
	}
	if err := validateManagedPath(paths.Script); err != nil {
		return false, fmt.Errorf("风扇脚本路径: %w", err)
	}
	if err := validateManagedPath(paths.Unit); err != nil {
		return false, fmt.Errorf("风扇 unit 路径: %w", err)
	}
	script, err := managedFanAssets.ReadFile("assets/teslabox-fan-early.sh")
	if err != nil {
		return false, fmt.Errorf("读取内置风扇脚本: %w", err)
	}
	unit, err := managedFanAssets.ReadFile("assets/teslabox-fan-early.service")
	if err != nil {
		return false, fmt.Errorf("读取内置风扇 unit: %w", err)
	}
	changed := false
	for _, component := range []struct {
		path    string
		content []byte
		mode    os.FileMode
	}{{paths.Script, script, 0o755}, {paths.Unit, unit, 0o644}} {
		created, createErr := createManagedComponent(component.path, component.content, component.mode)
		if createErr != nil {
			return changed, createErr
		}
		changed = changed || created
	}
	if changed {
		if _, err := runner.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			return true, fmt.Errorf("重新加载早期风扇 unit: %w", err)
		}
	}
	if _, err := runner.Run(ctx, "systemctl", "enable", "teslabox-fan-early.service"); err != nil {
		return changed, fmt.Errorf("启用早期风扇 unit: %w", err)
	}
	return changed, nil
}

func createManagedComponent(path string, content []byte, mode os.FileMode) (bool, error) {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return false, errors.New("早期风扇组件目标不是普通文件")
		}
		return false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := writeFileAtomic(path, content, mode); err != nil {
		return false, fmt.Errorf("创建早期风扇组件: %w", err)
	}
	return true, nil
}
