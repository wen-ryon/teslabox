// Package service 协调数据盘、backing store、Gadget 和后续快照生命周期。
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wen-ryon/teslabox/internal/backing"
	"github.com/wen-ryon/teslabox/internal/config"
	"github.com/wen-ryon/teslabox/internal/gadget"
	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/storage"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

var (
	ErrDataDriveDisconnected = errors.New("活动数据盘已掉线")
	ErrSnapshotUnavailable   = errors.New("当前状态不能创建媒体快照")
)

const (
	gadgetEnumerationWait        = 5 * time.Second
	gadgetBootBindingWindow      = 60 * time.Second
	dataDriveConfirmationWait    = 500 * time.Millisecond
	fallbackSnapshotScanInterval = 30 * time.Second
	minimumSnapshotScanInterval  = time.Minute
	fallbackSnapshotActiveWindow = 2 * time.Minute
)

// Phase 是 TeslaUSB 核心状态机对外暴露的稳定状态。
type Phase string

const (
	PhaseUnknown               Phase = "UNKNOWN"
	PhaseStarting              Phase = "STARTING"
	PhaseWaitingForDataDrive   Phase = "WAITING_FOR_DATA_DRIVE"
	PhasePreparingBackingStore Phase = "PREPARING_BACKING_STORE"
	PhaseReadyForGadget        Phase = "READY_FOR_GADGET"
	PhaseGadgetActive          Phase = "GADGET_ACTIVE"
	PhaseSnapshotCreating      Phase = "SNAPSHOT_CREATING"
	PhaseSnapshotReady         Phase = "SNAPSHOT_READY"
	PhaseDegraded              Phase = "DEGRADED"
	PhaseFault                 Phase = "FAULT"
)

// BackingInspector 定义状态机所需的只读 backing store 检查能力。
type BackingInspector interface {
	Inspect(device storage.Device) (backing.Status, error)
}

// GadgetProber 定义状态机所需的只读 UDC 和 configfs 检查能力。
type GadgetProber interface {
	Probe() (gadget.Environment, error)
}

// DataMounter 定义已确认身份的数据盘自动挂载能力。
type DataMounter interface {
	Mount(ctx context.Context, device storage.Device) error
	Unmount(ctx context.Context) error
}

// GadgetLifecycle 定义状态机统一管理的 Gadget 生命周期能力。
type GadgetLifecycle interface {
	Inspect() (gadget.Status, error)
	Start() (gadget.Status, error)
	Stop() (gadget.Status, error)
}

// SnapshotLifecycle 定义独立快照的创建和安全回收能力。
type SnapshotLifecycle interface {
	Recover(ctx context.Context) error
	Create(ctx context.Context) (mediasnapshot.View, error)
	Remove(ctx context.Context, view mediasnapshot.View) error
	Space() (mediasnapshot.SpaceStatus, error)
}

// VideoScanner 定义只从已只读挂载的快照建立媒体索引的能力。
type VideoScanner interface {
	Scan(root string) ([]teslacam.Video, error)
}

// Snapshot 是数据盘、backing store、Gadget 和媒体索引的一致性只读视图。
type Snapshot struct {
	Phase               Phase                     `json:"phase"`
	DataDrive           *storage.Device           `json:"data_drive,omitempty"`
	Backing             backing.Status            `json:"backing"`
	Gadget              gadget.Environment        `json:"gadget"`
	GadgetStatus        gadget.Status             `json:"gadget_status"`
	GadgetPaused        bool                      `json:"gadget_paused"`
	GadgetResumePending bool                      `json:"gadget_resume_pending"`
	MediaSnapshot       *mediasnapshot.View       `json:"media_snapshot,omitempty"`
	MediaVolumeSpace    mediasnapshot.VolumeSpace `json:"media_volume_space"`
	MediaVolumeSpaceAt  *time.Time                `json:"media_volume_space_updated_at,omitempty"`
	VideoCount          int                       `json:"video_count"`
	Videos              []teslacam.Video          `json:"-"`
	LastRefreshAt       *time.Time                `json:"last_refresh_at,omitempty"`
	LastError           string                    `json:"last_error,omitempty"`
	SnapshotError       string                    `json:"snapshot_error,omitempty"`
	SnapshotSpace       mediasnapshot.SpaceStatus `json:"snapshot_space"`
	SnapshotFailures    int                       `json:"snapshot_failures,omitempty"`
	SnapshotRetryAt     *time.Time                `json:"snapshot_retry_at,omitempty"`
}

// Manager 串行化 udev、轮询和 API 刷新，防止未来生命周期操作并发执行。
type Manager struct {
	cfg         config.Config
	detector    storage.Detector
	inspector   BackingInspector
	prober      GadgetProber
	mounter     DataMounter
	lifecycle   GadgetLifecycle
	snapshotter SnapshotLifecycle
	scanner     VideoScanner
	logger      *slog.Logger

	refreshMu           sync.Mutex
	stateMu             sync.RWMutex
	state               Snapshot
	triggerCh           chan struct{}
	cleanupCh           chan struct{}
	retired             []mediasnapshot.View
	mediaLeases         map[string]int
	mediaActiveUntil    time.Time
	snapshotRecovered   bool
	snapshotFailures    int
	snapshotRetryAt     time.Time
	lastSnapshotAt      time.Time
	gadgetPaused        bool
	gadgetResumePending bool
	bootID              string
	bindAttemptAllowed  bool
	bindOwnerRecorded   bool
	bindWindowEnforced  bool
	bindWindowEndsAt    time.Time
	startedAt           time.Time
	bootUptime          time.Duration
	bootUptimeKnown     bool
	waitForEnumeration  func(context.Context, gadget.Status) (gadget.Status, bool, error)
	waitForDriveConfirm func(context.Context) error
	now                 func() time.Time
}

