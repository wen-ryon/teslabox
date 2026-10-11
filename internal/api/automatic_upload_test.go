package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/nasupload"
	"github.com/wen-ryon/teslabox/internal/recordingcache"
	"github.com/wen-ryon/teslabox/internal/service"
	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

type autoController struct {
	state              service.Snapshot
	acquired, released atomic.Int32
}

func (c *autoController) AcquireEventSnapshot(context.Context) (service.Snapshot, func(), error) {
	state, release := c.AcquireMediaSnapshot()
	return state, release, nil
}
func (c *autoController) AcquireMediaSnapshot() (service.Snapshot, func()) {
	c.acquired.Add(1)
	return c.state, func() { c.released.Add(1) }
}
func (*autoController) RecordingIdentity(service.Snapshot) (string, error) { return "disk", nil }

type autoTransport struct{}

func (autoTransport) Obscure(context.Context, string) (string, error) { return "private", nil }
func (autoTransport) Test(context.Context, nasupload.Target) error    { return nil }
func (autoTransport) Upload(_ context.Context, _ nasupload.Target, local, remote, hash, id string) error {
	b, err := os.ReadFile(local)
	if err != nil {
		return err
	}
	got := sha256.Sum256(b)
	if hex.EncodeToString(got[:]) != hash {
		return fmt.Errorf("digest mismatch")
	}
	return nil
}

type autoProcessor struct{}

func (autoProcessor) Process(_ context.Context, _ nasupload.Quality, _ []nasupload.Input, path string) error {
	return os.WriteFile(path, []byte("encoded composite"), 0o600)
}

