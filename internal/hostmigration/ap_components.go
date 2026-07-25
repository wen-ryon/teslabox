package hostmigration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wen-ryon/teslabox/internal/system"
)

const maximumManagedComponentSize = 128 * 1024

var (
	//go:embed assets/teslabox-ap-setup.sh assets/teslabox-ap.service
	managedAPAssets embed.FS

	legacyAPScriptHashes = map[string]struct{}{
		// v0.1.7 会无条件创建虚拟 AP 接口，并假定所有无线网卡都支持 AP+STA 并发。
		"e49dbbe68f4402c581238c3d55dc2906bdfc18a5c9f81d3d59ceb2b5a80244b3": {},
		// v0.1.8 至 v0.1.10 已探测硬件能力，但尚未尊重热点开机自动连接开关。
		"2089669d49339e1d0ec39b5e78032490b7bea12c9eb382e40043e958528ac11a": {},
	}
	legacyAPUnitHashes = map[string]struct{}{
		// v0.1.7 的 RemainAfterExit=yes 会阻止无线设备重新出现时再次执行探测。
		"0551e701cb5a406315a034e2f18637073b1f60c7f99a3fa22cab9d12a9aa0feb": {},
	}
)

// APComponentPaths 固定主程序允许迁移的 AP 脚本和 systemd unit。
type APComponentPaths struct {
	Script string
	Unit   string
}

// APComponentMigration 描述本次启动实际替换了哪些旧版系统组件。
type APComponentMigration struct {
	Script bool
	Unit   bool
}

// UpgradeLegacyAPComponents 只替换已知发行版摘要对应的旧文件。
// 管理员自行修改的脚本或 unit 不会被覆盖，迁移也不会启动热点或改变当前无线连接。
func UpgradeLegacyAPComponents(
	ctx context.Context,
	paths APComponentPaths,
	runner system.Runner,
) (APComponentMigration, error) {
	if runner == nil {
		return APComponentMigration{}, errors.New("AP 组件迁移命令执行器为空")
	}
	if err := validateManagedPath(paths.Script); err != nil {
		return APComponentMigration{}, fmt.Errorf("AP 脚本路径: %w", err)
	}
	if err := validateManagedPath(paths.Unit); err != nil {
		return APComponentMigration{}, fmt.Errorf("AP unit 路径: %w", err)
	}

	scriptContent, err := managedAPAssets.ReadFile("assets/teslabox-ap-setup.sh")
	if err != nil {
		return APComponentMigration{}, fmt.Errorf("读取内置 AP 脚本: %w", err)
	}
	unitContent, err := managedAPAssets.ReadFile("assets/teslabox-ap.service")
	if err != nil {
		return APComponentMigration{}, fmt.Errorf("读取内置 AP unit: %w", err)
	}

	scriptChanged, err := replaceKnownComponent(paths.Script, scriptContent, 0o755, legacyAPScriptHashes)
	if err != nil {
		return APComponentMigration{}, fmt.Errorf("迁移 AP 脚本: %w", err)
	}
	unitChanged, err := componentNeedsReplacement(paths.Unit, unitContent, legacyAPUnitHashes)
	if err != nil {
		return APComponentMigration{Script: scriptChanged}, fmt.Errorf("检查 AP unit: %w", err)
	}
	if unitChanged {
		// 旧 unit 若已经 active(exited)，停止它不会关闭 NetworkManager 热点，只会允许后续热插拔重新探测。
		// 启动中的 oneshot 不能被迁移打断，否则可能把驱动留在接口刚创建但尚未交给 NetworkManager 的状态。
		subState, stateErr := runner.Run(
			ctx,
			"systemctl", "show", "teslabox-ap.service", "--property=SubState", "--value",
		)
		if stateErr == nil && string(bytes.TrimSpace(subState)) == "exited" {
			if _, stopErr := runner.Run(ctx, "systemctl", "stop", "teslabox-ap.service"); stopErr != nil {
				return APComponentMigration{Script: scriptChanged}, fmt.Errorf("停止旧 AP 探测 unit: %w", stopErr)
			}
		}
		if err := writeFileAtomic(paths.Unit, unitContent, 0o644); err != nil {
			return APComponentMigration{Script: scriptChanged}, fmt.Errorf("替换 AP unit: %w", err)
		}
		if _, reloadErr := runner.Run(ctx, "systemctl", "daemon-reload"); reloadErr != nil {
			return APComponentMigration{Script: scriptChanged, Unit: true}, fmt.Errorf("重新加载 AP unit: %w", reloadErr)
		}
	}
	return APComponentMigration{Script: scriptChanged, Unit: unitChanged}, nil
}

func replaceKnownComponent(
	path string,
	expected []byte,
	mode os.FileMode,
	legacyHashes map[string]struct{},
) (bool, error) {
	changed, err := componentNeedsReplacement(path, expected, legacyHashes)
	if err != nil || !changed {
		return changed, err
	}
	if err := writeFileAtomic(path, expected, mode); err != nil {
		return false, err
	}
	return true, nil
}

func componentNeedsReplacement(
	path string,
	expected []byte,
	legacyHashes map[string]struct{},
) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		// 独立运行开发版主程序时不能擅自创建系统组件；完整首装仍由 install.sh 负责。
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("目标不是普通文件，拒绝自动迁移")
	}
	if info.Size() > maximumManagedComponentSize {
		return false, errors.New("目标文件异常过大，拒绝自动迁移")
	}
	content, err := readLimitedFile(path, maximumManagedComponentSize)
	if err != nil {
		return false, err
	}
	if string(content) == string(expected) {
		return false, nil
	}
	digest := sha256.Sum256(content)
	if _, allowed := legacyHashes[hex.EncodeToString(digest[:])]; !allowed {
		return false, nil
	}
	return true, nil
}

func validateManagedPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
		return errors.New("必须是非根目录绝对路径")
	}
	return nil
}
