// Package hostmigration 收敛只能由新版主程序安全完成的旧系统配置。
package hostmigration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wen-ryon/teslabox/internal/system"
)

const (
	maximumUdevRuleSize = 64 * 1024
	legacyUdevSignal    = `RUN+="/bin/systemctl kill --kill-who=main --signal=SIGUSR1 teslabox.service"`
	legacyUdevComment   = "# loop 设备由快照流程管理，忽略其事件可避免通知信号打断 losetup、fsck 和 lsblk 子进程。"
	replacementComment  = "# 数据盘状态由 TeslaBox 的低频轮询收敛。不要从 udev 向主进程发送信号：\n" +
		"# 系统启动时块设备事件可能早于 Go 信号订阅，默认 SIGUSR1 会终止刚启动的服务并破坏 Gadget 绑定窗口。\n"
)

// RemoveLegacyUdevSignal 删除旧版本会向主进程发送 SIGUSR1 的规则并重新加载 udev。
// 迁移只接受普通文件且只删除精确匹配的旧规则，避免覆盖管理员添加的其他规则。
func RemoveLegacyUdevSignal(ctx context.Context, rulePath string, runner system.Runner) (bool, error) {
	if !filepath.IsAbs(rulePath) || filepath.Clean(rulePath) == string(filepath.Separator) {
		return false, errors.New("udev 规则路径必须是非根目录绝对路径")
	}
	if runner == nil {
		return false, errors.New("udev 迁移命令执行器为空")
	}
	info, err := os.Lstat(rulePath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("检查旧 udev 规则: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("旧 udev 规则不是普通文件，拒绝自动迁移")
	}
	if info.Size() > maximumUdevRuleSize {
		return false, errors.New("旧 udev 规则异常过大，拒绝自动迁移")
	}

	content, err := readLimitedFile(rulePath, maximumUdevRuleSize)
	if err != nil {
		return false, fmt.Errorf("读取旧 udev 规则: %w", err)
	}
	updated, changed := withoutLegacyUdevSignal(string(content))
	if !changed {
		return false, nil
	}
	if err := writeFileAtomic(rulePath, []byte(updated), 0o644); err != nil {
		return false, fmt.Errorf("迁移旧 udev 规则: %w", err)
	}
	if _, err := runner.Run(ctx, "udevadm", "control", "--reload-rules"); err != nil {
		return true, fmt.Errorf("重新加载 udev 规则: %w", err)
	}
	return true, nil
}

func withoutLegacyUdevSignal(content string) (string, bool) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(strings.TrimSuffix(normalized, "\n"), "\n")
	kept := make([]string, 0, len(lines)+2)
	changed := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == legacyUdevComment {
			changed = true
			continue
		}
		if strings.Contains(trimmed, legacyUdevSignal) {
			changed = true
			continue
		}
		kept = append(kept, line)
	}
	if !changed {
		return content, false
	}
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}
	var result strings.Builder
	if len(kept) > 0 {
		result.WriteString(strings.Join(kept, "\n"))
		result.WriteString("\n")
	}
	result.WriteString(replacementComment)
	return result.String(), true
}

func readLimitedFile(name string, limit int64) ([]byte, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, errors.New("文件超过大小限制")
	}
	return content, nil
}

func writeFileAtomic(name string, content []byte, mode os.FileMode) error {
	directory := filepath.Dir(name)
	temporary, err := os.CreateTemp(directory, ".teslabox-udev-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, name)
}
