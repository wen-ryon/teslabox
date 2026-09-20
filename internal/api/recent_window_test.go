package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/service"
	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

func TestRecentWindows24Hours(t *testing.T) {
	videos := largeIndex(24 * 60 * 6)
	for i := range videos {
		videos[i].ClipType = "recent"
		videos[i].Event = ""
	}
	sessions := groupVideoSessions(groupVideos(videos, videoGroupTolerance), sessionContinuityGap)
	windows := recentWindows(sessions)
	if len(windows) != 144 {
		t.Fatalf("windows=%d", len(windows))
	}
	seen := make(map[string]bool)
	for _, window := range windows {
		if len(window.Moments) != 10 || window.Moments[0].TimelineStart != 0 || !window.RecentWindow {
			t.Fatalf("invalid window: %+v", window)
		}
		for _, moment := range window.Moments {
			for _, video := range moment.Videos {
				if seen[video.ID] {
					t.Fatal("duplicate video")
				}
				seen[video.ID] = true
			}
		}
	}
	if len(seen) != len(videos) {
		t.Fatal("missing recordings")
	}
	if !windows[0].StartAt.After(windows[len(windows)-1].StartAt) {
		t.Fatal("latest window not first")
	}
	if sessions[0].Moments[10].TimelineStart != 600 {
		t.Fatal("window changed original timeline")
	}
	// 淘汰旧文件后旧窗口不能匹配为另一个时间范围。
	oldest := windows[len(windows)-1]
	if _, found := findVideoSession(videos[6:], oldest.ID); found {
		t.Fatal("overwritten window still found")
	}
}

func TestRecentWindowDenseInputBounded(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var groups []videoGroup
	for i := 0; i < 600; i++ {
		groups = append(groups, videoGroup{ID: fmt.Sprint(i), ClipType: "recent", RecordedAt: base.Add(time.Duration(i) * time.Second), Duration: 1, Videos: []teslacam.Video{{ID: fmt.Sprint(i), Camera: "front"}}})
	}
	windows := recentWindows(groupVideoSessions(groups, sessionContinuityGap))
	if len(windows) != 10 {
		t.Fatalf("windows=%d", len(windows))
	}
	for _, window := range windows {
		if len(window.Moments) > recentWindowMomentLimit {
			t.Fatal("unbounded detail")
		}
	}
}

func TestRecentDetailOnlyCalibratesSelectedWindow(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "TeslaCam", "RecentClips")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, duration := range map[string]uint32{"2026-01-01_00-00-00-front.mp4": 1000, "2026-01-01_23-50-00-front.mp4": 17500, "2026-01-01_23-51-00-front.mp4": 7250} {
		if err := os.WriteFile(filepath.Join(dir, name), videoDurationFixture(duration), 0600); err != nil {
			t.Fatal(err)
		}
	}
	videos, err := teslacam.NewScanner(time.UTC).Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	windows := recentWindows(groupVideoSessions(groupVideos(videos, videoGroupTolerance), sessionContinuityGap))
	admin := &fakeExportAdmin{}
	handler := NewHandler(fakeController{state: service.Snapshot{Videos: videos, MediaSnapshot: &mediasnapshot.View{ID: "current", MountPath: root}}}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithExportAdmin(admin), WithMediaInspector(&fakeMediaInspector{}), WithMediaCompositor(&fakeMediaCompositor{}))
	for _, snapshot := range []string{"old", "", "current"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/video-sessions/"+windows[0].ID+"?snapshot_id="+snapshot, nil))
		if snapshot != "current" {
			if response.Code != 409 {
				t.Fatalf("stale detail=%d", response.Code)
			}
			continue
		}
		var detail videoSession
		if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
			t.Fatal(err)
		}
		if response.Code != 200 || detail.Duration != 24.75 || detail.Moments[1].TimelineStart != 17.5 || detail.SnapshotID != "current" {
			t.Fatalf("wrong actual timeline: %+v", detail)
		}
	}
	teslacam.ApplyCachedDurations(videos)
	for _, video := range videos {
		if video.RecordedAt.Hour() == 0 && video.DurationState != "estimated" {
			t.Fatal("unselected historical file calibrated")
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/api/v1/exports", strings.NewReader(fmt.Sprintf(`{"session_id":%q,"snapshot_id":"old"}`, windows[0].ID))))
	if response.Code != 409 || admin.request.SessionID != "" {
		t.Fatal("stale export accepted")
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/api/v1/exports", strings.NewReader(fmt.Sprintf(`{"mode":"clip","session_id":%q,"snapshot_id":"current","start_seconds":17.5,"duration_seconds":5,"layout":"single","cameras":["front"]}`, windows[0].ID))))
	if response.Code != 202 || len(admin.source.Videos) != 2 || admin.request.StartSeconds != 17.5 {
		t.Fatalf("window export failed: %d %s", response.Code, response.Body.String())
	}
	if admin.release != nil {
		admin.release()
	}
	for _, suffix := range []string{"content", "download", "sei"} {
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/videos/"+videos[0].ID+"/"+suffix+"?snapshot_id=old", nil))
		if response.Code != 409 {
			t.Fatalf("stale %s accepted: %d", suffix, response.Code)
		}
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/video-groups/"+windows[0].Moments[0].ID+"/composite?snapshot_id=old", nil))
	if response.Code != 409 {
		t.Fatal("stale composite accepted")
	}
	_, paths, err := recordingDeleteSelection(videos, []string{windows[0].ID})
	if err != nil || len(paths) != 2 {
		t.Fatalf("window deletion escaped scope: %v %v", paths, err)
	}
}
