package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/backing"
	"github.com/wen-ryon/teslabox/internal/config"
	"github.com/wen-ryon/teslabox/internal/gadget"
	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/storage"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

type cancellationCleanupSnapshotter struct {
	fakeSnapshotter
	failure error
}

type cancellationScan struct {
	fakeVideoScanner
	cancel context.CancelFunc
}

func (s *cancellationScan) ScanContext(ctx context.Context, root string) ([]teslacam.Video, error) {
	s.cancel()
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("扫描必须限时")
	}
	return nil, ctx.Err()
}

func TestMediaActivationOwnsScanContext(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		name := "request-canceled"
		if shutdown {
			name = "service-stopped"
		}
		t.Run(name, func(t *testing.T) {
			requestCtx, cancelRequest := context.WithCancel(t.Context())
			defer cancelRequest()
			runCtx, cancelRun := context.WithCancel(t.Context())
			defer cancelRun()
			scanner := &cancellationScan{cancel: cancelRequest}
			if shutdown {
				scanner.cancel = cancelRun
			}
			snapshotter := &fakeSnapshotter{views: []mediasnapshot.View{{ID: "candidate"}}}
			manager := NewManager(config.Config{},
				fakeDetector{device: storage.Device{Path: "/dev/sda1", UUID: "data", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}},
				fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
				fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"udc"}}},
				&fakeMounter{}, &fakeLifecycle{}, snapshotter, scanner, slog.New(slog.NewTextHandler(io.Discard, nil)))
			manager.runContext = runCtx
			if err := manager.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			err := manager.ActivateMedia(requestCtx)
			if shutdown {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error=%v", err)
				}
			} else if err != nil || manager.Snapshot().MediaSnapshot == nil {
				t.Fatalf("request cancellation interrupted scan: %v", err)
			}
		})
	}
}

func (s *cancellationCleanupSnapshotter) Remove(ctx context.Context, view mediasnapshot.View) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("清理必须限时")
	}
	return s.failure
}

func TestFailedSnapshotCleanupSurvivesCancellationAndRetries(t *testing.T) {
	snapshotter := &cancellationCleanupSnapshotter{}
	manager := &Manager{snapshotter: snapshotter}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	view := mediasnapshot.View{ID: "failed"}
	if err := manager.removeFailedSnapshot(ctx, view); err != nil {
		t.Fatal(err)
	}
	if len(manager.retired) != 0 {
		t.Fatal("成功回收仍入队")
	}
	snapshotter.failure = errors.New("busy")
	if err := manager.removeFailedSnapshot(ctx, view); !errors.Is(err, snapshotter.failure) {
		t.Fatalf("error=%v", err)
	}
	if len(manager.retired) != 1 || manager.retired[0].ID != view.ID {
		t.Fatal("失败副本未进入重试队列")
	}
	snapshotter.failure = nil
	retryCtx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if err := manager.cleanupRetiredUntil(retryCtx, 0); err != nil {
		t.Fatal(err)
	}
	if len(manager.retired) != 0 {
		t.Fatal("重试成功后仍有遗留副本")
	}
}
