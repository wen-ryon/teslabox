package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/media"
	"github.com/wen-ryon/teslabox/internal/service"
	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

func TestRecentExportCrossesWindowWithoutScanningEarlierFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "TeslaCam", "RecentClips")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"2026-01-01_00-00-00-front.mp4", "2026-01-01_23-09-00-front.mp4", "2026-01-01_23-10-00-front.mp4"} {
		if err := os.WriteFile(filepath.Join(dir, name), videoDurationFixture(60000), 0600); err != nil {
			t.Fatal(err)
		}
	}
	videos, err := teslacam.NewScanner(time.UTC).Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 1, 1, 23, 9, 30, 0, time.UTC)
	end := start.Add(time.Minute)
	request := media.ExportRequest{Cameras: []string{"front"}, RangeStartAt: &start, RangeEndAt: &end}
	source, err := recentRangeSource(context.Background(), root, videos, &request)
	if err != nil {
		t.Fatal(err)
	}
	if len(source.Videos) != 2 || !source.SessionStart.Equal(start) || request.StartSeconds != 0 || request.DurationSeconds != 60 {
		t.Fatalf("source=%+v request=%+v", source, request)
	}
	if !source.Videos[0].RecordedAt.Equal(start.Add(-30 * time.Second)) {
		t.Fatal("lost source trim offset")
	}
	windows := recentWindows(groupVideoSessions(groupVideos(videos, videoGroupTolerance), sessionContinuityGap))
	request.SessionID = windows[0].ID
	request.SnapshotID = "snapshot"
	request.Mode = media.ExportModeClip
	admin := &fakeExportAdmin{}
	handler := NewHandler(fakeController{state: service.Snapshot{Videos: videos, MediaSnapshot: &mediasnapshot.View{ID: "snapshot", MountPath: root}}}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithExportAdmin(admin))
	input, _ := json.Marshal(request)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/api/v1/exports", bytes.NewReader(input)))
	if response.Code != 202 || len(admin.source.Videos) != 2 || admin.request.DurationSeconds != 60 {
		t.Fatalf("range endpoint: %d %s", response.Code, response.Body.String())
	}
	if admin.release != nil {
		admin.release()
	}
	teslacam.ApplyCachedDurations(videos)
	for _, video := range videos {
		if video.RecordedAt.Hour() == 0 && video.DurationState != "estimated" {
			t.Fatal("calibrated unrelated history")
		}
	}
	end = end.Add(3 * time.Minute)
	if _, err := recentRangeSource(context.Background(), root, videos, &request); err == nil {
		t.Fatal("accepted uncovered end")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := recentRangeSource(ctx, root, videos, &request); err != context.Canceled {
		t.Fatalf("cancellation=%v", err)
	}
}

func TestRecentExportRejectsEmptyCameraAndEmptyRange(t *testing.T) {
	start := time.Now()
	end := start.Add(500 * time.Millisecond)
	for _, cameras := range [][]string{nil, {"front"}} {
		request := media.ExportRequest{Cameras: cameras, RangeStartAt: &start, RangeEndAt: &end}
		if _, err := recentRangeSource(context.Background(), t.TempDir(), nil, &request); err == nil {
			t.Fatal("accepted empty footage")
		}
	}
}
