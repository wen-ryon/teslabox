package api

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/gadget"
	"github.com/wen-ryon/teslabox/internal/onboarding"
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

type fakeThumbnailController struct {
	fakeController
	thumbnails map[string][]byte
}

func (c *fakeThumbnailController) CachedThumbnail(id string) ([]byte, time.Time, bool) {
	data, ok := c.thumbnails[id]
	return append([]byte(nil), data...), time.Unix(1000, 0), ok
}

func (c *fakeThumbnailController) HasCachedThumbnail(id string) bool {
	_, ok := c.thumbnails[id]
	return ok
}

func (c *fakeThumbnailController) StoreCachedThumbnail(id string, data []byte) error {
	if c.thumbnails == nil {
		c.thumbnails = make(map[string][]byte)
	}
	c.thumbnails[id] = append([]byte(nil), data...)
	return nil
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

func TestGroupVideoSessionsMergesRecentClipsIntoOneTimeline(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 16, 10, 30, 0, 0, time.Local)
	groups := []videoGroup{
		{ID: "first", ClipType: "recent", ThumbnailID: "recent-thumb", RecordedAt: base, EndAt: base.Add(time.Minute), Complete: true, TotalSize: 10, Videos: []teslacam.Video{{Camera: "front"}}},
		{ID: "saved-between", ClipType: "saved", Event: "event-1", RecordedAt: base.Add(90 * time.Second), EndAt: base.Add(150 * time.Second), Complete: true, TotalSize: 5, Videos: []teslacam.Video{{Camera: "front"}}},
		{ID: "second", ClipType: "recent", ThumbnailID: "recent-thumb", RecordedAt: base.Add(time.Minute), EndAt: base.Add(2 * time.Minute), Complete: true, TotalSize: 20, Videos: []teslacam.Video{{Camera: "front"}}},
		{ID: "after-gap", ClipType: "recent", ThumbnailID: "recent-thumb", RecordedAt: base.Add(3 * time.Minute), EndAt: base.Add(4 * time.Minute), Complete: false, TotalSize: 30, Videos: []teslacam.Video{{Camera: "back"}}},
	}
	sessions := groupVideoSessions(groups, 5*time.Second)
	if len(sessions) != 2 {
		t.Fatalf("len(sessions) = %d, want 2: %#v", len(sessions), sessions)
	}
	var recent videoSession
	for _, session := range sessions {
		if session.ClipType == "recent" {
			recent = session
			break
		}
	}
	if recent.ID == "" {
		t.Fatalf("recent session not found: %#v", sessions)
	}
	if recent.SegmentCount != 3 || recent.Duration != 180 || recent.TotalSize != 60 || recent.Complete {
		t.Fatalf("recent session = %#v", recent)
	}
	for index, want := range []float64{0, 60, 120} {
		if recent.Moments[index].TimelineStart != want {
			t.Fatalf("moment %d timeline start = %f, want %f", index, recent.Moments[index].TimelineStart, want)
		}
	}
	if len(recent.Cameras) != 2 || recent.Cameras[0] != "front" || recent.Cameras[1] != "back" {
		t.Fatalf("recent cameras = %#v", recent.Cameras)
	}
	if recent.ThumbnailURL != "/api/v1/thumbnails/recent-thumb" {
		t.Fatalf("recent thumbnail URL = %q", recent.ThumbnailURL)
	}
}

func TestGroupVideoSessionsPreservesFiveSecondFinalMoment(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 31, 21, 25, 22, 0, time.Local)
	groups := make([]videoGroup, 0, 11)
	for minute := range 11 {
		duration := time.Minute
		if minute == 10 {
			duration = 5 * time.Second
		}
		start := base.Add(time.Duration(minute) * time.Minute)
		groups = append(groups, videoGroup{
			ID: "moment-" + strconv.Itoa(minute), ClipType: "saved", Event: "event-1",
			RecordedAt: start, EndAt: start.Add(duration), Duration: duration.Seconds(),
			Videos: []teslacam.Video{{Camera: "front", RecordedAt: start, Duration: duration.Seconds()}},
		})
	}

	sessions := groupVideoSessions(groups, 5*time.Second)
	if len(sessions) != 1 {
		t.Fatalf("len(sessions) = %d, want 1", len(sessions))
	}
	session := sessions[0]
	if session.Duration != 605 || session.Moments[10].TimelineStart != 600 || session.Moments[10].Duration != 5 {
		t.Fatalf("session duration = %f, last start = %f, last duration = %f",
			session.Duration, session.Moments[10].TimelineStart, session.Moments[10].Duration)
	}
}

