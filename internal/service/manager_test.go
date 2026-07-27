package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/backing"
	"github.com/wen-ryon/teslabox/internal/config"
	"github.com/wen-ryon/teslabox/internal/gadget"
	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/storage"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

type fakeDetector struct {
	device storage.Device
	err    error
}

func (d fakeDetector) Detect(context.Context) (storage.Device, error) {
	return d.device, d.err
}

type fakeInspector struct {
	status backing.Status
	err    error
}

func (i fakeInspector) Inspect(storage.Device) (backing.Status, error) {
	return i.status, i.err
}

type fakeProber struct {
	environment gadget.Environment
	err         error
}

type fakeMounter struct {
	calls        int
	err          error
	unmountCalls int
	unmountErr   error
}

func (m *fakeMounter) Unmount(context.Context) error {
	m.unmountCalls++
	return m.unmountErr
}

func (m *fakeMounter) Mount(context.Context, storage.Device) error {
	m.calls++
	return m.err
}

type fakeLifecycle struct {
	status     gadget.Status
	inspectErr error
	startErr   error
	stopErr    error
	startCalls int
	stopCalls  int
}

type fakeSnapshotter struct {
	views        []mediasnapshot.View
	createErr    error
	removeErr    error
	createCalls  int
	removed      []mediasnapshot.View
	recoverErr   error
	recoverCalls int
	space        mediasnapshot.SpaceStatus
	spaceErr     error
}

func (s *fakeSnapshotter) Recover(context.Context) error {
	s.recoverCalls++
	return s.recoverErr
}

func (s *fakeSnapshotter) Space() (mediasnapshot.SpaceStatus, error) {
	if s.space == (mediasnapshot.SpaceStatus{}) {
		return mediasnapshot.SpaceStatus{Ready: true}, s.spaceErr
	}
	return s.space, s.spaceErr
}

func (s *fakeSnapshotter) Create(context.Context) (mediasnapshot.View, error) {
	s.createCalls++
	if s.createErr != nil {
		return mediasnapshot.View{}, s.createErr
	}
	if len(s.views) == 0 {
		return mediasnapshot.View{ID: "snapshot-1", MountPath: "/run/teslabox/snapshots/snapshot-1", CreatedAt: time.Now()}, nil
	}
	index := min(s.createCalls-1, len(s.views)-1)
	return s.views[index], nil
}

func (s *fakeSnapshotter) Remove(_ context.Context, view mediasnapshot.View) error {
	s.removed = append(s.removed, view)
	return s.removeErr
}

type fakeVideoScanner struct {
	videos []teslacam.Video
	err    error
	root   string
}

func (s *fakeVideoScanner) Scan(root string) ([]teslacam.Video, error) {
	s.root = root
	return s.videos, s.err
}

func (l *fakeLifecycle) Inspect() (gadget.Status, error) {
	return l.status, l.inspectErr
}

func (l *fakeLifecycle) Start() (gadget.Status, error) {
	l.startCalls++
	if l.startErr != nil {
		return l.status, l.startErr
	}
	l.status.Exists = true
	l.status.Bound = true
	l.status.UDC = "fcc00000.dwc3"
	l.status.EnumerationComplete = true
	l.status.HostConnected = true
	return l.status, nil
}

func (l *fakeLifecycle) Stop() (gadget.Status, error) {
	l.stopCalls++
	if l.stopErr != nil {
		return l.status, l.stopErr
	}
	l.status.Exists = false
	l.status.Bound = false
	l.status.UDC = ""
	l.status.EnumerationComplete = false
	l.status.HostConnected = false
	return l.status, nil
}

type sequentialDetector struct {
	results []fakeDetector
	index   int
}

func (d *sequentialDetector) Detect(context.Context) (storage.Device, error) {
	index := min(d.index, len(d.results)-1)
	d.index++
	return d.results[index].device, d.results[index].err
}

type contextAwareDetector struct {
	device storage.Device
}

func (d contextAwareDetector) Detect(ctx context.Context) (storage.Device, error) {
	if err := ctx.Err(); err != nil {
		return storage.Device{}, fmt.Errorf("执行 lsblk: %w", err)
	}
	return d.device, nil
}

type blockingSnapshotter struct {
	*fakeSnapshotter
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingSnapshotter) Create(ctx context.Context) (mediasnapshot.View, error) {
	s.once.Do(func() { close(s.started) })
	select {
	case <-ctx.Done():
		return mediasnapshot.View{}, ctx.Err()
	case <-s.release:
		return s.fakeSnapshotter.Create(ctx)
	}
}

func (p fakeProber) Probe() (gadget.Environment, error) {
	return p.environment, p.err
}

