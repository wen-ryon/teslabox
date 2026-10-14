//go:build linux && integration

package updater

import (
	"os"
	"path/filepath"
	"testing"
)

func TestForwardGoPublishedBeforeCandidatePowerLossKeepsOldComponent(t *testing.T) {
	cfg, _, _, python := carPlayForwardFixture(t)
	dir := filepath.Join(cfg.BackupDir, "active")
	tx, err := prepareTransaction(dir, transactionTargets(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.publishPreviousBackup(dir, cfg, syncDirectory); err != nil {
		t.Fatal(err)
	}
	// 发布已经持久，candidate 尚未替换；断电恢复仍是 M1，本机组件必须保留。
	if err := python("recover_forward"); err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(filepath.Join(cfg.BackupDir, latestBackupName))
	if err != nil || string(previous) != "M1 actual previous main" {
		t.Fatal("真实准备备份未发布", err)
	}
	if err := tx.restore(dir); err != nil {
		t.Fatal(err)
	}
	if err := python("recover_forward"); err != nil {
		t.Fatal(err)
	}
}
