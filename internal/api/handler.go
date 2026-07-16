package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wen-ryon/teslabox/internal/service"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

// Controller 定义 API 所需的最小状态和刷新能力。
type Controller interface {
	Snapshot() service.Snapshot
	Refresh(ctx context.Context) error
	Scan(ctx context.Context) error
}

// NewHandler 创建版本化 REST 路由并限制允许的方法。
func NewHandler(controller Controller, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/v1/storage", func(w http.ResponseWriter, r *http.Request) {
		state := controller.Snapshot()
		state.Videos = nil
		writeJSON(w, http.StatusOK, state)
	})
	mux.HandleFunc("GET /api/v1/videos", func(w http.ResponseWriter, r *http.Request) {
		writeVideoList(w, r, controller.Snapshot().Videos)
	})
	mux.HandleFunc("GET /api/v1/video-groups", func(w http.ResponseWriter, r *http.Request) {
		writeVideoGroupList(w, r, controller.Snapshot().Videos)
	})
	mux.HandleFunc("GET /api/v1/video-sessions", func(w http.ResponseWriter, r *http.Request) {
		writeVideoSessionList(w, r, controller.Snapshot().Videos)
	})
	mux.HandleFunc("GET /api/v1/video-sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeVideoSession(w, r, controller.Snapshot().Videos)
	})
	mux.HandleFunc("GET /api/v1/videos/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		serveVideo(w, r, controller.Snapshot(), false)
	})
	mux.HandleFunc("GET /api/v1/videos/{id}/download", func(w http.ResponseWriter, r *http.Request) {
		serveVideo(w, r, controller.Snapshot(), true)
	})
	refresh := func(w http.ResponseWriter, r *http.Request) {
		if err := controller.Refresh(r.Context()); err != nil {
			logger.Warn("API 触发状态刷新失败", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "TeslaUSB 数据盘暂不可用"})
			return
		}
		state := controller.Snapshot()
		state.Videos = nil
		writeJSON(w, http.StatusOK, state)
	}
	mux.HandleFunc("POST /api/v1/refresh", refresh)
	mux.HandleFunc("POST /api/v1/scan", func(w http.ResponseWriter, r *http.Request) {
		if err := controller.Scan(r.Context()); err != nil {
			logger.Warn("API 触发媒体快照失败", "error", err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "TeslaCam 快照暂不可用"})
			return
		}
		state := controller.Snapshot()
		state.Videos = nil
		writeJSON(w, http.StatusOK, state)
	})
	return requestLogger(mux, logger)
}

func serveVideo(w http.ResponseWriter, r *http.Request, state service.Snapshot, download bool) {
	if state.MediaSnapshot == nil {
		http.Error(w, "媒体快照暂不可用", http.StatusServiceUnavailable)
		return
	}
	var selected *teslacam.Video
	for index := range state.Videos {
		if state.Videos[index].ID == r.PathValue("id") {
			selected = &state.Videos[index]
			break
		}
	}
	if selected == nil {
		http.NotFound(w, r)
		return
	}

	file, err := openSnapshotVideo(state.MediaSnapshot.MountPath, selected.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "视频文件暂不可用", http.StatusServiceUnavailable)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		http.Error(w, "视频文件暂不可用", http.StatusServiceUnavailable)
		return
	}
	if download {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(selected.Path)}))
	}
	w.Header().Set("Content-Type", "video/mp4")
	http.ServeContent(w, r, filepath.Base(selected.Path), info.ModTime(), file)
}

func openSnapshotVideo(root, relative string) (*os.File, error) {
	cleaned := filepath.Clean(filepath.FromSlash(relative))
	if filepath.IsAbs(cleaned) || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("视频路径越过快照根目录")
	}
	name := filepath.Join(filepath.Clean(root), cleaned)
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("视频路径不是普通文件")
	}
	return file, nil
}

