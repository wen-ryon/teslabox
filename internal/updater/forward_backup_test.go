package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestForwardBackupComesFromRealInstallAndPreservesOlderPrevious(t *testing.T) {
	cfg, request, pkg := installFixture(t)
	old, _ := os.ReadFile(cfg.TargetPath)
	previous := filepath.Join(cfg.BackupDir, latestBackupName)
	older := []byte("older real program")
	if err := os.WriteFile(previous, older, 0o600); err != nil {
		t.Fatal(err)
	}
	started := false
	err := transactionalInstall(t.Context(), cfg, request, pkg, installOperations{
		stop: func(context.Context) error { return nil },
		start: func(context.Context) error {
			started = true
			actual, err := os.ReadFile(previous)
			if err != nil || !bytes.Equal(actual, old) {
				t.Fatal("启动前没有真实旧程序前进证据", err)
			}
			return nil
		},
	})
	if err != nil || !started {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(cfg.BackupDir, "active", previousBackupBeforeName))
	if !bytes.Equal(before, older) {
		t.Fatal("更旧的 previous 历史丢失")
	}
	metadata, _ := os.ReadFile(filepath.Join(cfg.BackupDir, "active", previousBackupSnapshotName))
	var snapshot fileSnapshot
	if json.Unmarshal(metadata, &snapshot) != nil || !snapshot.Exists {
		t.Fatal("历史元数据缺失")
	}
	sum := sha256.Sum256(older)
	if snapshot.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("历史摘要不是原 previous")
	}
}

func TestForwardBackupSecondInstallPublishesImmediatePredecessor(t *testing.T) {
	cfg, request, pkg := installFixture(t)
	old, _ := os.ReadFile(cfg.TargetPath)
	older := []byte("M0")
	previous := filepath.Join(cfg.BackupDir, latestBackupName)
	if err := os.WriteFile(previous, older, 0o600); err != nil {
		t.Fatal(err)
	}
	ops := installOperations{stop: func(context.Context) error { return nil }, start: func(context.Context) error { return nil }}
	if err := transactionalInstall(t.Context(), cfg, request, pkg, ops); err != nil {
		t.Fatal(err)
	}
	middle, _ := os.ReadFile(cfg.TargetPath)
	third := []byte("M3 next program")
	if err := os.WriteFile(pkg, third, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(third)
	request.SHA256, request.Size = hex.EncodeToString(sum[:]), int64(len(third))
	if err := transactionalInstall(t.Context(), cfg, request, pkg, ops); err != nil {
		t.Fatal(err)
	}
	actual, _ := os.ReadFile(previous)
	if !bytes.Equal(actual, middle) {
		t.Fatal("次轮仍使用更旧的前进证据")
	}
	entries, _ := os.ReadDir(cfg.BackupDir)
	histories := 0
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "active" {
			continue
		}
		histories++
		dir := filepath.Join(cfg.BackupDir, entry.Name())
		main, _ := os.ReadFile(filepath.Join(dir, "before-0"))
		legacy, _ := os.ReadFile(filepath.Join(dir, previousBackupBeforeName))
		if !bytes.Equal(main, old) || !bytes.Equal(legacy, older) {
			t.Fatal("上一轮主程序或更旧previous未整体进入历史")
		}
	}
	if histories != 1 {
		t.Fatal("事务历史数量不正确", histories)
	}
}

func TestForwardBackupDirectorySyncFailureNeverWritesCandidate(t *testing.T) {
	cfg, request, pkg := installFixture(t)
	old, _ := os.ReadFile(cfg.TargetPath)
	previous := filepath.Join(cfg.BackupDir, latestBackupName)
	if err := os.WriteFile(previous, []byte("older"), 0o600); err != nil {
		t.Fatal(err)
	}
	fired, starts := false, 0
	err := transactionalInstallWithSync(t.Context(), cfg, request, pkg, installOperations{
		stop: func(context.Context) error { return nil },
		start: func(context.Context) error {
			starts++
			actual, _ := os.ReadFile(cfg.TargetPath)
			if !bytes.Equal(actual, old) {
				t.Fatal("legacy同步失败仍写入了候选")
			}
			return nil
		},
	}, func(dir string) error {
		if dir == cfg.BackupDir && !fired {
			fired = true
			return errors.New("injected legacy parent sync failure")
		}
		return syncDirectory(dir)
	})
	if err == nil || !fired || starts != 1 {
		t.Fatal("同步失败未阻止候选", err, starts)
	}
	actual, _ := os.ReadFile(cfg.TargetPath)
	if !bytes.Equal(actual, old) {
		t.Fatal("原程序发生变化")
	}
	// 只核对真实恢复器产生的准备事务终态，不手写成功或回滚记录。
	tx, readErr := readTransaction(filepath.Join(cfg.BackupDir, "active"), transactionTargets(cfg))
	if readErr != nil || tx.Candidate != nil {
		t.Fatal("失败前不应有候选快照", readErr)
	}
}

func TestForwardBackupRejectsCorruptBeforeZero(t *testing.T) {
	cfg, _, _ := installFixture(t)
	previous := filepath.Join(cfg.BackupDir, latestBackupName)
	if err := os.WriteFile(previous, []byte("M0"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.BackupDir, "active")
	tx, err := prepareTransaction(dir, transactionTargets(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "before-0"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tx.publishPreviousBackup(dir, cfg, syncDirectory) == nil {
		t.Fatal("篡改before-0仍发布前进证据")
	}
	actual, _ := os.ReadFile(previous)
	if string(actual) != "M0" {
		t.Fatal("拒绝时覆盖了原previous")
	}
}
