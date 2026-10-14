package updater

import (
	"encoding/json"
	"errors"
	"path/filepath"
)

const previousBackupBeforeName = "previous-backup.before"
const previousBackupSnapshotName = "previous-backup.snapshot.json"

// publishPreviousBackup 兼容已部署的旧组件恢复钩子，前进证据只能来自本轮真实程序备份。
func (tx *updateTransaction) publishPreviousBackup(dir string, config HelperConfig, syncDir func(string) error) error {
	if tx == nil || tx.Version != 1 || tx.Phase != "prepared" || len(tx.Files) == 0 ||
		!tx.Files[0].Before.Exists || tx.Files[0].Target != config.TargetPath ||
		dir != filepath.Join(config.BackupDir, "active") {
		return errors.New("前进备份不属于当前已准备的程序事务")
	}
	before, data, err := snapshotFile(filepath.Join(dir, "before-0"), 128<<20)
	if err != nil {
		return err
	}
	if !before.Exists || before.SHA256 != tx.Files[0].Before.SHA256 {
		return errors.New("前进备份与原程序快照不一致")
	}
	current, _, err := snapshotFile(config.TargetPath, 128<<20)
	if err != nil {
		return err
	}
	if !sameSnapshot(current, tx.Files[0].Before) {
		return errors.New("发布前进证据前原程序已发生变化")
	}
	previous := filepath.Join(config.BackupDir, latestBackupName)
	old, oldData, err := snapshotFile(previous, 128<<20)
	if err != nil {
		return err
	}
	// 既有事务格式保持不变；旧 previous 的原字节和元数据随 active 整体进入历史目录。
	for _, name := range []string{previousBackupBeforeName, previousBackupSnapshotName} {
		existing, _, err := snapshotFile(filepath.Join(dir, name), 128<<20)
		if err != nil {
			return err
		}
		if existing.Exists {
			return errors.New("本轮前进备份已存在，拒绝覆盖历史")
		}
	}
	if old.Exists {
		if err := durableFileWithSync(filepath.Join(dir, previousBackupBeforeName), oldData, 0o600, syncDir); err != nil {
			return err
		}
	}
	metadata, err := json.Marshal(old)
	if err != nil {
		return err
	}
	if err := durableFileWithSync(filepath.Join(dir, previousBackupSnapshotName), metadata, 0o600, syncDir); err != nil {
		return err
	}
	// 必须在任何候选程序替换前完成 rename 与目录同步，不能仅有内存中的旧摘要。
	return durableFileWithSync(previous, data, 0o600, syncDir)
}
