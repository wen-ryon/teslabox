package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/nasupload"
	"github.com/wen-ryon/teslabox/internal/service"
	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

type nasController struct{ fakeController }

func (c nasController) RecordingIdentity(service.Snapshot) (string, error) { return "test-disk", nil }

type capturedNAS struct {
	*nasupload.Manager
	request      nasupload.Request
	inputs       []nasupload.Input
	identity     string
	created      bool
	counts       map[string]int
	records      []nasupload.UploadedFile
	defaultID    string
	defaultError error
	savedTarget  nasupload.Target
	serverOnly   bool
}

func (a *capturedNAS) Counts(string) map[string]int { return a.counts }

func (a *capturedNAS) Records(string) []nasupload.UploadedFile { return a.records }

func (a *capturedNAS) SaveTarget(_ context.Context, target nasupload.Target) (nasupload.Target, error) {
	a.savedTarget = target
	return target, nil
}

func (a *capturedNAS) SaveServer(_ context.Context, target nasupload.Target) (nasupload.Target, error) {
	a.serverOnly = true
	a.savedTarget = target
	return target, nil
}

func (a *capturedNAS) SaveQuality(id string, quality nasupload.Quality) (nasupload.Target, error) {
	a.savedTarget = nasupload.Target{ID: id, Quality: quality}
	return a.savedTarget, nil
}

func (a *capturedNAS) DefaultTarget() (nasupload.Target, error) {
	if a.defaultError != nil {
		return nasupload.Target{}, a.defaultError
	}
	id := a.defaultID
	if id == "" {
		id = "nas"
	}
	return nasupload.Target{ID: id, IsDefault: true, Quality: nasupload.Quality{Mode: "original"}}, nil
}

func (a *capturedNAS) SetDefaultTarget(id string) (nasupload.Target, error) {
	a.defaultID = id
	return a.DefaultTarget()
}

func (a *capturedNAS) Create(req nasupload.Request, identity, root, version string, inputs []nasupload.Input, ctx context.Context, release func()) (nasupload.Task, error) {
	defer release()
	a.request = req
	a.inputs = inputs
	a.identity = identity
	a.created = true
	return nasupload.Task{ID: "test", State: "copying"}, nil
}

func TestNASUploadAPIFixesSourceSelectionAndRejectsStaleSnapshot(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	videos := []teslacam.Video{}
	for minute := range 2 {
		for _, camera := range []string{"front", "back"} {
			name := camera + time.Duration(minute).String() + ".mp4"
			if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
				t.Fatal(err)
			}
			videos = append(videos, teslacam.Video{ID: name, Path: name, Camera: camera, ClipType: "saved", RecordedAt: start.Add(time.Duration(minute) * time.Minute), Duration: 60})
		}
	}
	session := groupVideoSessions(groupVideos(videos, videoGroupTolerance), sessionContinuityGap)[0]
	for _, test := range []struct {
		name, snapshot, target string
		want                   int
	}{{"selected", "current", "nas", http.StatusAccepted}, {"default", "current", "", http.StatusAccepted}, {"stale", "old", "nas", http.StatusConflict}} {
		t.Run(test.name, func(t *testing.T) {
			released := 0
			controller := nasController{fakeController{state: service.Snapshot{MediaSnapshot: &mediasnapshot.View{ID: "current", MountPath: root}, Videos: videos}, released: func() { released++ }}}
			admin := &capturedNAS{}
			handler := NewHandler(controller, slog.New(slog.NewTextHandler(io.Discard, nil)), WithNASUploadAdmin(admin))
			q := nasupload.Quality{Mode: "compressed", Height: 720, FPS: 18, BitrateKbps: 2800, Layout: "separate"}
			body, _ := json.Marshal(nasupload.Request{TargetID: test.target, SessionID: session.ID, SnapshotID: test.snapshot, MomentIDs: []string{session.Moments[1].ID}, Cameras: []string{"front"}, Quality: &q,AutoKey:"forged",AutoEventID:"forged",Role:"event"})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest("POST", "/api/v1/uploads", bytes.NewReader(body)))
			if response.Code != test.want {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
			if released != 1 {
				t.Fatalf("snapshot release count %d", released)
			}
			if test.want == http.StatusAccepted {
				if admin.request.AutoKey!="" || admin.request.AutoEventID!="" || admin.request.Role!="" { t.Fatal("manual client forged automatic deduplication key") }
				if admin.request.TargetID != "nas" || len(admin.inputs) != 1 || admin.inputs[0].Camera != "front" || admin.inputs[0].MomentID != session.Moments[1].ID || admin.identity != "test-disk" || *admin.request.Quality != q {
					t.Fatal("source selection or custom quality changed")
				}
			} else if admin.created {
				t.Fatal("stale snapshot accepted")
			}
		})
	}
}