// NewManager 使用手工依赖注入保持系统边界可测试，初始状态不触发任何磁盘写入。
func NewManager(
	cfg config.Config,
	detector storage.Detector,
	inspector BackingInspector,
	prober GadgetProber,
	mounter DataMounter,
	lifecycle GadgetLifecycle,
	snapshotter SnapshotLifecycle,
	scanner VideoScanner,
	logger *slog.Logger,
) *Manager {
	bootID, bootErr := readBootID(cfg.BootIDPath)
	bootUptime, bindWindowEnforced, uptimeErr := readBootUptime(cfg.BootUptimePath)
	guard, guardErr := loadGadgetGuardState(cfg.GadgetPauseStatePath)
	gadgetPaused, resumePending, bindAttemptAllowed := resolveGadgetGuard(bootID, guard, errors.Join(bootErr, guardErr))
	if (bootErr != nil || guardErr != nil) && logger != nil {
		// 无法证明这是新的系统启动时必须保持解绑，避免服务重启被误当成冷启动。
		logger.Warn("读取 USB Gadget 启动保护失败，将保持安全断开", "error", errors.Join(bootErr, guardErr))
	}
	now := time.Now()
	var bindWindowEndsAt time.Time
	if bindWindowEnforced && uptimeErr == nil && bootUptime < gadgetBootBindingWindow {
		bindWindowEndsAt = now.Add(gadgetBootBindingWindow - bootUptime)
	}
	if uptimeErr != nil && logger != nil {
		// 无法确认仍处于冷启动阶段时禁止新绑定，但不能因此主动拆除已经工作的 Gadget。
		logger.Warn("读取系统启动时长失败，将禁止新建 USB Gadget 绑定", "error", uptimeErr)
	}
	manager := &Manager{
		cfg:                 cfg,
		detector:            detector,
		inspector:           inspector,
		prober:              prober,
		mounter:             mounter,
		lifecycle:           lifecycle,
		snapshotter:         snapshotter,
		scanner:             scanner,
		logger:              logger,
		state:               Snapshot{Phase: PhaseStarting, GadgetPaused: gadgetPaused, GadgetResumePending: resumePending},
		triggerCh:           make(chan struct{}, 1),
		cleanupCh:           make(chan struct{}, 1),
		mediaLeases:         make(map[string]int),
		gadgetPaused:        gadgetPaused,
		gadgetResumePending: resumePending,
		bootID:              bootID,
		bindAttemptAllowed:  bindAttemptAllowed,
		bindOwnerRecorded:   guard.exists && guard.mode == gadgetGuardService && guard.bootID == bootID,
		bindWindowEnforced:  bindWindowEnforced,
		bindWindowEndsAt:    bindWindowEndsAt,
		startedAt:           now,
		bootUptime:          bootUptime,
		bootUptimeKnown:     bindWindowEnforced && uptimeErr == nil,
		now:                 time.Now,
	}
	manager.waitForEnumeration = manager.pollGadgetEnumeration
	manager.waitForDriveConfirm = waitForDataDriveConfirmation
	return manager
}

func resolveGadgetGuard(bootID string, guard gadgetGuardState, loadErr error) (paused, resumePending, bindAttemptAllowed bool) {
	if loadErr != nil || bootID == "" {
		return true, false, false
	}
	if !guard.exists {
		return false, false, true
	}
	switch guard.mode {
	case gadgetGuardPaused:
		return true, false, false
	case gadgetGuardAttempt:
		if guard.bootID == bootID {
			// 已消费绑定机会不等于人工暂停；服务重启后应保留仍然健康的既有绑定。
			return false, false, false
		}
		return false, false, true
	case gadgetGuardService:
		if guard.bootID == bootID {
			// 同一开机周期的后续服务进程只能接管既有绑定，不能创建新的 UDC 绑定。
			return false, false, false
		}
		return false, false, true
	case gadgetGuardResumeAfter:
		if guard.bootID == bootID {
			return true, true, false
		}
		return false, false, true
	default:
		return true, false, false
	}
}

// Run 通过低频轮询串行管理数据盘、快照与 Gadget 生命周期，避免事件风暴并发触碰 UDC。
func (m *Manager) Run(ctx context.Context) {
	m.refreshWithTimeout(ctx)
	pollTicker := time.NewTicker(m.cfg.PollInterval)
	defer pollTicker.Stop()
	scanTicker := time.NewTicker(m.cfg.ScanInterval)
	defer scanTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-pollTicker.C:
			m.refreshWithTimeout(ctx)
		case <-scanTicker.C:
			m.snapshotWithTimeout(ctx)
		case <-m.triggerCh:
			m.refreshWithTimeout(ctx)
		case <-m.cleanupCh:
			m.cleanupWithTimeout(ctx)
		}
	}
}

// TriggerRefresh 合并密集 udev 事件，避免事件风暴造成重复系统检查。
func (m *Manager) TriggerRefresh() {
	select {
	case m.triggerCh <- struct{}{}:
	default:
	}
}

// Refresh 供 API 在请求上下文内触发一次完整的生命周期收敛。
func (m *Manager) Refresh(ctx context.Context) error {
	return m.reconcile(ctx)
}

// PauseGadget 在用户确认物理 OTG 已断开后保持 UDC 解绑，避免旧内核残留状态阻止更新。
func (m *Manager) PauseGadget(ctx context.Context) (Snapshot, error) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	if err := ctx.Err(); err != nil {
		return m.Snapshot(), err
	}

	previous := m.Snapshot()
	if err := persistGadgetGuardState(m.cfg.GadgetPauseStatePath, gadgetGuardPaused); err != nil {
		return previous, err
	}
	m.gadgetPaused = true
	m.gadgetResumePending = false
	m.bindAttemptAllowed = false
	status, err := m.lifecycle.Stop()
	now := m.now()
	previous.GadgetPaused = true
	previous.GadgetResumePending = false
	previous.GadgetStatus = status
	previous.LastRefreshAt = &now
	if err != nil {
		previous.Phase = PhaseFault
		previous.LastError = fmt.Sprintf("安全断开 USB Gadget 失败: %v", err)
		m.store(previous)
		return m.Snapshot(), fmt.Errorf("安全断开 USB Gadget: %w", err)
	}
	previous.Phase = PhaseReadyForGadget
	previous.LastError = ""
	m.store(previous)
	return m.Snapshot(), nil
}