type videoListResponse struct {
	Items  []teslacam.Video `json:"items"`
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

const (
	videoGroupTolerance    = 2 * time.Second
	defaultSegmentDuration = 60 * time.Second
	sessionContinuityGap   = 5 * time.Second
)

type videoGroup struct {
	ID            string                  `json:"id"`
	ClipType      string                  `json:"clip_type"`
	Event         string                  `json:"event,omitempty"`
	EventMetadata *teslacam.EventMetadata `json:"event_metadata,omitempty"`
	RecordedAt    time.Time               `json:"recorded_at"`
	EndAt         time.Time               `json:"end_at"`
	Duration      float64                 `json:"duration_seconds"`
	TimeSpreadMS  int64                   `json:"time_spread_ms"`
	TotalSize     int64                   `json:"total_size"`
	Complete      bool                    `json:"complete"`
	Videos        []teslacam.Video        `json:"videos"`
}

type videoSession struct {
	ID            string                  `json:"id"`
	ClipType      string                  `json:"clip_type"`
	Event         string                  `json:"event,omitempty"`
	EventMetadata *teslacam.EventMetadata `json:"event_metadata,omitempty"`
	StartAt       time.Time               `json:"start_at"`
	EndAt         time.Time               `json:"end_at"`
	Duration      float64                 `json:"duration_seconds"`
	SegmentCount  int                     `json:"segment_count"`
	TotalSize     int64                   `json:"total_size"`
	Complete      bool                    `json:"complete"`
	Cameras       []string                `json:"cameras"`
	Moments       []videoGroup            `json:"moments,omitempty"`
}

type videoSessionListResponse struct {
	Items  []videoSession `json:"items"`
	Total  int            `json:"total"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

type videoGroupListResponse struct {
	Items     []videoGroup `json:"items"`
	Total     int          `json:"total"`
	Limit     int          `json:"limit"`
	Offset    int          `json:"offset"`
	Tolerance int64        `json:"tolerance_ms"`
}

func writeVideoList(w http.ResponseWriter, r *http.Request, videos []teslacam.Video) {
	query := r.URL.Query()
	filtered := make([]teslacam.Video, 0, len(videos))
	for _, video := range videos {
		if value := query.Get("clip_type"); value != "" && video.ClipType != value {
			continue
		}
		if value := query.Get("camera"); value != "" && video.Camera != value {
			continue
		}
		if value := query.Get("event"); value != "" && video.Event != value {
			continue
		}
		if value := query.Get("date"); value != "" && video.RecordedAt.Format("2006-01-02") != value {
			continue
		}
		filtered = append(filtered, video)
	}

	limit := boundedInt(query.Get("limit"), 200, 1, 1000)
	offset := boundedInt(query.Get("offset"), 0, 0, len(filtered))
	end := min(offset+limit, len(filtered))
	items := append([]teslacam.Video(nil), filtered[offset:end]...)
	if items == nil {
		items = []teslacam.Video{}
	}
	writeJSON(w, http.StatusOK, videoListResponse{
		Items: items, Total: len(filtered), Limit: limit, Offset: offset,
	})
}

func writeVideoGroupList(w http.ResponseWriter, r *http.Request, videos []teslacam.Video) {
	query := r.URL.Query()
	groups := groupVideos(videos, videoGroupTolerance)
	filtered := make([]videoGroup, 0, len(groups))
	for _, group := range groups {
		if value := query.Get("clip_type"); value != "" && group.ClipType != value {
			continue
		}
		if value := query.Get("event"); value != "" && group.Event != value {
			continue
		}
		if value := query.Get("date"); value != "" && group.RecordedAt.Format("2006-01-02") != value {
			continue
		}
		if value := query.Get("camera"); value != "" && !groupHasCamera(group, value) {
			continue
		}
		filtered = append(filtered, group)
	}

	limit := boundedInt(query.Get("limit"), 100, 1, 500)
	offset := boundedInt(query.Get("offset"), 0, 0, len(filtered))
	end := min(offset+limit, len(filtered))
	items := append([]videoGroup(nil), filtered[offset:end]...)
	if items == nil {
		items = []videoGroup{}
	}
	writeJSON(w, http.StatusOK, videoGroupListResponse{
		Items: items, Total: len(filtered), Limit: limit, Offset: offset,
		Tolerance: videoGroupTolerance.Milliseconds(),
	})
}

func writeVideoSessionList(w http.ResponseWriter, r *http.Request, videos []teslacam.Video) {
	query := r.URL.Query()
	sessions := groupVideoSessions(groupVideos(videos, videoGroupTolerance), sessionContinuityGap)
	filtered := make([]videoSession, 0, len(sessions))
	for _, session := range sessions {
		if value := query.Get("clip_type"); value != "" && session.ClipType != value {
			continue
		}
		if value := query.Get("event"); value != "" && session.Event != value {
			continue
		}
		if value := query.Get("date"); value != "" && session.StartAt.Format("2006-01-02") != value {
			continue
		}
		if value := query.Get("camera"); value != "" && !slices.Contains(session.Cameras, value) {
			continue
		}
		filtered = append(filtered, session)
	}
	limit := boundedInt(query.Get("limit"), 200, 1, 1000)
	offset := boundedInt(query.Get("offset"), 0, 0, len(filtered))
	end := min(offset+limit, len(filtered))
	items := append([]videoSession(nil), filtered[offset:end]...)
	for index := range items {
		items[index].Moments = nil
	}
	if items == nil {
		items = []videoSession{}
	}
	writeJSON(w, http.StatusOK, videoSessionListResponse{
		Items: items, Total: len(filtered), Limit: limit, Offset: offset,
	})
}

func writeVideoSession(w http.ResponseWriter, r *http.Request, videos []teslacam.Video) {
	for _, session := range groupVideoSessions(groupVideos(videos, videoGroupTolerance), sessionContinuityGap) {
		if session.ID == r.PathValue("id") {
			writeJSON(w, http.StatusOK, session)
			return
		}
	}
	http.NotFound(w, r)
}

func groupVideos(videos []teslacam.Video, tolerance time.Duration) []videoGroup {
	sorted := append([]teslacam.Video(nil), videos...)
	slices.SortFunc(sorted, func(left, right teslacam.Video) int {
		return right.RecordedAt.Compare(left.RecordedAt)
	})
	groups := make([]videoGroup, 0, len(sorted)/6+1)
	for _, video := range sorted {
		groupIndex := matchingGroup(groups, video, tolerance)
		if groupIndex < 0 {
			groups = append(groups, videoGroup{
				ID: video.ID, ClipType: video.ClipType, Event: video.Event, EventMetadata: video.EventMetadata,
				RecordedAt: video.RecordedAt, Videos: []teslacam.Video{video}, TotalSize: video.Size,
			})
			continue
		}
		group := &groups[groupIndex]
		group.Videos = append(group.Videos, video)
		group.TotalSize += video.Size
		if group.EventMetadata == nil {
			group.EventMetadata = video.EventMetadata
		}
		if video.RecordedAt.Before(group.RecordedAt) {
			group.RecordedAt = video.RecordedAt
		}
	}
	for index := range groups {
		group := &groups[index]
		slices.SortFunc(group.Videos, func(left, right teslacam.Video) int {
			return cameraRank(left.Camera) - cameraRank(right.Camera)
		})
		latest := group.RecordedAt
		var endAt time.Time
		for _, video := range group.Videos {
			if video.RecordedAt.After(latest) {
				latest = video.RecordedAt
			}
			duration := time.Duration(video.Duration * float64(time.Second))
			if duration > 0 {
				videoEnd := video.RecordedAt.Add(duration)
				if videoEnd.After(endAt) {
					endAt = videoEnd
				}
			}
		}
		if endAt.IsZero() {
			endAt = latest.Add(defaultSegmentDuration)
		}
		group.TimeSpreadMS = latest.Sub(group.RecordedAt).Milliseconds()
		group.EndAt = endAt
		group.Duration = endAt.Sub(group.RecordedAt).Seconds()
		group.Complete = len(group.Videos) == 6
	}
	return groups
}

func groupVideoSessions(groups []videoGroup, continuityGap time.Duration) []videoSession {
	ordered := append([]videoGroup(nil), groups...)
	slices.SortFunc(ordered, func(left, right videoGroup) int {
		return left.RecordedAt.Compare(right.RecordedAt)
	})
	sessions := make([]videoSession, 0, len(ordered))
	for _, group := range ordered {
		if len(sessions) == 0 || !sessionAcceptsGroup(sessions[len(sessions)-1], group, continuityGap) {
			sessions = append(sessions, videoSession{
				ID: group.ID, ClipType: group.ClipType, Event: group.Event, EventMetadata: group.EventMetadata,
				StartAt: group.RecordedAt, EndAt: group.EndAt,
				TotalSize: group.TotalSize, Complete: group.Complete,
				Moments: []videoGroup{group},
			})
			continue
		}
		session := &sessions[len(sessions)-1]
		session.Moments = append(session.Moments, group)
		session.TotalSize += group.TotalSize
		session.Complete = session.Complete && group.Complete
		if session.EventMetadata == nil {
			session.EventMetadata = group.EventMetadata
		}
		if group.EndAt.After(session.EndAt) {
			session.EndAt = group.EndAt
		}
	}
	for index := range sessions {
		session := &sessions[index]
		session.Duration = session.EndAt.Sub(session.StartAt).Seconds()
		session.SegmentCount = len(session.Moments)
		seen := make(map[string]bool, 6)
		for _, group := range session.Moments {
			for _, video := range group.Videos {
				seen[video.Camera] = true
			}
		}
		for _, camera := range []string{"left_pillar", "front", "right_pillar", "left_repeater", "back", "right_repeater"} {
			if seen[camera] {
				session.Cameras = append(session.Cameras, camera)
			}
		}
	}
	slices.Reverse(sessions)
	return sessions
}

func sessionAcceptsGroup(session videoSession, group videoGroup, continuityGap time.Duration) bool {
	if session.ClipType != group.ClipType || session.Event != group.Event {
		return false
	}
	return !group.RecordedAt.After(session.EndAt.Add(continuityGap))
}

func matchingGroup(groups []videoGroup, video teslacam.Video, tolerance time.Duration) int {
	for index := len(groups) - 1; index >= 0; index-- {
		group := groups[index]
		if group.ClipType != video.ClipType || group.Event != video.Event || groupHasCamera(group, video.Camera) {
			continue
		}
		earliest := video.RecordedAt
		latest := video.RecordedAt
		for _, member := range group.Videos {
			if member.RecordedAt.Before(earliest) {
				earliest = member.RecordedAt
			}
			if member.RecordedAt.After(latest) {
				latest = member.RecordedAt
			}
		}
		if latest.Sub(earliest) <= tolerance {
			return index
		}
	}
	return -1
}

func groupHasCamera(group videoGroup, camera string) bool {
	return slices.ContainsFunc(group.Videos, func(video teslacam.Video) bool {
		return video.Camera == camera
	})
}

func cameraRank(camera string) int {
	for index, known := range []string{"left_pillar", "front", "right_pillar", "left_repeater", "back", "right_repeater"} {
		if camera == known {
			return index
		}
	}
	return 100
}

func boundedInt(value string, fallback, minimum, maximum int) int {
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	if parsed < minimum {
		return minimum
	}
	if parsed > maximum {
		return maximum
	}
	return parsed
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		return
	}
}

func requestLogger(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedAt := time.Now()
		tracked := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(tracked, r)
		logger.Info("HTTP 请求完成",
			"method", r.Method,
			"path", strings.TrimSpace(r.URL.Path),
			"status", tracked.status,
			"duration_ms", time.Since(startedAt).Milliseconds(),
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
