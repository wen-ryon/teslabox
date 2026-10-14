//go:build linux

package carplaypairing

import (
	"context"
	"time"

	"github.com/godbus/dbus/v5"
)

const incomingPhoneProfileUUID = "00000000-deca-fade-deca-deafdecacafe"

// cancelRefreshLocked 随连接所有权释放，防止旧解析请求追上新的接收会话。
func (p *incomingProfile) cancelRefreshLocked() {
	if p.refreshCancel != nil {
		p.refreshCancel()
		p.refreshCancel = nil
	}
}

func (p *incomingProfile) refreshEligibleLocked(device dbus.ObjectPath, generation uint64, now time.Time) bool {
	return p.agent.ctx.Err() == nil && p.registered && p.accepting && !p.closing && !p.handoff &&
		p.file >= 0 && p.device == device && p.generation == generation &&
		p.agent.pending == device && now.Before(p.agent.deadline)
}

func (p *incomingProfile) refreshServices(device dbus.ObjectPath, props map[string]dbus.Variant, now time.Time) {
	if incomingPhone(device, props) == "" || props["Paired"].Value() != true ||
		props["Connected"].Value() != true || props["ServicesResolved"].Value() == true || !p.agent.manager.incomingAvailable() {
		return
	}
	p.mu.Lock()
	p.agent.mu.Lock()
	generation := p.generation
	if !p.refreshEligibleLocked(device, generation, now) || p.refreshAttempt == generation ||
		p.acceptedAt.IsZero() || now.Before(p.acceptedAt.Add(2*time.Second)) {
		p.agent.mu.Unlock()
		p.mu.Unlock()
		return
	}
	deadline := p.agent.deadline
	if limit := time.Now().Add(8 * time.Second); limit.Before(deadline) {
		deadline = limit
	}
	ctx, cancel := context.WithDeadline(p.agent.ctx, deadline)
	p.refreshAttempt, p.refreshCancel = generation, cancel
	p.refreshWorkers.Add(1)
	p.agent.mu.Unlock()
	p.mu.Unlock()
	go p.refreshPhoneServices(ctx, cancel, device, generation)
}

func (p *incomingProfile) refreshCurrent(device dbus.ObjectPath, generation uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.agent.mu.Lock()
	defer p.agent.mu.Unlock()
	return p.refreshEligibleLocked(device, generation, time.Now())
}

func (p *incomingProfile) refreshPhoneServices(ctx context.Context, cancel context.CancelFunc, device dbus.ObjectPath, generation uint64) {
	defer p.refreshWorkers.Done()
	defer cancel()
	// 属性重读和总线调用均不持有 Profile/Agent 锁，断开及取消回调可以及时收回连接。
	props, err := phoneProperties(ctx, p.agent.conn, p.agent.owner, device)
	if err != nil || ctx.Err() != nil || incomingPhone(device, props) == "" || props["Paired"].Value() != true ||
		props["Connected"].Value() != true || props["ServicesResolved"].Value() == true || !p.refreshCurrent(device, generation) {
		return
	}
	p.agent.logCallback("ServiceRefresh", "requested")
	err = p.agent.conn.Object(p.agent.owner, device).CallWithContext(ctx, "org.bluez.Device1.ConnectProfile", 0, incomingPhoneProfileUUID).Err
	// BlueZ 可能在 SDP 刷新成功后报告没有客户端 Profile；结果只诊断，不代替原三态及媒体门禁。
	if ctx.Err() != nil || !p.refreshCurrent(device, generation) {
		return
	}
	result := "returned"
	if err != nil {
		result = "error"
		switch failure := err.(type) {
		case dbus.Error:
			result = failure.Name
		case *dbus.Error:
			result = failure.Name
		}
	}
	p.agent.logCallback("ServiceRefresh", result)
}

func (p *incomingProfile) waitRefresh(ctx context.Context) error {
	// 只等待本机调用退出；D-Bus 取消不证明 BlueZ 远端 SDP 已终止，迟到状态仍走严格门禁。
	done := make(chan struct{})
	go func() { p.refreshWorkers.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
