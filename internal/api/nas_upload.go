package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/wen-ryon/teslabox/internal/media"
	"github.com/wen-ryon/teslabox/internal/nasupload"
	"github.com/wen-ryon/teslabox/internal/service"
)

// NASUploadAdmin 只允许受认证设置和已通过快照校验的手动任务。
type NASUploadAdmin interface {
	Targets() []nasupload.Target
	DefaultTarget() (nasupload.Target, error)
	SetDefaultTarget(string) (nasupload.Target, error)
	SaveTarget(context.Context, nasupload.Target) (nasupload.Target, error)
	SaveServer(context.Context, nasupload.Target) (nasupload.Target, error)
	SaveQuality(string, nasupload.Quality) (nasupload.Target, error)
	DeleteTarget(string) error
	TestTarget(context.Context, string) error
	Create(nasupload.Request, string, string, string, []nasupload.Input, context.Context, func()) (nasupload.Task, error)
	History() []nasupload.Task
	Counts(string) map[string]int
	Records(string) []nasupload.UploadedFile
	Retry(string) (nasupload.Task, error)
	Cancel(string) (nasupload.Task, error)
	Automatic() nasupload.AutoConfig
	SaveAutomatic(nasupload.AutoConfig) (nasupload.AutoConfig, error)
	AutomaticEvents() []nasupload.AutoEvent
	AutomaticScan() nasupload.AutoScanStatus
	CancelAutomatic(string) error
}

// WithNASUploadAdmin 在现有登录保护内启用手动上传。
func WithNASUploadAdmin(admin NASUploadAdmin) Option {
	return func(o *handlerOptions) { o.nasUploadAdmin = admin }
}

