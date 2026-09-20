package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
)

const spaceRecoveryRearmDelay = time.Minute

// 空间监测不取得 refreshMu，避免扫描或修复占住锁时错过预留线。
type spaceGuardState struct {
	mu                sync.Mutex
	blocked           bool
	cancel            context.CancelFunc
	criticalCount     int
	incident          bool
	recoveryAttempted bool
	markerPresent     bool
	markerBoot        string
	healthySince      time.Time
	marker            string
	device            string
	leases            map[uint64]context.CancelFunc
	nextLease         uint64
	readFailed        bool
	reboot            func(context.Context) error
}

// ConfigureSpaceRecovery 必须在 Run 前设置；持久化标记使同一次事故最多提交一次自动重启。
func (m *Manager) ConfigureSpaceRecovery(marker string, reboot func(context.Context) error) {
	m.spaceGuard.mu.Lock()
	defer m.spaceGuard.mu.Unlock()
	m.spaceGuard.marker = marker
	m.spaceGuard.reboot = reboot
	boot, err := loadSpaceRecoveryBoot(marker)
	if err != nil && !os.IsNotExist(err) {
		m.spaceGuard.markerPresent = true
		m.logger.Warn("读取空间恢复标记失败，将保留标记避免循环重启", "error", err)
		return
	}
	m.spaceGuard.markerPresent = err == nil
	m.spaceGuard.markerBoot = boot
}

func (m *Manager) watchSnapshotSpace(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	m.spaceGuard.mu.Lock()
	m.spaceGuard.cancel = cancel
	if m.spaceGuard.blocked {
		cancel()
	}
	m.spaceGuard.mu.Unlock()
	return ctx, func() {
		m.spaceGuard.mu.Lock()
		m.spaceGuard.cancel = nil
		m.spaceGuard.mu.Unlock()
		cancel()
	}
}

func (m *Manager) runSpaceGuard(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkSpaceGuard(ctx)
		}
	}
}

func (m *Manager) checkSpaceGuard(ctx context.Context) {
	// 只取身份和运行状态，不能每秒复制上万条录像索引。
	m.stateMu.RLock()
	state := Snapshot{DataDrive: m.state.DataDrive, Backing: m.state.Backing, GadgetStatus: m.state.GadgetStatus, GadgetPaused: m.state.GadgetPaused}
	m.stateMu.RUnlock()
	if state.DataDrive == nil || !state.Backing.Ready {
		return
	}
	space, err := m.snapshotter.Space()
	if err != nil || space.TotalBytes == 0 {
		m.spaceGuard.mu.Lock()
		firstFailure := !m.spaceGuard.readFailed
		m.spaceGuard.readFailed = true
		m.spaceGuard.blocked = true
		m.spaceGuard.criticalCount = 0
		if m.spaceGuard.cancel != nil {
			m.spaceGuard.cancel()
		}
		m.spaceGuard.mu.Unlock()
		if firstFailure {
			m.logger.Warn("无法确认外层剩余空间，暂停新媒体任务，不执行自动重启", "error", err)
		}
		return
	}
	m.spaceGuard.mu.Lock()
	m.spaceGuard.readFailed = false
	device := deviceIndexKey(state)
	if m.spaceGuard.device != device {
		m.spaceGuard.device = device
		m.spaceGuard.criticalCount = 0
		m.spaceGuard.incident = false
	}
	wasBlocked := m.spaceGuard.blocked
	m.spaceGuard.blocked = !space.Ready
	if !space.Ready && m.spaceGuard.cancel != nil {
		m.spaceGuard.cancel()
	}
	// 在真正耗尽之前取消媒体工作；只通知退出，仍等待其释放租约后才卸载。
	if !space.Ready && space.FreeBytes < max(uint64(1<<30), space.RequiredBytes/4) {
		for _, cancel := range m.spaceGuard.leases {
			cancel()
		}
	}
	if state.GadgetStatus.Bound && !state.GadgetPaused && space.FreeBytes < 256<<20 {
		m.spaceGuard.criticalCount++
		if m.spaceGuard.criticalCount >= 3 || space.FreeBytes < 16<<20 {
			m.spaceGuard.incident = true
		}
	} else {
		m.spaceGuard.criticalCount = 0
	}
	incident := m.spaceGuard.incident
	m.spaceGuard.mu.Unlock()
	m.stateMu.Lock()
	if deviceIndexKey(m.state) == device {
		m.state.SnapshotSpace = space
	}
	m.stateMu.Unlock()
	m.rearmSpaceRecovery(state, space)
	if !space.Ready {
		if !wasBlocked {
			m.logger.Error("外层空间保护已触发，暂停索引并回收可释放快照", "free_bytes", space.FreeBytes, "required_bytes", space.RequiredBytes)
		}
		if m.refreshMu.TryLock() {
			operationCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := m.protectSnapshotSpaceLocked(operationCtx); err != nil {
				m.logger.Warn("独立空间保护回收失败", "error", err)
			}
			m.cleanupRetired(operationCtx)
			cancel()
			m.refreshMu.Unlock()
		}
	}
	if incident && space.Ready {
		m.recoverSpaceIncident(ctx, space)
	}
}