func TestGroupVideoSessionsCompressesSavedClipSeams(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 31, 21, 25, 22, 0, time.Local)
	groups := make([]videoGroup, 0, 11)
	for minute := range 11 {
		duration := time.Minute
		if minute == 10 {
			duration = 2*time.Second + 886600*time.Microsecond
		}
		start := base.Add(time.Duration(minute) * time.Minute)
		if minute > 0 {
			start = start.Add(3 * time.Second)
		}
		groups = append(groups, videoGroup{
			ID: "moment-" + strconv.Itoa(minute), ClipType: "saved", Event: "event-1",
			RecordedAt: start, EndAt: start.Add(duration), Duration: duration.Seconds(),
			Videos: []teslacam.Video{{Camera: "front", RecordedAt: start, Duration: duration.Seconds()}},
		})
	}

	sessions := groupVideoSessions(groups, sessionContinuityGap)
	if len(sessions) != 1 {
		t.Fatalf("len(sessions) = %d, want 1", len(sessions))
	}
	session := sessions[0]
	wantDuration := 602.8866
	if math.Abs(session.Duration-wantDuration) > 0.0001 {
		t.Fatalf("session duration = %f, want %f", session.Duration, wantDuration)
	}
	if session.Moments[10].TimelineStart != 600 {
		t.Fatalf("last timeline start = %f, want 600", session.Moments[10].TimelineStart)
	}
	if got := session.EndAt.Sub(session.StartAt).Seconds(); math.Abs(got-605.8866) > 0.0001 {
		t.Fatalf("wall clock duration = %f, want 605.8866", got)
	}
}

func TestGroupVideoSessionsKeepsOneSavedEventDirectoryAsOneRecording(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 8, 11, 23, 24, 54, 0, time.Local)
	groups := []videoGroup{
		{ID: "first", ClipType: "saved", Event: "2026-08-11_23-35-07", RecordedAt: base, EndAt: base.Add(2 * time.Minute), Duration: 120},
		{ID: "second", ClipType: "saved", Event: "2026-08-11_23-35-07", RecordedAt: base.Add(2*time.Minute + 44*time.Second), EndAt: base.Add(7*time.Minute + 2*time.Second), Duration: 258},
		{ID: "third", ClipType: "saved", Event: "2026-08-11_23-35-07", RecordedAt: base.Add(7*time.Minute + 48*time.Second), EndAt: base.Add(10*time.Minute + 12*time.Second), Duration: 189},
		{ID: "other-event", ClipType: "saved", Event: "2026-08-12_00-00-00", RecordedAt: base.Add(time.Hour), EndAt: base.Add(time.Hour + time.Minute), Duration: 60},
	}
	sessions := groupVideoSessions(groups, 5*time.Second)
	if len(sessions) != 2 {
		t.Fatalf("len(sessions) = %d, want 2: %#v", len(sessions), sessions)
	}
	var saved videoSession
	for _, session := range sessions {
		if session.Event == "2026-08-11_23-35-07" {
			saved = session
		}
	}
	if saved.SegmentCount != 3 || saved.StartAt != base || saved.EndAt != base.Add(10*time.Minute+12*time.Second) || saved.Duration != 567 {
		t.Fatalf("saved session = %#v", saved)
	}
}

