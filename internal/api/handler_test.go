package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/gadget"
	"github.com/wen-ryon/teslabox/internal/service"
	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

type fakeController struct {
	state     service.Snapshot
	acquired  func()
	released  func()
	activated func()
}

func (c fakeController) Snapshot() service.Snapshot {
	return c.state
}

func (c fakeController) AcquireMediaSnapshot() (service.Snapshot, func()) {
	if c.acquired != nil {
		c.acquired()
	}
	return c.state, func() {
		if c.released != nil {
			c.released()
		}
	}
}

func (c fakeController) Refresh(context.Context) error {
	return nil
}

func (c fakeController) Scan(context.Context) error {
	return nil
}

func (c fakeController) ActivateMedia(context.Context) error {
	if c.activated != nil {
		c.activated()
	}
	return nil
}

func TestVideoListFiltersAndPaginates(t *testing.T) {
	controller := fakeController{state: service.Snapshot{Videos: []teslacam.Video{
		{ID: "1", ClipType: "recent", Camera: "front", RecordedAt: time.Date(2026, 7, 15, 10, 0, 0, 0, time.Local)},
		{ID: "2", ClipType: "saved", Camera: "front", RecordedAt: time.Date(2026, 7, 15, 9, 0, 0, 0, time.Local)},
		{ID: "3", ClipType: "recent", Camera: "back", RecordedAt: time.Date(2026, 7, 14, 9, 0, 0, 0, time.Local)},
	}}}
	handler := NewHandler(controller, slog.New(slog.NewTextHandler(io.Discard, nil)))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/videos?clip_type=recent&date=2026-07-15&limit=1", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	var payload videoListResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Total != 1 || len(payload.Items) != 1 || payload.Items[0].ID != "1" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestVideoSessionsActivateMedia(t *testing.T) {
	var activations int
	handler := NewHandler(fakeController{
		state:     service.Snapshot{},
		activated: func() { activations++ },
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/video-sessions", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || activations != 1 {
		t.Fatalf("status = %d, activations = %d", response.Code, activations)
	}
}

func TestVideoGroupsMergeSixCamerasWithinTolerance(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 16, 10, 30, 0, 0, time.Local)
	cameras := []string{"front", "back", "left_repeater", "right_repeater", "left_pillar", "right_pillar"}
	videos := make([]teslacam.Video, 0, len(cameras)+1)
	for index, camera := range cameras {
		recordedAt := base
		if index >= 3 {
			recordedAt = base.Add(time.Second)
		}
		videos = append(videos, teslacam.Video{
			ID: camera, ClipType: "recent", Camera: camera, RecordedAt: recordedAt, Size: 1024,
		})
	}
	videos = append(videos, teslacam.Video{
		ID: "next-front", ClipType: "recent", Camera: "front", RecordedAt: base.Add(time.Minute), Size: 2048,
	})
	controller := fakeController{state: service.Snapshot{Videos: videos}}
	handler := NewHandler(controller, slog.New(slog.NewTextHandler(io.Discard, nil)))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/video-groups?camera=left_pillar&date=2026-07-16", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	var payload videoGroupListResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Total != 1 || len(payload.Items) != 1 {
		t.Fatalf("payload = %#v", payload)
	}
	group := payload.Items[0]
	if len(group.Videos) != 6 || !group.Complete || group.TimeSpreadMS != 1000 || group.TotalSize != 6*1024 {
		t.Fatalf("group = %#v", group)
	}
	if group.Duration != 61 {
		t.Fatalf("group duration = %f, want 61", group.Duration)
	}
	if payload.Tolerance != videoGroupTolerance.Milliseconds() {
		t.Fatalf("tolerance = %d", payload.Tolerance)
	}
}

