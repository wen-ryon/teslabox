// Package service 协调数据盘、backing store、Gadget 和后续快照生命周期。
package service

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/wen-ryon/teslabox/internal/backing"
	"github.com/wen-ryon/teslabox/internal/config"
	"github.com/wen-ryon/teslabox/internal/gadget"
	"github.com/wen-ryon/teslabox/internal/storage"
	"github.com/wen-ryon/teslabox/internal/teslacam"
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

// Snapshot 是数据盘、backing store、Gadget 和媒体索引的一致性只读视图。
type Snapshot struct {
	Phase         Phase              `json:"phase"`
	DataDrive     *storage.Device    `json:"data_drive,omitempty"`
	Backing       backing.Status     `json:"backing"`
	Gadget        gadget.Environment `json:"gadget"`
	VideoCount    int                `json:"video_count"`
	Videos        []teslacam.Video   `json:"-"`
	LastRefreshAt *time.Time         `json:"last_refresh_at,omitempty"`
	LastError     string             `json:"last_error,omitempty"`
}

// Manager 串行化 udev、轮询和 API 刷新，防止未来生命周期操作并发执行。
type Manager struct {
	cfg       config.Config
	detector  storage.Detector
	inspector BackingInspector
	prober    GadgetProber
	logger    *slog.Logger

	refreshMu sync.Mutex
	stateMu   sync.RWMutex
	state     Snapshot
	triggerCh chan struct{}
}

// NewManager 使用手工依赖注入保持系统边界可测试，初始状态不触发任何磁盘写入。
func NewManager(
	cfg config.Config,
	detector storage.Detector,
	inspector BackingInspector,
	prober GadgetProber,
	logger *slog.Logger,
) *Manager {
	return &Manager{
		cfg:       cfg,
		detector:  detector,
		inspector: inspector,
		prober:    prober,
		logger:    logger,
		state:     Snapshot{Phase: PhaseStarting},
		triggerCh: make(chan struct{}, 1),
	}
}

// Run 执行数据盘轮询和 udev 即时刷新，当前阶段只读取系统状态。
func (m *Manager) Run(ctx context.Context) {
	m.refreshWithTimeout(ctx)
	ticker := time.NewTicker(m.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.refreshWithTimeout(ctx)
		case <-m.triggerCh:
			m.refreshWithTimeout(ctx)
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

// Refresh 供 API 在请求上下文内刷新一次完整的只读预检状态。
func (m *Manager) Refresh(ctx context.Context) error {
	return m.reconcile(ctx)
}

// Snapshot 返回防御性拷贝，避免 API 修改内部设备和媒体状态。
func (m *Manager) Snapshot() Snapshot {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	result := m.state
	result.Videos = slices.Clone(m.state.Videos)
	result.Backing.Missing = slices.Clone(m.state.Backing.Missing)
	result.Gadget.UDCNames = slices.Clone(m.state.Gadget.UDCNames)
	if m.state.DataDrive != nil {
		device := *m.state.DataDrive
		device.MountPoints = slices.Clone(m.state.DataDrive.MountPoints)
		result.DataDrive = &device
	}
	return result
}

// Close 预留后续 Gadget 和快照的有序释放；当前只读阶段没有资源需要卸载。
func (m *Manager) Close(context.Context) error {
	return nil
}

func (m *Manager) refreshWithTimeout(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, m.cfg.OperationTimeout)
	defer cancel()
	if err := m.reconcile(ctx); err != nil && !errors.Is(err, storage.ErrDeviceNotFound) {
		m.logger.Warn("刷新 TeslaUSB 预检状态失败", "error", err)
	}
}

func (m *Manager) reconcile(ctx context.Context) error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()

	device, err := m.detector.Detect(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrDeviceNotFound) {
			m.store(Snapshot{Phase: PhaseWaitingForDataDrive, LastRefreshAt: timePointer(time.Now())})
			return err
		}
		m.storeFault(err)
		return err
	}

	backingStatus, err := m.inspector.Inspect(device)
	if err != nil {
		m.storeDeviceFault(device, err)
		return err
	}
	gadgetEnvironment, err := m.prober.Probe()
	if err != nil {
		m.storeDeviceFault(device, err)
		return err
	}

	phase := PhaseReadyForGadget
	if !backingStatus.Ready {
		phase = PhasePreparingBackingStore
	} else if !gadgetEnvironment.Ready {
		phase = PhaseDegraded
	}
	now := time.Now()
	state := Snapshot{
		Phase:         phase,
		DataDrive:     &device,
		Backing:       backingStatus,
		Gadget:        gadgetEnvironment,
		LastRefreshAt: &now,
	}
	if phase == PhaseDegraded {
		state.LastError = gadgetEnvironment.Reason
	}
	m.store(state)
	return nil
}

func (m *Manager) storeFault(err error) {
	now := time.Now()
	m.store(Snapshot{Phase: PhaseFault, LastRefreshAt: &now, LastError: err.Error()})
}

func (m *Manager) storeDeviceFault(device storage.Device, err error) {
	now := time.Now()
	m.store(Snapshot{Phase: PhaseFault, DataDrive: &device, LastRefreshAt: &now, LastError: err.Error()})
}

func (m *Manager) store(snapshot Snapshot) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	m.state = snapshot
}

func timePointer(value time.Time) *time.Time {
	return &value
}
