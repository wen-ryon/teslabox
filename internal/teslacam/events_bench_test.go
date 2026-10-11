package teslacam

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func BenchmarkEventDiscoveryVersusFullIndex(b *testing.B) {
	root := b.TempDir()
	start := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	for i := range 20 {
		directory := filepath.Join(root, "TeslaCam", "SentryClips", fmt.Sprintf("event-%03d", i))
		if err := os.MkdirAll(directory, 0o700); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "event.json"), []byte(`{"timestamp":"2026-10-02T12:00:01Z","reason":"sentry_aware_object_detection","camera":"0"}`), 0o600); err != nil {
			b.Fatal(err)
		}
		for _, camera := range []string{"front", "back", "left_repeater", "right_repeater", "left_pillar", "right_pillar"} {
			if err := os.WriteFile(filepath.Join(directory, start.Format("2006-01-02_15-04-05")+"-"+camera+".mp4"), []byte("sample"), 0o600); err != nil {
				b.Fatal(err)
			}
		}
	}
	recent := filepath.Join(root, "TeslaCam", "RecentClips")
	if err := os.MkdirAll(recent, 0o700); err != nil {
		b.Fatal(err)
	}
	for i := range 600 {
		if err := os.WriteFile(filepath.Join(recent, start.Add(time.Duration(i)*time.Minute).Format("2006-01-02_15-04-05")+"-front.mp4"), []byte("sample"), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	b.Run("full-index", func(b *testing.B) {
		scanner := NewScanner(time.UTC)
		b.ReportAllocs()
		for b.Loop() {
			if _, err := scanner.ScanContext(context.Background(), root); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("event-discovery", func(b *testing.B) {
		scanner := NewScanner(time.UTC)
		b.ReportAllocs()
		for b.Loop() {
			if _, err := scanner.ScanEventDirectories(context.Background(), root); err != nil {
				b.Fatal(err)
			}
		}
	})
}