func automaticFixture(t *testing.T, scope, supplement string) (*nasupload.Manager, *autoController, time.Time) {
	t.Helper()
	root := t.TempDir()
	media := filepath.Join(root, "media")
	cache, err := recordingcache.New(recordingcache.Config{Root: filepath.Join(media, "recordings"), MediaRoot: media, MaxBytes: 32 << 20, MaxTotalBytes: 256 << 20, MinFreeBytes: 1, IdleTTL: 0})
	if err != nil {
		t.Fatal(err)
	}
	m, err := nasupload.New(nasupload.Config{Root: filepath.Join(media, "uploads"), Cache: cache, Transport: autoTransport{}, Processor: autoProcessor{}})
	if err != nil {
		t.Fatal(err)
	}
	target, err := m.SaveTarget(context.Background(), nasupload.Target{Name: "NAS", Protocol: "smb", Host: "localhost", User: "test", Password: "private", Path: "share", Quality: nasupload.Quality{Mode: "original"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.SetDefaultTarget(target.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = m.SaveAutomatic(nasupload.AutoConfig{Enabled: true, IncludeExisting: true, Scope: scope, SupplementLayout: supplement, SupplementQuality: nasupload.Quality{Mode: "compressed", Height: 240, FPS: 24, BitrateKbps: 500}}); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 2, 20, 0, 0, 0, time.UTC)
	at := start.Add(3*time.Minute + time.Second)
	metadata := &teslacam.EventMetadata{Reason: "sentry_aware_object_detection", CameraView: "left_pillar", OccurredAt: &at}
	c := &autoController{state: service.Snapshot{MediaSnapshot: &mediasnapshot.View{ID: "a", MountPath: root}}}
	for minute := range 3 {
		for _, camera := range []string{"front", "back", "left_repeater", "right_repeater", "left_pillar", "right_pillar"} {
			name := fmt.Sprintf("%d-%s.mp4", minute, camera)
			b := []byte("sample " + name)
			if err = os.WriteFile(filepath.Join(root, name), b, 0o600); err != nil {
				t.Fatal(err)
			}
			c.state.Videos = append(c.state.Videos, teslacam.Video{ID: name, Path: name, Camera: camera, ClipType: "sentry", Event: "event", EventMetadata: metadata, RecordedAt: start.Add(time.Duration(minute) * time.Minute), Size: int64(len(b)), Duration: 60, DurationState: "verified"})
		}
	}
	return m, c, start
}

func TestAutoTailMinuteAllScopesAndGap(t *testing.T) {
	for _, test := range []struct {
		scope string
		want  []int
	}{{"event", []int{2}}, {"surrounding", []int{1, 2}}, {"directory", []int{0, 1, 2}}} {
		t.Run(test.scope, func(t *testing.T) {
			m, c, start := automaticFixture(t, test.scope, "")
			_ = m
			moments := groupVideos(c.state.Videos, videoGroupTolerance)
			slices.SortFunc(moments, func(a, b videoGroup) int { return a.RecordedAt.Compare(b.RecordedAt) })
			event := nasupload.AutoEvent{Camera: "left_pillar", OccurredAt: start.Add(3*time.Minute + time.Second), Policy: nasupload.AutoConfig{Scope: test.scope}}
			got, err := automaticMinuteRange(context.Background(), c.state.MediaSnapshot.MountPath, moments, event, nil)
			if err != nil || !slices.Equal(got, test.want) {
				t.Fatalf("range %v %v", got, err)
			}
			event.OccurredAt = start.Add(-time.Second)
			if _, err = automaticMinuteRange(context.Background(), "", moments, event, nil); err == nil {
				t.Fatal("before first incorrectly clamped")
			}
			for i := range moments[0].Videos {
				moments[0].Videos[i].Duration = 10
			}
			event.OccurredAt = start.Add(30 * time.Second)
			if _, err = automaticMinuteRange(context.Background(), "", moments, event, nil); err == nil {
				t.Fatal("internal gap incorrectly clamped")
			}
		})
	}
}

func TestAutoIndependentSnapshotsDedupSupplementAndFixedDestination(t *testing.T) {
	m, c, start := automaticFixture(t, "surrounding", "composite4a")
	tick := func(version string, offset time.Duration) {
		t.Helper()
		c.state.MediaSnapshot.ID = version
		if err := eventUploadTick(context.Background(), c, m, nil, start.Add(offset)); err != nil {
			t.Fatal(err)
		}
	}
	tick("a", 0)
	tick("a", 20*time.Second)
	if len(m.History()) != 0 {
		t.Fatal("same snapshot queued premature upload")
	}
	target, _ := m.DefaultTarget()
	target.Host = "changed-host"
	target.Path = "changed-share"
	if _, err := m.SaveTarget(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	tick("b", 40*time.Second)
	if len(m.History()) != 4 {
		t.Fatalf("tasks=%+v", m.History())
	}
	for _, task := range m.History() {
		if task.TargetRevision == m.Targets()[0].Revision {
			t.Fatal("changed target redirected event")
		}
		if task.Request.Role == "event" && (task.Request.Quality.Mode != "original" || !slices.Equal(task.Request.Cameras, []string{"left_pillar"})) {
			t.Fatal(task)
		}
		if task.Request.Role == "supplement" && (task.Request.Quality.Mode != "compressed" || task.Request.Quality.Layout != "composite4a" || len(task.Request.Cameras) != 4) {
			t.Fatal(task)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	t.Cleanup(func() {
		cancel()
		wait, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err := m.Wait(wait); err != nil {
			t.Error(err)
		}
	})
	// 四个任务包含多次刷盘，允许慢速 Windows 测试盘完成持久化，不能用调度延迟判断恢复失败。
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m.AutomaticEvents()[0].State == "completed" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if m.AutomaticEvents()[0].State != "completed" {
		t.Fatalf("not completed: %+v", m.History())
	}
	tick("c", 55*time.Second)
	if len(m.History()) != 4 {
		t.Fatal("repeated scan duplicated upload")
	}
	if c.acquired.Load() != c.released.Load() {
		t.Fatalf("lease leak: %d/%d", c.acquired.Load(), c.released.Load())
	}
	if m.AutomaticEvents()[0].CoverageNote == "" {
		t.Fatal("missing neighbor not explained")
	}
}

func TestAutoOnlyVehicleEventsAndMissingSupplementDoesNotBlockMain(t *testing.T) {
	m, c, start := automaticFixture(t, "directory", "composite6")
	c.state.Videos = slices.DeleteFunc(c.state.Videos, func(v teslacam.Video) bool { return v.Camera == "right_pillar" })
	for _, reason := range []string{"user_interaction_dashcam_icon_tapped", "user_interaction_honk", "unknown"} {
		if automaticVehicleEvent(&teslacam.EventMetadata{Reason: reason}) {
			t.Fatal(reason)
		}
	}
	if !automaticVehicleEvent(&teslacam.EventMetadata{Reason: "vehicle_auto_emergency_braking"}) {
		t.Fatal("braking excluded")
	}
	for i := range c.state.Videos {
		c.state.Videos[i].ClipType = "saved"
		c.state.Videos[i].EventMetadata.Reason = "vehicle_auto_emergency_braking"
	}
	if err := eventUploadTick(context.Background(), c, m, nil, start); err != nil {
		t.Fatal(err)
	}
	c.state.MediaSnapshot.ID = "b"
	if err := eventUploadTick(context.Background(), c, m, nil, start.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(m.History()) != 3 || m.AutomaticEvents()[0].State != "waiting_files" {
		t.Fatalf("missing supplement blocked main: %+v", m.History())
	}
	// 队列未运行时也必须归还所有快照租约。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Run(ctx)
	if c.acquired.Load() != c.released.Load() {
		t.Fatal("canceled preparation lease leak")
	}
}

func TestAutoRecentAndManualMetadataNeverTriggers(t *testing.T) {
	for _, kind := range []string{"recent", "saved"} {
		t.Run(kind, func(t *testing.T) {
			m, c, start := automaticFixture(t, "event", "")
			for i := range c.state.Videos {
				c.state.Videos[i].ClipType = kind
				if kind == "saved" {
					c.state.Videos[i].EventMetadata.Reason = "user_interaction_honk"
				}
			}
			for i := range 2 {
				c.state.MediaSnapshot.ID = fmt.Sprint(i)
				if err := eventUploadTick(context.Background(), c, m, nil, start.Add(time.Duration(i)*20*time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			if len(m.History()) != 0 || len(m.AutomaticEvents()) != 0 {
				t.Fatal("manual/recent triggered")
			}
		})
	}
}

func TestAutoLateMetadataAndLateCameraDoNotDuplicatePreparedMinute(t *testing.T) {
	m, c, start := automaticFixture(t, "event", "")
	metadata := c.state.Videos[0].EventMetadata
	for i := range c.state.Videos {
		c.state.Videos[i].EventMetadata = nil
	}
	config := m.Automatic()
	config.IncludeExisting = false
	if _, err := m.SaveAutomatic(config); err != nil {
		t.Fatal(err)
	}
	tick := func(version string, seconds int) {
		t.Helper()
		c.state.MediaSnapshot.ID = version
		if err := eventUploadTick(context.Background(), c, m, nil, start.Add(time.Duration(seconds)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	tick("a", 0)
	for i := range c.state.Videos {
		c.state.Videos[i].EventMetadata = metadata
	}
	tick("b", 20)
	if len(m.History()) != 0 || m.AutomaticEvents()[0].State != "historical" {
		t.Fatal("initial directory metadata arrival bypassed baseline")
	}
	for i := range c.state.Videos {
		c.state.Videos[i].Event = "new-event"
	}
	tick("c", 40)
	tick("d", 60)
	if len(m.History()) != 1 {
		t.Fatal("new directory not prepared")
	}
	// 同分钟较早的其它视角晚到会改变分组标识，但事件主视频的路径保持不变。
	video := c.state.Videos[len(c.state.Videos)-1]
	video.ID = "late"
	video.Path = "late.mp4"
	video.Camera = "front"
	video.RecordedAt = video.RecordedAt.Add(-time.Second)
	if err := os.WriteFile(filepath.Join(c.state.MediaSnapshot.MountPath, video.Path), []byte("late camera"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.state.Videos = append(c.state.Videos, video)
	tick("e", 80)
	tick("f", 100)
	if len(m.History()) != 1 {
		t.Fatalf("late camera duplicated main: %+v", m.History())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Run(ctx)
	if c.acquired.Load() != c.released.Load() {
		t.Fatal("late camera lease leak")
	}
}