func (m *Manager) recoverSpaceIncident(ctx context.Context, space mediasnapshot.SpaceStatus) {
	if !m.refreshMu.TryLock() {
		return
	}
	defer m.refreshMu.Unlock()
	state := m.Snapshot()
	if state.DataDrive == nil || state.GadgetPaused || !state.GadgetStatus.Bound || m.hasMediaLeases() {
		return
	}
	if m.maintenanceLocker != nil {
		lockCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		release, err := m.maintenanceLocker.Acquire(lockCtx)
		if err != nil {
			return
		}
		defer release()
	}
	// 清理后再次读取，不以早先采样推断当前仍有余量。
	current, err := m.snapshotter.Space()
	if err != nil || !current.Ready || current.FreeBytes < 1<<30 {
		return
	}
	m.spaceGuard.mu.Lock()
	defer m.spaceGuard.mu.Unlock()
	if m.spaceGuard.markerPresent || m.spaceGuard.recoveryAttempted || m.spaceGuard.reboot == nil || m.spaceGuard.marker == "" {
		return
	}
	if m.spaceGuard.device != deviceIndexKey(state) {
		return
	}
	if err := persistSpaceRecovery(m.spaceGuard.marker, fmt.Sprintf("boot=%s free_bytes=%d\n", m.bootID, current.FreeBytes)); err != nil {
		m.logger.Error("未提交空间恢复重启：一次性保护标记不可创建", "error", err)
		return
	}
	m.spaceGuard.markerPresent = true
	m.spaceGuard.recoveryAttempted = true
	m.spaceGuard.markerBoot = m.bootID
	m.logger.Error("外层空间恢复安全余量，提交一次温重启恢复车端枚举", "free_bytes", space.FreeBytes)
	if err := m.spaceGuard.reboot(ctx); err != nil {
		m.logger.Error("空间恢复重启提交失败，保留标记避免重复重启", "error", err)
	}
}

// rearmSpaceRecovery 只在新一轮系统启动稳定后清除旧标记，避免服务重启或启动抖动形成循环重启。
func (m *Manager) rearmSpaceRecovery(state Snapshot, space mediasnapshot.SpaceStatus) {
	m.spaceGuard.mu.Lock()
	defer m.spaceGuard.mu.Unlock()
	if !m.spaceGuard.markerPresent || m.spaceGuard.marker == "" || m.spaceGuard.markerBoot == "" || m.bootID == "" || m.spaceGuard.markerBoot == m.bootID {
		m.spaceGuard.healthySince = time.Time{}
		return
	}
	if !space.Ready || !state.GadgetStatus.Bound || state.GadgetPaused {
		m.spaceGuard.healthySince = time.Time{}
		return
	}
	now := time.Now()
	if m.now != nil {
		now = m.now()
	}
	if m.spaceGuard.healthySince.IsZero() {
		m.spaceGuard.healthySince = now
		return
	}
	if now.Sub(m.spaceGuard.healthySince) < spaceRecoveryRearmDelay {
		return
	}
	if err := removeSpaceRecovery(m.spaceGuard.marker); err != nil {
		m.logger.Warn("清除上一轮空间恢复标记失败，继续禁止自动重启", "error", err)
		return
	}
	m.logger.Info("新启动已稳定运行，重新启用外层空间自动恢复", "previous_boot", m.spaceGuard.markerBoot)
	m.spaceGuard.markerPresent = false
	m.spaceGuard.markerBoot = ""
	m.spaceGuard.healthySince = time.Time{}
	m.spaceGuard.recoveryAttempted = false
}

func persistSpaceRecovery(path, evidence string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.WriteString(evidence)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if runtime.GOOS == "linux" {
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		defer dir.Close()
		return dir.Sync()
	}
	return nil
}

func loadSpaceRecoveryBoot(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(data) > 4096 {
		return "", fmt.Errorf("空间恢复标记过大")
	}
	for _, field := range strings.Fields(string(data)) {
		if boot, ok := strings.CutPrefix(field, "boot="); ok && boot != "" {
			return boot, nil
		}
	}
	return "", fmt.Errorf("空间恢复标记缺少 boot")
}

func removeSpaceRecovery(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if runtime.GOOS != "linux" {
		return nil
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
