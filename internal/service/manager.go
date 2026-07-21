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
	Phase            Phase                     `json:"phase"`
	DataDrive        *storage.Device           `json:"data_drive,omitempty"`
	Backing          backing.Status            `json:"backing"`
	Gadget           gadget.Environment        `json:"gadget"`
	GadgetStatus     gadget.Status             `json:"gadget_status"`
	MediaSnapshot    *mediasnapshot.View       `json:"media_snapshot,omitempty"`
	VideoCount       int                       `json:"video_count"`
	Videos           []teslacam.Video          `json:"-"`
	LastRefreshAt    *time.Time                `json:"last_refresh_at,omitempty"`
	LastError        string                    `json:"last_error,omitempty"`
	SnapshotError    string                    `json:"snapshot_error,omitempty"`
	SnapshotSpace    mediasnapshot.SpaceStatus `json:"snapshot_space"`
	SnapshotFailures int                       `json:"snapshot_failures,omitempty"`
	SnapshotRetryAt  *time.Time                `json:"snapshot_retry_at,omitempty"`
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

	refreshMu         sync.Mutex
	stateMu           sync.RWMutex
	state             Snapshot
	triggerCh         chan struct{}
	cleanupCh         chan struct{}
	retired           []mediasnapshot.View
	mediaLeases       map[string]int
	snapshotRecovered bool
	snapshotFailures  int
	snapshotRetryAt   time.Time
	now               func() time.Time
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
	return &Manager{
		cfg:         cfg,
		detector:    detector,
		inspector:   inspector,
		prober:      prober,
		mounter:     mounter,
		lifecycle:   lifecycle,
		snapshotter: snapshotter,
		scanner:     scanner,
		logger:      logger,
		state:       Snapshot{Phase: PhaseStarting},
		triggerCh:   make(chan struct{}, 1),
		cleanupCh:   make(chan struct{}, 1),
		mediaLeases: make(map[string]int),
		now:         time.Now,
	}
}

// Run 执行数据盘轮询和 udev 即时收敛，串行管理挂载与 Gadget 生命周期。
func (m *Manager) Run(ctx context.Context) {
	m.refreshWithTimeout(ctx)
	m.snapshotWithTimeout(ctx)
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

// Scan 创建新的只读快照，并在扫描成功后原子替换对外媒体索引。
func (m *Manager) Scan(ctx context.Context) error {
	return m.refreshSnapshot(ctx)
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
	if !m.shouldRunAutomaticSnapshot() {
		return
	}
	ctx, cancel := context.WithTimeout(parent, m.cfg.SnapshotTimeout)
	defer cancel()
	if err := m.refreshSnapshot(ctx); err != nil && !errors.Is(err, ErrSnapshotUnavailable) {
		m.logger.Warn("刷新 TeslaCam 媒体快照失败", "error", err)
	}
}

func (m *Manager) shouldRunAutomaticSnapshot() bool {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	return m.snapshotRetryAt.IsZero() || !m.now().Before(m.snapshotRetryAt)
}

func (m *Manager) refreshWithTimeout(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, m.cfg.OperationTimeout)
	defer cancel()
	if err := m.reconcile(ctx); err != nil && !errors.Is(err, storage.ErrDeviceNotFound) && !errors.Is(err, ErrDataDriveDisconnected) {
		m.logger.Warn("收敛 TeslaUSB 运行状态失败", "error", err)
	}
}

func (m *Manager) cleanupWithTimeout(parent context.Context) {
	timeout := m.cfg.OperationTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	m.cleanupRetired(ctx)
}

