package teslacam

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEventDiscoveryIgnoresRecentVideosAndRetainsMetadataLateDirectory(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "TeslaCam", "SentryClips", "2026-10-02_20-00-00")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	// RecentClips 故意不是目录，事件发现不得遍历或依赖其可读性。
	if err := os.WriteFile(filepath.Join(root, "TeslaCam", "RecentClips"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	scanner := NewScanner(time.UTC)
	first, err := scanner.ScanEventDirectories(context.Background(), root)
	if err != nil || len(first) != 1 || first[0].Metadata != nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if err := os.WriteFile(filepath.Join(directory, "event.json"), []byte(`{"timestamp":"2026-10-02T20:00:01","reason":"sentry_aware_object_detection","camera":"0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := scanner.ScanEventDirectories(context.Background(), root)
	if err != nil || second[0].Metadata == nil || second[0].Revision == first[0].Revision {
		t.Fatalf("late=%+v err=%v", second, err)
	}
	for _, camera := range []string{"front", "back"} {
		if err := os.WriteFile(filepath.Join(directory, "2026-10-02_20-00-00-"+camera+".mp4"), []byte("video"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	videos, err := scanner.ScanEventVideos(context.Background(), root, second[0], []string{"front"})
	if err != nil || len(videos) != 2 {
		t.Fatalf("videos=%+v err=%v", videos, err)
	}
	for _, video := range videos {
		if video.EventMetadata == nil || (video.Camera == "front" && video.Size != 5) || (video.Camera == "back" && video.Size != 0) {
			t.Fatal(video)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := scanner.ScanEventDirectories(ctx, root); err == nil {
		t.Fatal("canceled discovery continued")
	}
}

func TestEventDirectoryRejectsTraversalAndWaitsForIncompleteMetadata(t *testing.T) {
	scanner := NewScanner(time.UTC)
	if _, err := scanner.ScanEventVideos(context.Background(), t.TempDir(), EventDirectory{Path: "TeslaCam/SentryClips/../../outside"}, nil); err == nil {
		t.Fatal("traversal accepted")
	}
	root := t.TempDir()
	directory := filepath.Join(root, "TeslaCam", "SavedClips", "event")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "event.json"), []byte(`{"timestamp":`), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := scanner.ScanEventDirectories(context.Background(), root)
	if err != nil || len(catalog) != 1 || catalog[0].Metadata != nil {
		t.Fatalf("catalog=%+v err=%v", catalog, err)
	}
}

func TestEventAttributeBudgetIncludesOffsetViewsAndTailNeighbors(t *testing.T) {
	start := time.Date(2026, 10, 2, 20, 0, 0, 0, time.UTC)
	at := start.Add(time.Second)
	metadata := &EventMetadata{OccurredAt: &at}
	if !eventVideoNeedsAttributes(start.Add(2*time.Second), start.Add(10*time.Minute), metadata, "event") {
		t.Fatal("slightly later camera in same minute was excluded")
	}
	if eventVideoNeedsAttributes(start.Add(8*time.Minute), start.Add(10*time.Minute), metadata, "event") {
		t.Fatal("unselected future minute was read")
	}
	at = start.Add(20 * time.Minute)
	if !eventVideoNeedsAttributes(start.Add(9*time.Minute), start.Add(10*time.Minute), metadata, "surrounding") {
		t.Fatal("tail fallback lost previous minute")
	}
	if eventVideoNeedsAttributes(start, start.Add(10*time.Minute), metadata, "surrounding") {
		t.Fatal("old unrelated minute was read")
	}
}
