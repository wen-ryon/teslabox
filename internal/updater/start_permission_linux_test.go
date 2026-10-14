package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func permissionFixture(t *testing.T) (HelperConfig, *updateTransaction, func()) {
	t.Helper()
	cfg, _, _ := installFixture(t)
	dir, err := os.MkdirTemp("", "tb-upd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cfg.BackupDir = dir
	tx, err := prepareTransaction(filepath.Join(dir, "active"), transactionTargets(cfg))
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := lockInstallation(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	return cfg, tx, unlock
}

func TestBusyStartupRequiresLiveReadyPermission(t *testing.T) {
	for _, phase := range []string{"prepared", "activating", "restoring", "missing", "corrupt", "ready", "rolled_back"} {
		t.Run(phase, func(t *testing.T) {
			cfg, tx, _ := permissionFixture(t)
			dir := filepath.Join(cfg.BackupDir, "active")
			if phase == "ready" {
				c, _, err := snapshotFile(cfg.TargetPath, 128<<20)
				if err != nil {
					t.Fatal(err)
				}
				tx.Candidate = &c
			}
			tx.Phase = phase
			if err := tx.save(dir); err != nil {
				t.Fatal(err)
			}
			if phase == "missing" {
				if err := os.Remove(filepath.Join(dir, transactionName)); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "corrupt" {
				if err := os.WriteFile(filepath.Join(dir, transactionName), []byte("bad"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := RecoverBeforeStart(t.Context(), cfg); err == nil {
				t.Fatal("busy 无许可不能放行")
			}
			if phase == "ready" || phase == "rolled_back" {
				err := authorizedStart(t.Context(), cfg, func() error { return RecoverBeforeStart(t.Context(), cfg) })
				if err != nil {
					t.Fatal("完整组合的单次许可被拒绝", err)
				}
				if err = RecoverBeforeStart(t.Context(), cfg); err == nil {
					t.Fatal("已消费许可仍有效")
				}
			} else if err := authorizedStart(t.Context(), cfg, func() error { t.Fatal("非安全阶段不应启动"); return nil }); err == nil {
				t.Fatal("不安全阶段发出了许可")
			}
		})
	}
}

func TestStartPermissionRejectsChangedJournalAndStaleSocket(t *testing.T) {
	for _, name := range []string{"changed journal", "changed program", "stale socket", "active foreign socket"} {
		t.Run(name, func(t *testing.T) {
			cfg, tx, _ := permissionFixture(t)
			dir := filepath.Join(cfg.BackupDir, "active")
			tx.Phase = "rolled_back"
			if err := tx.save(dir); err != nil {
				t.Fatal(err)
			}
			if name == "changed journal" || name == "changed program" {
				err := authorizedStart(t.Context(), cfg, func() error {
					if name == "changed journal" {
						tx.Phase = "restoring"
						if err := tx.save(dir); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := os.WriteFile(cfg.TargetPath, []byte("unexpected"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					return RecoverBeforeStart(t.Context(), cfg)
				})
				if err == nil {
					t.Fatal("许可交换期间的变化未拦截")
				}
				return
			}
			path := filepath.Join(cfg.BackupDir, "start.sock")
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			listener.SetUnlinkOnClose(false)
			if err = os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			if name == "active foreign socket" {
				if err = authorizedStart(t.Context(), cfg, func() error { return nil }); err == nil {
					t.Fatal("不应移除活动 socket")
				}
				return
			}
			_ = listener.Close()
			if err = RecoverBeforeStart(t.Context(), cfg); err == nil {
				t.Fatal("残留路径不能授权")
			}
			if err = authorizedStart(t.Context(), cfg, func() error { return RecoverBeforeStart(t.Context(), cfg) }); err != nil {
				t.Fatal("安全清理无监听残留后应可启动", err)
			}
		})
	}
}

// 真实 systemd 对照只在显式启用的 GitHub 托管 runner 上运行，其他平台不得操作服务。
func TestUpdaterSystemdGuardProcess(t *testing.T) {
	path := os.Getenv("TESLABOX_TEST_GUARD_CONFIG")
	if path == "" {
		t.Skip("仅供隔离 systemd 子进程")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" {
		t.Fatal("不是隔离 CI runner")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg HelperConfig
	if err = json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if err = RecoverBeforeStart(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
}

func TestUpdaterRealSystemdStartConsumesSinglePermission(t *testing.T) {
	if os.Getenv("TESLABOX_TEST_SYSTEMD") != "1" {
		t.Skip("真实 systemd 仅在显式 CI 步骤启用")
	}
	if os.Geteuid() != 0 || os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" {
		t.Fatal("禁止在非托管 runner 操作服务")
	}
	cfg, tx, _ := permissionFixture(t)
	program, err := os.ReadFile("/bin/sleep")
	if err != nil {
		t.Fatal(err)
	}
	if err = durableFile(cfg.TargetPath, program, 0o755); err != nil {
		t.Fatal(err)
	}
	candidate, _, err := snapshotFile(cfg.TargetPath, 128<<20)
	if err != nil {
		t.Fatal(err)
	}
	tx.Candidate = &candidate
	tx.Phase = "ready"
	if err = tx.save(filepath.Join(cfg.BackupDir, "active")); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(cfg.BackupDir, "guard-config.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = durableFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	testProgram, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	unit := fmt.Sprintf("teslabox-updater-ci-%d-%d.service", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "systemctl", "stop", unit).Run()
		_ = exec.CommandContext(ctx, "systemctl", "reset-failed", unit).Run()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	installed, err := installedProgramIdentity(cfg.TargetPath)
	if err != nil {
		t.Fatal(err)
	}
	execGate := filepath.Join(cfg.BackupDir, "allow-exec")
	err = authorizedStart(ctx, cfg, func() error {
		out, err := exec.CommandContext(ctx, "systemd-run", "--unit="+unit, "--property=Type=simple", "--property=Restart=no",
			"--property=ExecStartPre="+testProgram+" -test.run=^TestUpdaterSystemdGuardProcess$",
			"--property=Environment=GITHUB_ACTIONS=true RUNNER_ENVIRONMENT=github-hosted TESLABOX_TEST_GUARD_CONFIG="+configPath,
			"/bin/sh", "-c", `while ! test -f "$2"; do sleep 0.01; done; exec "$1" 60`, "teslabox-ci", cfg.TargetPath, execGate).CombinedOutput()
		if err != nil {
			return fmt.Errorf("隔离 systemd 启动失败: %w: %s", err, out)
		}
		// 用受控 exec 门闩真实复现 Type=simple 的启动过渡，避免依赖调度时机才能覆盖等待。
		observedTransition := false
		_, err = waitForProgramIdentity(ctx, 5*time.Second, 25*time.Millisecond, func(checkCtx context.Context) (string, error) {
			pid, checkErr := verifyRunningProgramIdentity(checkCtx, unit, cfg.TargetPath, installed)
			if !observedTransition {
				logSystemdProgramIdentity(t, checkCtx, unit, cfg.TargetPath)
				if checkErr == nil || checkErr.Error() != "运行进程不是本次安装的程序" {
					t.Fatalf("exec 门闩打开前必须观测真实错误 inode: pid=%s err=%v", pid, checkErr)
				}
				observedTransition = true
				if err := durableFile(execGate, []byte("allow\n"), 0o600); err != nil {
					return "", err
				}
			}
			return pid, checkErr
		})
		logSystemdProgramIdentity(t, ctx, unit, cfg.TargetPath)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	wrongPath := filepath.Join(cfg.BackupDir, "wrong-program")
	if err := durableFile(wrongPath, program, 0o755); err != nil {
		t.Fatal(err)
	}
	wrongIdentity, err := installedProgramIdentity(wrongPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyRunningProgramIdentity(ctx, unit, cfg.TargetPath, wrongIdentity); err == nil {
		t.Fatal("不能接受内容相同但 inode 不同的程序")
	}
	pid, err := verifyRunningProgram(ctx, unit, cfg.TargetPath)
	if err != nil {
		t.Fatal(err)
	}
	// 单次许可已经关闭。若 start 再执行守卫，这一请求应失败；成功且 PID 不变证明合并语义。
	if out, err := exec.CommandContext(ctx, "systemctl", "start", unit).CombinedOutput(); err != nil {
		t.Fatalf("重复 start: %v: %s", err, out)
	}
	after, err := verifyRunningProgram(ctx, unit, cfg.TargetPath)
	if err != nil || after != pid {
		t.Fatalf("重复 start 改变主进程: %s -> %s: %v", pid, after, err)
	}
}

func logSystemdProgramIdentity(t *testing.T, ctx context.Context, unit, target string) {
	t.Helper()
	pidBytes, err := exec.CommandContext(ctx, "systemctl", "show", unit, "--property=MainPID", "--value").Output()
	if err != nil {
		t.Logf("identity diagnostic MainPID error: %v", err)
		return
	}
	pid := strings.TrimSpace(string(pidBytes))
	exePath := filepath.Join("/proc", pid, "exe")
	exeLink, linkErr := os.Readlink(exePath)
	t.Logf("identity diagnostic MainPID=%s exe=%q readlink_error=%v", pid, exeLink, linkErr)
	for _, path := range []string{target, exePath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Logf("identity diagnostic path=%q stat_error=%v", path, err)
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Logf("identity diagnostic path=%q stat_type=%T", path, info.Sys())
			continue
		}
		t.Logf("identity diagnostic path=%q device=%d inode=%d", path, stat.Dev, stat.Ino)
	}
}