func TestNASDefaultTargetGateBeforeAcquiringSnapshot(t *testing.T) {
	for _, test := range []struct {
		name   string
		target string
		err    error
		want   int
	}{
		{name: "未选择默认目标", err: errors.New("请指定默认上传目标"), want: http.StatusUnprocessableEntity},
		{name: "请求不能指定其他目标", target: "other", want: http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			released := 0
			controller := nasController{fakeController{released: func() { released++ }}}
			admin := &capturedNAS{defaultError: test.err}
			handler := NewHandler(controller, slog.New(slog.NewTextHandler(io.Discard, nil)), WithNASUploadAdmin(admin))
			body, _ := json.Marshal(nasupload.Request{TargetID: test.target})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest("POST", "/api/v1/uploads", bytes.NewReader(body)))
			if response.Code != test.want || admin.created || released != 0 {
				t.Fatalf("默认目标保护失败：%d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestNASDefaultTargetConfigRoute(t *testing.T) {
	admin := &capturedNAS{}
	handler := NewHandler(nasController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithNASUploadAdmin(admin))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("PUT", "/api/v1/upload-targets/default", bytes.NewBufferString(`{"target_id":"chosen"}`)))
	var target nasupload.Target
	if err := json.Unmarshal(response.Body.Bytes(), &target); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || admin.defaultID != "chosen" || target.ID != "chosen" || !target.IsDefault {
		t.Fatalf("默认配置路由失败：%s", response.Body.String())
	}
}

func TestNASIndependentConfigRoutes(t *testing.T) {
	for _, test := range []struct {
		name, method, path, body string
		serverOnly               bool
	}{
		{name: "独立服务器配置", method: "POST", path: "/api/v1/upload-targets", body: `{"id":"nas","server_only":true,"name":"server"}`, serverOnly: true},
		{name: "兼容旧的整体配置", method: "POST", path: "/api/v1/upload-targets", body: `{"id":"nas","name":"server"}`},
		{name: "独立画质设置", method: "PUT", path: "/api/v1/upload-targets/quality", body: `{"target_id":"nas","quality":{"mode":"compressed","height":480,"fps":15,"bitrate_kbps":1500,"layout":"composite4a"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			admin := &capturedNAS{}
			handler := NewHandler(nasController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithNASUploadAdmin(admin))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, bytes.NewBufferString(test.body)))
			if response.Code != http.StatusOK || admin.serverOnly != test.serverOnly || admin.savedTarget.ID != "nas" {
				t.Fatalf("保存边界不符合预期：%d %s", response.Code, response.Body.String())
			}
			if test.method == "PUT" && admin.savedTarget.Quality != (nasupload.Quality{Mode: "compressed", Height: 480, FPS: 15, BitrateKbps: 1500, Layout: "composite4a"}) {
				t.Fatal("独立画质参数未传递完整")
			}
		})
	}
}

func TestNASUploadCountsWorksWithSummaryOnlyVideoList(t *testing.T) {
	videos := []teslacam.Video{{ID: "front", Path: "front.mp4", Camera: "front", ClipType: "saved", RecordedAt: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC), Duration: 60}, {ID: "back", Path: "back.mp4", Camera: "back", ClipType: "saved", RecordedAt: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC), Duration: 60}}
	session := groupVideoSessions(groupVideos(videos, videoGroupTolerance), sessionContinuityGap)[0]
	controller := nasController{fakeController{state: service.Snapshot{MediaSnapshot: &mediasnapshot.View{ID: "snapshot"}, Videos: videos}}}
	admin := &capturedNAS{counts: map[string]int{session.Moments[0].ID: 1}, records: []nasupload.UploadedFile{
		{ID: "original", FileResult: nasupload.FileResult{MomentID: session.Moments[0].ID, Cameras: []string{"front"}}, Quality: nasupload.Quality{Mode: "original"}},
		{ID: "merged", FileResult: nasupload.FileResult{MomentID: session.Moments[0].ID, Cameras: []string{"front", "back"}}, Quality: nasupload.Quality{Mode: "compressed", Layout: "composite4a"}},
	}}
	handler := NewHandler(controller, slog.New(slog.NewTextHandler(io.Discard, nil)), WithNASUploadAdmin(admin))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/video-sessions?metadata_only=true", nil))
	var list videoSessionListResponse
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || len(list.Items[0].Moments) != 0 {
		t.Fatal("list should remain metadata only")
	}
	if len(list.Items[0].MomentRefs) != 1 || list.Items[0].MomentRefs[0].ID != session.Moments[0].ID || !list.Items[0].MomentRefs[0].RecordedAt.Equal(videos[0].RecordedAt) {
		t.Fatal("summary must retain one minute reference across two camera files")
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/api/v1/upload-counts", nil))
	var result struct {
		Available bool                     `json:"available"`
		Files     []nasupload.UploadedFile `json:"files"`
		Sessions  map[string]struct {
			Uploaded int `json:"uploaded"`
			Total    int `json:"total"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Available || result.Sessions[session.ID].Uploaded != 1 || result.Sessions[session.ID].Total != 1 {
		t.Fatalf("minute totals: %s", response.Body.String())
	}
	if len(result.Files) != 2 || result.Files[1].Quality.Layout != "composite4a" || len(result.Files[1].Cameras) != 2 {
		t.Fatalf("文件统计必须保留单路与合成的不同结果：%s", response.Body.String())
	}
}
