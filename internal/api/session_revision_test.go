package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
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
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

type revisionController struct {
	fakeController
	identity string
}

func (c *revisionController) RecordingIdentity(service.Snapshot) (string, error) {
	return c.identity, nil
}

func TestExportAndUploadRebindOnlyUnchangedArchiveSelection(t *testing.T) {
	for _, change := range []string{"unchanged", "mtime", "size", "missing", "timeline", "camera", "disk", "recent", "legacy"} {
		t.Run(change, func(t *testing.T) {
			roots := []string{t.TempDir(), t.TempDir()}
			start := time.Date(2026, 10, 10, 22, 0, 0, 0, time.UTC)
			data := videoDurationFixture(60000)
			var videos []teslacam.Video
			for _, camera := range []string{"front", "back"} {
				path := camera + ".mp4"
				for _, root := range roots {
					if err := os.WriteFile(filepath.Join(root, path), data, 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(filepath.Join(root, path), start, start); err != nil {
						t.Fatal(err)
					}
				}
				clip := "sentry"
				if change == "recent" {
					clip = "recent"
				}
				videos = append(videos, teslacam.Video{ID: camera, Path: path, Camera: camera, ClipType: clip, Event: "event", RecordedAt: start, Duration: 60, Size: int64(len(data))})
			}
			controller := &revisionController{fakeController: fakeController{state: service.Snapshot{MediaSnapshot: &mediasnapshot.View{ID: "old", MountPath: roots[0]}, Videos: videos}}, identity: "disk-generation-1"}
			exporter := &fakeExportAdmin{task: media.ExportTask{ID: "export", State: media.ExportQueued}}
			upload := &capturedNAS{}
			handler := NewHandler(controller, slog.New(slog.NewTextHandler(io.Discard, nil)), WithExportAdmin(exporter), WithNASUploadAdmin(upload))
			session, _ := findVideoSession(videos, groupVideoSessions(groupVideos(videos, videoGroupTolerance), sessionContinuityGap)[0].ID)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/video-sessions/"+session.ID, nil))
			var original videoSession
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &original) != nil || change != "recent" && original.SourceRevision == "" {
				t.Fatalf("missing selection revision: %d %s", response.Code, response.Body.String())
			}
			controller.state.MediaSnapshot = &mediasnapshot.View{ID: "new", MountPath: roots[1]}
			path := filepath.Join(roots[1], videos[0].Path)
			var err error
			switch change {
			case "mtime":
				err = os.Chtimes(path, start, start.Add(time.Second))
			case "size":
				err = os.WriteFile(path, append(data, 1), 0o600)
			case "missing":
				err = os.Remove(path)
			case "timeline":
				controller.state.Videos[0].RecordedAt = start.Add(time.Second)
			case "camera":
				controller.state.Videos = controller.state.Videos[:1]
			case "disk":
				controller.identity = "disk-generation-2"
			case "legacy":
				original.SourceRevision = ""
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, endpoint := range []string{"exports", "uploads"} {
				request := map[string]any{"session_id": original.ID, "snapshot_id": "old", "source_revision": original.SourceRevision, "moment_ids": []string{original.Moments[0].ID}, "cameras": []string{"front"}}
				if endpoint == "exports" {
					request["mode"] = "source"
				}
				payload, _ := json.Marshal(request)
				response = httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/"+endpoint, bytes.NewReader(payload)))
				want := http.StatusConflict
				if change == "unchanged" {
					want = http.StatusAccepted
				}
				if response.Code != want {
					t.Fatalf("%s: status=%d want=%d body=%s", endpoint, response.Code, want, response.Body.String())
				}
			}
			if change == "unchanged" {
				if exporter.request.SnapshotID != "new" || upload.request.SnapshotID != "new" || !strings.HasPrefix(exporter.source.SourceFiles[0].Path, roots[1]) || !strings.HasPrefix(upload.inputs[0].Path, roots[1]) {
					t.Fatal("accepted request still points to retired snapshot")
				}
				if exporter.release != nil {
					exporter.release()
				}
			} else if exporter.request.SessionID != "" || upload.created {
				t.Fatal("changed selection created a task")
			}
		})
	}
}
