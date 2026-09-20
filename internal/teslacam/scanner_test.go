package teslacam

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkScannerScanTenThousandMP4(b *testing.B) {
	root := b.TempDir()
	directory := filepath.Join(root, "TeslaCam", "RecentClips")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		b.Fatal(err)
	}
	fixture := movieHeaderFixture(0, 1_000, 60_000)
	startedAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for index := range 10_000 {
		name := fmt.Sprintf("%s-front.mp4", startedAt.Add(time.Duration(index)*time.Minute).Format("2006-01-02_15-04-05"))
		if err := os.WriteFile(filepath.Join(directory, name), fixture, 0o644); err != nil {
			b.Fatal(err)
		}
	}
	scanner := NewScanner(time.UTC)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		videos, err := scanner.Scan(root)
		if err != nil {
			b.Fatal(err)
		}
		if len(videos) != 10_000 {
			b.Fatalf("video count = %d, want 10000", len(videos))
		}
	}
}

func TestScannerUsesConfiguredVehicleTimeZone(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	directory := filepath.Join(root, "TeslaCam", "RecentClips")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "2026-07-23_08-00-00-front.mp4"), []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	location := time.FixedZone("UTC+08", 8*60*60)
	videos, err := NewScanner(location).Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(videos) != 1 || videos[0].RecordedAt.Hour() != 8 || videos[0].RecordedAt.Location() != location {
		t.Fatalf("recorded time = %#v", videos)
	}
}

func TestScannerDefersMP4DurationUntilRequested(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	directory := filepath.Join(root, "TeslaCam", "RecentClips")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "2026-07-23_08-00-00-front.mp4"
	if err := os.WriteFile(filepath.Join(directory, name), movieHeaderFixture(0, 1_000, 17_500), 0o644); err != nil {
		t.Fatal(err)
	}
	videos, err := NewScanner(time.UTC).Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(videos) != 1 || videos[0].Duration != defaultClipDurationSeconds {
		t.Fatalf("scanned videos = %#v", videos)
	}
	if videos[0].DurationState != durationStatusEstimated {
		t.Fatalf("scanned duration state = %q, want %q", videos[0].DurationState, durationStatusEstimated)
	}
	RefineVideoDurations(root, videos)
	if videos[0].Duration != 17.5 {
		t.Fatalf("refined duration = %f, want 17.5", videos[0].Duration)
	}
	if videos[0].DurationState != durationStatusVerified {
		t.Fatalf("refined duration state = %q, want %q", videos[0].DurationState, durationStatusVerified)
	}
}

func TestScannerRefreshDropsExternallyDeletedVideo(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	directory := filepath.Join(root, "TeslaCam", "RecentClips")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(directory, "2026-07-23_08-00-00-front.mp4")
	if err := os.WriteFile(name, movieHeaderFixture(0, 1_000, 60_000), 0o644); err != nil {
		t.Fatal(err)
	}
	scanner := NewScanner(time.UTC)
	if videos, err := scanner.Scan(root); err != nil || len(videos) != 1 {
		t.Fatalf("initial scan videos = %#v, error = %v", videos, err)
	}
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	videos, err := scanner.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(videos) != 0 {
		t.Fatalf("videos after external delete = %#v, want empty", videos)
	}
}

func TestRefineVideoDurationsMarksUnreadableVideo(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	videos := []Video{{
		Path:          "TeslaCam/RecentClips/2026-07-23_08-00-00-front.mp4",
		Duration:      defaultClipDurationSeconds,
		DurationState: durationStatusEstimated,
	}}
	RefineVideoDurations(root, videos)
	if videos[0].Duration != defaultClipDurationSeconds || videos[0].DurationState != durationStatusUnavailable {
		t.Fatalf("unreadable video = %#v", videos[0])
	}
}

func TestRefineVideoDurationsKeepsReadableVideosWhenAnotherIsUnreadable(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	directory := filepath.Join(root, "TeslaCam", "RecentClips")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	validPath := filepath.Join(directory, "2026-07-23_08-00-00-front.mp4")
	if err := os.WriteFile(validPath, movieHeaderFixture(0, 1_000, 17_500), 0o644); err != nil {
		t.Fatal(err)
	}
	videos := []Video{
		{Path: filepath.ToSlash(filepath.Join("TeslaCam", "RecentClips", "2026-07-23_08-00-00-front.mp4")), Duration: defaultClipDurationSeconds, DurationState: durationStatusEstimated},
		{Path: filepath.ToSlash(filepath.Join("TeslaCam", "RecentClips", "2026-07-23_08-00-00-back.mp4")), Duration: defaultClipDurationSeconds, DurationState: durationStatusEstimated},
	}
	RefineVideoDurations(root, videos)
	if videos[0].Duration != 17.5 || videos[0].DurationState != durationStatusVerified {
		t.Fatalf("readable video = %#v", videos[0])
	}
	if videos[1].Duration != defaultClipDurationSeconds || videos[1].DurationState != durationStatusUnavailable {
		t.Fatalf("unreadable video = %#v", videos[1])
	}
}

