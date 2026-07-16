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
	calls int
	err   error
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

func TestManagerDataDriveDisconnectStopsGadgetAndFaults(t *testing.T) {
	t.Parallel()
	device := storage.Device{Path: "/dev/sda1", UUID: "data-uuid", FSType: "xfs", MountPoints: []string{"/var/lib/teslabox"}}
	detector := &sequentialDetector{results: []fakeDetector{{device: device}, {err: storage.ErrDeviceNotFound}, {err: storage.ErrDeviceNotFound}, {device: device}}}
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
	if err := manager.Refresh(t.Context()); err != nil || manager.Snapshot().Phase != PhaseGadgetActive {
		t.Fatalf("recovered drive error = %v, state = %#v", err, manager.Snapshot())
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
	manager.snapshotWithTimeout(t.Context())
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
