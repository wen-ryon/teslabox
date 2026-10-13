package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// playbackTraceEntry 使用独立的诊断身份，不冒充业务会话或尚未实现的快照租约。
type playbackTraceEntry struct {
	SchemaVersion      int                `json:"schema_version"`
	FrontendVersion    string             `json:"frontend_version"`
	OperationID        string             `json:"operation_id"`
	BatchID            string             `json:"batch_id"`
	SessionID          string             `json:"session_id"`
	SnapshotID         string             `json:"snapshot_id"`
	VideoID            string             `json:"video_id"`
	Camera             string             `json:"camera"`
	Display            string             `json:"display"`
	Quality            string             `json:"quality"`
	Transport          string             `json:"transport"`
	MediaPath          string             `json:"media_path"`
	HLSSessionID       string             `json:"hls_session_id"`
	InstanceID         string             `json:"instance_id,omitempty"`
	PreviousInstanceID string             `json:"previous_instance_id,omitempty"`
	SwitchID           string             `json:"switch_id,omitempty"`
	SpanID             string             `json:"span_id,omitempty"`
	HistoryStatus      string             `json:"history_status,omitempty"`
	StateScope         string             `json:"state_scope,omitempty"`
	ProgressState      string             `json:"progress_state,omitempty"`
	EverClockAdvanced  bool               `json:"ever_clock_advanced"`
	LastLoadMS         int64              `json:"last_load_ms,omitempty"`
	LastPlayMS         int64              `json:"last_play_ms,omitempty"`
	LastProgressMS     int64              `json:"last_progress_ms,omitempty"`
	Phase              string             `json:"phase"`
	ReasonCode         string             `json:"reason_code"`
	EventSeq           int64              `json:"event_seq"`
	ClientTimeMS       int64              `json:"client_time_ms"`
	MonotonicMS        int64              `json:"monotonic_ms"`
	ElapsedMS          int64              `json:"elapsed_ms"`
	TargetTimeSeconds  *float64           `json:"target_time_seconds,omitempty"`
	State              playbackTraceState `json:"state"`
}

type playbackTraceState struct {
	CurrentTimeSeconds     float64      `json:"current_time_seconds"`
	ReadyState             int          `json:"ready_state"`
	NetworkState           int          `json:"network_state"`
	Paused                 bool         `json:"paused"`
	Seeking                bool         `json:"seeking"`
	MediaErrorCode         int          `json:"media_error_code"`
	TotalFrames            int          `json:"total_frames"`
	DroppedFrames          int          `json:"dropped_frames"`
	BufferedRanges         [][2]float64 `json:"buffered_ranges"`
	FrameCallbackSupported bool         `json:"frame_callback_supported"`
	FrameCounterSupported  bool         `json:"frame_counter_supported"`
}

func registerPlaybackTraceRoute(mux *http.ServeMux, logger *slog.Logger) {
	var quotaMu sync.Mutex
	var quotaStart time.Time
	var quotaUsed int
	mux.HandleFunc("POST /api/v1/diagnostics/playback-trace", func(w http.ResponseWriter, r *http.Request) {
		var batch struct {
			Events        []playbackTraceEntry `json:"events"`
			DroppedEvents int                  `json:"dropped_events"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&batch); err != nil || len(batch.Events) == 0 || len(batch.Events) > 24 || batch.DroppedEvents < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "播放阶段诊断批次无效"})
			return
		}
		for _, event := range batch.Events {
			if !event.valid() {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "播放阶段诊断字段无效"})
				return
			}
		}
		if decoder.Decode(&struct{}{}) != io.EOF {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "诊断请求必须只有一个 JSON 对象"})
			return
		}
		// 服务端也有总量上限，多个页面不能无限放大磁盘日志量。
		quotaMu.Lock()
		if time.Since(quotaStart) >= time.Minute {
			quotaStart = time.Now()
			quotaUsed = 0
		}
		allowed := quotaUsed+len(batch.Events) <= 720
		if allowed {
			quotaUsed += len(batch.Events)
		}
		quotaMu.Unlock()
		if !allowed {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "播放诊断记录限流"})
			return
		}
		// 每条保持在报告单行上限内，不能把整批 JSON 写成一行再被截断。
		for i, event := range batch.Events {
			dropped := 0
			if i == 0 {
				dropped = min(batch.DroppedEvents, 1000000)
			}
			logger.InfoContext(r.Context(), "客户端播放诊断", "event", "playback_trace", "trace", event,
				"dropped_events", dropped, "user_agent", normalizeDiagnosticText(r.UserAgent(), 240))
		}
		writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
	})
}

func (e playbackTraceEntry) valid() bool {
	if e.StateScope != "" && e.StateScope != "pre_assignment" && e.StateScope != "current_element" {
		return false
	}
	for _, id := range []string{e.InstanceID, e.PreviousInstanceID, e.SwitchID, e.SpanID} {
		if id != "" && !validPlaybackProbeID(id) {
			return false
		}
	}
	if e.LastLoadMS < 0 || e.LastPlayMS < 0 || e.LastProgressMS < 0 {
		return false
	}
	if e.HistoryStatus != "" && e.HistoryStatus != "observed" && e.HistoryStatus != "unknown" {
		return false
	}
	switch e.ProgressState {
	case "", "never_advanced", "advancing", "stationary_after_advance", "paused", "hidden", "seeking", "unknown":
	default:
		return false
	}
	if e.MediaPath != "" && (len(e.MediaPath) > 160 || !strings.HasPrefix(e.MediaPath, "/api/v1/") || strings.ContainsAny(e.MediaPath, "?\r\n#") || strings.Contains(e.MediaPath, "..")) {
		return false
	}
	if e.SchemaVersion != 1 || !validPlaybackProbeID(e.OperationID) || !validPlaybackProbeID(e.BatchID) {
		return false
	}
	if len(e.SnapshotID) > 96 || e.SnapshotID != "" && !validPlaybackProbeToken(strings.ReplaceAll(e.SnapshotID, ".", ""), 96) {
		return false
	}
	if e.HLSSessionID != "" && !validPlaybackProbeID(e.HLSSessionID) {
		return false
	}
	for _, value := range []string{e.FrontendVersion, e.SessionID, e.VideoID, e.Camera, e.Display, e.Quality, e.Transport, e.Phase, e.ReasonCode} {
		if value != "" && !validPlaybackProbeToken(value, 96) {
			return false
		}
	}
	if e.FrontendVersion == "" || e.Phase == "" || e.ReasonCode == "" || e.EventSeq < 1 || e.ClientTimeMS < 0 || e.MonotonicMS < 0 || e.ElapsedMS < 0 || e.ElapsedMS > 86400000 {
		return false
	}
	s := e.State
	if e.TargetTimeSeconds != nil && (*e.TargetTimeSeconds < 0 || *e.TargetTimeSeconds > 86400) {
		return false
	}
	if s.ReadyState < 0 || s.ReadyState > 4 || s.NetworkState < 0 || s.NetworkState > 3 || s.MediaErrorCode < 0 || s.MediaErrorCode > 4 || s.CurrentTimeSeconds < 0 || s.CurrentTimeSeconds > 86400 || s.TotalFrames < 0 || s.DroppedFrames < 0 || len(s.BufferedRanges) > 4 {
		return false
	}
	for _, span := range s.BufferedRanges {
		if span[0] < 0 || span[1] < span[0] || span[1] > 86400 {
			return false
		}
	}
	return true
}
