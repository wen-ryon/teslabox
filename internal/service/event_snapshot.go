package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"syscall"
	"time"

	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

// AcquireEventSnapshot 单次核对事件后借出只读快照，不模拟页面活动或改变 Gadget 绑定状态。
func (m *Manager) AcquireEventSnapshot(ctx context.Context) (Snapshot, func(), error) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, func() {}, err
	}
	state := m.Snapshot()
	if state.MediaSnapshot == nil || m.now().Sub(m.lastSnapshotAt) >= 15*time.Second {
		if err := m.refreshSnapshotLocked(ctx); err != nil {
			return Snapshot{}, func() {}, err
		}
	}
	state, release := m.AcquireMediaSnapshot()
	if state.MediaSnapshot == nil {
		release()
		return Snapshot{}, func() {}, ErrSnapshotUnavailable
	}
	return state, release, nil
}

// MediaSnapshotReady 合并新媒体快照通知，不在通知队列保存快照或占用租约。
func (m *Manager) MediaSnapshotReady() <-chan struct{} { return m.snapshotReadyCh }

// AcquireScannedEventSnapshot 复用新媒体快照，否则创建短期副本；部分索引不进入页面状态。
func (m *Manager) AcquireScannedEventSnapshot(ctx context.Context, scan func(context.Context, Snapshot) ([]teslacam.Video, error)) (Snapshot, func(), error) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	ctx, stop := m.watchSnapshotSpace(ctx)
	defer stop()
	if err := ctx.Err(); err != nil {
		return Snapshot{}, func() {}, err
	}
	state := m.Snapshot()
	if !canRefreshMediaSnapshot(state) {
		return Snapshot{}, func() {}, ErrSnapshotUnavailable
	}
	state.eventSnapshotEpoch = m.eventSnapshotEpoch
	if state.MediaSnapshot != nil && m.now().Sub(m.lastSnapshotAt) < 15*time.Second {
		borrowed, release := m.AcquireMediaSnapshot()
		if borrowed.MediaSnapshot == nil {
			release()
			return Snapshot{}, func() {}, ErrSnapshotUnavailable
		}
		videos, err := scan(ctx, borrowed)
		if err != nil {
			release()
			return Snapshot{}, func() {}, err
		}
		borrowed.Videos = videos
		borrowed.eventSnapshotEpoch = m.eventSnapshotEpoch
		return borrowed, release, nil
	}
	if !m.eventSnapshotRetryAt.IsZero() && m.now().Before(m.eventSnapshotRetryAt) {
		return Snapshot{}, func() {}, errors.New("事件快照处于失败冷却，等待重试")
	}
	view, err := m.createEventViewLocked(ctx)
	if err != nil {
		return Snapshot{}, func() {}, m.eventSnapshotFailure(err)
	}
	state.MediaSnapshot = &view
	videos, err := scan(ctx, state)
	if errors.Is(err, syscall.EIO) {
		if cleanupErr := m.removeFailedSnapshot(ctx, view); cleanupErr != nil {
			return Snapshot{}, func() {}, m.eventSnapshotFailure(errors.Join(err, cleanupErr))
		}
		view, err = m.snapshotter.CreateRepaired(ctx)
		if err == nil {
			state.MediaSnapshot = &view
			videos, err = scan(ctx, state)
		}
	}
	if err != nil {
		if view.ID != "" {
			err = errors.Join(err, m.removeFailedSnapshot(ctx, view))
		}
		return Snapshot{}, func() {}, m.eventSnapshotFailure(err)
	}
	m.eventSnapshotFailures, m.eventSnapshotRetryAt = 0, time.Time{}
	state.MediaSnapshot, state.Videos = &view, videos
	state.SnapshotSpace, err = m.snapshotter.Space()
	if err != nil {
		return Snapshot{}, func() {}, errors.Join(err, m.removeFailedSnapshot(ctx, view))
	}
	m.stateMu.Lock()
	m.state.SnapshotSpace = state.SnapshotSpace
	m.stateMu.Unlock()
	// 从发布起按退休视图管理，只有扫描和系统盘复制租约可延长其生命。
	m.retired = append(m.retired, view)
	borrowed, release := m.acquireSnapshotState(&state)
	if borrowed.MediaSnapshot == nil {
		release()
		m.cleanupRetired(ctx)
		return Snapshot{}, func() {}, ErrSnapshotUnavailable
	}
	return borrowed, release, nil
}

func (m *Manager) createEventViewLocked(ctx context.Context) (mediasnapshot.View, error) {
	if !m.snapshotRecovered {
		if err := m.snapshotter.Recover(ctx); err != nil {
			return mediasnapshot.View{}, err
		}
		m.snapshotRecovered = true
	}
	space, err := m.snapshotter.Space()
	if err != nil {
		return mediasnapshot.View{}, err
	}
	if !space.Ready {
		if err := m.cleanupRetiredUntil(ctx, 0); err != nil {
			return mediasnapshot.View{}, err
		}
		space, err = m.snapshotter.Space()
		if err != nil {
			return mediasnapshot.View{}, err
		}
	}
	if !space.Ready {
		return mediasnapshot.View{}, &mediasnapshot.InsufficientSpaceError{Status: space}
	}
	return m.snapshotter.Create(ctx)
}

func (m *Manager) eventSnapshotFailure(err error) error {
	m.eventSnapshotFailures++
	delay := snapshotRetryDelay(15*time.Second, m.cfg.SnapshotRetryMax, m.eventSnapshotFailures)
	m.eventSnapshotRetryAt = m.now().Add(delay)
	return fmt.Errorf("事件快照检查失败: %w", err)
}

// AcquireEventSnapshotLease 让暂存任务继续使用已退休的事件副本，换盘或空间不足时拒绝新租约。
func (m *Manager) AcquireEventSnapshotLease(source Snapshot) (Snapshot, func()) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	current := m.Snapshot()
	valid := source.MediaSnapshot != nil && source.eventSnapshotEpoch == m.eventSnapshotEpoch && deviceIndexKey(source) == deviceIndexKey(current) && canRefreshMediaSnapshot(current)
	if valid {
		valid = current.MediaSnapshot != nil && current.MediaSnapshot.ID == source.MediaSnapshot.ID || slices.ContainsFunc(m.retired, func(v mediasnapshot.View) bool { return v.ID == source.MediaSnapshot.ID })
	}
	if !valid {
		return Snapshot{}, func() {}
	}
	source.SnapshotSpace = current.SnapshotSpace
	return m.acquireSnapshotState(&source)
}
