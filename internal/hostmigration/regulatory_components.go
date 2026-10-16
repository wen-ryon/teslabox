package hostmigration

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"os"

	"github.com/wen-ryon/teslabox/internal/system"
)

//go:embed assets/teslabox-wireless-region.sh assets/teslabox-wireless-region.service
var managedRegulatoryAssets embed.FS

// RegulatoryComponentPaths 固定监管域启动组件的落点，不接受网页提供的路径。
type RegulatoryComponentPaths struct {
	Script string
	Unit   string
}

// EnsureRegulatoryComponents 为单文件 OTA 用户补齐下一次启动所需组件，不覆盖管理员文件。
func EnsureRegulatoryComponents(ctx context.Context, paths RegulatoryComponentPaths, runner system.Runner) (bool, error) {
	if runner == nil {
		return false, errors.New("监管域组件安装命令执行器为空")
	}
	changed := false
	for _, component := range []struct {
		path, asset string
		mode        os.FileMode
	}{
		{paths.Script, "assets/teslabox-wireless-region.sh", 0o755},
		{paths.Unit, "assets/teslabox-wireless-region.service", 0o644},
	} {
		if err := validateManagedPath(component.path); err != nil {
			return changed, err
		}
		data, err := managedRegulatoryAssets.ReadFile(component.asset)
		if err != nil {
			return changed, err
		}
		created, err := createManagedComponent(component.path, data, component.mode)
		if err != nil {
			return changed, err
		}
		changed = changed || created
		actual, err := readLimitedFile(component.path, maximumManagedComponentSize)
		if err != nil {
			return changed, err
		}
		if string(actual) != string(data) {
			return changed, errors.New("保留管理员自定义监管域组件，未自动启用")
		}
	}
	if changed {
		if _, err := runner.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			return changed, fmt.Errorf("重新加载监管域启动组件: %w", err)
		}
	}
	if _, err := runner.Run(ctx, "systemctl", "enable", "teslabox-wireless-region.service"); err != nil {
		return changed, fmt.Errorf("启用监管域启动组件: %w", err)
	}
	return changed, nil
}
