package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/media"
	"github.com/wen-ryon/teslabox/internal/service"
	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/system"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

type fakeMediaInspector struct {
	capabilities media.Capabilities
	metadata     media.Metadata
	probePath    string
	err          error
}

type fakeMediaCompositor struct {
	request media.CompositeRequest
	err     error
}

type fakeExportAdmin struct {
	request      media.ExportRequest
	source       media.ExportSource
	release      func()
	task         media.ExportTask
	downloadPath string
	settings     media.ExportSettings
	history      []media.ExportTask
	consumedID   string
	err          error
}

func (a *fakeExportAdmin) Create(request media.ExportRequest, source media.ExportSource, release func()) (media.ExportTask, error) {
	a.request, a.source, a.release = request, source, release
	if a.err != nil {
		release()
	}
	return a.task, a.err
}

func (a *fakeExportAdmin) Task(string) (media.ExportTask, error) { return a.task, a.err }

func (a *fakeExportAdmin) Cancel(string) (media.ExportTask, error) { return a.task, a.err }

func (a *fakeExportAdmin) Open(string) (*os.File, media.ExportTask, error) {
	if a.err != nil {
		return nil, media.ExportTask{}, a.err
	}
	file, err := os.Open(a.downloadPath)
	return file, a.task, err
}

func (a *fakeExportAdmin) Settings() media.ExportSettings { return a.settings }

func (a *fakeExportAdmin) UpdateSettings(settings media.ExportSettings) (media.ExportSettings, error) {
	if a.err != nil {
		return media.ExportSettings{}, a.err
	}
	a.settings = settings
	return settings, nil
}

func (a *fakeExportAdmin) History() []media.ExportTask { return a.history }

func (a *fakeExportAdmin) Consume(id string) error {
	a.consumedID = id
	return a.err
}

func (c *fakeMediaCompositor) Stream(_ context.Context, request media.CompositeRequest, output io.Writer) error {
	c.request = request
	if c.err != nil {
		return c.err
	}
	_, err := io.WriteString(output, "fragmented-mp4")
	return err
}

func (i *fakeMediaInspector) Capabilities(context.Context) (media.Capabilities, error) {
	return i.capabilities, i.err
}

func (i *fakeMediaInspector) Probe(_ context.Context, path string) (media.Metadata, error) {
	i.probePath = path
	return i.metadata, i.err
}

func TestMediaCapabilities(t *testing.T) {
	t.Parallel()
	inspector := &fakeMediaInspector{capabilities: media.Capabilities{
		FFmpegVersion: "ffmpeg version test", HardwareAcceleration: []string{"drm", "rkmpp"}, RockchipReady: true,
	}}
	handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithMediaInspector(inspector))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/media/capabilities", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload media.Capabilities
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.RockchipReady || payload.FFmpegVersion != "ffmpeg version test" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestVideoMetadata(t *testing.T) {
	t.Parallel()
	acquired := 0
	released := 0
	root := t.TempDir()
	relative := filepath.ToSlash(filepath.Join("TeslaCam", "RecentClips", "2026-07-20_10-00-00-front.mp4"))
	name := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspector := &fakeMediaInspector{metadata: media.Metadata{
		Streams: []media.StreamMetadata{{CodecName: "h264", Width: 2896, Height: 1876, FrameRate: "36/1"}},
	}}
	controller := fakeController{
		state: service.Snapshot{
			MediaSnapshot: &mediasnapshot.View{ID: "snapshot", MountPath: root},
			Videos:        []teslacam.Video{{ID: "front-video", Path: relative}},
		},
		acquired: func() { acquired++ },
		released: func() { released++ },
	}
	handler := NewHandler(controller, slog.New(slog.NewTextHandler(io.Discard, nil)), WithMediaInspector(inspector))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/videos/front-video/metadata", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if inspector.probePath != name {
		t.Fatalf("probe path = %q, want %q", inspector.probePath, name)
	}
	var payload media.Metadata
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Streams) != 1 || payload.Streams[0].Width != 2896 {
		t.Fatalf("payload = %#v", payload)
	}
	if acquired != 1 || released != 1 {
		t.Fatalf("media lease acquired = %d, released = %d", acquired, released)
	}
}

func TestVideoMetadataRejectsEscapingPath(t *testing.T) {
	t.Parallel()
	inspector := &fakeMediaInspector{}
	controller := fakeController{state: service.Snapshot{
		MediaSnapshot: &mediasnapshot.View{ID: "snapshot", MountPath: t.TempDir()},
		Videos:        []teslacam.Video{{ID: "unsafe", Path: "../outside.mp4"}},
	}}
	handler := NewHandler(controller, slog.New(slog.NewTextHandler(io.Discard, nil)), WithMediaInspector(inspector))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/videos/unsafe/metadata", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if inspector.probePath != "" {
		t.Fatalf("unsafe path reached inspector: %q", inspector.probePath)
	}
}

