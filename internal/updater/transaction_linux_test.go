package updater

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTransactionLinuxLockAndMetadata(t *testing.T) {
	root := t.TempDir()
	unlock, err := lockInstallation(root)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if !installationActive(root) {
		t.Fatal("安装持锁时不能放行新任务")
	}
	if release, err := lockInstallation(root); !errors.Is(err, errInstallationBusy) {
		if release != nil {
			release()
		}
		t.Fatalf("未阻止并发安装: %v", err)
	}
	for _, name := range []string{"hardlink", "writable", "other owner"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state")
			if err := os.WriteFile(path, []byte("private"), 0o600); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "hardlink":
				if err := os.Link(path, path+".link"); err != nil {
					t.Fatal(err)
				}
			case "writable":
				if err := os.Chmod(path, 0o666); err != nil {
					t.Fatal(err)
				}
			case "other owner":
				if os.Geteuid() != 0 {
					t.Skip("所有权反例需 root CI")
				}
				if err := os.Chown(path, 65534, 65534); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := snapshotFile(path, 100); err == nil {
				t.Fatal("不安全文件被接受")
			}
		})
	}
}

func TestRecoverBeforeStartAfterInterruptedActivation(t *testing.T) {
	cfg, _, _ := installFixture(t)
	tx, err := prepareTransaction(filepath.Join(cfg.BackupDir, "active"), transactionTargets(cfg))
	if err != nil {
		t.Fatal(err)
	}
	tx.Phase = "activating"
	if err = tx.save(filepath.Join(cfg.BackupDir, "active")); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cfg.TargetPath, []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cfg.ExportStatePath, []byte(`{"version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 只替换 systemctl 查询为临时夹具，绝不控制 CI 主机服务。
	bin := t.TempDir()
	script := filepath.Join(bin, "systemctl")
	if err = os.WriteFile(script, []byte("#!/bin/sh\n[ \"$1\" = show ] || exit 9\nprintf '0\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	cfg.Service = "teslabox.service"
	if err = RecoverBeforeStart(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(cfg.ExportStatePath)
	if string(data) != legacyExportState {
		t.Fatal("启动前未恢复旧状态")
	}
	if err = RecoverBeforeStart(t.Context(), cfg); err != nil {
		t.Fatal("恢复必须幂等", err)
	}
}

func TestCommitSyncAndStopFailureRejectsStartAfterUnlock(t *testing.T) {
	cfg, _ := installWithCommitSyncFailure(t, true)
	if installationActive(cfg.BackupDir) {
		t.Fatal("负例必须在助手释放安装锁后验证")
	}
	if !activationPending(filepath.Join(cfg.BackupDir, "active", transactionName)) {
		t.Fatal("释放安装锁后误放行业务")
	}
	if err := RecoverBeforeStart(t.Context(), cfg); err == nil {
		t.Fatal("提交持久性不明时错误放行启动")
	}
}

func TestRecoverNonterminalCommitIsolation(t *testing.T) {
	for _, phase := range []string{"ready", "activating", "restoring", "rolled_back"} {
		t.Run(phase, func(t *testing.T) {
			cfg, _, _ := installFixture(t)
			dir := filepath.Join(cfg.BackupDir, "active")
			tx, err := prepareTransaction(dir, transactionTargets(cfg))
			if err != nil {
				t.Fatal(err)
			}
			if phase != "rolled_back" {
				if err = durableFile(cfg.TargetPath, []byte("candidate"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err = durableFile(cfg.ExportStatePath, []byte(`{"version":2}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			tx.Phase = phase
			if err = tx.save(dir); err != nil {
				t.Fatal(err)
			}
			if err = durableFile(filepath.Join(dir, commitPendingName), []byte(commitPendingContent), 0o600); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			if err = os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\n[ \"$1\" = show ] || exit 9\nprintf '0\\n'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			cfg.Service = "teslabox.service"
			if err = RecoverBeforeStart(t.Context(), cfg); err != nil {
				t.Fatal("非提交阶段应自动恢复，不应全部转为人工维护", err)
			}
			if commitPending(dir) || activationPending(filepath.Join(dir, transactionName)) {
				t.Fatal("成功恢复后未解除隔离")
			}
			data, err := os.ReadFile(cfg.ExportStatePath)
			if err != nil || string(data) != legacyExportState {
				t.Fatal("没有配对恢复旧状态", err)
			}
		})
	}
}

func TestRecoverRejectsInvalidCommitIsolation(t *testing.T) {
	for _, name := range []string{"missing journal", "broken journal", "directory marker", "symlink marker", "broken marker"} {
		t.Run(name, func(t *testing.T) {
			cfg, _, _ := installFixture(t)
			dir := filepath.Join(cfg.BackupDir, "active")
			if _, err := prepareTransaction(dir, transactionTargets(cfg)); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(dir, commitPendingName)
			if err := durableFile(marker, []byte(commitPendingContent), 0o600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch name {
			case "missing journal":
				err = os.Remove(filepath.Join(dir, transactionName))
			case "broken journal":
				err = durableFile(filepath.Join(dir, transactionName), []byte("broken"), 0o600)
			case "directory marker", "symlink marker":
				if err = os.Remove(marker); err != nil {
					t.Fatal(err)
				}
				if name == "directory marker" {
					err = os.Mkdir(marker, 0o700)
				} else {
					err = os.Symlink(filepath.Join(dir, "missing"), marker)
				}
			case "broken marker":
				err = durableFile(marker, []byte("broken"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !activationPending(filepath.Join(dir, transactionName)) {
				t.Fatal("无效隔离状态错误放行业务")
			}
			if err = RecoverBeforeStart(t.Context(), cfg); err == nil {
				t.Fatal("无效隔离状态错误放行启动")
			}
		})
	}
}