func (m *Manager) reconcile(ctx context.Context) error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	previous := m.Snapshot()
	device, err := m.detector.Detect(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrDeviceNotFound) {
			return m.handleMissingDataDrive(ctx, previous)
		}
		return m.stopForFault(nil, fmt.Errorf("检测数据盘: %w", err))
	}

	if len(device.MountPoints) == 0 && stringsEqualFold(device.FSType, "xfs") {
		if err := m.mounter.Mount(ctx, device); err != nil {
			return m.stopForFault(&device, err)
		}
		device, err = m.detector.Detect(ctx)
		if err != nil {
			return m.stopForFault(nil, fmt.Errorf("挂载后重新检测数据盘: %w", err))
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
			Phase:         PhasePreparingBackingStore,
			DataDrive:     &device,
			Backing:       backingStatus,
			GadgetStatus:  gadgetStatus,
			LastRefreshAt: &now,
			LastError:     backingStatus.Reason,
		}, previous)
		return nil
	}
	gadgetEnvironment, err := m.prober.Probe()
	if err != nil {
		return m.stopForFault(&device, err)
	}
	if !gadgetEnvironment.Ready {
		gadgetStatus, stopErr := m.lifecycle.Stop()
		if stopErr != nil {
			return m.stopForFault(&device, fmt.Errorf("Gadget 环境未就绪且停止失败: %w", stopErr))
		}
		now := time.Now()
		m.store(Snapshot{
			Phase:         PhaseDegraded,
			DataDrive:     &device,
			Backing:       backingStatus,
			Gadget:        gadgetEnvironment,
			GadgetStatus:  gadgetStatus,
			LastRefreshAt: &now,
			LastError:     gadgetEnvironment.Reason,
		}, previous)
		return nil
	}

	gadgetStatus, err := m.lifecycle.Start()
	if err != nil {
		return m.stopForFault(&device, fmt.Errorf("启动 TeslaBox Gadget: %w", err))
	}
	if !gadgetStatus.Bound {
		return m.stopForFault(&device, errors.New("Gadget 启动完成但没有绑定 UDC"))
	}
	now := time.Now()
	state := Snapshot{
		Phase:         PhaseGadgetActive,
		DataDrive:     &device,
		Backing:       backingStatus,
		Gadget:        gadgetEnvironment,
		GadgetStatus:  gadgetStatus,
		LastRefreshAt: &now,
	}
	m.store(state, previous)
	return nil
}

func (m *Manager) refreshSnapshot(ctx context.Context) error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	previous := m.Snapshot()
	if previous.Phase != PhaseGadgetActive && previous.Phase != PhaseSnapshotReady && previous.Phase != PhaseDegraded {
		return ErrSnapshotUnavailable
	}
	if !previous.GadgetStatus.Bound || !previous.Backing.Ready {
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
	ready.Videos = slices.Clone(videos)
	ready.VideoCount = len(videos)
	ready.LastError = ""
	ready.SnapshotError = ""
	ready.SnapshotSpace = space
	ready.SnapshotFailures = 0
	ready.SnapshotRetryAt = nil
	now := time.Now()
	ready.LastRefreshAt = &now
	m.store(ready)
	m.snapshotFailures = 0
	m.snapshotRetryAt = time.Time{}

	if previous.MediaSnapshot != nil && previous.MediaSnapshot.ID != view.ID {
		m.retired = append(m.retired, *previous.MediaSnapshot)
	}
	m.cleanupRetired(ctx)
	return nil
}

func (m *Manager) storeSnapshotFailure(previous Snapshot, cause error) error {
	m.snapshotFailures++
	delay := snapshotRetryDelay(m.cfg.ScanInterval, m.cfg.SnapshotRetryMax, m.snapshotFailures)
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
	keep := 0
	if m.Snapshot().MediaSnapshot != nil {
		keep = 1
	}
	if err := m.cleanupRetiredUntil(ctx, keep); err != nil {
		m.logger.Warn("回收旧 TeslaCam 快照失败", "error", err)
	}
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
	cleanupErr := m.cleanupDisconnectedSnapshots(ctx, previous)
	now := time.Now()
	state := Snapshot{Phase: PhaseWaitingForDataDrive, GadgetStatus: status, LastRefreshAt: &now}
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

func (m *Manager) stopForFault(device *storage.Device, cause error) error {
	status, stopErr := m.lifecycle.Stop()
	if stopErr != nil {
		cause = errors.Join(cause, fmt.Errorf("停止 TeslaBox Gadget: %w", stopErr))
	}
	now := time.Now()
	m.store(Snapshot{
		Phase:         PhaseFault,
		DataDrive:     device,
		GadgetStatus:  status,
		LastRefreshAt: &now,
		LastError:     cause.Error(),
	})
	return cause
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