func registerNASUploadRoutes(mux *http.ServeMux, controller Controller, admin NASUploadAdmin, catalog *sessionIndex, logger *slog.Logger) {
	fail := func(w http.ResponseWriter, err error) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
	}
	mux.HandleFunc("GET /api/v1/upload-targets", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, admin.Targets()) })
	mux.HandleFunc("GET /api/v1/automatic-uploads", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"config": admin.Automatic(), "events": admin.AutomaticEvents(), "scan": admin.AutomaticScan()})
	})
	mux.HandleFunc("PUT /api/v1/automatic-uploads", func(w http.ResponseWriter, r *http.Request) {
		var config nasupload.AutoConfig
		if !decodeJSON(w, r, &config) {
			return
		}
		saved, err := admin.SaveAutomatic(config)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, saved)
	})
	mux.HandleFunc("DELETE /api/v1/automatic-uploads/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := admin.CancelAutomatic(r.PathValue("id")); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"canceled": true})
	})
	mux.HandleFunc("PUT /api/v1/upload-targets/default", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TargetID string `json:"target_id"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		target, err := admin.SetDefaultTarget(req.TargetID)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, target)
	})
	mux.HandleFunc("POST /api/v1/upload-targets", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			nasupload.Target
			ServerOnly bool `json:"server_only"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		var out nasupload.Target
		var err error
		if req.ServerOnly {
			out, err = admin.SaveServer(ctx, req.Target)
		} else {
			out, err = admin.SaveTarget(ctx, req.Target)
		}
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("PUT /api/v1/upload-targets/quality", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TargetID string            `json:"target_id"`
			Quality  nasupload.Quality `json:"quality"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		target, err := admin.SaveQuality(req.TargetID, req.Quality)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, target)
	})
	mux.HandleFunc("DELETE /api/v1/upload-targets/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := admin.DeleteTarget(r.PathValue("id")); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
	})
	mux.HandleFunc("POST /api/v1/upload-targets/{id}/test", func(w http.ResponseWriter, r *http.Request) {
		if err := admin.TestTarget(r.Context(), r.PathValue("id")); err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"verified": true})
	})
	mux.HandleFunc("GET /api/v1/uploads", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, admin.History()) })
	mux.HandleFunc("POST /api/v1/uploads/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		task, err := admin.Retry(r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, task)
	})
	mux.HandleFunc("DELETE /api/v1/uploads/{id}", func(w http.ResponseWriter, r *http.Request) {
		task, err := admin.Cancel(r.PathValue("id"))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, task)
	})
	mux.HandleFunc("GET /api/v1/upload-counts", func(w http.ResponseWriter, r *http.Request) {
		provider, ok := controller.(interface {
			RecordingIdentity(service.Snapshot) (string, error)
		})
		if !ok {
			fail(w, errors.New("录像盘身份校验不可用"))
			return
		}
		state := controller.Snapshot()
		identity, err := provider.RecordingIdentity(state)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"available": false})
			return
		}
		id := ""
		if state.MediaSnapshot != nil {
			id = state.MediaSnapshot.ID
		}
		if cache, ok := controller.(interface {
			CachedMedia(bool) service.CachedMediaIndex
		}); ok {
			cached := cache.CachedMedia(false)
			if cached.ID != "" {
				id = cached.ID
				state.Videos = cached.Videos
			}
		}
		moments := admin.Counts(identity)
		sessions := map[string]map[string]int{}
		allSessions := append([]videoSession(nil), catalog.getList(id, state.Videos, false)...)
		allSessions = append(allSessions, catalog.getList(id, state.Videos, true)...)
		for _, session := range allSessions {
			uploaded := 0
			for _, moment := range session.Moments {
				if moments[moment.ID] > 0 || moments[fmt.Sprintf("at:%d", moment.RecordedAt.Unix()/60)] > 0 {
					uploaded++
				}
			}
			sessions[session.ID] = map[string]int{"uploaded": uploaded, "total": len(session.Moments)}
		}
		writeJSON(w, http.StatusOK, map[string]any{"available": true, "moments": moments, "sessions": sessions, "files": admin.Records(identity)})
	})
	mux.HandleFunc("POST /api/v1/uploads", func(w http.ResponseWriter, r *http.Request) {
		var req nasupload.Request
		if !decodeJSON(w, r, &req) {
			return
		}
		// 自动去重键只能由后台从事件快照生成，手动请求不能冒充自动事件。
		req.AutoEventID, req.AutoKey, req.Role = "", "", ""
		target, err := admin.DefaultTarget()
		if err != nil {
			fail(w, err)
			return
		}
		if req.TargetID != "" && req.TargetID != target.ID {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "默认上传目标已变更，请重新打开上传窗口"})
			return
		}
		req.TargetID = target.ID
		activateMedia(controller, logger, r.Context())
		state, release := controller.AcquireMediaSnapshot()
		if state.MediaSnapshot == nil {
			release()
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "媒体快照暂不可用"})
			return
		}
		if !recentSnapshotMatches(req.SessionID, req.SnapshotID, state.MediaSnapshot.ID) || req.SnapshotID != "" && req.SnapshotID != state.MediaSnapshot.ID {
			if !unchangedSessionSelection(controller, state, req.SessionID, req.SourceRevision) {
				release()
				writeJSON(w, http.StatusConflict, map[string]string{"error": "录像内容或时间轴已变化，请重新打开后选择上传范围"})
				return
			}
			logger.Info("上传选择已衔接新快照", "session_id", req.SessionID, "previous_snapshot_id", req.SnapshotID, "snapshot_id", state.MediaSnapshot.ID)
			req.SnapshotID = state.MediaSnapshot.ID
		}
		session, found := findVideoSession(state.Videos, req.SessionID)
		if !found {
			release()
			http.NotFound(w, r)
			return
		}
		source, err := sourceFileExportSource(state.MediaSnapshot.MountPath, session, media.ExportRequest{MomentIDs: req.MomentIDs, Cameras: req.Cameras})
		if err != nil {
			release()
			fail(w, err)
			return
		}
		provider, ok := controller.(interface {
			RecordingIdentity(service.Snapshot) (string, error)
		})
		if !ok {
			release()
			fail(w, errors.New("录像盘身份校验不可用"))
			return
		}
		identity, err := provider.RecordingIdentity(state)
		if err != nil {
			release()
			fail(w, errors.New("无法确认录像盘身份"))
			return
		}
		inputs := make([]nasupload.Input, 0, len(source.SourceFiles))
		for _, f := range source.SourceFiles {
			inputs = append(inputs, nasupload.Input{MomentID: f.MomentID, Camera: f.Camera, Name: f.Name, Path: f.Path, RecordedAt: f.RecordedAt, Size: f.Size})
		}
		task, err := admin.Create(req, identity, state.MediaSnapshot.MountPath, state.MediaSnapshot.ID, inputs, state.MediaContext, release)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, task)
	})
}
