//go:build linux && integration

package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/media"
)

func carPlayForwardFixture(t *testing.T) (HelperConfig, InstallRequest, string, func(string) error) {
	t.Helper()
	root := t.TempDir()
	cfg := HelperConfig{TargetPath: filepath.Join(root, "usr/local/bin/teslabox"), ExportStatePath: filepath.Join(root, "export-state.json"), BackupDir: filepath.Join(root, "var/lib/teslabox-system/update-backups"), Timeout: time.Second}
	for _, dir := range []string{filepath.Dir(cfg.TargetPath), cfg.BackupDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, value := range map[string]string{cfg.TargetPath: "M1 actual previous main", cfg.ExportStatePath: legacyExportState, filepath.Join(cfg.BackupDir, latestBackupName): "M0 older previous"} {
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pkg := filepath.Join(root, "candidate")
	candidate := []byte("M2 actual candidate main")
	if err := os.WriteFile(pkg, candidate, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(candidate)
	request := InstallRequest{Version: "0.2.0-rc.1", Size: int64(len(candidate)), SHA256: hex.EncodeToString(sum[:])}
	source := os.Getenv("TESLABOX_CARPLAY_OTA_TEST_ROOT")
	if source == "" {
		var err error
		source, err = filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(source, "internal/updater/testdata/carplay_forward_fixture.py")
	if _, err := os.Stat(script); err != nil {
		t.Fatal("联动测试源码路径未就绪", err)
	}
	run := func(action string) error {
		ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "python3", script, root, source, action).CombinedOutput()
		t.Logf("Python %s: %s", action, out)
		return err
	}
	if err := run("setup"); err != nil {
		t.Fatal(err)
	}
	return cfg, request, pkg, run
}

func TestForwardGoInstallRetainsCarPlayThroughOldRecoveryAndNewActivation(t *testing.T) {
	cfg, request, pkg, python := carPlayForwardFixture(t)
	// 正例从真实 Go 安装路径产生 previous，夹具只设置更旧初态，不能人工补 M1 证据。
	err := transactionalInstall(t.Context(), cfg, request, pkg, installOperations{
		stop: func(context.Context) error { return nil },
		start: func(context.Context) error {
			if err := python("recover_forward"); err != nil {
				return err
			}
			return python("activate")
		},
	})
	if err != nil {
		t.Fatal("Go安装未通过旧Python恢复与新配置继承联动", err)
	}
	tx, err := readTransaction(filepath.Join(cfg.BackupDir, "active"), transactionTargets(cfg))
	if err != nil || tx.Phase != "committed" {
		t.Fatal("真实Go安装事务未提交", err)
	}
}

func TestForwardGoFailureRestoresMainStateAndActualCarPlayConfiguration(t *testing.T) {
	cfg, request, pkg, python := carPlayForwardFixture(t)
	before, _ := os.ReadFile(cfg.TargetPath)
	starts := 0
	err := transactionalInstall(t.Context(), cfg, request, pkg, installOperations{
		stop: func(context.Context) error { return nil },
		start: func(context.Context) error {
			starts++
			if starts == 1 {
				if err := python("recover_forward"); err != nil {
					return err
				}
				if err := python("activate"); err != nil {
					return err
				}
				exporter, err := media.NewExporter(unusedExportRunner{}, media.ExportConfig{FFmpegPath: "/opt/ffmpeg", OutputDir: filepath.Join(filepath.Dir(pkg), "exports"), StatePath: cfg.ExportStatePath, QueueSize: 1})
				if err != nil {
					return err
				}
				if _, err = exporter.UpdateSettings(media.ExportSettings{RetainCount: 3}); err != nil {
					return err
				}
				migrated, _ := os.ReadFile(cfg.ExportStatePath)
				if !bytes.Contains(migrated, []byte(`"version":2`)) {
					t.Fatal("没有实际导出状态迁移")
				}
				return errors.New("injected candidate health failure")
			}
			return python("recover_rollback")
		},
	})
	if err == nil || starts != 2 {
		t.Fatal("候选失败未执行真实回滚", err, starts)
	}
	after, _ := os.ReadFile(cfg.TargetPath)
	state, _ := os.ReadFile(cfg.ExportStatePath)
	if !bytes.Equal(before, after) || string(state) != legacyExportState {
		t.Fatal("程序/状态未成对恢复")
	}
	tx, readErr := readTransaction(filepath.Join(cfg.BackupDir, "active"), transactionTargets(cfg))
	if readErr != nil || tx.Phase != "rolled_back" {
		t.Fatal("回滚不是由真实Go事务完成", readErr)
	}
}

func TestForwardGoPreparedPowerLossNeverRemovesOldComponent(t *testing.T) {
	cfg, _, _, python := carPlayForwardFixture(t)
	dir := filepath.Join(cfg.BackupDir, "active")
	tx, err := prepareTransaction(dir, transactionTargets(cfg))
	if err != nil {
		t.Fatal(err)
	}
	// 准备后、发布前的断点仍运行 M1，旧恢复器不能据此恢复更早的空组件。
	if err := python("recover_forward"); err != nil {
		t.Fatal(err)
	}
	if err := tx.restore(dir); err != nil {
		t.Fatal(err)
	}
	if err := python("recover_forward"); err != nil {
		t.Fatal(err)
	}
}