func TestVideoGroupComposite(t *testing.T) {
	t.Parallel()
	acquired := 0
	released := 0
	root := t.TempDir()
	recordedAt := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	videos := make([]teslacam.Video, 0, 3)
	for _, camera := range []string{"front", "right_repeater", "left_repeater"} {
		relative := filepath.ToSlash(filepath.Join("TeslaCam", "RecentClips", "clip-"+camera+".mp4"))
		name := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("video"), 0o600); err != nil {
			t.Fatal(err)
		}
		videos = append(videos, teslacam.Video{ID: camera, Camera: camera, Path: relative, RecordedAt: recordedAt})
	}
	compositor := &fakeMediaCompositor{}
	controller := fakeController{
		state: service.Snapshot{
			MediaSnapshot: &mediasnapshot.View{ID: "snapshot", MountPath: root}, Videos: videos,
		},
		acquired: func() { acquired++ },
		released: func() { released++ },
	}
	groupID := groupVideos(videos, videoGroupTolerance)[0].ID
	handler := NewHandler(controller, slog.New(slog.NewTextHandler(io.Discard, nil)), WithMediaCompositor(compositor))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/video-groups/"+groupID+"/composite?layout=composite3&quality=standard&camera=front", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != "fragmented-mp4" {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if compositor.request.Layout != "composite3" || compositor.request.Quality != "standard" || compositor.request.Camera != "front" || len(compositor.request.Inputs) != 3 {
		t.Fatalf("request = %#v", compositor.request)
	}
	if acquired != 1 || released != 1 {
		t.Fatalf("media lease acquired = %d, released = %d", acquired, released)
	}
}

func TestVideoGroupCompositeFailureLogsPlaybackContext(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	relative := filepath.ToSlash(filepath.Join("TeslaCam", "RecentClips", "clip-front.mp4"))
	name := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("video"), 0o600); err != nil {
		t.Fatal(err)
	}
	videos := []teslacam.Video{{
		ID: "front", Camera: "front", Path: relative,
		RecordedAt: time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC),
	}}
	controller := fakeController{state: service.Snapshot{
		MediaSnapshot: &mediasnapshot.View{ID: "snapshot", MountPath: root},
		Videos:        videos,
	}}
	compositor := &fakeMediaCompositor{err: &system.CommandError{
		Name: "ffmpeg", Code: 137, Output: "rkmpp decoder failed", Internal: context.DeadlineExceeded,
	}}
	var logs strings.Builder
	handler := NewHandler(
		controller,
		slog.New(slog.NewJSONHandler(&logs, nil)),
		WithMediaCompositor(compositor),
	)
	groupID := groupVideos(videos, videoGroupTolerance)[0].ID
	request := httptest.NewRequest(http.MethodGet, "/api/v1/video-groups/"+groupID+"/composite?layout=composite6&quality=smooth", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	output := logs.String()
	for _, expected := range []string{"实时合成录像失败", `"layout":"composite6"`, `"quality":"smooth"`, `"exit_code":137`, "rkmpp decoder failed"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("log missing %q: %s", expected, output)
		}
	}
}