// ResumeGadget 只预约下一次冷启动恢复，避免旧 BSP 内核运行时重新绑定 UDC 导致系统挂死。
func (m *Manager) ResumeGadget(ctx context.Context) (Snapshot, error) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	if err := ctx.Err(); err != nil {
		return m.Snapshot(), err
	}
	if err := persistGadgetGuardState(m.cfg.GadgetPauseStatePath, gadgetGuardResumeAfter+m.bootID); err != nil {
		return m.Snapshot(), err
	}
	m.gadgetPaused = true
	m.gadgetResumePending = true
	m.bindAttemptAllowed = false
	status, stopErr := m.lifecycle.Stop()
	state := m.Snapshot()
	state.GadgetPaused = true
	state.GadgetResumePending = true
	state.GadgetStatus = status
	state.Phase = PhaseReadyForGadget
	state.LastError = ""
	now := m.now()
	state.LastRefreshAt = &now
	if stopErr != nil {
		state.Phase = PhaseFault
		state.LastError = fmt.Sprintf("准备下次冷启动前保持 Gadget 解绑失败: %v", stopErr)
	}
	m.store(state)
	if stopErr != nil {
		return m.Snapshot(), stopErr
	}
	return m.Snapshot(), nil
}

// BeginStorageMaintenance 独占状态机并释放 Gadget、快照和数据盘挂载，避免重新分区时仍有内核引用。
func (m *Manager) BeginStorageMaintenance(ctx context.Context) (func(), error) {
	return m.beginMaintenance(ctx, true)
}

// BeginMediaMaintenance 在写入虚拟盘内媒体前释放 Gadget 和快照，但保持 XFS 数据盘挂载以保留受控暂存文件。
func (m *Manager) BeginMediaMaintenance(ctx context.Context) (func(), error) {
	return m.beginMaintenance(ctx, false)
}

func (m *Manager) beginMaintenance(ctx context.Context, unmountDataDrive bool) (func(), error) {
	m.refreshMu.Lock()
	release := sync.OnceFunc(func() { m.refreshMu.Unlock() })
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	if m.hasMediaLeases() {
		release()
		return nil, errors.New("仍有录像播放或导出请求正在使用数据盘")
	}
	if err := persistGadgetGuardState(m.cfg.GadgetPauseStatePath, gadgetGuardPaused); err != nil {
		release()
		return nil, err
	}
	m.gadgetPaused = true
	m.gadgetResumePending = false
	m.bindAttemptAllowed = false
	previous := m.Snapshot()
	status, stopErr := m.lifecycle.Stop()
	cleanupErr := m.cleanupDisconnectedSnapshots(ctx, previous)
	var unmountErr error
	if unmountDataDrive {
		unmountErr = m.mounter.Unmount(ctx)
	}
	now := m.now()
	state := previous
	state.Phase = PhaseReadyForGadget
	if unmountDataDrive {
		state.DataDrive = nil
	}
	state.GadgetStatus = status
	state.GadgetPaused = true
	state.GadgetResumePending = false
	state.MediaSnapshot = nil
	state.VideoCount = 0
	state.Videos = nil
	state.LastRefreshAt = &now
	state.LastError = ""
	if cause := errors.Join(stopErr, cleanupErr, unmountErr); cause != nil {
		state.Phase = PhaseFault
		state.LastError = fmt.Sprintf("准备存储维护失败: %v", cause)
		m.store(state)
		release()
		return nil, cause
	}
	m.store(state)
	return release, nil
}

func (m *Manager) hasMediaLeases() bool {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	for _, count := range m.mediaLeases {
		if count > 0 {
			return true
		}
	}
	return false
}

// Scan 创建新的只读快照，并在扫描成功后原子替换对外媒体索引。
func (m *Manager) Scan(ctx context.Context) error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	m.extendMediaActivityLocked(m.now())
	return m.refreshSnapshotLocked(ctx)
}

// ActivateMedia 在首次进入录像浏览时立即刷新，并在短时间内恢复高频快照。
func (m *Manager) ActivateMedia(ctx context.Context) error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	now := m.now()
	wasActive := now.Before(m.mediaActiveUntil)
	m.extendMediaActivityLocked(now)
	state := m.Snapshot()
	snapshotFresh := state.MediaSnapshot != nil &&
		!m.lastSnapshotAt.IsZero() &&
		now.Sub(m.lastSnapshotAt) < m.snapshotScanInterval()
	if wasActive || snapshotFresh {
		return nil
	}
	return m.refreshSnapshotLocked(ctx)
}

// Snapshot 返回防御性拷贝，避免 API 修改内部设备和媒体状态。
func (m *Manager) Snapshot() Snapshot {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return cloneSnapshot(m.state)
}

// AcquireMediaSnapshot 把媒体视图借给一次请求，防止播放期间的定时轮换卸载底层 loop 设备。
func (m *Manager) AcquireMediaSnapshot() (Snapshot, func()) {
	m.stateMu.Lock()
	result := cloneSnapshot(m.state)
	leaseID := ""
	if result.MediaSnapshot != nil {
		leaseID = result.MediaSnapshot.ID
		m.mediaLeases[leaseID]++
	}
	m.stateMu.Unlock()

	var once sync.Once
	return result, func() {
		once.Do(func() {
			if leaseID == "" {
				return
			}
			m.stateMu.Lock()
			if m.mediaLeases[leaseID] <= 1 {
				delete(m.mediaLeases, leaseID)
			} else {
				m.mediaLeases[leaseID]--
			}
			m.stateMu.Unlock()
			select {
			case m.cleanupCh <- struct{}{}:
			default:
			}
		})
	}
}

// AcquireMediaSnapshotView 为只读业务提供当前媒体快照，并在调用方完成后释放租约。
// 通过租约避免媒体中心扫描时快照被定时任务提前卸载。
func (m *Manager) AcquireMediaSnapshotView() (*mediasnapshot.View, func()) {
	state, release := m.AcquireMediaSnapshot()
	return state.MediaSnapshot, release
}

