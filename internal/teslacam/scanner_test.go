package teslacam

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