func TestExportSourceUsesCompressedRecentTimeline(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	base := time.Date(2026, 7, 16, 10, 30, 0, 0, time.Local)
	firstPath := filepath.ToSlash(filepath.Join("TeslaCam", "RecentClips", "first-front.mp4"))
	secondPath := filepath.ToSlash(filepath.Join("TeslaCam", "RecentClips", "second-front.mp4"))
	for _, relative := range []string{firstPath, secondPath} {
		name := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("video"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	session := videoSession{
		ClipType: "recent",
		StartAt:  base,
		EndAt:    base.Add(11 * time.Minute),
		Duration: 120,
		Moments: []videoGroup{
			{
				RecordedAt:    base,
				TimelineStart: 0,
				Videos: []teslacam.Video{{
					Camera: "front", Path: firstPath, RecordedAt: base, Duration: 60,
				}},
			},
			{
				RecordedAt:    base.Add(10 * time.Minute),
				TimelineStart: 60,
				Videos: []teslacam.Video{{
					Camera: "front", Path: secondPath, RecordedAt: base.Add(10*time.Minute + time.Second), Duration: 60,
				}},
			},
		},
	}

	source, err := exportSource(root, session)
	if err != nil {
		t.Fatal(err)
	}
	if got := source.SessionEnd.Sub(source.SessionStart); got != 2*time.Minute {
		t.Fatalf("export duration = %s, want 2m", got)
	}
	if got := source.Videos[1].RecordedAt.Sub(source.SessionStart); got != 61*time.Second {
		t.Fatalf("second video timeline offset = %s, want 1m1s", got)
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

func TestThumbnailEndpointCachesSnapshotAndServesHistory(t *testing.T) {
	root := t.TempDir()
	relative := filepath.ToSlash(filepath.Join("TeslaCam", "RecentClips", "thumb.png"))
	name := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("thumbnail"), 0o644); err != nil {
		t.Fatal(err)
	}
	controller := &fakeThumbnailController{fakeController: fakeController{state: service.Snapshot{
		MediaSnapshot: &mediasnapshot.View{ID: "snapshot", MountPath: root},
		Videos:        []teslacam.Video{{ThumbnailID: "1111111111111111", ThumbnailPath: relative}},
	}}}
	handler := NewHandler(controller, slog.New(slog.NewTextHandler(io.Discard, nil)))
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/v1/thumbnails/1111111111111111", nil))
	if first.Code != http.StatusOK || string(controller.thumbnails["1111111111111111"]) != "thumbnail" {
		t.Fatalf("snapshot response = %d cache=%q", first.Code, controller.thumbnails["1111111111111111"])
	}
	controller.state.MediaSnapshot = nil
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/api/v1/thumbnails/1111111111111111", nil))
	if second.Code != http.StatusOK || second.Body.String() != "thumbnail" {
		t.Fatalf("cached response = %d %q", second.Code, second.Body.String())
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
		Reason: "sentry_aware_accel_0.8", TriggerType: "sentry_motion", Camera: "0", CameraView: "front",
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
	if session.EventMetadata == nil || session.EventMetadata.OccurredAt == nil || !session.EventMetadata.OccurredAt.Equal(eventAt) || session.EventMetadata.City != "宁波" || session.EventMetadata.TriggerType != "sentry_motion" || session.EventMetadata.CameraView != "front" {
		t.Fatalf("session event metadata = %#v", session.EventMetadata)
	}
	detailRequest := httptest.NewRequest(http.MethodGet, "/api/v1/video-sessions/"+session.ID, nil)
	detailResponse := httptest.NewRecorder()
	handler.ServeHTTP(detailResponse, detailRequest)
	if detailResponse.Code != http.StatusServiceUnavailable {
		t.Fatalf("detail status = %d", detailResponse.Code)
	}
	// 旧清单仍可列出，但无快照时不能把估算时间轴交给播放器。
	detailResponse = httptest.NewRecorder()
	detailRequest.SetPathValue("id", session.ID)
	writeVideoSession(detailResponse, detailRequest, videos, t.TempDir())
	var detail videoSession
	if err := json.Unmarshal(detailResponse.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Moments) != 2 || detail.EventMetadata == nil || detail.EventMetadata.Reason != "sentry_aware_accel_0.8" {
		t.Fatalf("detail = %#v", detail)
	}
}

func TestVideoSessionDetailRefinesIndexedFilesForAccurateTimeline(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	relative := filepath.ToSlash(filepath.Join("TeslaCam", "RecentClips", "2026-07-16_10-00-00-front.mp4"))
	name := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, videoDurationFixture(17_500), 0o600); err != nil {
		t.Fatal(err)
	}
	video := teslacam.Video{
		ID: "video-1", Path: relative, ClipType: "recent", Camera: "front",
		RecordedAt: time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC), Duration: 60, DurationState: "estimated",
	}
	session := groupVideoSessions(groupVideos([]teslacam.Video{video}, videoGroupTolerance), sessionContinuityGap)[0]
	handler := NewHandler(fakeController{state: service.Snapshot{
		MediaSnapshot: &mediasnapshot.View{ID: "snapshot-1", MountPath: root},
		Videos:        []teslacam.Video{video},
	}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/video-sessions/"+session.ID, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	var detail videoSession
	if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Moments) != 1 || len(detail.Moments[0].Videos) != 1 {
		t.Fatalf("detail = %#v", detail)
	}
	indexed := detail.Moments[0].Videos[0]
	if math.Abs(indexed.Duration-17.5) > 0.001 || indexed.DurationState != "verified" || math.Abs(detail.Duration-17.5) > 0.001 {
		t.Fatalf("refined video = %#v", indexed)
	}
}

func videoDurationFixture(duration uint32) []byte {
	payload := make([]byte, 20)
	binary.BigEndian.PutUint32(payload[12:16], 1_000)
	binary.BigEndian.PutUint32(payload[16:20], duration)
	mvhd := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(mvhd[:4], uint32(len(mvhd)))
	copy(mvhd[4:8], "mvhd")
	copy(mvhd[8:], payload)
	moov := make([]byte, 8+len(mvhd))
	binary.BigEndian.PutUint32(moov[:4], uint32(len(moov)))
	copy(moov[4:8], "moov")
	copy(moov[8:], mvhd)
	return moov
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

func TestOnboardingRoutesPersistDeviceState(t *testing.T) {
	t.Parallel()
	manager, err := onboarding.New(filepath.Join(t.TempDir(), "onboarding.json"))
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithOnboardingAdmin(manager))

	complete := httptest.NewRecorder()
	handler.ServeHTTP(complete, httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/page-complete", nil))
	if complete.Code != http.StatusOK {
		t.Fatalf("complete status = %d, body = %s", complete.Code, complete.Body.String())
	}

	dismiss := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/onboarding/storage-dismiss", strings.NewReader(`{"stable_path":"/dev/disk/by-id/usb-test"}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(dismiss, request)
	if dismiss.Code != http.StatusOK {
		t.Fatalf("dismiss status = %d, body = %s", dismiss.Code, dismiss.Body.String())
	}

	read := httptest.NewRecorder()
	handler.ServeHTTP(read, httptest.NewRequest(http.MethodGet, "/api/v1/onboarding", nil))
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"page_guide_complete":true`) || !strings.Contains(read.Body.String(), "/dev/disk/by-id/usb-test") {
		t.Fatalf("read status = %d, body = %s", read.Code, read.Body.String())
	}
}