func cloneSnapshot(source Snapshot) Snapshot {
	result := source
	result.Videos = slices.Clone(source.Videos)
	result.Backing.Missing = slices.Clone(source.Backing.Missing)
	result.Gadget.UDCNames = slices.Clone(source.Gadget.UDCNames)
	result.GadgetStatus.OtherBindings = slices.Clone(source.GadgetStatus.OtherBindings)
	if source.MediaSnapshot != nil {
		view := *source.MediaSnapshot
		result.MediaSnapshot = &view
	}
	if source.MediaVolumeSpaceAt != nil {
		updatedAt := *source.MediaVolumeSpaceAt
		result.MediaVolumeSpaceAt = &updatedAt
	}
	if source.SnapshotRetryAt != nil {
		retryAt := *source.SnapshotRetryAt
		result.SnapshotRetryAt = &retryAt
	}
	if source.DataDrive != nil {
		device := *source.DataDrive
		device.MountPoints = slices.Clone(source.DataDrive.MountPoints)
		result.DataDrive = &device
	}
	return result
}

// Close 在服务退出前有序停止 Gadget，避免 LUN 继续引用无人管理的 backing file。
func (m *Manager) Close(ctx context.Context) error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	var result error
	if _, err := m.lifecycle.Stop(); err != nil {
		result = errors.Join(result, fmt.Errorf("停止 TeslaBox Gadget: %w", err))
	}
	state := m.Snapshot()
	views := slices.Clone(m.retired)
	if state.MediaSnapshot != nil {
		views = append(views, *state.MediaSnapshot)
	}
	for _, view := range views {
		if m.snapshotLeased(view.ID) {
			result = errors.Join(result, fmt.Errorf("TeslaCam 快照 %s 仍被媒体请求使用", view.ID))
			continue
		}
		if err := m.snapshotter.Remove(ctx, view); err != nil {
			result = errors.Join(result, fmt.Errorf("清理 TeslaCam 快照 %s: %w", view.ID, err))
		}
	}
	return result
}

func (m *Manager) snapshotWithTimeout(parent context.Context) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	ctx, cancel := context.WithTimeout(parent, m.cfg.SnapshotTimeout)
	defer cancel()
	now := m.now()
	if err := m.releaseIdleMediaSnapshotLocked(ctx, now); err != nil && !isContextTermination(ctx, err) {
		m.logger.Warn("释放空闲 TeslaCam 媒体快照失败", "error", err)
		m.cleanupRetired(ctx)
		return
	}
	m.cleanupRetired(ctx)
	if !m.shouldRunAutomaticSnapshotLocked(now) {
		return
	}
	if err := m.refreshSnapshotLocked(ctx); err != nil && !errors.Is(err, ErrSnapshotUnavailable) && !isContextTermination(ctx, err) {
		m.logger.Warn("刷新 TeslaCam 媒体快照失败", "error", err)
	}
}

func (m *Manager) refreshWithTimeout(parent context.Context) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	ctx, cancel := context.WithTimeout(parent, m.cfg.OperationTimeout)
	defer cancel()
	if err := m.reconcileLocked(ctx); err != nil &&
		!errors.Is(err, storage.ErrDeviceNotFound) &&
		!errors.Is(err, ErrDataDriveDisconnected) &&
		!isContextTermination(ctx, err) {
		m.logger.Warn("收敛 TeslaUSB 运行状态失败", "error", err)
	}
}

func (m *Manager) cleanupWithTimeout(parent context.Context) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	timeout := m.cfg.OperationTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	if err := m.releaseIdleMediaSnapshotLocked(ctx, m.now()); err != nil && !isContextTermination(ctx, err) {
		m.logger.Warn("释放空闲 TeslaCam 媒体快照失败", "error", err)
	}
	m.cleanupRetired(ctx)
}

func (m *Manager) reconcile(ctx context.Context) error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	return m.reconcileLocked(ctx)
}

