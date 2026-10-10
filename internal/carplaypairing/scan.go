package carplaypairing

import (
	"context"
	"errors"
	"time"
)

// Device 只提供用户辨认目标所需的蓝牙身份，不保存控制器密钥或自动授权。
type Device struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Paired bool   `json:"paired"`
}

// Scanning 供页面区分搜索与配对，搜索期间不允许另起一轮任务。
func (m *Manager) Scanning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.scanDone != nil
}

// Scan 持有独占槽直到自己的发现会话退出；离开请求或应用退出都取消搜索。
func (m *Manager) Scan(ctx context.Context) ([]Device, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, errors.New("搜索需要有效上下文")
	}
	m.mu.Lock()
	if m.closed || m.scanner == nil {
		m.mu.Unlock()
		return nil, errors.New("蓝牙搜索暂不可用")
	}
	if m.scanDone != nil || m.current != nil && (m.current.status.Running || m.current.cleanupPending) {
		m.mu.Unlock()
		return nil, ErrBusy
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	done := make(chan struct{})
	m.scanCancel, m.scanDone = cancel, done
	scanner := m.scanner
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		m.scanCancel, m.scanDone = nil, nil
		close(done)
		m.mu.Unlock()
	}()
	return scanner(bounded)
}
