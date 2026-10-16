package hostmigration

import (
	"context"
	"embed"
	"errors"
	"os"
	"path/filepath"

	"github.com/wen-ryon/teslabox/internal/system"
)

//go:embed assets/native/*
var nativeRoleAssets embed.FS

// NativeRolePaths 只允许本机固定系统组件路径，测试时使用隔离根目录。
type NativeRolePaths struct{ Unit, Module, Udev, Link string }

// DefaultNativeRolePaths 给首装与 OTA 使用同一套可核验资产。
func DefaultNativeRolePaths() NativeRolePaths {
	return NativeRolePaths{"/etc/systemd/system/teslabox-network-roles.service", "/etc/modprobe.d/90-teslabox-8852be-roles.conf", "/etc/udev/rules.d/90-teslabox-native-ap.rules", "/etc/systemd/network/10-teslabox-native-wireless.link"}
}

// EnsureNativeRoleComponents 只准备下次启动，不卸载驱动、不重启 NM 或改变当前路由。
func EnsureNativeRoleComponents(ctx context.Context, paths NativeRolePaths, runner system.Runner) (bool, error) {
	if runner == nil {
		return false, errors.New("网络角色组件命令执行器为空")
	}
	type component struct {
		path string
		data []byte
	}
	var components []component
	for _, path := range []string{paths.Unit, paths.Module, paths.Udev, paths.Link} {
		if err := validateManagedPath(path); err != nil {
			return false, err
		}
		data, err := nativeRoleAssets.ReadFile("assets/native/" + filepath.Base(path))
		if err != nil {
			return false, err
		}
		if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
			return false, errors.New("网络角色组件不是普通文件，未启用迁移")
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		actual, err := readLimitedFile(path, maximumManagedComponentSize)
		if err == nil && string(actual) != string(data) {
			return false, errors.New("保留管理员自定义网络角色组件，未启用迁移")
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		components = append(components, component{path, data})
	}
	changed := false
	for _, c := range components {
		if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
			return changed, err
		}
		created, err := createManagedComponent(c.path, c.data, 0o644)
		if err != nil {
			return changed, err
		}
		changed = changed || created
	}
	if changed {
		if _, err := runner.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			return changed, err
		}
		if _, err := runner.Run(ctx, "udevadm", "control", "--reload-rules"); err != nil {
			return changed, err
		}
	}
	_, err := runner.Run(ctx, "systemctl", "enable", "teslabox-network-roles.service")
	return changed, err
}