func TestScannerScan(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"TeslaCam/RecentClips/2026-07-15_12-30-00-front.mp4":                            "front",
		"TeslaCam/RecentClips/thumb.png":                                                "recent thumbnail",
		"TeslaCam/SavedClips/2026-07-15_12-31-00/2026-07-15_12-31-01-left_repeater.mp4": "left",
		"TeslaCam/SavedClips/2026-07-15_12-31-00/thumb.png":                             "saved thumbnail",
		"TeslaCam/RecentClips/2026-07-15_12-32-00-left_pillar.mp4":                      "pillar",
		"TeslaCam/SentryClips/2026-07-15_12-32-00/event.json":                           "{}",
		"TeslaCam/SavedClips/2026-07-15_12-31-00/event.json":                            `{"timestamp":"2026-07-15T12:31:42","city":"宁波","street":"测试路","est_lat":"29.8421","est_lon":"121.622","reason":"user_interaction_dashcam_icon_tapped","camera":"0"}`,
		"TeslaCam/RecentClips/incomplete.tmp":                                           "tmp",
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	videos, err := NewScanner().Scan(root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(videos) != 3 {
		t.Fatalf("len(videos) = %d, want 3", len(videos))
	}
	if videos[0].ClipType != "recent" || videos[0].Camera != "left_pillar" {
		t.Errorf("pillar video = %#v", videos[0])
	}
	if videos[0].ThumbnailPath != "TeslaCam/RecentClips/thumb.png" || videos[0].ThumbnailID == "" {
		t.Errorf("pillar thumbnail = %#v", videos[0])
	}
	if videos[1].ClipType != "saved" || videos[1].Event != "2026-07-15_12-31-00" {
		t.Errorf("saved video = %#v", videos[1])
	}
	if videos[1].ThumbnailPath != "TeslaCam/SavedClips/2026-07-15_12-31-00/thumb.png" || videos[1].ThumbnailID == videos[0].ThumbnailID {
		t.Errorf("saved thumbnail = %#v", videos[1])
	}
	metadata := videos[1].EventMetadata
	if metadata == nil || metadata.OccurredAt == nil || metadata.OccurredAt.Second() != 42 ||
		metadata.City != "宁波" || metadata.Street != "测试路" || metadata.Latitude == nil || *metadata.Latitude != 29.8421 ||
		metadata.Longitude == nil || *metadata.Longitude != 121.622 || metadata.Reason != "user_interaction_dashcam_icon_tapped" ||
		metadata.TriggerType != "manual_save" || metadata.Camera != "0" || metadata.CameraView != "front" {
		t.Errorf("event metadata = %#v", metadata)
	}
	if videos[2].ClipType != "recent" || videos[2].Camera != "front" {
		t.Errorf("recent video = %#v", videos[2])
	}
	if videos[2].ThumbnailID != videos[0].ThumbnailID || videos[2].ThumbnailPath != videos[0].ThumbnailPath {
		t.Errorf("recent videos should share thumbnail: first=%#v second=%#v", videos[0], videos[2])
	}
}

func TestScannerThumbnailIDChangesWhenRecentThumbnailIsReplaced(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "TeslaCam", "RecentClips")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	video := filepath.Join(directory, "2026-07-15_12-30-00-front.mp4")
	thumbnail := filepath.Join(directory, "thumb.png")
	if err := os.WriteFile(video, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(thumbnail, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := NewScanner().Scan(root)
	if err != nil || len(first) != 1 {
		t.Fatalf("first scan = %#v, %v", first, err)
	}
	info, err := os.Stat(thumbnail)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(thumbnail, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 模拟 exFAT 上同大小、同修改时间的快速覆盖，仍必须识别内容变化。
	if err := os.Chtimes(thumbnail, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	second, err := NewScanner().Scan(root)
	if err != nil || len(second) != 1 {
		t.Fatalf("second scan = %#v, %v", second, err)
	}
	if first[0].ThumbnailID == second[0].ThumbnailID {
		t.Fatalf("replaced recent thumbnail kept id %q", first[0].ThumbnailID)
	}
}

func TestReadEventMetadataClassifiesTriggerAndCameraView(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "event.json")
	if err := os.WriteFile(path, []byte(`{"timestamp":"2026-07-15T12:31:42","reason":"sentry_aware_object_detection","camera":"5"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	metadata, err := readEventMetadata(path, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.TriggerType != "sentry_object" || metadata.CameraView != "left_pillar" {
		t.Fatalf("event metadata = %#v", metadata)
	}
}

func TestClassifyEventTrigger(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		reason string
		want   string
	}{
		{name: "仪表盘手动保存", reason: "user_interaction_dashcam_icon_tapped", want: "manual_save"},
		{name: "新版启动器手动保存", reason: "user_interaction_dashcam_launcher_action_tapped", want: "manual_save"},
		{name: "鸣笛保存", reason: "user_interaction_honk", want: "manual_honk"},
		{name: "哨兵对象检测", reason: "sentry_aware_object_detection", want: "sentry_object"},
		{name: "哨兵震动检测", reason: "sentry_aware_accel_0.8", want: "sentry_motion"},
		{name: "哨兵警报", reason: "sentry_panic_0", want: "sentry_alarm"},
		{name: "自动紧急制动", reason: "vehicle_auto_emergency_braking", want: "vehicle_emergency_braking"},
		{name: "未知事件", reason: "future_event", want: "unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyEventTrigger(test.reason); got != test.want {
				t.Fatalf("classifyEventTrigger(%q) = %q, want %q", test.reason, got, test.want)
			}
		})
	}
}

func TestEventCameraViewRejectsUnknownCamera(t *testing.T) {
	t.Parallel()
	if got := eventCameraView("4294967295"); got != "" {
		t.Fatalf("camera view = %q, want empty", got)
	}
}

func TestScannerScanMissingDirectories(t *testing.T) {
	videos, err := NewScanner().Scan(t.TempDir())
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(videos) != 0 {
		t.Fatalf("len(videos) = %d, want 0", len(videos))
	}
}

func TestScannerScanIgnoresMalformedEventMetadata(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	directory := filepath.Join(root, "TeslaCam", "SentryClips", "2026-07-15_12-32-00")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "event.json"), []byte(`{"timestamp":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "2026-07-15_12-32-01-front.mp4"), []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}

	videos, err := NewScanner().Scan(root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(videos) != 1 || videos[0].EventMetadata != nil {
		t.Fatalf("videos = %#v", videos)
	}
}
