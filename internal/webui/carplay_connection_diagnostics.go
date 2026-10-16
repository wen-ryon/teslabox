package webui

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sync"
	"time"
)

var carPlayPageID = regexp.MustCompile(`^[a-z0-9-]{8,48}$`)

type carPlayConnectionEvent struct {
	PageID        string `json:"page_id"`
	Phase         string `json:"phase"`
	Reason        string `json:"reason"`
	Sequence      uint64 `json:"seq"`
	Attempt       uint64 `json:"attempt"`
	Generation    uint64 `json:"generation"`
	ElapsedMS     uint64 `json:"elapsed_ms"`
	PageElapsedMS uint64 `json:"page_elapsed_ms"`
	ClientTimeMS  uint64 `json:"client_time_ms"`
	Status        uint16 `json:"status"`
	CloseCode     uint16 `json:"close_code"`
	Hidden        bool   `json:"hidden"`
	Dropped       uint64 `json:"dropped"`
}

func carPlayConnectionDiagnostics(logger *slog.Logger) http.Handler {
	var mu sync.Mutex
	window, count := time.Now(), 0
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 原始异常、URL、Cookie 和视频内容均不接受；诊断请求不能变成任意日志写入口。
		var event carPlayConnectionEvent
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&event) != nil || !event.valid() || decoder.Decode(new(any)) != io.EOF {
			http.Error(w, "CarPlay 连接诊断字段无效", http.StatusBadRequest)
			return
		}
		mu.Lock()
		if time.Since(window) >= time.Minute {
			window, count = time.Now(), 0
		}
		allowed := count < 120
		if allowed {
			count++
		}
		mu.Unlock()
		if !allowed {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		logger.Info("CarPlay 浏览器连接诊断", "page_id", event.PageID, "seq", event.Sequence,
			"attempt", event.Attempt, "generation", event.Generation, "phase", event.Phase,
			"reason", event.Reason, "elapsed_ms", event.ElapsedMS, "page_elapsed_ms", event.PageElapsedMS, "http_status", event.Status,
			"client_time_ms", event.ClientTimeMS,
			"close_code", event.CloseCode, "hidden", event.Hidden, "dropped", event.Dropped)
		w.WriteHeader(http.StatusNoContent)
	})
}

func (e carPlayConnectionEvent) valid() bool {
	if !carPlayPageID.MatchString(e.PageID) || e.ElapsedMS > 86400000 || e.PageElapsedMS > 86400000 || e.ClientTimeMS > 4102444800000 || e.Status > 599 {
		return false
	}
	switch e.Phase {
	case "probe_start", "probe_result", "probe_error", "ws_start", "ws_open", "ws_error", "ws_close", "timeout", "retry", "paused", "resumed", "codec_ready", "first_packet", "first_frame", "first_frame_wait", "display_result", "display_error", "stopped":
	default:
		return false
	}
	switch e.Reason {
	case "none", "network", "probe_timeout", "ws_timeout", "http", "hidden", "visible", "client_stop", "player_error", "display_error":
		return true
	default:
		return false
	}
}