func TestManagerRefreshPhases(t *testing.T) {
	t.Parallel()
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	tests := []struct {
		name              string
		detector          fakeDetector
		inspector         fakeInspector
		prober            fakeProber
		expectedPhase     Phase
		expectedErrorKind error
	}{
		{
			name:              "waiting for data drive",
			detector:          fakeDetector{err: storage.ErrDeviceNotFound},
			expectedPhase:     PhaseWaitingForDataDrive,
			expectedErrorKind: storage.ErrDeviceNotFound,
		},
		{
			name:          "preparing backing store",
			detector:      fakeDetector{device: device},
			inspector:     fakeInspector{status: backing.Status{State: backing.StateIncomplete}},
			prober:        fakeProber{environment: gadget.Environment{Ready: true}},
			expectedPhase: PhasePreparingBackingStore,
		},
		{
			name:          "gadget environment degraded",
			detector:      fakeDetector{device: device},
			inspector:     fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
			prober:        fakeProber{environment: gadget.Environment{Reason: "未发现 UDC"}},
			expectedPhase: PhaseDegraded,
		},
		{
			name:          "gadget active",
			detector:      fakeDetector{device: device},
			inspector:     fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
			prober:        fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
			expectedPhase: PhaseGadgetActive,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lifecycle := &fakeLifecycle{}
			manager := NewManager(
				config.Config{},
				test.detector,
				test.inspector,
				test.prober,
				&fakeMounter{},
				lifecycle,
				&fakeSnapshotter{},
				&fakeVideoScanner{},
				slog.New(slog.NewTextHandler(io.Discard, nil)),
			)
			err := manager.Refresh(t.Context())
			if test.expectedErrorKind != nil && !errors.Is(err, test.expectedErrorKind) {
				t.Fatalf("Refresh() error = %v", err)
			}
			if test.expectedErrorKind == nil && err != nil {
				t.Fatalf("Refresh() error = %v", err)
			}
			state := manager.Snapshot()
			if state.Phase != test.expectedPhase {
				t.Fatalf("Phase = %q, want %q", state.Phase, test.expectedPhase)
			}
		})
	}
}

func TestManagerRefreshTimeoutStartsAfterSnapshotLock(t *testing.T) {
	t.Parallel()
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	lifecycle := &fakeLifecycle{}
	snapshotter := &blockingSnapshotter{
		fakeSnapshotter: &fakeSnapshotter{},
		started:         make(chan struct{}),
		release:         make(chan struct{}),
	}
	manager := NewManager(
		config.Config{OperationTimeout: 20 * time.Millisecond},
		contextAwareDetector{device: device},
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		&fakeMounter{}, lifecycle, snapshotter, &fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}

	scanDone := make(chan error, 1)
	go func() { scanDone <- manager.Scan(t.Context()) }()
	<-snapshotter.started
	refreshDone := make(chan struct{})
	go func() {
		manager.refreshWithTimeout(t.Context())
		close(refreshDone)
	}()
	time.Sleep(50 * time.Millisecond)
	close(snapshotter.release)
	if err := <-scanDone; err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	<-refreshDone

	state := manager.Snapshot()
	if lifecycle.stopCalls != 0 || !state.GadgetStatus.Bound || state.Phase == PhaseFault {
		t.Fatalf("state = %#v, stop calls = %d", state, lifecycle.stopCalls)
	}
}

func TestManagerTransientDetectorFailureKeepsBoundGadget(t *testing.T) {
	t.Parallel()
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	detector := &sequentialDetector{results: []fakeDetector{{device: device}, {err: errors.New("lsblk 暂时不可用")}}}
	lifecycle := &fakeLifecycle{}
	manager := NewManager(
		config.Config{},
		detector,
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		&fakeMounter{}, lifecycle, &fakeSnapshotter{}, &fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Refresh(t.Context()); err == nil {
		t.Fatal("Refresh() error = nil, want detector error")
	}
	state := manager.Snapshot()
	if lifecycle.stopCalls != 0 || !state.GadgetStatus.Bound || state.Phase != PhaseDegraded {
		t.Fatalf("state = %#v, stop calls = %d", state, lifecycle.stopCalls)
	}
}

func TestManagerBoundGadgetWaitsForHostReenumeration(t *testing.T) {
	t.Parallel()
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	lifecycle := &fakeLifecycle{}
	manager := NewManager(
		config.Config{},
		fakeDetector{device: device},
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		&fakeMounter{}, lifecycle, &fakeSnapshotter{}, &fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	lifecycle.status.EnumerationComplete = false
	lifecycle.status.HostConnected = false
	manager.waitForEnumeration = func(context.Context, gadget.Status) (gadget.Status, bool, error) {
		t.Fatal("already-bound Gadget must not enter the boot enumeration wait")
		return gadget.Status{}, false, nil
	}
	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	state := manager.Snapshot()
	if lifecycle.stopCalls != 0 || !state.GadgetStatus.Bound || state.Phase != PhaseDegraded || state.GadgetPaused {
		t.Fatalf("state = %#v, stop calls = %d", state, lifecycle.stopCalls)
	}
}

func TestManagerSnapshotReturnsDefensiveCopies(t *testing.T) {
	t.Parallel()
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", MountPoints: []string{"/var/lib/teslabox"}}
	manager := NewManager(
		config.Config{},
		fakeDetector{device: device},
		fakeInspector{status: backing.Status{Ready: true, Missing: []string{"one"}}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"udc0"}}},
		&fakeMounter{},
		&fakeLifecycle{status: gadget.Status{OtherBindings: []gadget.Binding{{Gadget: "other", UDC: "udc0"}}}},
		&fakeSnapshotter{},
		&fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}

	first := manager.Snapshot()
	first.DataDrive.MountPoints[0] = "changed"
	first.Backing.Missing[0] = "changed"
	first.Gadget.UDCNames[0] = "changed"
	first.GadgetStatus.OtherBindings[0].Gadget = "changed"
	second := manager.Snapshot()
	if second.DataDrive.MountPoints[0] != "/var/lib/teslabox" || second.Backing.Missing[0] != "one" || second.Gadget.UDCNames[0] != "udc0" || second.GadgetStatus.OtherBindings[0].Gadget != "other" {
		t.Fatalf("Snapshot() returned shared state: %#v", second)
	}
}

