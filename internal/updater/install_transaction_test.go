package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/media"
)

// 夹具符合正式 0.1.31 的导出结构；保留真实已完成任务，不能用空列表掩盖兼容性。
const legacyExportState = `{"version":1,"settings":{"retain_count":3},"tasks":[{"id":"0123456789abcdef01234567","state":"completed","request":{"mode":"clip","session_id":"legacy-session","layout":"single","cameras":["front"],"quality":"standard"},"created_at":"2026-10-01T12:00:00+08:00","video_start_at":"2026-10-01T11:59:00+08:00","download_ready":true,"output_bytes":12}]}`

type unusedExportRunner struct{}

func (unusedExportRunner) RunProgress(context.Context, string, []string, func(time.Duration)) error {
	return errors.New("验证窗口不应执行转码")
}

func installFixture(t *testing.T) (HelperConfig, InstallRequest, string) {
	t.Helper()
	root := t.TempDir()
	cfg := HelperConfig{TargetPath: filepath.Join(root, "teslabox"), ExportStatePath: filepath.Join(root, "export-state.json"), BackupDir: filepath.Join(root, "backups"), Timeout: time.Second}
	for path, data := range map[string]string{cfg.TargetPath: "old executable", cfg.ExportStatePath: legacyExportState} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(cfg.BackupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(root, "candidate")
	data := []byte("new executable")
	if err := os.WriteFile(pkg, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return cfg, InstallRequest{Version: "0.2.0-rc.1", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}, pkg
}

func TestTransactionalInstallLegacyExporterFailure(t *testing.T) {
	cfg, req, pkg := installFixture(t)
	starts, stops := 0, 0
	var migrated []byte
	err := transactionalInstall(t.Context(), cfg, req, pkg, installOperations{
		stop: func(context.Context) error { stops++; return nil },
		start: func(context.Context) error {
			starts++
			if starts == 2 {
				return nil
			}
			exporter, err := media.NewExporter(unusedExportRunner{}, media.ExportConfig{FFmpegPath: "/opt/ffmpeg", OutputDir: filepath.Join(filepath.Dir(pkg), "exports"), StatePath: cfg.ExportStatePath, QueueSize: 1})
			if err != nil {
				t.Fatal(err)
			}
			if len(exporter.History()) != 1 {
				t.Fatal("旧历史任务未载入")
			}
			if _, err = exporter.UpdateSettings(media.ExportSettings{RetainCount: 3}); err != nil {
				t.Fatal(err)
			}
			migrated, err = os.ReadFile(cfg.ExportStatePath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(migrated, []byte(`"version":2`)) {
				t.Fatal("未通过真实导出器写入 v2")
			}
			return errors.New("injected health failure")
		},
	})
	if err == nil || starts != 2 || stops != 2 {
		t.Fatalf("starts=%d stops=%d err=%v", starts, stops, err)
	}
	restored, err := os.ReadFile(cfg.ExportStatePath)
	if err != nil || string(restored) != legacyExportState {
		t.Fatalf("原状态未逐字节恢复: %v", err)
	}
	program, _ := os.ReadFile(cfg.TargetPath)
	if string(program) != "old executable" {
		t.Fatal("原程序未恢复")
	}
	failed, err := os.ReadFile(filepath.Join(cfg.BackupDir, "active", "failed-1"))
	if err != nil || !bytes.Equal(failed, migrated) {
		t.Fatal("失败状态证据丢失")
	}
	// Linux CI 将这两份产物交给固定旧源码的真实读取器，不能只检查 JSON version。
	if dir := os.Getenv("TESLABOX_ROLLBACK_EVIDENCE"); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, data := range map[string][]byte{"restored.json": restored, "failed-v2.json": failed} {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestTransactionalInstallBoundaries(t *testing.T) {
	for _, name := range []string{"success", "stop failure", "package changed", "state changed on stop", "rollback stop failure", "cancel activation"} {
		t.Run(name, func(t *testing.T) {
			cfg, req, pkg := installFixture(t)
			starts, stops := 0, 0
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ops := installOperations{
				stop: func(context.Context) error {
					stops++
					if name == "stop failure" || name == "rollback stop failure" && stops == 2 {
						return errors.New("stop failed")
					}
					if name == "state changed on stop" {
						return os.WriteFile(cfg.ExportStatePath, []byte(`{"version":2}`), 0o600)
					}
					return nil
				},
				start: func(ctx context.Context) error {
					starts++
					if starts == 1 && name == "rollback stop failure" {
						return errors.New("health failed")
					}
					if starts == 1 && name == "cancel activation" {
						cancel()
						return ctx.Err()
					}
					return nil
				},
			}
			if name == "state changed on stop" {
				req.Version = "0.1.31"
			}
			if name == "package changed" {
				if err := os.WriteFile(pkg, []byte("tampered"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := transactionalInstall(ctx, cfg, req, pkg, ops)
			if (err == nil) != (name == "success") {
				t.Fatalf("unexpected result: %v", err)
			}
			program, _ := os.ReadFile(cfg.TargetPath)
			want := "old executable"
			if name == "success" || name == "rollback stop failure" {
				want = "new executable"
			}
			if string(program) != want {
				t.Fatalf("program=%q want=%q", program, want)
			}
			if name == "rollback stop failure" {
				tx, e := readTransaction(filepath.Join(cfg.BackupDir, "active"), transactionTargets(cfg))
				if e != nil || tx.Phase != "ready" {
					t.Fatal("恢复失败不应标记成功")
				}
			}
		})
	}
}

func TestIncompatibleDowngradeKeepsStateAndDoesNotStop(t *testing.T) {
	cfg, req, pkg := installFixture(t)
	req.Version = "0.1.31"
	data := []byte(`{"version":2,"tasks":[{"state":"running","id":"new-task"}]}`)
	if err := os.WriteFile(cfg.ExportStatePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	ops := installOperations{stop: func(context.Context) error { t.Fatal("不能停止服务"); return nil }}
	if err := transactionalInstall(t.Context(), cfg, req, pkg, ops); err == nil {
		t.Fatal("不兼容降级未拦截")
	}
	got, _ := os.ReadFile(cfg.ExportStatePath)
	if !bytes.Equal(got, data) {
		t.Fatal("不兼容数据被改写")
	}
	if _, err := os.Stat(filepath.Join(cfg.BackupDir, "active")); !os.IsNotExist(err) {
		t.Fatal("拒绝路径不应创建事务")
	}
}

func TestTransactionRejectsStateInsideBackupBeforeStop(t *testing.T) {
	cfg, req, pkg := installFixture(t)
	cfg.ExportStatePath = filepath.Join(cfg.BackupDir, "install.lock")
	err := transactionalInstall(t.Context(), cfg, req, pkg, installOperations{stop: func(context.Context) error { t.Fatal("应在停止前拦截冲突路径"); return nil }})
	if err == nil {
		t.Fatal("状态不能覆盖安装锁")
	}
}

func TestUpdatePageRejectsDowngradeBeforeRequest(t *testing.T) {
	cfg, _, pkg := installFixture(t)
	if err := os.WriteFile(cfg.ExportStatePath, []byte(`{"version":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	request := filepath.Join(filepath.Dir(pkg), "request.json")
	starter := &fakeStarter{}
	m := &Manager{config: Config{ExportStatePath: cfg.ExportStatePath, RequestPath: request}, client: &Client{}, starter: starter, manifest: &Manifest{Version: "0.1.31"}, packagePath: pkg, state: Status{State: StateDownloaded}}
	if _, err := m.Install(t.Context()); err == nil {
		t.Fatal("页面安装请求未拦截")
	}
	if starter.started.Load() {
		t.Fatal("不兼容降级不能启动安装助手")
	}
	if _, err := os.Stat(request); !os.IsNotExist(err) {
		t.Fatal("不兼容降级不应写安装请求")
	}
}

func TestPreparationFailureTerminatesJournalBeforeRestart(t *testing.T) {
	for _, name := range []string{"before journal", "after journal rename", "corrupt journal"} {
		t.Run(name, func(t *testing.T) {
			cfg, _, _ := installFixture(t)
			dir := filepath.Join(cfg.BackupDir, "active")
			if name != "before journal" {
				if _, err := prepareTransaction(dir, transactionTargets(cfg)); err != nil {
					t.Fatal(err)
				}
				if name == "corrupt journal" {
					if err := os.WriteFile(filepath.Join(dir, transactionName), []byte("corrupt"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			started := false
			ops := installOperations{start: func(context.Context) error {
				started = true
				if name == "after journal rename" {
					tx, err := readTransaction(dir, transactionTargets(cfg))
					if err != nil || tx.Phase != "rolled_back" {
						t.Fatal("重启旧程序之前事务尚未结束")
					}
				}
				return nil
			}}
			if err := recoverPreparationFailure(cfg, dir, transactionTargets(cfg), ops, errors.New("injected fsync error")); err == nil {
				t.Fatal("准备失败不能变成成功")
			}
			if started != (name != "corrupt journal") {
				t.Fatalf("started=%v", started)
			}
		})
	}
}
