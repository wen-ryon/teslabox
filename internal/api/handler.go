package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
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
	mux.HandleFunc("POST /api/v1/scan", refresh)
	return requestLogger(mux, logger)
}

type videoListResponse struct {
	Items  []teslacam.Video `json:"items"`
	Total  int              `json:"total"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
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
