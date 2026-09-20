package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/backing"
	"github.com/wen-ryon/teslabox/internal/gadget"
	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/storage"
)

func spaceGuardFixture(t *testing.T) (*Manager, *fakeSnapshotter) {
	t.Helper()
	s := &fakeSnapshotter{space: mediasnapshot.SpaceStatus{TotalBytes: 100 << 30, FreeBytes: 1 << 20, RequiredBytes: 8 << 30, Reason: "空间不足"}}
	m := &Manager{snapshotter: s, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), mediaLeases: make(map[string]int)}
	m.state = Snapshot{DataDrive: &storage.Device{UUID: "test-disk"}, Backing: backing.Status{Ready: true, ImagePath: "test.img"}, GadgetStatus: gadget.Status{Bound: true}}
	return m, s
}

func TestSpaceGuardChecksWithoutSnapshot(t *testing.T) {
	m, _ := spaceGuardFixture(t)
	m.checkSpaceGuard(t.Context())
	if m.Snapshot().SnapshotSpace.FreeBytes != 1<<20 {
		t.Fatal("未读取没有媒体快照的数据盘空间")
	}
}

func TestSpaceGuardCancelsLeaseBeforeReclaim(t *testing.T) {
	m, s := spaceGuardFixture(t)
	m.state.MediaSnapshot = &mediasnapshot.View{ID: "leased"}
	m.state.SnapshotSpace.Ready = true
	state, release := m.AcquireMediaSnapshot()
	m.checkSpaceGuard(t.Context())
	if state.MediaContext.Err() == nil {
		t.Fatal("临界空间未取消活动媒体")
	}
	if len(s.removed) != 0 {
		t.Fatal("媒体退出前不能卸载快照")
	}
	release()
	m.checkSpaceGuard(t.Context())
	if len(s.removed) != 1 {
		t.Fatal("媒体释放后未回收快照")
	}
}

func TestSpaceGuardProbeErrorBlocksNewWorkWithoutReboot(t *testing.T) {
	m, s := spaceGuardFixture(t)
	m.state.MediaSnapshot = &mediasnapshot.View{ID: "view"}
	m.state.SnapshotSpace.Ready = true
	s.spaceErr = errors.New("statfs failed")
	m.ConfigureSpaceRecovery(filepath.Join(t.TempDir(), "once"), func(context.Context) error { t.Fatal("采样失败不能重启"); return nil })
	m.checkSpaceGuard(t.Context())
	state, release := m.AcquireMediaSnapshot()
	defer release()
	if state.MediaSnapshot != nil {
		t.Fatal("采样失败仍允许新租约")
	}
	if len(s.removed) != 0 {
		t.Fatal("未知空间状态不应删除快照")
	}
}

func TestSpaceGuardCancelsScanWithoutWaitingForRefreshLock(t *testing.T) {
	m, _ := spaceGuardFixture(t)
	ctx, cancel := m.watchSnapshotSpace(t.Context())
	defer cancel()
	m.refreshMu.Lock()
	m.checkSpaceGuard(t.Context())
	m.refreshMu.Unlock()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("空间保护被扫描锁阻塞")
	}
}

func TestSpaceGuardRecoveryNeedsFreeSpaceAndIsPersistentOnce(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "recovery.once")
	calls := 0
	reboot := func(context.Context) error { calls++; return nil }
	for i := 0; i < 2; i++ {
		m, s := spaceGuardFixture(t)
		m.ConfigureSpaceRecovery(marker, reboot)
		m.checkSpaceGuard(t.Context())
		if calls != i {
			t.Fatalf("空间不足时提交重启: %d", calls)
		}
		s.space.Ready = true
		s.space.FreeBytes = 10 << 30
		m.checkSpaceGuard(t.Context())
		m.checkSpaceGuard(t.Context())
		if calls != 1 {
			t.Fatalf("重启次数 = %d，预期一次", calls)
		}
	}
}

