package teslacam

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScannerScan(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"TeslaCam/RecentClips/2026-07-15_12-30-00-front.mp4":                            "front",
		"TeslaCam/SavedClips/2026-07-15_12-31-00/2026-07-15_12-31-01-left_repeater.mp4": "left",
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
	if videos[1].ClipType != "saved" || videos[1].Event != "2026-07-15_12-31-00" {
		t.Errorf("saved video = %#v", videos[1])
	}
	metadata := videos[1].EventMetadata
	if metadata == nil || metadata.OccurredAt == nil || metadata.OccurredAt.Second() != 42 ||
		metadata.City != "宁波" || metadata.Street != "测试路" || metadata.Latitude == nil || *metadata.Latitude != 29.8421 ||
		metadata.Longitude == nil || *metadata.Longitude != 121.622 || metadata.Reason != "user_interaction_dashcam_icon_tapped" || metadata.Camera != "0" {
		t.Errorf("event metadata = %#v", metadata)
	}
	if videos[2].ClipType != "recent" || videos[2].Camera != "front" {
		t.Errorf("recent video = %#v", videos[2])
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
