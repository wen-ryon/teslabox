package teslacam

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScanReportsActualProgress(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "TeslaCam", "RecentClips")
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 130; i++ {
		name := fmt.Sprintf("2026-09-10_%02d-%02d-00-front.mp4", i/60, i%60)
		if err := os.WriteFile(filepath.Join(directory, name), []byte("test"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	var counts []int
	videos, err := NewScanner(time.UTC).ScanWithProgress(t.Context(), root, func(n int) { counts = append(counts, n) })
	if err != nil {
		t.Fatal(err)
	}
	if len(videos) != 130 || len(counts) != 4 || counts[0] != 0 || counts[1] != 64 || counts[2] != 128 || counts[3] != 130 {
		t.Fatalf("videos=%d progress=%v", len(videos), counts)
	}
}