func TestManagerMediaLeaseDefersRetiredSnapshotCleanup(t *testing.T) {
	t.Parallel()
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	first := mediasnapshot.View{ID: "snapshot-1", MountPath: "/run/teslabox/snapshots/snapshot-1"}
	second := mediasnapshot.View{ID: "snapshot-2", MountPath: "/run/teslabox/snapshots/snapshot-2"}
	third := mediasnapshot.View{ID: "snapshot-3", MountPath: "/run/teslabox/snapshots/snapshot-3"}
	snapshotter := &fakeSnapshotter{views: []mediasnapshot.View{first, second, third}}
	manager := NewManager(
		config.Config{},
		fakeDetector{device: device},
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		&fakeMounter{},
		&fakeLifecycle{},
		snapshotter,
		&fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	leased, release := manager.AcquireMediaSnapshot()
	if leased.MediaSnapshot == nil || leased.MediaSnapshot.ID != first.ID {
		t.Fatalf("leased snapshot = %#v", leased.MediaSnapshot)
	}
	if err := manager.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(snapshotter.removed) != 1 || snapshotter.removed[0].ID != second.ID {
		t.Fatalf("removed while snapshot-1 is leased = %#v", snapshotter.removed)
	}

	release()
	release()
	if err := manager.cleanupRetiredUntil(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if len(snapshotter.removed) != 2 || snapshotter.removed[1].ID != first.ID {
		t.Fatalf("removed after release = %#v", snapshotter.removed)
	}
}

func TestManagerLateDataDriveMountsAndStartsGadget(t *testing.T) {
	t.Parallel()
	unmounted := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs"}
	mounted := unmounted
	mounted.MountPoints = []string{"/var/lib/teslabox"}
	detector := &sequentialDetector{results: []fakeDetector{{device: unmounted}, {device: mounted}}}
	mounter := &fakeMounter{}
	lifecycle := &fakeLifecycle{}
	manager := NewManager(
		config.Config{},
		detector,
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		mounter,
		lifecycle,
		&fakeSnapshotter{},
		&fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	state := manager.Snapshot()
	if state.Phase != PhaseGadgetActive || mounter.calls != 1 || lifecycle.startCalls != 1 {
		t.Fatalf("state = %#v, mount calls = %d, start calls = %d", state, mounter.calls, lifecycle.startCalls)
	}
}

func TestManagerPauseKeepsGadgetUnboundAndResumeOnlySchedulesColdBoot(t *testing.T) {
	t.Parallel()
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	lifecycle := &fakeLifecycle{}
	manager := NewManager(
		config.Config{},
		fakeDetector{device: device},
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		&fakeMounter{},
		lifecycle,
		&fakeSnapshotter{},
		&fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	paused, err := manager.PauseGadget(t.Context())
	if err != nil {
		t.Fatalf("PauseGadget() error = %v", err)
	}
	if !paused.GadgetPaused || paused.Phase != PhaseReadyForGadget || paused.GadgetStatus.Bound {
		t.Fatalf("paused state = %#v", paused)
	}
	startsBeforeRefresh := lifecycle.startCalls
	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatalf("paused Refresh() error = %v", err)
	}
	if state := manager.Snapshot(); !state.GadgetPaused || state.GadgetStatus.Bound || lifecycle.startCalls != startsBeforeRefresh {
		t.Fatalf("paused refresh state = %#v, start calls = %d", state, lifecycle.startCalls)
	}

	resumed, err := manager.ResumeGadget(t.Context())
	if err != nil {
		t.Fatalf("ResumeGadget() error = %v", err)
	}
	if !resumed.GadgetPaused || !resumed.GadgetResumePending || resumed.Phase != PhaseReadyForGadget || resumed.GadgetStatus.Bound || lifecycle.startCalls != startsBeforeRefresh {
		t.Fatalf("resumed state = %#v, start calls = %d", resumed, lifecycle.startCalls)
	}
}

func TestManagerBeginStorageMaintenanceStopsAndUnmounts(t *testing.T) {
	t.Parallel()
	mounter := &fakeMounter{}
	lifecycle := &fakeLifecycle{status: gadget.Status{Exists: true, Bound: true, UDC: "fcc00000.dwc3"}}
	manager := NewManager(
		config.Config{},
		fakeDetector{},
		fakeInspector{},
		fakeProber{},
		mounter,
		lifecycle,
		&fakeSnapshotter{},
		&fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	release, err := manager.BeginStorageMaintenance(t.Context())
	if err != nil {
		t.Fatalf("BeginStorageMaintenance() error = %v", err)
	}
	state := manager.Snapshot()
	if !state.GadgetPaused || state.GadgetStatus.Bound || state.DataDrive != nil || lifecycle.stopCalls != 1 || mounter.unmountCalls != 1 {
		t.Fatalf("state = %#v, stop calls = %d, unmount calls = %d", state, lifecycle.stopCalls, mounter.unmountCalls)
	}
	// 维护结束回调可能同时经过正常返回和错误清理路径，必须保持幂等。
	release()
	release()
}

func TestManagerBeginMediaMaintenanceKeepsDataDriveMounted(t *testing.T) {
	t.Parallel()
	mounter := &fakeMounter{}
	lifecycle := &fakeLifecycle{status: gadget.Status{Exists: true, Bound: true, UDC: "fcc00000.dwc3"}}
	manager := NewManager(
		config.Config{},
		fakeDetector{},
		fakeInspector{},
		fakeProber{},
		mounter,
		lifecycle,
		&fakeSnapshotter{},
		&fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	manager.store(Snapshot{DataDrive: &device, GadgetStatus: lifecycle.status})

	release, err := manager.BeginMediaMaintenance(t.Context())
	if err != nil {
		t.Fatalf("BeginMediaMaintenance() error = %v", err)
	}
	state := manager.Snapshot()
	if !state.GadgetPaused || state.GadgetStatus.Bound || state.DataDrive == nil || mounter.unmountCalls != 0 || lifecycle.stopCalls != 1 {
		t.Fatalf("state = %#v, unmount calls = %d, stop calls = %d", state, mounter.unmountCalls, lifecycle.stopCalls)
	}
	release()
}

func TestManagerStopsUnconfiguredBootAttemptAndDoesNotRetry(t *testing.T) {
	t.Parallel()
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	lifecycle := &fakeLifecycle{}
	manager := NewManager(
		config.Config{},
		fakeDetector{device: device},
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		&fakeMounter{}, lifecycle, &fakeSnapshotter{}, &fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	manager.waitForEnumeration = func(context.Context, gadget.Status) (gadget.Status, bool, error) {
		return lifecycle.status, false, nil
	}

	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	state := manager.Snapshot()
	if state.Phase != PhaseReadyForGadget || !state.GadgetPaused || state.GadgetStatus.Bound || lifecycle.startCalls != 1 || lifecycle.stopCalls == 0 {
		t.Fatalf("state = %#v, start calls = %d", state, lifecycle.startCalls)
	}

	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatalf("second Refresh() error = %v", err)
	}
	if state = manager.Snapshot(); !state.GadgetPaused || state.GadgetStatus.Bound || lifecycle.startCalls != 1 {
		t.Fatalf("state = %#v, start calls = %d", state, lifecycle.startCalls)
	}
}

func TestManagerActivatesOfflineMediaAfterUnconfiguredBootAttempt(t *testing.T) {
	t.Parallel()
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	lifecycle := &fakeLifecycle{}
	snapshotter := &fakeSnapshotter{}
	scanner := &fakeVideoScanner{videos: []teslacam.Video{{ID: "offline-video"}}}
	manager := NewManager(
		config.Config{},
		fakeDetector{device: device},
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		&fakeMounter{}, lifecycle, snapshotter, scanner,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	manager.waitForEnumeration = func(context.Context, gadget.Status) (gadget.Status, bool, error) {
		return lifecycle.status, false, nil
	}

	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	startsBeforeMedia := lifecycle.startCalls
	if err := manager.ActivateMedia(t.Context()); err != nil {
		t.Fatalf("ActivateMedia() error = %v", err)
	}

	state := manager.Snapshot()
	if state.Phase != PhaseSnapshotReady || !state.GadgetPaused || state.GadgetStatus.Bound {
		t.Fatalf("state = %#v", state)
	}
	if snapshotter.createCalls != 1 || scanner.root == "" || state.VideoCount != 1 {
		t.Fatalf("snapshot calls = %d, scanner root = %q, video count = %d", snapshotter.createCalls, scanner.root, state.VideoCount)
	}
	if lifecycle.startCalls != startsBeforeMedia {
		t.Fatalf("offline media unexpectedly started Gadget: before = %d, after = %d", startsBeforeMedia, lifecycle.startCalls)
	}
}

func TestManagerRejectsOfflineMediaWithoutExplicitGadgetPause(t *testing.T) {
	t.Parallel()
	snapshotter := &fakeSnapshotter{}
	manager := NewManager(
		config.Config{},
		fakeDetector{},
		fakeInspector{},
		fakeProber{},
		&fakeMounter{},
		&fakeLifecycle{},
		snapshotter,
		&fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	manager.store(Snapshot{
		Phase:   PhaseReadyForGadget,
		Backing: backing.Status{State: backing.StateReady, Ready: true},
	})

	err := manager.ActivateMedia(t.Context())
	if !errors.Is(err, ErrSnapshotUnavailable) {
		t.Fatalf("ActivateMedia() error = %v, want ErrSnapshotUnavailable", err)
	}
	if snapshotter.createCalls != 0 {
		t.Fatalf("snapshot creation calls = %d, want 0", snapshotter.createCalls)
	}
}

func TestManagerPauseSurvivesServiceRestart(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pausePath := filepath.Join(root, "gadget-paused")
	bootIDPath := filepath.Join(root, "boot-id")
	if err := os.WriteFile(bootIDPath, []byte("boot-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	cfg := config.Config{GadgetPauseStatePath: pausePath, BootIDPath: bootIDPath}
	newManager := func(lifecycle *fakeLifecycle) *Manager {
		return NewManager(
			cfg,
			fakeDetector{device: device},
			fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
			fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
			&fakeMounter{}, lifecycle, &fakeSnapshotter{}, &fakeVideoScanner{},
			slog.New(slog.NewTextHandler(io.Discard, nil)),
		)
	}

	firstLifecycle := &fakeLifecycle{}
	first := newManager(firstLifecycle)
	if _, err := first.PauseGadget(t.Context()); err != nil {
		t.Fatalf("PauseGadget() error = %v", err)
	}
	if _, err := os.Stat(pausePath); err != nil {
		t.Fatalf("pause marker: %v", err)
	}

	secondLifecycle := &fakeLifecycle{}
	second := newManager(secondLifecycle)
	if err := second.Refresh(t.Context()); err != nil {
		t.Fatalf("restarted Refresh() error = %v", err)
	}
	if state := second.Snapshot(); !state.GadgetPaused || state.GadgetStatus.Bound || secondLifecycle.startCalls != 0 {
		t.Fatalf("state = %#v, start calls = %d", state, secondLifecycle.startCalls)
	}

	if _, err := second.ResumeGadget(t.Context()); err != nil {
		t.Fatalf("ResumeGadget() error = %v", err)
	}
	content, err := os.ReadFile(pausePath)
	if err != nil || strings.TrimSpace(string(content)) != "resume-after:boot-a" {
		t.Fatalf("resume marker = %q, error = %v", content, err)
	}

	sameBootLifecycle := &fakeLifecycle{}
	sameBoot := newManager(sameBootLifecycle)
	if err := sameBoot.Refresh(t.Context()); err != nil {
		t.Fatalf("same boot Refresh() error = %v", err)
	}
	if state := sameBoot.Snapshot(); !state.GadgetPaused || !state.GadgetResumePending || sameBootLifecycle.startCalls != 0 {
		t.Fatalf("same boot state = %#v, start calls = %d", state, sameBootLifecycle.startCalls)
	}

	if err := os.WriteFile(bootIDPath, []byte("boot-b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nextBootLifecycle := &fakeLifecycle{}
	nextBoot := newManager(nextBootLifecycle)
	if err := nextBoot.Refresh(t.Context()); err != nil {
		t.Fatalf("next boot Refresh() error = %v", err)
	}
	if state := nextBoot.Snapshot(); state.GadgetPaused || state.GadgetResumePending || state.Phase != PhaseGadgetActive || nextBootLifecycle.startCalls != 1 {
		t.Fatalf("next boot state = %#v, start calls = %d", state, nextBootLifecycle.startCalls)
	}
}

func TestManagerSameBootIDCannotBindAgainAfterServiceRestart(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	bootIDPath := filepath.Join(root, "boot-id")
	guardPath := filepath.Join(root, "gadget-guard")
	if err := os.WriteFile(bootIDPath, []byte("boot-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	cfg := config.Config{GadgetPauseStatePath: guardPath, BootIDPath: bootIDPath}
	newManager := func(lifecycle *fakeLifecycle) *Manager {
		return NewManager(
			cfg,
			fakeDetector{device: device},
			fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
			fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
			&fakeMounter{}, lifecycle, &fakeSnapshotter{}, &fakeVideoScanner{},
			slog.New(slog.NewTextHandler(io.Discard, nil)),
		)
	}

	firstLifecycle := &fakeLifecycle{}
	first := newManager(firstLifecycle)
	if err := first.Refresh(t.Context()); err != nil {
		t.Fatalf("first Refresh() error = %v", err)
	}
	if firstLifecycle.startCalls != 1 {
		t.Fatalf("first start calls = %d", firstLifecycle.startCalls)
	}
	content, err := os.ReadFile(guardPath)
	if err != nil || strings.TrimSpace(string(content)) != "attempt:boot-a" {
		t.Fatalf("attempt marker = %q, error = %v", content, err)
	}

	restartedLifecycle := &fakeLifecycle{}
	restarted := newManager(restartedLifecycle)
	if err := restarted.Refresh(t.Context()); err != nil {
		t.Fatalf("restarted Refresh() error = %v", err)
	}
	if state := restarted.Snapshot(); !state.GadgetPaused || state.GadgetStatus.Bound || restartedLifecycle.startCalls != 0 {
		t.Fatalf("restarted state = %#v, start calls = %d", state, restartedLifecycle.startCalls)
	}
}

func TestManagerTransientProbeDoesNotConsumeBootBindingAttempt(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	bootIDPath := filepath.Join(root, "boot-id")
	guardPath := filepath.Join(root, "gadget-guard")
	if err := os.WriteFile(bootIDPath, []byte("boot-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	detector := &sequentialDetector{results: []fakeDetector{
		{err: errors.New("lsblk 暂时不可用")},
		{device: device},
	}}
	lifecycle := &fakeLifecycle{}
	manager := NewManager(
		config.Config{GadgetPauseStatePath: guardPath, BootIDPath: bootIDPath},
		detector,
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		&fakeMounter{}, lifecycle, &fakeSnapshotter{}, &fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	if err := manager.Refresh(t.Context()); err == nil {
		t.Fatal("first Refresh() error = nil, want transient detector error")
	}
	content, err := os.ReadFile(guardPath)
	if err != nil || strings.TrimSpace(string(content)) != "service:boot-a" {
		t.Fatalf("service ownership marker = %q, error = %v", content, err)
	}
	restartedLifecycle := &fakeLifecycle{}
	restarted := NewManager(
		config.Config{GadgetPauseStatePath: guardPath, BootIDPath: bootIDPath},
		fakeDetector{device: device},
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		&fakeMounter{}, restartedLifecycle, &fakeSnapshotter{}, &fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := restarted.Refresh(t.Context()); err != nil {
		t.Fatalf("restarted Refresh() error = %v", err)
	}
	if restartedLifecycle.startCalls != 0 || !restarted.Snapshot().GadgetPaused {
		t.Fatalf("restarted start calls = %d, state = %#v", restartedLifecycle.startCalls, restarted.Snapshot())
	}
	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatalf("second Refresh() error = %v", err)
	}
	if lifecycle.startCalls != 1 || !manager.Snapshot().GadgetStatus.Bound {
		t.Fatalf("start calls = %d, state = %#v", lifecycle.startCalls, manager.Snapshot())
	}
}

func TestManagerSameBootRestartKeepsExistingGadgetBound(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	bootIDPath := filepath.Join(root, "boot-id")
	guardPath := filepath.Join(root, "gadget-guard")
	if err := os.WriteFile(bootIDPath, []byte("boot-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(guardPath, []byte("attempt:boot-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	lifecycle := &fakeLifecycle{status: gadget.Status{
		Exists: true, Bound: true, UDC: "fcc00000.dwc3", EnumerationComplete: true,
	}}
	manager := NewManager(
		config.Config{GadgetPauseStatePath: guardPath, BootIDPath: bootIDPath},
		fakeDetector{device: device},
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		&fakeMounter{}, lifecycle, &fakeSnapshotter{}, &fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	state := manager.Snapshot()
	if state.GadgetPaused || !state.GadgetStatus.Bound || lifecycle.stopCalls != 0 || lifecycle.startCalls != 0 {
		t.Fatalf("state = %#v, start calls = %d, stop calls = %d", state, lifecycle.startCalls, lifecycle.stopCalls)
	}
}

func TestReadBootUptimeRejectsInvalidOrOverflowValues(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "-1 0\n", "NaN 0\n", "+Inf 0\n", "1e30 0\n"} {
		value := value
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "uptime")
			if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := readBootUptime(path); err == nil {
				t.Fatalf("readBootUptime(%q) error = nil", value)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "uptime")
	if err := os.WriteFile(path, []byte("12.5 30.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	uptime, enforced, err := readBootUptime(path)
	if err != nil || !enforced || uptime != 12500*time.Millisecond {
		t.Fatalf("uptime = %s, enforced = %t, err = %v", uptime, enforced, err)
	}
}

func TestManagerDoesNotBindAfterColdBootSafetyWindow(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	bootIDPath := filepath.Join(root, "boot-id")
	uptimePath := filepath.Join(root, "uptime")
	guardPath := filepath.Join(root, "gadget-guard")
	if err := os.WriteFile(bootIDPath, []byte("boot-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(uptimePath, []byte("3600.00 7200.00\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	lifecycle := &fakeLifecycle{}
	manager := NewManager(
		config.Config{
			GadgetPauseStatePath: guardPath,
			BootIDPath:           bootIDPath,
			BootUptimePath:       uptimePath,
		},
		fakeDetector{device: device},
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		&fakeMounter{}, lifecycle, &fakeSnapshotter{}, &fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	state := manager.Snapshot()
	if !state.GadgetPaused || state.GadgetStatus.Bound || lifecycle.startCalls != 0 {
		t.Fatalf("state = %#v, start calls = %d", state, lifecycle.startCalls)
	}
	if !strings.Contains(state.LastError, "安全绑定窗口") {
		t.Fatalf("LastError = %q", state.LastError)
	}
	content, err := os.ReadFile(guardPath)
	if err != nil || strings.TrimSpace(string(content)) != "attempt:boot-a" {
		t.Fatalf("attempt marker = %q, error = %v", content, err)
	}
}

func TestManagerExpiredBindingWindowKeepsExistingGadgetBound(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	uptimePath := filepath.Join(root, "uptime")
	if err := os.WriteFile(uptimePath, []byte("3600.00 7200.00\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	lifecycle := &fakeLifecycle{status: gadget.Status{
		Exists:              true,
		Bound:               true,
		UDC:                 "fcc00000.dwc3",
		EnumerationComplete: true,
	}}
	manager := NewManager(
		config.Config{BootUptimePath: uptimePath},
		fakeDetector{device: device},
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		&fakeMounter{}, lifecycle, &fakeSnapshotter{}, &fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if state := manager.Snapshot(); state.GadgetPaused || !state.GadgetStatus.Bound || lifecycle.stopCalls != 0 {
		t.Fatalf("state = %#v, stop calls = %d", state, lifecycle.stopCalls)
	}
}

func TestManagerPauseFailureRemainsPausedForRetry(t *testing.T) {
	t.Parallel()
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	lifecycle := &fakeLifecycle{stopErr: errors.New("unbind failed")}
	manager := NewManager(
		config.Config{},
		fakeDetector{device: device},
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		&fakeMounter{},
		lifecycle,
		&fakeSnapshotter{},
		&fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	failed, err := manager.PauseGadget(t.Context())
	if err == nil || !failed.GadgetPaused || failed.Phase != PhaseFault || !failed.GadgetStatus.Bound {
		t.Fatalf("failed pause state = %#v, error = %v", failed, err)
	}

	lifecycle.stopErr = nil
	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatalf("retry Refresh() error = %v", err)
	}
	if state := manager.Snapshot(); !state.GadgetPaused || state.GadgetStatus.Bound || state.Phase != PhaseReadyForGadget {
		t.Fatalf("retried pause state = %#v", state)
	}
}

func TestManagerDataDriveDisconnectStopsGadgetAndFaults(t *testing.T) {
	t.Parallel()
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	detector := &sequentialDetector{results: []fakeDetector{
		{device: device},
		{err: storage.ErrDeviceNotFound},
		{err: storage.ErrDeviceNotFound},
		{err: storage.ErrDeviceNotFound},
		{device: device},
	}}
	lifecycle := &fakeLifecycle{}
	manager := NewManager(
		config.Config{},
		detector,
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		&fakeMounter{},
		lifecycle,
		&fakeSnapshotter{},
		&fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	manager.waitForDriveConfirm = func(context.Context) error { return nil }

	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatalf("first Refresh() error = %v", err)
	}
	err := manager.Refresh(t.Context())
	if !errors.Is(err, ErrDataDriveDisconnected) {
		t.Fatalf("second Refresh() error = %v, want disconnect", err)
	}
	state := manager.Snapshot()
	if state.Phase != PhaseFault || state.GadgetStatus.Bound || lifecycle.stopCalls == 0 {
		t.Fatalf("state = %#v, stop calls = %d", state, lifecycle.stopCalls)
	}
	if err := manager.Refresh(t.Context()); !errors.Is(err, ErrDataDriveDisconnected) || manager.Snapshot().Phase != PhaseFault {
		t.Fatalf("repeated missing drive error = %v, state = %#v", err, manager.Snapshot())
	}
	if err := manager.Refresh(t.Context()); err != nil || manager.Snapshot().Phase != PhaseReadyForGadget || !manager.Snapshot().GadgetPaused {
		t.Fatalf("recovered drive error = %v, state = %#v", err, manager.Snapshot())
	}
}

func TestManagerTransientMissingDataDriveKeepsActiveGadgetBound(t *testing.T) {
	t.Parallel()
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	detector := &sequentialDetector{results: []fakeDetector{
		{device: device},
		{err: storage.ErrDeviceNotFound},
		{device: device},
	}}
	lifecycle := &fakeLifecycle{}
	manager := NewManager(
		config.Config{},
		detector,
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		&fakeMounter{},
		lifecycle,
		&fakeSnapshotter{},
		&fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	manager.waitForDriveConfirm = func(context.Context) error { return nil }

	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatalf("first Refresh() error = %v", err)
	}
	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatalf("transient Refresh() error = %v", err)
	}
	state := manager.Snapshot()
	if !state.GadgetStatus.Bound || state.Phase != PhaseGadgetActive || lifecycle.stopCalls != 0 {
		t.Fatalf("state = %#v, stop calls = %d", state, lifecycle.stopCalls)
	}
}

func TestManagerColdStartWithoutDriveStopsStaleGadget(t *testing.T) {
	t.Parallel()
	lifecycle := &fakeLifecycle{status: gadget.Status{Exists: true, Bound: true, UDC: "fcc00000.dwc3"}}
	manager := NewManager(
		config.Config{},
		fakeDetector{err: storage.ErrDeviceNotFound},
		fakeInspector{},
		fakeProber{},
		&fakeMounter{},
		lifecycle,
		&fakeSnapshotter{},
		&fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	manager.waitForDriveConfirm = func(context.Context) error { return nil }

	err := manager.Refresh(t.Context())
	if !errors.Is(err, ErrDataDriveDisconnected) {
		t.Fatalf("Refresh() error = %v, want disconnect", err)
	}
	state := manager.Snapshot()
	if state.Phase != PhaseFault || state.GadgetStatus.Bound || lifecycle.stopCalls != 1 {
		t.Fatalf("state = %#v, stop calls = %d", state, lifecycle.stopCalls)
	}
}

func TestManagerScanPublishesSnapshotVideos(t *testing.T) {
	t.Parallel()
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	view := mediasnapshot.View{ID: "snapshot-1", MountPath: "/run/teslabox/snapshots/snapshot-1"}
	snapshotter := &fakeSnapshotter{views: []mediasnapshot.View{view}}
	scanner := &fakeVideoScanner{videos: []teslacam.Video{{ID: "video-1", Path: "TeslaCam/RecentClips/example.mp4"}}}
	manager := NewManager(
		config.Config{},
		fakeDetector{device: device},
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		&fakeMounter{},
		&fakeLifecycle{},
		snapshotter,
		scanner,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Scan(t.Context()); err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	state := manager.Snapshot()
	if state.Phase != PhaseSnapshotReady || state.MediaSnapshot == nil || state.MediaSnapshot.ID != view.ID || state.VideoCount != 1 || len(state.Videos) != 1 {
		t.Fatalf("state = %#v", state)
	}
	if scanner.root != view.MountPath {
		t.Fatalf("scanner root = %q, want %q", scanner.root, view.MountPath)
	}
}

func TestManagerScanFailureKeepsLastUsableSnapshot(t *testing.T) {
	t.Parallel()
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	first := mediasnapshot.View{ID: "snapshot-1", MountPath: "/run/teslabox/snapshots/snapshot-1"}
	second := mediasnapshot.View{ID: "snapshot-2", MountPath: "/run/teslabox/snapshots/snapshot-2"}
	snapshotter := &fakeSnapshotter{views: []mediasnapshot.View{first, second}}
	scanner := &fakeVideoScanner{videos: []teslacam.Video{{ID: "video-1"}}}
	manager := NewManager(
		config.Config{},
		fakeDetector{device: device},
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		&fakeMounter{},
		&fakeLifecycle{},
		snapshotter,
		scanner,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	scanner.err = errors.New("测试扫描失败")
	if err := manager.Scan(t.Context()); err == nil {
		t.Fatal("Scan() error = nil, want scan error")
	}
	state := manager.Snapshot()
	if state.Phase != PhaseDegraded || state.MediaSnapshot == nil || state.MediaSnapshot.ID != first.ID || state.VideoCount != 1 || state.SnapshotError == "" {
		t.Fatalf("state = %#v", state)
	}
	if len(snapshotter.removed) != 1 || snapshotter.removed[0].ID != second.ID {
		t.Fatalf("removed = %#v", snapshotter.removed)
	}
}

func TestManagerScanRecoversOrphansOnlyOnce(t *testing.T) {
	t.Parallel()
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	snapshotter := &fakeSnapshotter{}
	manager := NewManager(
		config.Config{},
		fakeDetector{device: device},
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		&fakeMounter{}, &fakeLifecycle{}, snapshotter, &fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	if snapshotter.recoverCalls != 1 {
		t.Fatalf("recover calls = %d, want 1", snapshotter.recoverCalls)
	}
}

func TestManagerLowSpaceKeepsGadgetAndEntersDegradedState(t *testing.T) {
	t.Parallel()
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	space := mediasnapshot.SpaceStatus{TotalBytes: 100, FreeBytes: 4, MinFreeBytes: 10, MinFreePercent: 5, FreePercent: 4, Reason: "快照空间不足"}
	snapshotter := &fakeSnapshotter{space: space}
	lifecycle := &fakeLifecycle{}
	manager := NewManager(
		config.Config{},
		fakeDetector{device: device},
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		&fakeMounter{}, lifecycle, snapshotter, &fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Scan(t.Context()); !errors.Is(err, mediasnapshot.ErrInsufficientSpace) {
		t.Fatalf("Scan() error = %v, want insufficient space", err)
	}
	state := manager.Snapshot()
	if state.Phase != PhaseDegraded || !state.GadgetStatus.Bound || snapshotter.createCalls != 0 || state.SnapshotSpace.FreeBytes != 4 {
		t.Fatalf("state = %#v, create calls = %d", state, snapshotter.createCalls)
	}
}

func TestManagerAutomaticSnapshotUsesFailureBackoff(t *testing.T) {
	t.Parallel()
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	snapshotter := &fakeSnapshotter{createErr: errors.New("测试快照失败")}
	manager := NewManager(
		config.Config{ScanInterval: time.Minute, SnapshotTimeout: time.Second, SnapshotRetryMax: 10 * time.Minute},
		fakeDetector{device: device},
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		&fakeMounter{}, &fakeLifecycle{}, snapshotter, &fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	if err := manager.ActivateMedia(t.Context()); err == nil {
		t.Fatal("ActivateMedia() error = nil, want snapshot failure")
	}
	if snapshotter.createCalls != 1 {
		t.Fatalf("first create calls = %d", snapshotter.createCalls)
	}
	now = now.Add(30 * time.Second)
	manager.snapshotWithTimeout(t.Context())
	if snapshotter.createCalls != 1 {
		t.Fatalf("backoff create calls = %d, want 1", snapshotter.createCalls)
	}
	now = now.Add(31 * time.Second)
	manager.snapshotWithTimeout(t.Context())
	if snapshotter.createCalls != 2 {
		t.Fatalf("retry create calls = %d, want 2", snapshotter.createCalls)
	}
}

func TestManagerAutomaticSnapshotUsesIdleAndActiveCadence(t *testing.T) {
	t.Parallel()
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	snapshotter := &fakeSnapshotter{}
	manager := NewManager(
		config.Config{
			ScanInterval:         30 * time.Second,
			SnapshotIdleInterval: 5 * time.Minute,
			SnapshotActiveWindow: 2 * time.Minute,
			SnapshotTimeout:      time.Second,
			SnapshotRetryMax:     10 * time.Minute,
		},
		fakeDetector{device: device},
		fakeInspector{status: backing.Status{State: backing.StateReady, Ready: true}},
		fakeProber{environment: gadget.Environment{Ready: true, UDCNames: []string{"fcc00000.dwc3"}}},
		&fakeMounter{}, &fakeLifecycle{}, snapshotter, &fakeVideoScanner{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	now := time.Date(2026, 7, 23, 17, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}

	manager.snapshotWithTimeout(t.Context())
	if snapshotter.createCalls != 0 {
		t.Fatalf("idle startup create calls = %d, want 0", snapshotter.createCalls)
	}
	if err := manager.ActivateMedia(t.Context()); err != nil {
		t.Fatal(err)
	}
	if snapshotter.createCalls != 1 {
		t.Fatalf("media activation create calls = %d", snapshotter.createCalls)
	}
	now = now.Add(29 * time.Second)
	if err := manager.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	manager.snapshotWithTimeout(t.Context())
	if snapshotter.createCalls != 1 {
		t.Fatalf("lifecycle refresh changed snapshot cadence, create calls = %d", snapshotter.createCalls)
	}
	now = now.Add(time.Second)
	manager.snapshotWithTimeout(t.Context())
	if snapshotter.createCalls != 2 {
		t.Fatalf("active create calls = %d", snapshotter.createCalls)
	}
	now = now.Add(2 * time.Minute)
	manager.snapshotWithTimeout(t.Context())
	if snapshotter.createCalls != 2 {
		t.Fatalf("idle create calls before interval = %d", snapshotter.createCalls)
	}
	now = now.Add(3 * time.Minute)
	manager.snapshotWithTimeout(t.Context())
	if snapshotter.createCalls != 3 {
		t.Fatalf("idle create calls at interval = %d", snapshotter.createCalls)
	}
}

func TestSnapshotRetryDelay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		interval time.Duration
		maximum  time.Duration
		failures int
		want     time.Duration
	}{
		{name: "first failure", interval: time.Minute, maximum: 10 * time.Minute, failures: 1, want: time.Minute},
		{name: "third failure", interval: time.Minute, maximum: 10 * time.Minute, failures: 3, want: 4 * time.Minute},
		{name: "capped", interval: time.Minute, maximum: 10 * time.Minute, failures: 8, want: 10 * time.Minute},
		{name: "maximum below interval", interval: time.Minute, maximum: 30 * time.Second, failures: 1, want: 30 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := snapshotRetryDelay(test.interval, test.maximum, test.failures); got != test.want {
				t.Fatalf("snapshotRetryDelay() = %s, want %s", got, test.want)
			}
		})
	}
}
