package updater

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRestoreRetriesTargetDirectorySync(t *testing.T) {
	for _, exists := range []bool{true, false} {
		name := "remove"
		if exists {
			name = "rename"
		}
		t.Run(name, func(t *testing.T) {
			dir, targets, tx := transactionFixture(t, exists)
			for _, target := range targets {
				if err := durableFile(target, []byte("failed candidate"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			injected := errors.New("target parent fsync failure")
			failTargetSync := func(path string) error {
				if path == filepath.Dir(targets[1]) {
					return injected
				}
				return syncDirectory(path)
			}
			if err := tx.restoreWithSync(dir, failTargetSync); !errors.Is(err, injected) {
				t.Fatalf("未命中恢复 rename/remove 后同步故障: %v", err)
			}
			actual, _, err := snapshotFile(targets[1], 16<<20)
			if err != nil || !sameSnapshot(actual, tx.Files[1].Before) {
				t.Fatalf("注入时恢复操作尚未生效: %+v %v", actual, err)
			}
			// 丢弃内存对象，模拟再次进入恢复；相同内容仍须经过失败的目录屏障。
			loaded, err := readTransaction(dir, targets)
			if err != nil {
				t.Fatal(err)
			}
			if err = loaded.restoreWithSync(dir, failTargetSync); !errors.Is(err, injected) {
				t.Fatalf("重试跳过了尚未完成的目录同步: %v", err)
			}
			loaded, err = readTransaction(dir, targets)
			if err != nil || loaded.Phase != "restoring" {
				t.Fatalf("同步失败错误写入终态: %+v %v", loaded, err)
			}
			if !activationPending(filepath.Join(dir, transactionName)) {
				t.Fatal("恢复未持久完成却放行业务")
			}
			if err = loaded.restore(dir); err != nil {
				t.Fatal(err)
			}
			if activationPending(filepath.Join(dir, transactionName)) {
				t.Fatal("最终恢复成功仍未放行")
			}
		})
	}
}

// 保留真实 rename 后可见的 committed，不能把“保存前失败”当成这项负例。
func installWithCommitSyncFailure(t *testing.T, stopFails bool) (HelperConfig, error) {
	t.Helper()
	cfg, request, pkg := installFixture(t)
	if runtime.GOOS == "linux" {
		unlock, err := lockInstallation(cfg.BackupDir)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
	}
	dir := filepath.Join(cfg.BackupDir, "active")
	stops := 0
	failed := false
	ops := installOperations{
		stop: func(context.Context) error {
			stops++
			if stops == 2 && stopFails {
				return errors.New("candidate refuses to stop")
			}
			return nil
		},
		start: func(context.Context) error {
			if stops == 1 {
				return durableFile(cfg.ExportStatePath, []byte(`{"version":2,"tasks":[]}`), 0o600)
			}
			return nil
		},
	}
	err := transactionalInstallWithSync(t.Context(), cfg, request, pkg, ops, func(path string) error {
		data, readErr := os.ReadFile(filepath.Join(dir, transactionName))
		if path == dir && readErr == nil && bytes.Contains(data, []byte(`"phase":"committed"`)) && !failed {
			failed = true
			return errors.New("committed rename succeeded but directory fsync failed")
		}
		return syncDirectory(path)
	})
	if !failed || stops != 2 || err == nil {
		t.Fatalf("未命中提交后同步故障: failed=%v stops=%d err=%v", failed, stops, err)
	}
	return cfg, err
}

func TestCommitSyncFailureKeepsIsolationWhenStopFails(t *testing.T) {
	cfg, err := installWithCommitSyncFailure(t, true)
	if !strings.Contains(err.Error(), "candidate refuses to stop") {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.BackupDir, "active")
	tx, err := readTransaction(dir, transactionTargets(cfg))
	if err != nil || tx.Phase != "committed" {
		t.Fatalf("必须保留 rename 后 committed 可见的真实失败条件: %+v %v", tx, err)
	}
	if !commitPending(dir) || !activationPending(filepath.Join(dir, transactionName)) {
		t.Fatal("安装锁释放后仍必须由持久隔离标记阻止业务")
	}
	called := false
	err = transactionalInstall(t.Context(), cfg, InstallRequest{Version: "0.2.0-rc.1"}, "unused", installOperations{
		stop: func(context.Context) error { called = true; return nil },
	})
	if err == nil || called {
		t.Fatal("后续安装覆盖了隔离现场")
	}
}

func TestCommitSyncFailureRollsBackWhenStopSucceeds(t *testing.T) {
	cfg, _ := installWithCommitSyncFailure(t, false)
	dir := filepath.Join(cfg.BackupDir, "active")
	tx, err := readTransaction(dir, transactionTargets(cfg))
	if err != nil || tx.Phase != "rolled_back" || commitPending(dir) {
		t.Fatalf("回滚完成后未解除隔离: %+v %v", tx, err)
	}
	data, err := os.ReadFile(cfg.ExportStatePath)
	if err != nil || string(data) != legacyExportState {
		t.Fatalf("旧状态未恢复: %s %v", data, err)
	}
}

func TestCommitCleanupFailureDoesNotRollbackCommittedState(t *testing.T) {
	cfg, request, pkg := installFixture(t)
	dir := filepath.Join(cfg.BackupDir, "active")
	stops := 0
	err := transactionalInstallWithSync(t.Context(), cfg, request, pkg, installOperations{
		stop:  func(context.Context) error { stops++; return nil },
		start: func(context.Context) error { return nil },
	}, func(path string) error {
		data, _ := os.ReadFile(filepath.Join(dir, transactionName))
		if path == dir && bytes.Contains(data, []byte(`"phase":"committed"`)) && !commitPending(dir) {
			return errors.New("cleanup fsync failure")
		}
		return syncDirectory(path)
	})
	if err == nil || !strings.Contains(err.Error(), "已持久提交") || stops != 1 {
		t.Fatalf("清理故障错误尝试回滚或谎报成功: %v stops=%d", err, stops)
	}
	tx, err := readTransaction(dir, transactionTargets(cfg))
	if err != nil || tx.Phase != "committed" {
		t.Fatalf("已持久提交不应被改成失败回滚: %+v %v", tx, err)
	}
	if commitPending(dir) || activationPending(filepath.Join(dir, transactionName)) {
		t.Fatal("删除已可见；不能把同步失败错误当成标记仍在")
	}
	// 模拟未同步的删除在掉电后丢失：已提交状态保留，但重新出现的标记必须阻断自动放行。
	if err = durableFile(filepath.Join(dir, commitPendingName), []byte(commitPendingContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if !activationPending(filepath.Join(dir, transactionName)) {
		t.Fatal("重启后标记重新出现却自动放行")
	}
}

func TestRestoreOriginallyMissingParent(t *testing.T) {
	root := t.TempDir()
	program := filepath.Join(root, "program")
	if err := durableFile(program, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "missing", "state.json")
	dir := filepath.Join(root, "transaction")
	tx, err := prepareTransaction(dir, []string{program, state})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.restore(dir); err != nil {
		t.Fatal("恢复不存在的状态不应创建父目录或永久失败", err)
	}
	if _, err = os.Lstat(filepath.Dir(state)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("恢复不应新增状态目录", err)
	}
}
