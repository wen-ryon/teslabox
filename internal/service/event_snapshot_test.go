package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"syscall"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/backing"
	"github.com/wen-ryon/teslabox/internal/config"
	"github.com/wen-ryon/teslabox/internal/gadget"
	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/storage"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

func TestEventSnapshotDoesNotActivatePageOrRebindGadget(t *testing.T) {
	lifecycle := &fakeLifecycle{}
	snapshotter := &fakeSnapshotter{views: []mediasnapshot.View{{ID: "a", MountPath: "/a"}, {ID: "b", MountPath: "/b"}}}
	manager := NewManager(config.Config{ScanInterval: time.Minute, SnapshotTimeout: time.Second},
		fakeDetector{device: storage.Device{Path: "/dev/sda1", UUID: "data", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}},
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"udc"}}}, &fakeMounter{}, lifecycle, snapshotter, &fakeVideoScanner{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 10, 2, 20, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	if err := manager.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	starts, stops := lifecycle.startCalls, lifecycle.stopCalls
	first, release, err := manager.AcquireEventSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	if first.MediaSnapshot == nil || !manager.mediaActiveUntil.IsZero() {
		t.Fatal("background scan simulated page activity")
	}
	now = now.Add(10 * time.Second)
	_, release, err = manager.AcquireEventSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	if snapshotter.createCalls != 1 {
		t.Fatal("fresh snapshot replaced too early")
	}
	now = now.Add(6 * time.Second)
	second, release, err := manager.AcquireEventSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	if second.MediaSnapshot == nil || second.MediaSnapshot.ID == first.MediaSnapshot.ID || lifecycle.startCalls != starts || lifecycle.stopCalls != stops {
		t.Fatal("event scan changed gadget state")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, release, err = manager.AcquireEventSnapshot(ctx); err == nil {
		release()
		t.Fatal("canceled scan continued")
	}
}

func eventScanManager(t *testing.T, snapshotter *fakeSnapshotter, scanner *fakeVideoScanner) *Manager {
	t.Helper()
	lifecycle := &fakeLifecycle{}
	m := NewManager(config.Config{ScanInterval: time.Minute, SnapshotTimeout: time.Second},
		fakeDetector{device: storage.Device{Path: "/dev/sda1", UUID: "data", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}},
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"udc"}}}, &fakeMounter{}, lifecycle, snapshotter, scanner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := m.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestScopedEventSnapshotKeepsPageIndexAndReleasesAllLeases(t *testing.T) {
	views := &fakeSnapshotter{views: []mediasnapshot.View{{ID: "events", MountPath: "/events"}, {ID: "page", MountPath: "/page"}}}
	full := &fakeVideoScanner{videos: []teslacam.Video{{ID: "page-video"}}}
	m := eventScanManager(t, views, full)
	starts, stops := m.lifecycle.(*fakeLifecycle).startCalls, m.lifecycle.(*fakeLifecycle).stopCalls
	scan := func(_ context.Context, state Snapshot) ([]teslacam.Video, error) {
		return []teslacam.Video{{ID: "event-video"}}, nil
	}
	state, release, err := m.AcquireScannedEventSnapshot(context.Background(), scan)
	if err != nil || state.MediaSnapshot.ID != "events" || full.calls != 0 || m.Snapshot().MediaSnapshot != nil {
		t.Fatalf("state=%+v full=%d err=%v", state, full.calls, err)
	}
	copyState, releaseCopy := m.AcquireEventSnapshotLease(state)
	if copyState.MediaSnapshot == nil {
		t.Fatal("retired event view could not be borrowed")
	}
	release()
	release()
	m.cleanupRetired(context.Background())
	if len(views.removed) != 0 {
		t.Fatal("copy lease was ignored")
	}
	releaseCopy()
	m.cleanupRetired(context.Background())
	if len(views.removed) != 1 || m.hasMediaLeases() {
		t.Fatal("event snapshot retained after staging")
	}
	if stale, release := m.AcquireEventSnapshotLease(state); stale.MediaSnapshot != nil {
		release()
		t.Fatal("removed view was borrowed")
	}
	if err := m.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.MediaSnapshotReady():
	default:
		t.Fatal("new page snapshot notification missing")
	}
	shared, release, err := m.AcquireScannedEventSnapshot(context.Background(), scan)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if shared.MediaSnapshot.ID != "page" || views.createCalls != 2 || m.Snapshot().Videos[0].ID != "page-video" {
		t.Fatal("shared scan replaced full index or cloned again")
	}
	if !m.mediaActiveUntil.IsZero() && full.calls != 1 {
		t.Fatal("event scan activated page scanning")
	}
	if m.lifecycle.(*fakeLifecycle).startCalls != starts || m.lifecycle.(*fakeLifecycle).stopCalls != stops {
		t.Fatal("event scan rebound gadget")
	}
}

func TestScopedEventSnapshotRepairsEIOAndCoolsDownFailures(t *testing.T) {
	views := &fakeSnapshotter{views: []mediasnapshot.View{{ID: "first"}, {ID: "repaired"}}}
	m := eventScanManager(t, views, &fakeVideoScanner{})
	now := time.Now()
	m.now = func() time.Time { return now }
	state, release, err := m.AcquireScannedEventSnapshot(context.Background(), func(_ context.Context, state Snapshot) ([]teslacam.Video, error) {
		if state.MediaSnapshot.ID == "first" {
			return nil, syscall.EIO
		}
		return nil, nil
	})
	if err != nil || state.MediaSnapshot.ID != "repaired" || views.repairCalls != 1 {
		t.Fatalf("repair=%+v %v", state, err)
	}
	release()
	m.cleanupRetired(context.Background())
	views.createErr = errors.New("unavailable")
	if _, _, err := m.AcquireScannedEventSnapshot(context.Background(), func(context.Context, Snapshot) ([]teslacam.Video, error) { return nil, nil }); err == nil {
		t.Fatal("creation failure hidden")
	}
	calls := views.createCalls
	if _, _, err := m.AcquireScannedEventSnapshot(context.Background(), nil); err == nil || views.createCalls != calls {
		t.Fatal("failure was retried without cooldown")
	}
}

func TestRetiredEventViewCannotBeBorrowedAfterStorageMaintenance(t *testing.T) {
	views := &fakeSnapshotter{views: []mediasnapshot.View{{ID: "events", MountPath: "/events"}}}
	m := eventScanManager(t, views, &fakeVideoScanner{})
	state, release, err := m.AcquireScannedEventSnapshot(context.Background(), func(context.Context, Snapshot) ([]teslacam.Video, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := m.cleanupDisconnectedSnapshots(context.Background(), m.Snapshot()); err != nil {
		t.Fatal(err)
	}
	borrowed, releaseNew := m.AcquireEventSnapshotLease(state)
	releaseNew()
	if borrowed.MediaSnapshot != nil {
		t.Fatal("maintenance retained authority to enqueue old-view tasks")
	}
	release()
	m.cleanupRetired(context.Background())
	if m.hasMediaLeases() || len(views.removed) != 1 {
		t.Fatal("retired view was not reclaimed")
	}
}