func (m *Manager) reconcileLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.bindAttemptAllowed && !m.bindOwnerRecorded {
		if err := persistGadgetGuardState(m.cfg.GadgetPauseStatePath, gadgetGuardService+m.bootID); err != nil {
			m.bindAttemptAllowed = false
			return fmt.Errorf("保存 USB Gadget 服务启动保护: %w", err)
		}
		m.bindOwnerRecorded = true
	}
	previous := m.Snapshot()
	initialGadgetStatus, err := m.lifecycle.Inspect()
	if err != nil {
		return m.storeTransientFault(previous, fmt.Errorf("读取 USB Gadget 状态: %w", err))
	}
	previous.GadgetStatus = initialGadgetStatus
	attemptRecorded := false
	attemptCompleted := initialGadgetStatus.Bound && initialGadgetStatus.EnumerationComplete
	if !m.gadgetPaused && !initialGadgetStatus.Bound && !m.bindAttemptAllowed {
		// 相同 boot_id 已消费过绑定机会，服务重启或页面操作都不得再次触碰 UDC。
		m.gadgetPaused = true
	}
	defer func() {
		if !attemptRecorded || attemptCompleted {
			return
		}
		m.gadgetPaused = true
		m.gadgetResumePending = false
		state := m.Snapshot()
		state.GadgetPaused = true
		state.GadgetResumePending = false
		m.store(state)
	}()

	device, err := m.detector.Detect(ctx)
	if errors.Is(err, storage.ErrDeviceNotFound) && initialGadgetStatus.Bound {
		// 活动 Gadget 的 backing store 不能因一次 lsblk 瞬时空结果被解绑，短暂确认后再判定物理掉盘。
		if waitErr := m.waitForDriveConfirm(ctx); waitErr != nil {
			return fmt.Errorf("确认活动数据盘状态: %w", waitErr)
		}
		device, err = m.detector.Detect(ctx)
	}
	if err != nil {
		if errors.Is(err, storage.ErrDeviceNotFound) {
			return m.handleMissingDataDrive(ctx, previous)
		}
		cause := fmt.Errorf("检测数据盘: %w", err)
		if isContextTermination(ctx, err) {
			return cause
		}
		return m.storeTransientFault(previous, cause)
	}

	if len(device.MountPoints) == 0 && stringsEqualFold(device.FSType, "xfs") {
		if err := m.mounter.Mount(ctx, device); err != nil {
			if isContextTermination(ctx, err) {
				return fmt.Errorf("挂载数据盘: %w", err)
			}
			return m.stopForFault(&device, err)
		}
		device, err = m.detector.Detect(ctx)
		if err != nil {
			cause := fmt.Errorf("挂载后重新检测数据盘: %w", err)
			if isContextTermination(ctx, err) {
				return cause
			}
			return m.storeTransientFault(previous, cause)
		}
		if len(device.MountPoints) == 0 {
			return m.stopForFault(&device, errors.New("挂载命令成功后数据盘仍未显示挂载点"))
		}
	}

	backingStatus, err := m.inspector.Inspect(device)
	if err != nil {
		return m.stopForFault(&device, err)
	}
	if !backingStatus.Ready {
		gadgetStatus, stopErr := m.lifecycle.Stop()
		if stopErr != nil {
			return m.stopForFault(&device, fmt.Errorf("backing store 未就绪且停止 Gadget 失败: %w", stopErr))
		}
		now := time.Now()
		m.store(Snapshot{
			Phase:               PhasePreparingBackingStore,
			DataDrive:           &device,
			Backing:             backingStatus,
			GadgetStatus:        gadgetStatus,
			GadgetPaused:        m.gadgetPaused,
			GadgetResumePending: m.gadgetResumePending,
			LastRefreshAt:       &now,
			LastError:           backingStatus.Reason,
		}, previous)
		return nil
	}
	gadgetEnvironment, err := m.prober.Probe()
	if err != nil {
		return m.storeTransientFault(previous, fmt.Errorf("检查 Gadget 环境: %w", err))
	}
	if !gadgetEnvironment.Ready {
		gadgetStatus, stopErr := m.lifecycle.Stop()
		if stopErr != nil {
			return m.stopForFault(&device, fmt.Errorf("Gadget 环境未就绪且停止失败: %w", stopErr))
		}
		now := time.Now()
		m.store(Snapshot{
			Phase:               PhaseDegraded,
			DataDrive:           &device,
			Backing:             backingStatus,
			Gadget:              gadgetEnvironment,
			GadgetStatus:        gadgetStatus,
			GadgetPaused:        m.gadgetPaused,
			GadgetResumePending: m.gadgetResumePending,
			LastRefreshAt:       &now,
			LastError:           gadgetEnvironment.Reason,
		}, previous)
		return nil
	}
	bindingWindowExpired := false
	if !initialGadgetStatus.Bound && !m.gadgetPaused && m.bindAttemptAllowed &&
		m.bindWindowEnforced && (m.bindWindowEndsAt.IsZero() || !m.now().Before(m.bindWindowEndsAt)) {
		if err := persistGadgetGuardState(m.cfg.GadgetPauseStatePath, gadgetGuardAttempt+m.bootID); err != nil {
			return m.stopForFault(nil, fmt.Errorf("保存 USB Gadget 冷启动绑定窗口状态: %w", err))
		}
		m.bindAttemptAllowed = false
		m.gadgetPaused = true
		m.gadgetResumePending = false
		bindingWindowExpired = true
	}
	if m.gadgetPaused {
		gadgetStatus, stopErr := m.lifecycle.Stop()
		if stopErr != nil {
			return m.stopForFault(&device, fmt.Errorf("保持 USB Gadget 安全断开失败: %w", stopErr))
		}
		now := m.now()
		state := Snapshot{
			Phase:               PhaseReadyForGadget,
			DataDrive:           &device,
			Backing:             backingStatus,
			Gadget:              gadgetEnvironment,
			GadgetStatus:        gadgetStatus,
			GadgetPaused:        true,
			GadgetResumePending: m.gadgetResumePending,
			LastRefreshAt:       &now,
		}
		if bindingWindowExpired {
			state.LastError = "已超过本次冷启动的 USB Gadget 安全绑定窗口；请预先插好 OTG 线后冷启动"
		}
		m.store(state, previous)
		return nil
	}

	gadgetStatus := initialGadgetStatus
	startedNow := false
	if !gadgetStatus.Bound {
		if err := persistGadgetGuardState(m.cfg.GadgetPauseStatePath, gadgetGuardAttempt+m.bootID); err != nil {
			return m.stopForFault(nil, fmt.Errorf("保存 USB Gadget 本次启动绑定保护: %w", err))
		}
		m.bindAttemptAllowed = false
		attemptRecorded = true
		gadgetStatus, err = m.lifecycle.Start()
		if err != nil {
			return m.stopForFault(&device, fmt.Errorf("启动 TeslaBox Gadget: %w", err))
		}
		startedNow = true
	}
	if !gadgetStatus.Bound {
		return m.stopForFault(&device, errors.New("Gadget 启动完成但没有绑定 UDC"))
	}
	if startedNow {
		var enumerationComplete bool
		gadgetStatus, enumerationComplete, err = m.waitForEnumeration(ctx, gadgetStatus)
		if err != nil {
			if isContextTermination(ctx, err) {
				return fmt.Errorf("等待 USB 主机枚举: %w", err)
			}
			return m.stopForFault(&device, fmt.Errorf("等待 USB 主机枚举: %w", err))
		}
		if !enumerationComplete {
			gadgetStatus, stopErr := m.lifecycle.Stop()
			if stopErr != nil {
				return m.stopForFault(&device, fmt.Errorf("未完成枚举后安全解绑 Gadget: %w", stopErr))
			}
			m.gadgetPaused = true
			m.gadgetResumePending = false
			now := m.now()
			m.store(Snapshot{
				Phase:               PhaseReadyForGadget,
				DataDrive:           &device,
				Backing:             backingStatus,
				Gadget:              gadgetEnvironment,
				GadgetStatus:        gadgetStatus,
				GadgetPaused:        true,
				GadgetResumePending: false,
				LastRefreshAt:       &now,
				LastError:           "本次开机未检测到 USB 主机完成枚举，已安全解绑；请预先插好 OTG 线后冷启动",
			}, previous)
			return nil
		}
	} else if !gadgetStatus.EnumerationComplete {
		// 已绑定设备可能因车机复位暂时离开 configured；运行期解绑会让旧 BSP 无法安全恢复。
		now := m.now()
		m.store(Snapshot{
			Phase:               PhaseDegraded,
			DataDrive:           &device,
			Backing:             backingStatus,
			Gadget:              gadgetEnvironment,
			GadgetStatus:        gadgetStatus,
			GadgetPaused:        false,
			GadgetResumePending: false,
			LastRefreshAt:       &now,
			LastError:           "USB 主机正在重新枚举，TeslaBox 保持 Gadget 绑定并等待恢复",
		}, previous)
		return nil
	}
	attemptCompleted = true
	now := m.now()
	if startedNow {
		startupElapsed := now.Sub(m.startedAt)
		attributes := []any{
			"manager_elapsed_ms", startupElapsed.Milliseconds(),
			"udc", gadgetStatus.UDC,
			"current_speed", gadgetStatus.CurrentSpeed,
		}
		if m.bootUptimeKnown {
			attributes = append(attributes, "kernel_uptime_ms", (m.bootUptime + startupElapsed).Milliseconds())
		}
		m.logger.Info("TeslaUSB Gadget 已完成本次冷启动绑定和枚举", attributes...)
	}
	state := Snapshot{
		Phase:               PhaseGadgetActive,
		DataDrive:           &device,
		Backing:             backingStatus,
		Gadget:              gadgetEnvironment,
		GadgetStatus:        gadgetStatus,
		GadgetPaused:        false,
		GadgetResumePending: false,
		LastRefreshAt:       &now,
	}
	m.store(state, previous)
	return nil
}

