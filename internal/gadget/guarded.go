package gadget

import "context"

// StartGuard 在绑定 UDC 前取得跨进程锁并校验持久化维护状态。
type StartGuard interface {
	AcquireGadgetStart(context.Context) (func(), error)
}

// GuardedLifecycle 只保护可能重新绑定 UDC 的 Start；停止操作必须在维护锁内仍可调用。
type GuardedLifecycle struct {
	manager *Manager
	guard   StartGuard
}

// NewGuardedLifecycle 让主服务和独立命令共享同一启动守卫。
func NewGuardedLifecycle(manager *Manager, guard StartGuard) *GuardedLifecycle {
	return &GuardedLifecycle{manager: manager, guard: guard}
}

// Inspect 返回底层 Gadget 状态。
func (g *GuardedLifecycle) Inspect() (Status, error) { return g.manager.Inspect() }

// Start 在同一锁临界区内检查维护状态并绑定 Gadget。
func (g *GuardedLifecycle) Start() (Status, error) {
	release, err := g.guard.AcquireGadgetStart(context.Background())
	if err != nil {
		return Status{}, err
	}
	defer release()
	return g.manager.Start()
}

// Stop 解绑 Gadget；维护流程可能已经持有同一把跨进程锁，因此这里不能重复加锁。
func (g *GuardedLifecycle) Stop() (Status, error) { return g.manager.Stop() }
