package carplaypairing

import (
	"context"
	"errors"
	"time"
)

// HandoffPlan 绑定用户看到的原热点配置，避免确认之后接管另一个热点。
type HandoffPlan struct {
	Fingerprint string `json:"fingerprint"`
	Required    bool   `json:"required"`
}

// NetworkHandoff 由统一网络管理器持久化交接，不允许登记执行器直接操作网卡。
type NetworkHandoff interface {
	PlanCarPlayHandoff(context.Context) (HandoffPlan, error)
	BeginCarPlayHandoff(context.Context, string, string, HandoffPlan) error
	FinishCarPlayHandoff(context.Context, string, bool) error
}

// ConfigureHandoff 在启动管理接口前绑定交接能力。
func (m *Manager) ConfigureHandoff(handoff NetworkHandoff) { m.handoff = handoff }

// HandoffPlan 只读取配置，查询页面不会关闭热点。
func (m *Manager) HandoffPlan(ctx context.Context) (HandoffPlan, error) {
	if m.handoff == nil {
		return HandoffPlan{}, nil
	}
	return m.handoff.PlanCarPlayHandoff(ctx)
}

// StartConfirmed 只在明确确认与配置指纹一致时启动登记；重复请求返回同一活动任务。
func (m *Manager) StartConfirmed(ctx context.Context, phone string, window time.Duration, fingerprint string, confirmed bool) (TaskStatus, error) {
	if !confirmed {
		return TaskStatus{}, errors.New("请先确认首次登记期间的热点交接")
	}
	m.mu.Lock()
	if m.current != nil && m.current.status.Running && m.current.status.State == "pairing" && !m.current.cleanupPending && m.current.status.PhoneID == phone && m.current.handoff.Fingerprint == fingerprint {
		status := m.snapshotLocked(m.current)
		m.mu.Unlock()
		return status, nil
	}
	m.mu.Unlock()
	plan, err := m.HandoffPlan(ctx)
	if err != nil {
		return TaskStatus{}, err
	}
	if plan.Fingerprint != fingerprint {
		return TaskStatus{}, errors.New("热点配置已变化，请刷新后重新确认")
	}
	return m.start(ctx, phone, window, plan)
}
