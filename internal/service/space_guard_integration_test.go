//go:build integration && linux

package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/storagepolicy"
)

type realSpaceSnapshotter struct {
	fakeSnapshotter
	probe *mediasnapshot.Manager
}

func (s *realSpaceSnapshotter) Space() (mediasnapshot.SpaceStatus, error) { return s.probe.Space() }

// TestSpaceGuardIsolatedXFS 只允许显式指定的小型隔离 XFS，不操作产品数据盘。
func TestSpaceGuardIsolatedXFS(t *testing.T) {
	root := os.Getenv("TESLABOX_SPACE_GUARD_TEST_ROOT")
	if root == "" {
		t.Skip("未指定隔离测试挂载点")
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(root, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type != 0x58465342 || fs.Blocks*uint64(fs.Bsize) > 768<<20 {
		t.Fatal("只允许不超过 768 MiB 的隔离 XFS")
	}
	policy, err := storagepolicy.NewSnapshotReservePolicy(64<<20, 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	probe := mediasnapshot.New(nil, mediasnapshot.Config{SnapshotPath: root, Policy: policy})
	m, _ := spaceGuardFixture(t)
	m.snapshotter = &realSpaceSnapshotter{probe: probe}
	// 隔离盘不足生产恢复所需的 1 GiB，必须拒绝真正的自动重启。
	m.ConfigureSpaceRecovery(filepath.Join(t.TempDir(), "recovery.once"), func(context.Context) error { t.Fatal("小型隔离盘不能达到生产恢复余量"); return nil })
	path := filepath.Join(root, "teslabox-space-guard-test.bin")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	buffer := make([]byte, 1<<20)
	var writeErr error
	for i := 0; i < 768; i++ {
		_, writeErr = f.Write(buffer)
		if writeErr != nil {
			break
		}
	}
	closeErr := f.Close()
	if !errors.Is(writeErr, syscall.ENOSPC) && !errors.Is(closeErr, syscall.ENOSPC) {
		t.Fatalf("未触及 ENOSPC: %v %v", writeErr, closeErr)
	}
	m.checkSpaceGuard(t.Context())
	if !m.spaceGuard.blocked || !m.spaceGuard.incident {
		t.Fatal("真实满盘未触发保护")
	}
	full, _ := probe.Space()
	t.Logf("满盘 free_bytes=%d，已触发保护且未重启", full.FreeBytes)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	m.checkSpaceGuard(t.Context())
	if m.spaceGuard.blocked {
		t.Fatal("释放后未解除空间阻塞")
	}
	recovered, _ := probe.Space()
	t.Logf("回收后 free_bytes=%d，小容量盘正确拒绝重启", recovered.FreeBytes)
}