func TestSpaceGuardUnreadableMarkerFailsClosed(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "recovery.once")
	if err := os.WriteFile(marker, []byte("invalid marker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, s := spaceGuardFixture(t)
	m.ConfigureSpaceRecovery(marker, func(context.Context) error {
		t.Fatal("无法识别旧标记时不能重复重启")
		return nil
	})
	m.checkSpaceGuard(t.Context())
	s.space.Ready = true
	s.space.FreeBytes = 10 << 30
	m.checkSpaceGuard(t.Context())
	if !m.spaceGuard.markerPresent || m.spaceGuard.recoveryAttempted {
		t.Fatalf("标记状态 = present:%v attempted:%v", m.spaceGuard.markerPresent, m.spaceGuard.recoveryAttempted)
	}
}

func TestSpaceGuardDoesNotRebootForOrdinaryReservePressure(t *testing.T) {
	m, s := spaceGuardFixture(t)
	calls := 0
	m.ConfigureSpaceRecovery(filepath.Join(t.TempDir(), "once"), func(context.Context) error { calls++; return nil })
	s.space.FreeBytes = 7 << 30
	m.checkSpaceGuard(t.Context())
	s.space.Ready = true
	s.space.FreeBytes = 10 << 30
	m.checkSpaceGuard(t.Context())
	if calls != 0 {
		t.Fatal("普通预留线告警不应重启")
	}
}

func TestSpaceGuardDoesNotRebootWithActiveLease(t *testing.T) {
	m, s := spaceGuardFixture(t)
	m.ConfigureSpaceRecovery(filepath.Join(t.TempDir(), "once"), func(context.Context) error { t.Fatal("活动租约期间不能重启"); return nil })
	m.checkSpaceGuard(t.Context())
	s.space.Ready = true
	s.space.FreeBytes = 10 << 30
	m.mediaLeases["playing"] = 1
	m.checkSpaceGuard(t.Context())
}

func TestSpaceGuardRearmsOnlyAfterStableNewBoot(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "recovery.once")
	if err := persistSpaceRecovery(marker, "boot=previous free_bytes=10737418240\n"); err != nil {
		t.Fatal(err)
	}
	m, s := spaceGuardFixture(t)
	m.bootID = "current"
	now := time.Unix(1000, 0)
	m.now = func() time.Time { return now }
	calls := 0
	m.ConfigureSpaceRecovery(marker, func(context.Context) error { calls++; return nil })
	s.space.Ready = true
	s.space.FreeBytes = 10 << 30
	m.checkSpaceGuard(t.Context())
	now = now.Add(spaceRecoveryRearmDelay - time.Second)
	m.checkSpaceGuard(t.Context())
	if _, err := loadSpaceRecoveryBoot(marker); err != nil {
		t.Fatalf("稳定期结束前清除了标记: %v", err)
	}
	// 稳定期内再次空间不足必须重新计时。
	s.space.Ready = false
	s.space.FreeBytes = 7 << 30
	m.checkSpaceGuard(t.Context())
	s.space.Ready = true
	s.space.FreeBytes = 10 << 30
	m.checkSpaceGuard(t.Context())
	now = now.Add(spaceRecoveryRearmDelay)
	m.checkSpaceGuard(t.Context())
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("新启动稳定后标记仍存在: %v", err)
	}
	// 重新启用后，下一次独立严重事故仍只能提交一次重启。
	s.space.Ready = false
	s.space.FreeBytes = 1 << 20
	for range 3 {
		m.checkSpaceGuard(t.Context())
	}
	s.space.Ready = true
	s.space.FreeBytes = 10 << 30
	m.checkSpaceGuard(t.Context())
	m.checkSpaceGuard(t.Context())
	if calls != 1 {
		t.Fatalf("下一次独立事故重启次数 = %d，预期一次", calls)
	}
	if boot, err := loadSpaceRecoveryBoot(marker); err != nil || boot != "current" {
		t.Fatalf("新事故标记 = %q, %v", boot, err)
	}
}
