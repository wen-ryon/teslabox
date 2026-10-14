package carplaypairing

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"sync"
	"time"
)

// ErrBusy 表示上一轮的接收进程或资源清理尚未结束，不能开始新的配对。
var ErrBusy = errors.New("上一轮 CarPlay 配对尚未结束")

// ErrCleanupPending 要求重新核对服务状态，不能在清理未确认时开始另一轮。
var ErrCleanupPending = errors.New("CarPlay 登记清理尚未确认")

var phoneAddress = regexp.MustCompile(`^(?:[A-F0-9]{2}:){5}[A-F0-9]{2}$`)

// PairRunner 必须在自有进程和确认通道清理后返回，成功仅证明系统蓝牙配对。
type PairRunner func(context.Context, string, *Confirmation) error

// TaskStatus 不保存原始错误或配对日志；数字仅在本轮等待确认时返回。
type TaskStatus struct {
	FailurePhase string             `json:"failure_phase,omitempty"`
	Phase        string             `json:"phase,omitempty"`
	RequestID    string             `json:"request_id"`
	PhoneID      string             `json:"phone_id"`
	State        string             `json:"state"`
	Running      bool               `json:"running"`
	ExpiresAt    time.Time          `json:"expires_at"`
	Prompt       ConfirmationStatus `json:"prompt"`
}

type pairingTask struct {
	incoming        bool
	incomingRelease func(context.Context) error
	handoff         HandoffPlan
	confirmation    *Confirmation
	status          TaskStatus
	cancel          context.CancelFunc
	done            chan struct{}
	cleanupPending  bool
}

// Manager 保留最后一轮状态并互斥持有当前任务，不操作录像、热点或系统默认 Agent。
type Manager struct {
	logger            *slog.Logger
	incomingCancel    context.CancelFunc
	incomingDone      chan struct{}
	incomingReady     func() bool
	incomingListening bool
	handoff           NetworkHandoff
	mu                sync.Mutex
	run               PairRunner
	current           *pairingTask
	completedState    string
	reconcile         func(context.Context, string, string, time.Time) error
	recoveredVideo    func(string) bool
	ready             func() bool
	closed            bool
	scanner           func(context.Context) ([]Device, error)
	scanCancel        context.CancelFunc
	scanDone          chan struct{}
}

// NewManager 要求真实配对执行器，避免管理入口仅显示模拟成功。
func NewManager(run PairRunner) (*Manager, error) {
	if run == nil {
		return nil, errors.New("缺少 CarPlay 配对执行器")
	}
	return &Manager{run: run, completedState: "bluetooth_paired"}, nil
}

// Start 使用应用生命周期上下文；HTTP 请求返回不能取消已经确认启动的任务。
func (m *Manager) Start(parent context.Context, phone string, window time.Duration) (TaskStatus, error) {
	if m.handoff != nil {
		return TaskStatus{}, errors.New("首次登记需要明确确认热点交接")
	}
	return m.start(parent, phone, window, HandoffPlan{})
}

func (m *Manager) start(parent context.Context, phone string, window time.Duration, plan HandoffPlan, incoming ...bool) (TaskStatus, error) {
	return m.startWithRelease(parent, phone, window, plan, nil, incoming...)
}

func (m *Manager) startWithRelease(parent context.Context, phone string, window time.Duration, plan HandoffPlan, release func(context.Context) error, incoming ...bool) (TaskStatus, error) {
	if parent == nil || parent.Err() != nil || !phoneAddress.MatchString(phone) || phone == "00:00:00:00:00:00" {
		return TaskStatus{}, errors.New("配对需要有效应用上下文和明确手机蓝牙地址")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return TaskStatus{}, errors.New("CarPlay 配对管理已关闭")
	}
	if m.scanDone != nil || m.current != nil && (m.current.status.Running || m.current.cleanupPending) {
		return TaskStatus{}, ErrBusy
	}
	ctx, cancel := context.WithTimeout(parent, window)
	confirmation, err := NewConfirmation(ctx, window)
	if err != nil {
		cancel()
		return TaskStatus{}, err
	}
	prompt := confirmation.Status()
	task := &pairingTask{confirmation: confirmation, cancel: cancel, done: make(chan struct{}),
		incoming:        len(incoming) == 1 && incoming[0],
		incomingRelease: release,
		handoff:         plan,
		status:          TaskStatus{RequestID: prompt.RequestID, PhoneID: phone, State: "pairing", Running: true, ExpiresAt: prompt.ExpiresAt}}
	m.current = task
	go m.execute(ctx, task)
	return m.snapshotLocked(task), nil
}

// Ready 只检查登记能力，不扫描手机、不启动进程或改变服务状态。
func (m *Manager) Ready() bool {
	m.mu.Lock()
	closed, ready := m.closed, m.ready
	m.mu.Unlock()
	return !closed && (ready == nil || ready())
}

// IncomingListening 仅在系统 Agent 和可发现状态实际设置成功后报告入口可用。
func (m *Manager) IncomingListening() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.closed && m.incomingListening
}