func TestGroupVideoSessionsMergesContinuousMinuteSegments(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 16, 10, 30, 0, 0, time.Local)
	groups := []videoGroup{
		{ID: "first", ClipType: "recent", ThumbnailID: "recent-thumb", RecordedAt: base, EndAt: base.Add(time.Minute), Complete: true, TotalSize: 10, Videos: []teslacam.Video{{Camera: "front"}}},
		{ID: "second", ClipType: "recent", ThumbnailID: "recent-thumb", RecordedAt: base.Add(time.Minute), EndAt: base.Add(2 * time.Minute), Complete: true, TotalSize: 20, Videos: []teslacam.Video{{Camera: "front"}}},
		{ID: "after-gap", ClipType: "recent", ThumbnailID: "recent-thumb", RecordedAt: base.Add(3 * time.Minute), EndAt: base.Add(4 * time.Minute), Complete: false, TotalSize: 30, Videos: []teslacam.Video{{Camera: "back"}}},
	}
	sessions := groupVideoSessions(groups, 5*time.Second)
	if len(sessions) != 2 {
		t.Fatalf("len(sessions) = %d, want 2: %#v", len(sessions), sessions)
	}
	continuous := sessions[1]
	if continuous.SegmentCount != 2 || continuous.Duration != 120 || continuous.TotalSize != 30 || !continuous.Complete {
		t.Fatalf("continuous session = %#v", continuous)
	}
	if len(continuous.Cameras) != 1 || continuous.Cameras[0] != "front" {
		t.Fatalf("continuous cameras = %#v", continuous.Cameras)
	}
	if sessions[0].SegmentCount != 1 || sessions[0].Complete {
		t.Fatalf("newer session = %#v", sessions[0])
	}
	if continuous.ThumbnailURL != "/api/v1/thumbnails/recent-thumb" || sessions[0].ThumbnailURL != continuous.ThumbnailURL {
		t.Fatalf("recent sessions should share thumbnail URL: %#v", sessions)
	}
}

func TestThumbnailEndpoint(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	relative := filepath.ToSlash(filepath.Join("TeslaCam", "RecentClips", "thumb.png"))
	thumbnail := []byte("png thumbnail")
	name := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, thumbnail, 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		id         string
		state      service.Snapshot
		wantStatus int
		wantBody   []byte
	}{
		{
			name: "serves indexed thumbnail",
			id:   "shared-thumb",
			state: service.Snapshot{
				MediaSnapshot: &mediasnapshot.View{ID: "snapshot", MountPath: root},
				Videos:        []teslacam.Video{{ThumbnailID: "shared-thumb", ThumbnailPath: relative}},
			},
			wantStatus: http.StatusOK,
			wantBody:   thumbnail,
		},
		{
			name: "rejects unknown thumbnail",
			id:   "missing",
			state: service.Snapshot{
				MediaSnapshot: &mediasnapshot.View{ID: "snapshot", MountPath: root},
				Videos:        []teslacam.Video{{ThumbnailID: "shared-thumb", ThumbnailPath: relative}},
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "requires media snapshot",
			id:         "shared-thumb",
			state:      service.Snapshot{Videos: []teslacam.Video{{ThumbnailID: "shared-thumb", ThumbnailPath: relative}}},
			wantStatus: http.StatusServiceUnavailable,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(fakeController{state: test.state}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			request := httptest.NewRequest(http.MethodGet, "/api/v1/thumbnails/"+test.id, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if test.wantBody != nil && string(response.Body.Bytes()) != string(test.wantBody) {
				t.Fatalf("body = %q, want %q", response.Body.Bytes(), test.wantBody)
			}
			if test.wantStatus == http.StatusOK && response.Header().Get("Content-Type") != "image/png" {
				t.Fatalf("content type = %q", response.Header().Get("Content-Type"))
			}
		})
	}
}

func TestVideoSessionListReturnsCompleteTimeline(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 16, 10, 30, 0, 0, time.Local)
	eventAt := base.Add(90 * time.Second)
	latitude := 29.8421
	longitude := 121.622
	metadata := &teslacam.EventMetadata{
		OccurredAt: &eventAt, City: "宁波", Street: "测试路", Latitude: &latitude, Longitude: &longitude,
		Reason: "sentry_aware_accel_0.8", Camera: "0",
	}
	videos := make([]teslacam.Video, 0, 12)
	for minute := range 2 {
		for _, camera := range []string{"front", "back", "left_repeater", "right_repeater", "left_pillar", "right_pillar"} {
			videos = append(videos, teslacam.Video{
				ID: camera + time.Duration(minute).String(), ClipType: "recent", Camera: camera,
				RecordedAt: base.Add(time.Duration(minute) * time.Minute), Duration: 60, Size: 100, EventMetadata: metadata,
				ThumbnailID: "timeline-thumb",
			})
		}
	}
	handler := NewHandler(fakeController{state: service.Snapshot{Videos: videos}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/video-sessions?camera=left_pillar", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	var payload videoSessionListResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Total != 1 || len(payload.Items) != 1 {
		t.Fatalf("payload = %#v", payload)
	}
	session := payload.Items[0]
	if session.Duration != 120 || session.SegmentCount != 2 || len(session.Moments) != 0 || len(session.Cameras) != 6 || !session.Complete {
		t.Fatalf("session = %#v", session)
	}
	if session.ThumbnailURL != "/api/v1/thumbnails/timeline-thumb" {
		t.Fatalf("thumbnail URL = %q", session.ThumbnailURL)
	}
	if session.EventMetadata == nil || session.EventMetadata.OccurredAt == nil || !session.EventMetadata.OccurredAt.Equal(eventAt) || session.EventMetadata.City != "宁波" {
		t.Fatalf("session event metadata = %#v", session.EventMetadata)
	}
	detailRequest := httptest.NewRequest(http.MethodGet, "/api/v1/video-sessions/"+session.ID, nil)
	detailResponse := httptest.NewRecorder()
	handler.ServeHTTP(detailResponse, detailRequest)
	if detailResponse.Code != http.StatusOK {
		t.Fatalf("detail status = %d", detailResponse.Code)
	}
	var detail videoSession
	if err := json.Unmarshal(detailResponse.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Moments) != 2 || detail.EventMetadata == nil || detail.EventMetadata.Reason != "sentry_aware_accel_0.8" {
		t.Fatalf("detail = %#v", detail)
	}
}

func TestGroupVideosDoesNotCreateToleranceChain(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 16, 10, 30, 0, 0, time.Local)
	groups := groupVideos([]teslacam.Video{
		{ID: "front", ClipType: "recent", Camera: "front", RecordedAt: base},
		{ID: "back", ClipType: "recent", Camera: "back", RecordedAt: base.Add(2 * time.Second)},
		{ID: "left", ClipType: "recent", Camera: "left_repeater", RecordedAt: base.Add(4 * time.Second)},
	}, 2*time.Second)
	if len(groups) != 2 {
		t.Fatalf("len(groups) = %d, want 2: %#v", len(groups), groups)
	}
}

