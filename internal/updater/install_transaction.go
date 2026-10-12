package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type installOperations struct {
	stop            func(context.Context) error
	start           func(context.Context) error
	resumeUnchanged func(context.Context) error
}

func (ops installOperations) resume(ctx context.Context) error {
	if ops.resumeUnchanged != nil {
		return ops.resumeUnchanged(ctx)
	}
	return ops.start(ctx)
}

func transactionTargets(config HelperConfig) []string {
	state := config.ExportStatePath
	if state == "" {
		state = DefaultExportState
	}
	return []string{config.TargetPath, state}
}

func serviceOperations(config HelperConfig) installOperations {
	return installOperations{
		stop: func(ctx context.Context) error {
			if config.Service != "teslabox.service" {
				return errors.New("未知更新服务")
			}
			if err := checkRunningStatePath(ctx, config); err != nil {
				return err
			}
			out, err := exec.CommandContext(ctx, "systemctl", "stop", config.Service).CombinedOutput()
			if err != nil {
				return fmt.Errorf("停止服务失败: %w: %s", err, strings.TrimSpace(string(out)))
			}
			return serviceStopped(ctx, config.Service)
		},
		start: func(ctx context.Context) error {
			tx, err := readTransaction(filepath.Join(config.BackupDir, "active"), transactionTargets(config))
			if err != nil {
				return err
			}
			return authorizedStart(ctx, config, func() error {
				return restartAndWaitHealthy(ctx, config.Service, config.HealthURL, config.TargetPath, tx.Phase == "ready")
			})
		},
	}
}

func serviceStopped(ctx context.Context, service string) error {
	if service != "teslabox.service" {
		return errors.New("未知更新服务")
	}
	out, err := exec.CommandContext(ctx, "systemctl", "show", "--property=MainPID", "--value", service).Output()
	if err != nil || strings.TrimSpace(string(out)) != "0" {
		return errors.New("主程序仍在运行，拒绝恢复持久状态")
	}
	return nil
}

// RefuseIncompatibleDowngrade 在替换程序前拦截旧版无法读取的状态，保留全部任务。
func RefuseIncompatibleDowngrade(version, statePath string) error {
	v, err := parseReleaseVersion(version)
	if err != nil {
		return err
	}
	if v.core[0] > 0 || v.core[1] >= 2 {
		return nil
	}
	snapshot, data, err := snapshotFile(statePath, 1<<20)
	if err != nil {
		return err
	}
	if !snapshot.Exists {
		return nil
	}
	var state struct {
		Version int `json:"version"`
	}
	if json.Unmarshal(data, &state) != nil {
		return errors.New("无法读取导出状态，拒绝降级")
	}
	if state.Version > 1 || state.Version < 0 {
		return errors.New("当前导出状态需要新版程序，不能无损降级；原数据已保留")
	}
	return nil
}

func transactionalInstall(ctx context.Context, config HelperConfig, request InstallRequest, packagePath string, ops installOperations) error {
	return transactionalInstallWithSync(ctx, config, request, packagePath, ops, syncDirectory)
}