func TestCreateExportTransfersSnapshotLeaseAndBuildsSessionSource(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	start := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	videos := make([]teslacam.Video, 0, 4)
	for minute := range 2 {
		for _, camera := range []string{"front", "back"} {
			relative := filepath.ToSlash(filepath.Join("TeslaCam", "RecentClips", camera+time.Duration(minute).String()+".mp4"))
			name := filepath.Join(root, filepath.FromSlash(relative))
			if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(name, []byte("video"), 0o600); err != nil {
				t.Fatal(err)
			}
			videos = append(videos, teslacam.Video{
				ID: camera + time.Duration(minute).String(), Path: relative, ClipType: "recent", Camera: camera,
				RecordedAt: start.Add(time.Duration(minute) * time.Minute), Duration: 60,
			})
		}
	}
	sessionID := groupVideoSessions(groupVideos(videos, videoGroupTolerance), sessionContinuityGap)[0].ID
	released := 0
	controller := fakeController{
		state:    service.Snapshot{MediaSnapshot: &mediasnapshot.View{ID: "snapshot", MountPath: root}, Videos: videos},
		released: func() { released++ },
	}
	admin := &fakeExportAdmin{task: media.ExportTask{ID: "task", State: media.ExportQueued}}
	handler := NewHandler(controller, slog.New(slog.NewTextHandler(io.Discard, nil)), WithExportAdmin(admin))
	body := `{"session_id":"` + sessionID + `","start_seconds":10,"duration_seconds":90,"layout":"single","cameras":["front"],"quality":"standard","burn_timestamp":true}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/exports", strings.NewReader(body))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if released != 0 || admin.release == nil {
		t.Fatalf("snapshot lease released before async task: released = %d", released)
	}
	if admin.request.SessionID != sessionID || admin.request.Layout != "single" || len(admin.source.Videos) != 4 || len(admin.source.OverlayCues) != 90 || admin.source.OverlayCues[0].Timestamp != "2026-07-20 10:00:10" || admin.source.SessionEnd.Sub(admin.source.SessionStart) != 2*time.Minute {
		t.Fatalf("request = %#v, source = %#v", admin.request, admin.source)
	}
	admin.release()
	if released != 1 {
		t.Fatalf("released = %d", released)
	}
}

func TestCreateSourceExportSelectsOnlyRequestedMinutesAndCameras(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	start := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	videos := make([]teslacam.Video, 0, 4)
	for minute := range 2 {
		for _, camera := range []string{"front", "back"} {
			relative := filepath.ToSlash(filepath.Join("TeslaCam", "RecentClips", camera+time.Duration(minute).String()+".mp4"))
			name := filepath.Join(root, filepath.FromSlash(relative))
			if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(name, []byte(camera+"-source"), 0o600); err != nil {
				t.Fatal(err)
			}
			videos = append(videos, teslacam.Video{
				ID: camera + time.Duration(minute).String(), Path: relative, ClipType: "recent", Camera: camera,
				RecordedAt: start.Add(time.Duration(minute) * time.Minute), Duration: 60,
			})
		}
	}
	session := groupVideoSessions(groupVideos(videos, videoGroupTolerance), sessionContinuityGap)[0]
	released := 0
	controller := fakeController{
		state:    service.Snapshot{MediaSnapshot: &mediasnapshot.View{ID: "snapshot", MountPath: root}, Videos: videos},
		released: func() { released++ },
	}
	admin := &fakeExportAdmin{task: media.ExportTask{ID: "task", State: media.ExportQueued}}
	handler := NewHandler(controller, slog.New(slog.NewTextHandler(io.Discard, nil)), WithExportAdmin(admin))
	body := `{"mode":"source","session_id":"` + session.ID + `","moment_ids":["` + session.Moments[1].ID + `"],"cameras":["front","left_pillar"]}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/exports", strings.NewReader(body))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if admin.request.Mode != media.ExportModeSource || len(admin.source.SourceFiles) != 1 || admin.source.SourceFiles[0].Camera != "front" ||
		admin.source.SourceFiles[0].MomentID != session.Moments[1].ID || admin.source.SourceFiles[0].Size != uint64(len("front-source")) {
		t.Fatalf("request = %#v, source = %#v", admin.request, admin.source)
	}
	if released != 0 || admin.release == nil {
		t.Fatalf("snapshot lease released before async source export: released = %d", released)
	}
	admin.release()
	if released != 1 {
		t.Fatalf("released = %d", released)
	}
}

func TestExportDownloadUsesCompletedFile(t *testing.T) {
	t.Parallel()
	name := filepath.Join(t.TempDir(), "export.mp4")
	if err := os.WriteFile(name, []byte("complete-export"), 0o600); err != nil {
		t.Fatal(err)
	}
	admin := &fakeExportAdmin{
		task: media.ExportTask{
			ID: "task", State: media.ExportCompleted, DownloadReady: true,
			Request:      media.ExportRequest{Mode: media.ExportModeClip, Layout: "composite6"},
			VideoStartAt: time.Date(2026, 7, 30, 17, 40, 30, 0, time.FixedZone("CST", 8*60*60)),
		}, downloadPath: name,
	}
	handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithExportAdmin(admin))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/exports/task/download", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != "complete-export" {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	_, parameters, err := mime.ParseMediaType(response.Header().Get("Content-Disposition"))
	if err != nil || parameters["filename"] != "export-六路合成-20260730-174030.mp4" {
		t.Fatalf("content disposition = %q, parameters = %#v, error = %v", response.Header().Get("Content-Disposition"), parameters, err)
	}
}

