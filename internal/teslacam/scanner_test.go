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
		"TeslaCam/SentryClips/2026-07-15_12-32-00/event.json":                           "{}",
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
	if len(videos) != 2 {
		t.Fatalf("len(videos) = %d, want 2", len(videos))
	}
	if videos[0].ClipType != "saved" || videos[0].Event != "2026-07-15_12-31-00" {
		t.Errorf("saved video = %#v", videos[0])
	}
	if videos[1].ClipType != "recent" || videos[1].Camera != "front" {
		t.Errorf("recent video = %#v", videos[1])
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
