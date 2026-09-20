//go:build integration && linux

package snapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/storagepolicy"
	"github.com/wen-ryon/teslabox/internal/system"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

func TestRealSnapshotCreateScanAndRemove(t *testing.T) {
	image := os.Getenv("TESLABOX_VERIFY_SNAPSHOT_IMAGE")
	if image == "" {
		t.Skip("未显式指定真实镜像")
	}
	root, err := os.MkdirTemp(filepath.Dir(image), "snapshot-verify-")
	if err != nil {
		t.Fatal(err)
	}
	mounts, err := os.MkdirTemp("/run", "teslabox-snapshot-verify-")
	if err != nil {
		os.Remove(root)
		t.Fatal(err)
	}
	m := New(system.NewOSRunner(), Config{ImagePath: image, SnapshotPath: root, MountRoot: mounts, FSCKPath: "/opt/teslabox/media/bin/teslabox-fsck.exfat", Policy: storagepolicy.DefaultSnapshotReservePolicy()})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := m.Recover(ctx); err != nil {
			t.Error(err)
			return
		}
		if err := os.Remove(root); err != nil {
			t.Error(err)
		}
		if err := os.Remove(mounts); err != nil {
			t.Error(err)
		}
	})
	view, err := m.Create(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if view.VolumeSpace.TotalBytes == 0 {
		t.Fatal("内层容量未读取")
	}
	videos, err := teslacam.NewScanner(time.Local).Scan(view.MountPath)
	if errors.Is(err, syscall.EIO) {
		t.Logf("普通副本扫描 EIO，验证产品既有的副本修复重试: %v", err)
		if err := m.Remove(t.Context(), view); err != nil {
			t.Fatal(err)
		}
		view, err = m.CreateRepaired(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		videos, err = teslacam.NewScanner(time.Local).Scan(view.MountPath)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("真实快照成功，内层容量=%d，录像文件=%d", view.VolumeSpace.TotalBytes, len(videos))
	if err := m.Remove(t.Context(), view); err != nil {
		t.Fatal(err)
	}
}