func TestSourceExportDownloadUsesZipMetadata(t *testing.T) {
	t.Parallel()
	name := filepath.Join(t.TempDir(), "export.zip")
	if err := os.WriteFile(name, []byte("complete-source-export"), 0o600); err != nil {
		t.Fatal(err)
	}
	admin := &fakeExportAdmin{
		task: media.ExportTask{ID: "task", State: media.ExportCompleted, DownloadReady: true, Request: media.ExportRequest{Mode: media.ExportModeSource}}, downloadPath: name,
	}
	handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithExportAdmin(admin))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/exports/task/download", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != "complete-source-export" {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Header().Get("Content-Disposition"), "teslabox-source-task.zip") || response.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("headers = %#v", response.Header())
	}
}

func TestExportSettingsAndHistoryRoutes(t *testing.T) {
	t.Parallel()
	admin := &fakeExportAdmin{
		settings: media.ExportSettings{RetainCount: 8},
		history:  []media.ExportTask{{ID: "history", State: media.ExportCompleted, DownloadReady: true}},
	}
	handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithExportAdmin(admin))
	settingsRequest := httptest.NewRequest(http.MethodPut, "/api/v1/export-settings", strings.NewReader(`{"delete_after_download":true,"retain_count":3}`))
	settingsResponse := httptest.NewRecorder()
	handler.ServeHTTP(settingsResponse, settingsRequest)
	if settingsResponse.Code != http.StatusOK || !admin.settings.DeleteAfterDownload || admin.settings.RetainCount != 3 {
		t.Fatalf("settings response = %d %s, settings = %#v", settingsResponse.Code, settingsResponse.Body.String(), admin.settings)
	}
	historyRequest := httptest.NewRequest(http.MethodGet, "/api/v1/exports/history", nil)
	historyResponse := httptest.NewRecorder()
	handler.ServeHTTP(historyResponse, historyRequest)
	var history []media.ExportTask
	if historyResponse.Code != http.StatusOK || json.Unmarshal(historyResponse.Body.Bytes(), &history) != nil || len(history) != 1 || history[0].ID != "history" {
		t.Fatalf("history response = %d %s", historyResponse.Code, historyResponse.Body.String())
	}
}

func TestExportDownloadConsumesOnlyAfterCompleteResponse(t *testing.T) {
	t.Parallel()
	name := filepath.Join(t.TempDir(), "export.mp4")
	if err := os.WriteFile(name, []byte("complete-export"), 0o600); err != nil {
		t.Fatal(err)
	}
	admin := &fakeExportAdmin{
		task:         media.ExportTask{ID: "task", State: media.ExportCompleted, DownloadReady: true, Request: media.ExportRequest{Mode: media.ExportModeClip, Layout: "single", Cameras: []string{"front"}}},
		downloadPath: name, settings: media.ExportSettings{DeleteAfterDownload: true, RetainCount: 8},
	}
	handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithExportAdmin(admin))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/exports/task/download", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "complete-export" || admin.consumedID != "task" {
		t.Fatalf("response = %d %q, consumed = %q", response.Code, response.Body.String(), admin.consumedID)
	}
}

func TestVideoSEIReturnsUnavailableForRecordingWithoutTelemetry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	relative := filepath.ToSlash(filepath.Join("TeslaCam", "RecentClips", "clip-front.mp4"))
	name := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte{0, 0, 0, 15, 'm', 'd', 'a', 't', 0, 0, 0, 3, 0x65, 0x88, 0x99}, 0o600); err != nil {
		t.Fatal(err)
	}
	controller := fakeController{state: service.Snapshot{
		MediaSnapshot: &mediasnapshot.View{ID: "snapshot", MountPath: root},
		Videos:        []teslacam.Video{{ID: "front", Path: relative, Duration: 60}},
	}}
	handler := NewHandler(controller, slog.New(slog.NewTextHandler(io.Discard, nil)), WithMediaInspector(&fakeMediaInspector{}))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/videos/front/sei", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var payload seiTimelineResponse
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload.Available {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestAttachExportOverlayUsesSourceWallClock(t *testing.T) {
	t.Parallel()
	timelineStart := time.Date(2026, 7, 30, 8, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	source := media.ExportSource{
		SessionStart: timelineStart,
		SessionEnd:   timelineStart.Add(2 * time.Minute),
		Videos: []media.ExportVideo{{
			Camera: "front", Path: filepath.Join(t.TempDir(), "front.mp4"), RecordedAt: timelineStart,
			WallClockAt: timelineStart.Add(15 * time.Minute), Duration: 60,
		}},
	}
	err := attachExportOverlayCues(t.Context(), media.ExportRequest{
		StartSeconds: 5, DurationSeconds: 2, Cameras: []string{"front"}, BurnTimestamp: true,
	}, &source, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if len(source.OverlayCues) != 2 || source.OverlayCues[0].Timestamp != "2026-07-30 08:15:05" {
		t.Fatalf("overlay cues = %#v", source.OverlayCues)
	}
}
