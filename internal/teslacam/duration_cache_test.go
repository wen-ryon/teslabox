package teslacam

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCanceledScanKeepsPreviousIndex(t *testing.T) {
	scanner := NewScanner()
	previous := &durationResult{}
	scanner.previous = map[string]*durationResult{"existing": previous}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := scanner.ScanContext(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if scanner.previous["existing"] != previous {
		t.Fatal("canceled scan replaced previous index")
	}
}

func TestDurationProofAcrossSnapshotsAndSameSizeOverwrite(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "TeslaCam", "RecentClips")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-09-09_10-00-00-front.mp4")
	if err := os.WriteFile(path, movieHeaderFixture(0, 1000, 17500), 0600); err != nil {
		t.Fatal(err)
	}
	scanner := NewScanner()
	first, err := scanner.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	RefineVideoDurations(root, first)
	second, err := scanner.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	ApplyCachedDurations(second)
	if DurationKnown(second[0]) {
		t.Fatal("目录扫描不应核验或发布旧时长")
	}
	RefineVideoDurations(root, second)
	if second[0].DurationState != "verified" || second[0].Duration != 17.5 {
		t.Fatal("unchanged metadata was not reused")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, movieHeaderFixture(0, 1000, 9500), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	third, err := scanner.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	ApplyCachedDurations(third)
	if third[0].DurationState != "estimated" {
		t.Fatal("same-size overwrite reused old duration")
	}
	RefineVideoDurations(root, third)
	if third[0].Duration != 9.5 {
		t.Fatalf("duration=%v", third[0].Duration)
	}
}

func TestProofReadErrorPreventsReuse(t *testing.T) {
	r := &proofReader{source: bytes.NewReader([]byte{1})}
	_, err := r.ReadAt(make([]byte, 2), 0)
	if err != io.EOF || !r.overflow {
		t.Fatal("failed read left reusable proof")
	}
}

func TestDeferredProofDoesNotPublishDeletedVideoDuration(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "TeslaCam", "RecentClips")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-09-09_10-00-00-front.mp4")
	if err := os.WriteFile(path, movieHeaderFixture(0, 1000, 17500), 0600); err != nil {
		t.Fatal(err)
	}
	s := NewScanner()
	first, err := s.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	RefineVideoDurations(root, first)
	next, err := s.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	RefineVideoDurations(root, next)
	if next[0].DurationState != durationStatusUnavailable {
		t.Fatal("文件消失后仍复用了旧时长")
	}
}

func TestPriorityQueueBoundedAndDeduplicated(t *testing.T) {
	p := &durationPriority{queue: make(chan int, 256)}
	videos := make([]Video, 1000)
	for i := range videos {
		videos[i].durationCache = &durationResult{priority: p, index: i}
	}
	PrioritizeDurations(videos)
	PrioritizeDurations(videos)
	if len(p.queue) != 256 {
		t.Fatalf("queued=%d", len(p.queue))
	}
	for i := 0; i < 256; i++ {
		index, ok := TakePriorityDuration(videos)
		if !ok || index != i {
			t.Fatal("duplicate or wrong priority")
		}
	}
}

func TestDurationCacheSharedAndScanIsolated(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "TeslaCam", "RecentClips")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-09-09_10-00-00-front.mp4")
	if err := os.WriteFile(path, movieHeaderFixture(0, 1000, 17500), 0600); err != nil {
		t.Fatal(err)
	}
	scanner := NewScanner()
	videos, err := scanner.Scan(root)
	if err != nil || len(videos) != 1 {
		t.Fatalf("scan: %v %v", videos, err)
	}
	detail := append([]Video(nil), videos...)
	RefineVideoDurations(root, detail)
	ApplyCachedDurations(videos)
	if videos[0].Duration != 17.5 || videos[0].DurationState != "verified" {
		t.Fatalf("cache: %+v", videos[0])
	}
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	fresh, err := scanner.Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	ApplyCachedDurations(fresh)
	if fresh[0].DurationState != "estimated" {
		t.Fatal("new scan inherited old result")
	}
	RefineVideoDurations(root, fresh)
	if fresh[0].DurationState != "unavailable" {
		t.Fatal("broken file marked verified")
	}
}

func TestDurationCacheReadDoesNotWaitForProbe(t *testing.T) {
	cached := &durationResult{}
	cached.mu.Lock()
	defer cached.mu.Unlock()
	videos := []Video{{durationCache: cached, DurationState: "estimated"}}
	ApplyCachedDurations(videos)
	if videos[0].DurationState != "estimated" {
		t.Fatal("unfinished result published")
	}
}