func waitForDataDriveConfirmation(ctx context.Context) error {
	timer := time.NewTimer(dataDriveConfirmationWait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (m *Manager) pollGadgetEnumeration(ctx context.Context, initial gadget.Status) (gadget.Status, bool, error) {
	if initial.EnumerationComplete {
		return initial, true, nil
	}
	timer := time.NewTimer(gadgetEnumerationWait)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	status := initial
	for {
		select {
		case <-ctx.Done():
			return status, false, ctx.Err()
		case <-timer.C:
			return status, false, nil
		case <-ticker.C:
			var err error
			status, err = m.lifecycle.Inspect()
			if err != nil {
				return status, false, err
			}
			if status.EnumerationComplete {
				return status, true, nil
			}
		}
	}
}

// canRefreshMediaSnapshot 只允许在车机持有 LUN，或 Gadget 已明确暂停并解绑时读取原镜像。
// 后一种状态不会重新绑定 UDC，因而既避免与 USB 主机并发写入，也保持 ROCK 3A 的启动期安全限制。
func canRefreshMediaSnapshot(state Snapshot) bool {
	if !state.Backing.Ready {
		return false
	}

	switch state.Phase {
	case PhaseGadgetActive:
		return state.GadgetStatus.Bound
	case PhaseSnapshotReady, PhaseDegraded:
		return state.GadgetStatus.Bound || (state.GadgetPaused && !state.GadgetStatus.Bound)
	case PhaseReadyForGadget:
		return state.GadgetPaused && !state.GadgetStatus.Bound
	default:
		return false
	}
}

func (m *Manager) refreshSnapshotLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	startedAt := time.Now()

	previous := m.Snapshot()
	if !canRefreshMediaSnapshot(previous) {
		return ErrSnapshotUnavailable
	}

	creating := previous
	creating.Phase = PhaseSnapshotCreating
	creating.LastError = ""
	m.store(creating)

	if !m.snapshotRecovered {
		if err := m.snapshotter.Recover(ctx); err != nil {
			return m.storeSnapshotFailure(previous, fmt.Errorf("回收异常断电遗留快照: %w", err))
		}
		m.snapshotRecovered = true
	}
	space, err := m.snapshotter.Space()
	if err != nil {
		return m.storeSnapshotFailure(previous, fmt.Errorf("检查快照可用空间: %w", err))
	}
	if !space.Ready {
		if cleanupErr := m.cleanupRetiredUntil(ctx, 0); cleanupErr != nil {
			return m.storeSnapshotFailure(previous, errors.Join(
				&mediasnapshot.InsufficientSpaceError{Status: space},
				fmt.Errorf("空间不足时回收旧快照: %w", cleanupErr),
			))
		}
		space, err = m.snapshotter.Space()
		if err != nil {
			return m.storeSnapshotFailure(previous, fmt.Errorf("回收旧快照后检查可用空间: %w", err))
		}
	}
	if !space.Ready {
		return m.storeSnapshotFailure(previous, &mediasnapshot.InsufficientSpaceError{Status: space})
	}

	view, err := m.snapshotter.Create(ctx)
	if err != nil {
		return m.storeSnapshotFailure(previous, fmt.Errorf("创建 TeslaCam 快照: %w", err))
	}
	videos, err := m.scanner.Scan(view.MountPath)
	if err != nil {
		cleanupErr := m.snapshotter.Remove(ctx, view)
		cause := fmt.Errorf("扫描 TeslaCam 快照: %w", err)
		if cleanupErr != nil {
			cause = errors.Join(cause, fmt.Errorf("清理不可用快照: %w", cleanupErr))
		}
		return m.storeSnapshotFailure(previous, cause)
	}

	ready := previous
	ready.Phase = PhaseSnapshotReady
	ready.MediaSnapshot = &view
	ready.MediaVolumeSpace = view.VolumeSpace
	ready.Videos = slices.Clone(videos)
	ready.VideoCount = len(videos)
	ready.LastError = ""
	ready.SnapshotError = ""
	ready.SnapshotSpace = space
	ready.SnapshotFailures = 0
	ready.SnapshotRetryAt = nil
	now := m.now()
	ready.MediaVolumeSpaceAt = &now
	ready.LastRefreshAt = &now
	m.store(ready)
	m.snapshotFailures = 0
	m.snapshotRetryAt = time.Time{}
	m.lastSnapshotAt = now
	elapsed := time.Since(startedAt)
	if view.RepairApplied {
		m.logger.Warn(
			"TeslaCam 快照只读挂载失败后已修复副本",
			"snapshot_id", view.ID,
			"duration_ms", elapsed.Milliseconds(),
			"video_count", len(videos),
		)
	} else if elapsed >= 2*time.Second {
		m.logger.Info(
			"TeslaCam 快照耗时偏高",
			"snapshot_id", view.ID,
			"duration_ms", elapsed.Milliseconds(),
			"video_count", len(videos),
		)
	}

	if previous.MediaSnapshot != nil && previous.MediaSnapshot.ID != view.ID {
		m.retired = append(m.retired, *previous.MediaSnapshot)
	}
	m.cleanupRetired(ctx)
	return nil
}

func (m *Manager) storeSnapshotFailure(previous Snapshot, cause error) error {
	m.snapshotFailures++
	delay := snapshotRetryDelay(m.snapshotScanInterval(), m.cfg.SnapshotRetryMax, m.snapshotFailures)
	m.snapshotRetryAt = m.now().Add(delay)
	degraded := previous
	degraded.Phase = PhaseDegraded
	degraded.LastError = cause.Error()
	degraded.SnapshotError = cause.Error()
	degraded.SnapshotFailures = m.snapshotFailures
	retryAt := m.snapshotRetryAt
	degraded.SnapshotRetryAt = &retryAt
	if spaceError, ok := errors.AsType[*mediasnapshot.InsufficientSpaceError](cause); ok {
		degraded.SnapshotSpace = spaceError.Status
	}
	now := m.now()
	degraded.LastRefreshAt = &now
	m.store(degraded)
	return cause
}

func snapshotRetryDelay(interval, maximum time.Duration, failures int) time.Duration {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if maximum <= 0 {
		maximum = 10 * time.Minute
	}
	if interval >= maximum {
		return maximum
	}
	delay := interval
	for attempt := 1; attempt < failures; attempt++ {
		if delay >= maximum-delay {
			return maximum
		}
		delay *= 2
	}
	return delay
}

func (m *Manager) cleanupRetired(ctx context.Context) {
	if err := m.cleanupRetiredUntil(ctx, 0); err != nil {
		m.logger.Warn("回收旧 TeslaCam 快照失败", "error", err)
	}
}

func (m *Manager) shouldRunAutomaticSnapshotLocked(now time.Time) bool {
	if !m.snapshotRetryAt.IsZero() && now.Before(m.snapshotRetryAt) {
		return false
	}
	if m.hasMediaLeases() {
		// 轮换被播放或导出的 reflink 会同时保留新旧两份共享视图，车机随后覆盖写时会制造更多 COW extent。
		// 显式扫描仍可由用户触发；后台任务等待租约释放后再更新索引。
		return false
	}
	state := m.Snapshot()
	if state.MediaSnapshot == nil || m.lastSnapshotAt.IsZero() {
		// 启动后没有浏览请求时不扫描 96GB/更大镜像，避免无意义的磁盘唤醒和车端 I/O 抖动。
		return now.Before(m.mediaActiveUntil)
	}
	return now.Before(m.mediaActiveUntil) && now.Sub(m.lastSnapshotAt) >= m.snapshotScanInterval()
}

// releaseIdleMediaSnapshotLocked 在媒体页面停止活动后解除 reflink 共享，避免车机覆盖写持续产生 COW extent。
// 调用方必须持有 refreshMu；状态摘除和租约检查在同一个 stateMu 临界区完成，防止新请求借到待删除视图。
func (m *Manager) releaseIdleMediaSnapshotLocked(ctx context.Context, now time.Time) error {
	if now.Before(m.mediaActiveUntil) {
		return nil
	}

	m.stateMu.Lock()
	state := m.state
	if state.MediaSnapshot == nil || !state.GadgetStatus.Bound || m.mediaLeases[state.MediaSnapshot.ID] > 0 {
		m.stateMu.Unlock()
		return nil
	}
	view := *state.MediaSnapshot
	previousPhase := state.Phase
	state.MediaSnapshot = nil
	if state.Phase == PhaseSnapshotReady {
		state.Phase = PhaseGadgetActive
	}
	m.state = state
	m.lastSnapshotAt = time.Time{}
	m.snapshotFailures = 0
	m.snapshotRetryAt = time.Time{}
	m.stateMu.Unlock()

	if previousPhase != state.Phase {
		m.logger.Info("TeslaUSB 状态已切换", "from", previousPhase, "to", state.Phase)
	}
	if err := m.snapshotter.Remove(ctx, view); err != nil {
		m.retired = append(m.retired, view)
		return fmt.Errorf("删除空闲 TeslaCam 快照 %s: %w", view.ID, err)
	}
	m.logger.Info("媒体浏览已空闲，已释放 TeslaCam reflink 快照", "snapshot_id", view.ID, "video_count", state.VideoCount)
	return nil
}

func (m *Manager) extendMediaActivityLocked(now time.Time) {
	m.mediaActiveUntil = now.Add(m.snapshotActiveWindow())
}

func (m *Manager) snapshotScanInterval() time.Duration {
	interval := m.cfg.ScanInterval
	if interval <= 0 {
		interval = fallbackSnapshotScanInterval
	}
	// TeslaCam 文件按分钟滚动，小于一分钟只会重复克隆同一批数据并增加 COW 写放大。
	return max(interval, minimumSnapshotScanInterval)
}

func (m *Manager) snapshotActiveWindow() time.Duration {
	if m.cfg.SnapshotActiveWindow > 0 {
		return m.cfg.SnapshotActiveWindow
	}
	return fallbackSnapshotActiveWindow
}

func (m *Manager) cleanupRetiredUntil(ctx context.Context, keep int) error {
	for len(m.retired) > keep {
		candidate := -1
		for index := 0; index < len(m.retired)-keep; index++ {
			if !m.snapshotLeased(m.retired[index].ID) {
				candidate = index
				break
			}
		}
		if candidate < 0 {
			return nil
		}
		view := m.retired[candidate]
		if err := m.snapshotter.Remove(ctx, view); err != nil {
			return fmt.Errorf("回收 TeslaCam 快照 %s: %w", view.ID, err)
		}
		m.retired = slices.Delete(m.retired, candidate, candidate+1)
	}
	return nil
}

func (m *Manager) snapshotLeased(id string) bool {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.mediaLeases[id] > 0
}

func (m *Manager) handleMissingDataDrive(ctx context.Context, previous Snapshot) error {
	status, inspectErr := m.lifecycle.Inspect()
	wasActive := previous.Phase == PhaseGadgetActive || previous.Phase == PhaseSnapshotCreating || previous.Phase == PhaseSnapshotReady ||
		(previous.Phase == PhaseFault && previous.LastError == ErrDataDriveDisconnected.Error())
	if inspectErr == nil {
		wasActive = wasActive || status.Bound
	}
	status, stopErr := m.lifecycle.Stop()
	if !m.bindAttemptAllowed {
		m.gadgetPaused = true
		m.gadgetResumePending = false
	}
	cleanupErr := m.cleanupDisconnectedSnapshots(ctx, previous)
	now := time.Now()
	state := Snapshot{
		Phase:               PhaseWaitingForDataDrive,
		GadgetStatus:        status,
		GadgetPaused:        m.gadgetPaused,
		GadgetResumePending: m.gadgetResumePending,
		LastRefreshAt:       &now,
	}
	if stopErr != nil || cleanupErr != nil {
		state.Phase = PhaseFault
		cause := errors.Join(stopErr, cleanupErr)
		state.LastError = fmt.Sprintf("数据盘缺失且安全回收失败: %v", cause)
		m.store(state)
		resultErr := fmt.Errorf("数据盘缺失且安全回收: %w", cause)
		if inspectErr != nil {
			resultErr = errors.Join(fmt.Errorf("检查缺盘时的 Gadget: %w", inspectErr), resultErr)
		}
		return resultErr
	}
	if wasActive {
		state.Phase = PhaseFault
		state.LastError = ErrDataDriveDisconnected.Error()
		m.store(state)
		return ErrDataDriveDisconnected
	}
	m.store(state)
	return storage.ErrDeviceNotFound
}

func (m *Manager) cleanupDisconnectedSnapshots(ctx context.Context, previous Snapshot) error {
	m.stateMu.Lock()
	if m.state.MediaSnapshot != nil && previous.MediaSnapshot != nil && m.state.MediaSnapshot.ID == previous.MediaSnapshot.ID {
		m.state.MediaSnapshot = nil
		m.state.Videos = nil
		m.state.VideoCount = 0
	}
	m.stateMu.Unlock()

	views := slices.Clone(m.retired)
	if previous.MediaSnapshot != nil {
		views = append(views, *previous.MediaSnapshot)
	}
	m.retired = nil
	m.snapshotRecovered = false
	m.snapshotFailures = 0
	m.snapshotRetryAt = time.Time{}
	var result error
	for _, view := range views {
		if m.snapshotLeased(view.ID) {
			m.retired = append(m.retired, view)
			continue
		}
		if err := m.snapshotter.Remove(ctx, view); err != nil {
			result = errors.Join(result, fmt.Errorf("回收掉盘快照 %s: %w", view.ID, err))
		}
	}
	return result
}

func (m *Manager) storeTransientFault(previous Snapshot, cause error) error {
	state := previous
	state.Phase = PhaseFault
	if previous.GadgetStatus.Bound {
		// 无法完成一次只读盘点不等于 backing store 已掉线，保留现有 LUN 比运行期重绑更安全。
		state.Phase = PhaseDegraded
	}
	state.LastError = cause.Error()
	now := m.now()
	state.LastRefreshAt = &now
	m.store(state)
	return cause
}

func (m *Manager) stopForFault(device *storage.Device, cause error) error {
	status, stopErr := m.lifecycle.Stop()
	if !m.bindAttemptAllowed {
		m.gadgetPaused = true
		m.gadgetResumePending = false
	}
	if stopErr != nil {
		cause = errors.Join(cause, fmt.Errorf("停止 TeslaBox Gadget: %w", stopErr))
	}
	now := time.Now()
	m.store(Snapshot{
		Phase:               PhaseFault,
		DataDrive:           device,
		GadgetStatus:        status,
		GadgetPaused:        m.gadgetPaused,
		GadgetResumePending: m.gadgetResumePending,
		LastRefreshAt:       &now,
		LastError:           cause.Error(),
	})
	return cause
}

func isContextTermination(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (m *Manager) store(snapshot Snapshot, mediaSource ...Snapshot) {
	if len(mediaSource) > 0 {
		snapshot = preserveMedia(snapshot, mediaSource[0])
	}
	m.stateMu.Lock()
	previous := m.state.Phase
	m.state = snapshot
	m.stateMu.Unlock()
	if previous != snapshot.Phase {
		if snapshot.Phase == PhaseFault {
			m.logger.Error("TeslaUSB 进入故障状态", "from", previous, "error", snapshot.LastError)
		} else {
			m.logger.Info("TeslaUSB 状态已切换", "from", previous, "to", snapshot.Phase)
		}
	}
}

func preserveMedia(target, source Snapshot) Snapshot {
	if source.MediaSnapshot != nil {
		view := *source.MediaSnapshot
		target.MediaSnapshot = &view
	}
	target.MediaVolumeSpace = source.MediaVolumeSpace
	if source.MediaVolumeSpaceAt != nil {
		updatedAt := *source.MediaVolumeSpaceAt
		target.MediaVolumeSpaceAt = &updatedAt
	}
	target.Videos = slices.Clone(source.Videos)
	target.VideoCount = source.VideoCount
	target.SnapshotError = source.SnapshotError
	target.SnapshotSpace = source.SnapshotSpace
	target.SnapshotFailures = source.SnapshotFailures
	if source.SnapshotRetryAt != nil {
		retryAt := *source.SnapshotRetryAt
		target.SnapshotRetryAt = &retryAt
	}
	if source.SnapshotError != "" && target.Phase == PhaseGadgetActive {
		target.Phase = PhaseDegraded
		target.LastError = source.SnapshotError
	}
	return target
}

func stringsEqualFold(left, right string) bool {
	return strings.EqualFold(strings.TrimSpace(left), strings.TrimSpace(right))
}