func transactionalInstallWithSync(ctx context.Context, config HelperConfig, request InstallRequest, packagePath string, ops installOperations, syncDir func(string) error) error {
	targets := transactionTargets(config)
	if targets[0] == targets[1] {
		return errors.New("程序与状态路径不能相同")
	}
	for _, target := range targets {
		rel, err := filepath.Rel(config.BackupDir, target)
		if err != nil {
			return err
		}
		if rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return errors.New("事务目标不能位于备份目录中")
		}
	}
	if err := RefuseIncompatibleDowngrade(request.Version, targets[1]); err != nil {
		return err
	}
	for _, path := range append(append([]string{}, targets...), config.BackupDir) {
		if err := plainPath(path); err != nil {
			return err
		}
	}
	dir := filepath.Join(config.BackupDir, "active")
	if commitPending(dir) {
		return errors.New("存在提交隔离标记，需核对失败现场后恢复，禁止覆盖事务")
	}
	if _, err := os.Stat(dir); err == nil {
		tx, err := readTransaction(dir, targets)
		_, journalErr := os.Lstat(filepath.Join(dir, transactionName))
		if err != nil && !errors.Is(journalErr, os.ErrNotExist) {
			return err
		}
		// 记录落盘前绝不替换程序；准备阶段断电留下的目录只归档，不覆盖。
		if tx != nil && tx.Phase != "committed" && tx.Phase != "rolled_back" {
			return errors.New("存在未结束的更新事务，必须先恢复")
		}
		archive := filepath.Join(config.BackupDir, fmt.Sprintf("history-%d", time.Now().UnixNano()))
		if err = os.Rename(dir, archive); err != nil {
			return err
		}
		if err = syncDirectory(config.BackupDir); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	stopCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := ops.stop(stopCtx)
	cancel()
	if err != nil {
		return err
	}
	// 停止期间可能完成最后一次落盘，必须重新检查降级边界。
	if err := RefuseIncompatibleDowngrade(request.Version, targets[1]); err != nil {
		startCtx, done := context.WithTimeout(context.Background(), config.Timeout)
		defer done()
		return errors.Join(err, ops.resume(startCtx))
	}
	tx, err := prepareTransaction(dir, targets)
	if err != nil {
		return recoverPreparationFailure(config, dir, targets, ops, err)
	}
	rollback := func(cause error) error {
		recoveryCtx, done := context.WithTimeout(context.Background(), config.Timeout+30*time.Second)
		defer done()
		if err := ops.stop(recoveryCtx); err != nil {
			return errors.Join(cause, err)
		}
		if err := tx.restoreWithSync(dir, syncDir); err != nil {
			return errors.Join(cause, fmt.Errorf("成对回滚失败，已保留现场: %w", err))
		}
		return errors.Join(cause, ops.start(recoveryCtx))
	}
	tx.Phase = "activating"
	if err = tx.saveWithSync(dir, syncDir); err != nil {
		return rollback(err)
	}
	_, data, err := snapshotFile(packagePath, 128<<20)
	if err != nil {
		return rollback(err)
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != request.Size || hex.EncodeToString(sum[:]) != request.SHA256 {
		return rollback(errors.New("安装前发布物发生变化"))
	}
	if err = durableFile(config.TargetPath, data, 0o755); err != nil {
		return rollback(err)
	}
	candidate, _, err := snapshotFile(config.TargetPath, 128<<20)
	if err != nil {
		return rollback(err)
	}
	if candidate.SHA256 != request.SHA256 {
		return rollback(errors.New("落盘候选校验失败"))
	}
	tx.Candidate = &candidate
	tx.Phase = "ready"
	if err = tx.saveWithSync(dir, syncDir); err != nil {
		return rollback(err)
	}
	activationCtx, done := context.WithTimeout(ctx, config.Timeout)
	err = ops.start(activationCtx)
	done()
	if err != nil {
		return rollback(fmt.Errorf("候选启动或健康检查失败: %w", err))
	}
	// 先持久隔离，再尝试提交；即使提交 rename 后同步失败且候选停不下来，也不能放行业务。
	if err = durableFileWithSync(filepath.Join(dir, commitPendingName), []byte(commitPendingContent), 0o600, syncDir); err != nil {
		return rollback(err)
	}
	tx.Phase = "committed"
	if err = tx.saveWithSync(dir, syncDir); err != nil {
		tx.Phase = "activating"
		return rollback(err)
	}
	// 持久提交是不可逆边界；之后清理失败不能再恢复旧数据，也不能声称已回滚。
	if err = clearCommitPending(dir, syncDir); err != nil {
		return fmt.Errorf("更新已持久提交，但隔离标记清理未确认；未执行回滚，需维护检查: %w", err)
	}
	return nil
}

func recoverPreparationFailure(config HelperConfig, dir string, targets []string, ops installOperations, cause error) error {
	// 记录可能已重命名、但目录同步失败；必须先结束事务，不能带着 prepared 放行旧程序写入。
	journalExists := false
	if _, err := os.Lstat(filepath.Join(dir, transactionName)); err == nil {
		journalExists = true
		tx, err := readTransaction(dir, targets)
		if err != nil {
			return errors.Join(cause, err)
		}
		if err = tx.restore(dir); err != nil {
			return errors.Join(cause, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.Join(cause, err)
	}
	// 没有记录代表仍未替换程序；保留不完整的准备目录供下一轮归档。
	startCtx, done := context.WithTimeout(context.Background(), config.Timeout)
	defer done()
	if journalExists {
		return errors.Join(cause, ops.start(startCtx))
	}
	return errors.Join(cause, ops.resume(startCtx))
}

// RecoverBeforeStart 由 systemd 的启动前守卫执行，未完成恢复时不放行主程序。
func RecoverBeforeStart(ctx context.Context, config HelperConfig) error {
	if runtime.GOOS != "linux" {
		return errors.New("设备恢复仅支持 Linux")
	}
	unlock, err := lockInstallation(config.BackupDir)
	if errors.Is(err, errInstallationBusy) {
		return claimAuthorizedStart(ctx, config)
	}
	if err != nil {
		return err
	}
	defer unlock()
	dir := filepath.Join(config.BackupDir, "active")
	isolated := commitPending(dir)
	if err = validateCommitPending(dir); err != nil {
		return err
	}
	if _, err = os.Lstat(filepath.Join(dir, transactionName)); errors.Is(err, os.ErrNotExist) {
		if isolated {
			return errors.New("隔离事务记录缺失，拒绝自动启动")
		}
		return nil
	} else if err != nil {
		return err
	}
	tx, err := readTransaction(dir, transactionTargets(config))
	if err != nil {
		return err
	}
	if tx.Phase == "committed" && isolated {
		return errors.New("更新提交持久性尚未确认，保留隔离现场，拒绝自动启动")
	}
	if tx.Phase == "committed" || tx.Phase == "rolled_back" && !isolated {
		return nil
	}
	if err = serviceStopped(ctx, config.Service); err != nil {
		return err
	}
	return tx.restore(dir)
}