func (m *Manager) setIncomingListening(value bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.incomingListening = value
}

// Close 随应用退出取消自有任务，等待真实退出；观察超时不会假装资源已经释放。
func (m *Manager) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("停止配对管理需要上下文")
	}
	m.mu.Lock()
	m.closed = true
	incomingDone := m.incomingDone
	if m.incomingCancel != nil {
		m.incomingCancel()
	}
	scanDone := m.scanDone
	if m.scanCancel != nil {
		m.scanCancel()
	}
	task := m.current
	if task != nil && task.status.Running && !task.cleanupPending {
		task.cancel()
		task.confirmation.cancel()
	}
	m.mu.Unlock()
	if incomingDone != nil {
		select {
		case <-incomingDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if scanDone != nil {
		select {
		case <-scanDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if task == nil {
		return nil
	}
	if err := m.Wait(ctx, task.status.RequestID); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if task.cleanupPending {
		return ErrCleanupPending
	}
	return nil
}

func (m *Manager) execute(ctx context.Context, task *pairingTask) {
	task.confirmation.incoming = task.incoming
	task.confirmation.incomingRelease = task.incomingRelease
	task.confirmation.handoff = m.handoff
	task.confirmation.handoffPlan = task.handoff
	err := m.run(ctx, task.status.PhoneID, task.confirmation)
	// 暂停或配对提前失败时也必须释放首登 Profile，不依赖正常接收端交接路径。
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	if releaseErr := task.confirmation.releaseIncoming(cleanup); releaseErr != nil {
		err = ErrCleanupPending
	}
	cancel()
	// 执行器退出前仍占有互斥槽；取消和到期不代表进程已停止。
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.completedState
	if ctx.Err() != nil && !(err == nil && m.completedState == "video_verified") {
		state = "canceled"
	} else if err != nil {
		state = "failed"
	}
	if errors.Is(err, ErrCleanupPending) {
		state, task.cleanupPending = "cleanup_pending", true
	}
	task.cancel()
	task.confirmation.cancel()
	task.status.State, task.status.Running = state, false
	if m.logger != nil {
		m.logger.Info("CarPlay 登记任务结束", "request_id", task.status.RequestID, "state", state, "failure_phase", task.confirmation.failedPhase())
	}
	close(task.done)
}

// Reconcile 在清理未确认后重新检查同一轮服务状态，不重新配对或开启登记窗口。
func (m *Manager) Reconcile(ctx context.Context, requestID string) error {
	if ctx == nil {
		return errors.New("清理确认需要上下文")
	}
	m.mu.Lock()
	task := m.current
	if task == nil || task.status.RequestID != requestID || task.status.Running || !task.cleanupPending || m.reconcile == nil {
		m.mu.Unlock()
		return ErrNotPending
	}
	task.status.Running = true
	task.done = make(chan struct{})
	m.mu.Unlock()
	err := m.reconcile(ctx, requestID, task.status.PhoneID, task.status.ExpiresAt)
	m.mu.Lock()
	defer m.mu.Unlock()
	task.status.Running = false
	close(task.done)
	if err == nil {
		task.cleanupPending = false
		task.status.State = "failed"
		if m.recoveredVideo != nil && m.recoveredVideo(requestID) {
			task.status.State = "video_verified"
		}
	}
	return err
}

func (m *Manager) snapshotLocked(task *pairingTask) TaskStatus {
	status := task.status
	status.Prompt = task.confirmation.Status()
	status.Phase = task.confirmation.currentPhase()
	status.FailurePhase = task.confirmation.failedPhase()
	if !status.Running {
		status.Prompt.Digits = ""
	}
	return status
}

// Status 返回最后一轮的内存快照，调用者不得记录其中的人工确认数字。
func (m *Manager) Status() TaskStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil {
		return TaskStatus{State: "idle"}
	}
	return m.snapshotLocked(m.current)
}

// Confirm 只接收当前任务的数字，旧页面不能授权后来启动的手机任务。
func (m *Manager) Confirm(requestID, digits string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil || !m.current.status.Running || m.current.cleanupPending || m.current.status.RequestID != requestID {
		return ErrNotPending
	}
	return m.current.confirmation.Confirm(requestID, digits)
}

// Cancel 隐藏提示并请求退出，但仍保留任务槽直到执行器确认清理完成。
func (m *Manager) Cancel(requestID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil || !m.current.status.Running || m.current.cleanupPending || m.current.status.RequestID != requestID {
		return ErrNotPending
	}
	m.current.status.State = "canceling"
	m.current.cancel()
	m.current.confirmation.cancel()
	return nil
}

// Wait 只等待指定轮次的真实退出；观察超时不会释放任务槽或重新启动执行器。
func (m *Manager) Wait(ctx context.Context, requestID string) error {
	if ctx == nil {
		return errors.New("等待配对需要上下文")
	}
	m.mu.Lock()
	if m.current == nil || m.current.status.RequestID != requestID {
		m.mu.Unlock()
		return ErrNotPending
	}
	done := m.current.done
	m.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
