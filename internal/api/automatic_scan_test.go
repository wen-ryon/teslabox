package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/service"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

type scannedAutoController struct{ *autoController }

type notifiedAutoController struct {
	*scannedAutoController
	ready   chan struct{}
	scanned chan struct{}
}

func (c *notifiedAutoController) MediaSnapshotReady() <-chan struct{} { return c.ready }
func (c *notifiedAutoController) AcquireScannedEventSnapshot(ctx context.Context, scan func(context.Context, service.Snapshot) ([]teslacam.Video, error)) (service.Snapshot, func(), error) {
	select {
	case c.scanned <- struct{}{}:
	default:
	}
	return c.scannedAutoController.AcquireScannedEventSnapshot(ctx, scan)
}

func TestSharedSnapshotNotificationWakesScannerAndDisabledModeDoesNotScan(t *testing.T) {
	m, fake, _ := automaticFixture(t, "event", "")
	config := m.Automatic()
	config.Enabled = false
	if _, err := m.SaveAutomatic(config); err != nil {
		t.Fatal(err)
	}
	c := &notifiedAutoController{scannedAutoController: &scannedAutoController{autoController: fake}, ready: make(chan struct{}, 1), scanned: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		RunEventUploadsInLocation(ctx, c, m, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), time.UTC)
		close(done)
	}()
	c.ready <- struct{}{}
	select {
	case <-c.scanned:
		t.Fatal("disabled scanner read a snapshot")
	case <-time.After(50 * time.Millisecond):
	}
	config.Enabled = true
	if _, err := m.SaveAutomatic(config); err != nil {
		t.Fatal(err)
	}
	c.ready <- struct{}{}
	select {
	case <-c.scanned:
	case <-time.After(time.Second):
		t.Fatal("new snapshot waited for periodic timer")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scanner did not stop")
	}
	if c.acquired.Load() != c.released.Load() {
		t.Fatal("stopping scanner retained snapshot")
	}
}

func (c *scannedAutoController) AcquireScannedEventSnapshot(ctx context.Context, scan func(context.Context, service.Snapshot) ([]teslacam.Video, error)) (service.Snapshot, func(), error) {
	state, release := c.AcquireMediaSnapshot()
	videos, err := scan(ctx, state)
	if err != nil {
		release()
		return service.Snapshot{}, func() {}, err
	}
	// 此测试只验证发现与调度，视频可读性沿用已核验的 60 秒证据。
	for i := range videos {
		videos[i].Duration, videos[i].DurationState = 60, "verified"
	}
	state.Videos = videos
	return state, release, nil
}

func TestIncrementalEventScanAuditsCompletedDirectoriesAndSkipsDuplicateView(t *testing.T) {
	m, fake, start := automaticFixture(t, "event", "")
	c := &scannedAutoController{autoController: fake}
	root := c.state.MediaSnapshot.MountPath
	directory := filepath.Join(root, "TeslaCam", "SentryClips", "event")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "event.json"), []byte(`{"timestamp":"2026-10-02T20:03:01Z","reason":"sentry_aware_object_detection","camera":"5"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for minute := range 3 {
		for _, camera := range []string{"front", "left_pillar"} {
			name := start.Add(time.Duration(minute)*time.Minute).Format("2006-01-02_15-04-05") + "-" + camera + ".mp4"
			if err := os.WriteFile(filepath.Join(directory, name), []byte(name), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	scanner := newAutomaticEventScanner(time.UTC)
	tick := func(version string, seconds int, scans int) {
		t.Helper()
		c.state.MediaSnapshot.ID = version
		if err := scanner.tick(context.Background(), c, m, nil, start.Add(time.Duration(seconds)*time.Second)); err != nil {
			t.Fatal(err)
		}
		if scanner.scannedDirectories != scans {
			t.Fatalf("scanned %d directories, expected %d", scanner.scannedDirectories, scans)
		}
	}
	tick("a", 0, 1)
	tick("b", 20, 1)
	if len(m.History()) != 1 {
		t.Fatalf("tasks=%+v", m.History())
	}
	tick("c", 35, 0)
	tick("c", 40, 0)
	tick("d", 85, 1)
	if len(m.History()) != 1 {
		t.Fatal("audit duplicated upload")
	}
	if c.state.Videos[0].Path != "0-front.mp4" {
		t.Fatal("partial event index replaced page index")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Run(ctx)
	if c.acquired.Load() != c.released.Load() {
		t.Fatal("snapshot lease leaked")
	}
}

func TestEmptyHistoricalDirectoryWithLateMetadataIsNotNewEvent(t *testing.T) {
	m, fake, start := automaticFixture(t, "event", "")
	config := m.Automatic()
	config.IncludeExisting = false
	if _, err := m.SaveAutomatic(config); err != nil {
		t.Fatal(err)
	}
	c := &scannedAutoController{autoController: fake}
	directory := filepath.Join(c.state.MediaSnapshot.MountPath, "TeslaCam", "SavedClips", "empty")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	scanner := newAutomaticEventScanner(time.UTC)
	if err := scanner.tick(context.Background(), c, m, nil, start); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "event.json"), []byte(`{"timestamp":"2026-10-02T20:00:01Z","reason":"vehicle_auto_emergency_braking","camera":"0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c.state.MediaSnapshot.ID = "b"
	if err := scanner.tick(context.Background(), c, m, nil, start.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	if events := m.AutomaticEvents(); len(events) != 1 || events[0].State != "historical" || len(m.History()) != 0 {
		t.Fatalf("events=%+v", events)
	}
}

func TestWritingSupplementDoesNotResetStableMainAndUnusedMinuteIsIgnored(t *testing.T) {
	m, c, start := automaticFixture(t, "event", "composite4a")
	tick := func(version string, seconds int) {
		t.Helper()
		c.state.MediaSnapshot.ID = version
		if err := eventUploadTick(context.Background(), c, m, nil, start.Add(time.Duration(seconds)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	tick("a", 0)
	for n, version := range []string{"b", "c"} {
		if err := os.WriteFile(filepath.Join(c.state.MediaSnapshot.MountPath, "2-front.mp4"), []byte(fmt.Sprintf("supplement writing %d", n)), 0o600); err != nil {
			t.Fatal(err)
		}
		tick(version, 20*(n+1))
		if tasks := m.History(); len(tasks) != 1 || tasks[0].Request.Role != "event" {
			t.Fatalf("main blocked=%+v", tasks)
		}
	}
	tick("d", 60)
	if len(m.History()) != 2 {
		t.Fatal("completed supplement did not catch up")
	}
	if err := os.WriteFile(filepath.Join(c.state.MediaSnapshot.MountPath, "0-left_pillar.mp4"), []byte("unselected minute changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	tick("e", 80)
	if len(m.History()) != 2 {
		t.Fatal("unselected minute created another task")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Run(ctx)
	if c.acquired.Load() != c.released.Load() {
		t.Fatal("preparation lease leaked")
	}
}