func TestVideoContentSupportsHTTPRange(t *testing.T) {
	t.Parallel()
	acquired := 0
	released := 0
	root := t.TempDir()
	relative := filepath.ToSlash(filepath.Join("TeslaCam", "RecentClips", "2026-07-16_10-00-00-front.mp4"))
	name := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	controller := fakeController{
		state: service.Snapshot{
			MediaSnapshot: &mediasnapshot.View{ID: "snapshot-1", MountPath: root},
			Videos:        []teslacam.Video{{ID: "video-1", Path: relative}},
		},
		acquired: func() { acquired++ },
		released: func() { released++ },
	}
	handler := NewHandler(controller, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/videos/video-1/content", nil)
	request.Header.Set("Range", "bytes=2-5")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusPartialContent || response.Body.String() != "2345" {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("Content-Range = %q", response.Header().Get("Content-Range"))
	}
	if acquired != 1 || released != 1 {
		t.Fatalf("media lease acquired = %d, released = %d", acquired, released)
	}
}

func TestStorageStatusIncludesTeslaUSBPhase(t *testing.T) {
	t.Parallel()
	controller := fakeController{state: service.Snapshot{
		Phase: service.PhaseGadgetActive,
		GadgetStatus: gadget.Status{
			Bound:               true,
			UDC:                 "fcc00000.dwc3",
			UDCState:            "configured",
			CurrentSpeed:        "high-speed",
			HostConnected:       true,
			EnumerationComplete: true,
			BusSuspendKnown:     true,
			BusSuspended:        true,
		},
	}}
	handler := NewHandler(controller, slog.New(slog.NewTextHandler(io.Discard, nil)))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/storage", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	var payload service.Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Phase != service.PhaseGadgetActive {
		t.Fatalf("Phase = %q", payload.Phase)
	}
	if !payload.GadgetStatus.Bound || !payload.GadgetStatus.HostConnected || !payload.GadgetStatus.EnumerationComplete || !payload.GadgetStatus.BusSuspendKnown || !payload.GadgetStatus.BusSuspended || payload.GadgetStatus.UDCState != "configured" || payload.GadgetStatus.CurrentSpeed != "high-speed" {
		t.Fatalf("GadgetStatus = %#v", payload.GadgetStatus)
	}
}
