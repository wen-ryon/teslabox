package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPlaybackTraceRoute(t *testing.T) {
	t.Parallel()
	base := playbackTraceEntry{SchemaVersion: 1, FrontendVersion: "20260930-4", OperationID: strings.Repeat("a", 32), BatchID: strings.Repeat("b", 32), SnapshotID: "20260930T172522.123456789Z", Phase: "seek", ReasonCode: "positioned", EventSeq: 1}
	for _, tc := range []struct {
		name   string
		mutate func(*playbackTraceEntry)
		count  int
		want   int
	}{
		{"valid", func(*playbackTraceEntry) {}, 1, http.StatusAccepted},
		{"bad_id", func(e *playbackTraceEntry) { e.OperationID = "bad" }, 1, http.StatusBadRequest},
		{"url_secret", func(e *playbackTraceEntry) { e.MediaPath = "/api/v1/videos/a?token=secret" }, 1, http.StatusBadRequest},
		{"bad_state", func(e *playbackTraceEntry) { e.State.ReadyState = 9 }, 1, http.StatusBadRequest},
		{"bad_instance", func(e *playbackTraceEntry) { e.InstanceID = "bad" }, 1, http.StatusBadRequest},
		{"negative_history", func(e *playbackTraceEntry) { e.LastLoadMS = -1 }, 1, http.StatusBadRequest},
		{"false_cold_cache", func(e *playbackTraceEntry) { e.HistoryStatus = "cold" }, 1, http.StatusBadRequest},
		{"diagnostic_v5", func(e *playbackTraceEntry) {
			e.InstanceID = strings.Repeat("c", 32)
			e.SwitchID = strings.Repeat("d", 32)
			e.HistoryStatus = "unknown"
			e.ProgressState = "never_advanced"
		}, 1, http.StatusAccepted},
		{"oversize_batch", func(*playbackTraceEntry) {}, 25, http.StatusBadRequest},
		{"empty_batch", func(*playbackTraceEntry) {}, 0, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output strings.Builder
			mux := http.NewServeMux()
			registerPlaybackTraceRoute(mux, slog.New(slog.NewJSONHandler(&output, nil)))
			e := base
			tc.mutate(&e)
			events := make([]playbackTraceEntry, tc.count)
			for i := range events {
				events[i] = e
			}
			body, _ := json.Marshal(map[string]any{"events": events, "dropped_events": 0})
			r := httptest.NewRecorder()
			mux.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/api/v1/diagnostics/playback-trace", bytes.NewReader(body)))
			if r.Code != tc.want {
				t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
			}
			if tc.want == http.StatusAccepted && (!strings.Contains(output.String(), base.SnapshotID) || !strings.Contains(output.String(), `"reason_code":"positioned"`)) {
				t.Fatalf("missing identity: %s", output.String())
			}
			if tc.want != http.StatusAccepted && output.Len() != 0 {
				t.Fatal("invalid batch was logged")
			}
		})
	}
}

func TestPlaybackTraceQuotaAndLineSize(t *testing.T) {
	var output strings.Builder
	mux := http.NewServeMux()
	registerPlaybackTraceRoute(mux, slog.New(slog.NewJSONHandler(&output, nil)))
	e := playbackTraceEntry{SchemaVersion: 1, FrontendVersion: "20260930-4", OperationID: strings.Repeat("a", 32), BatchID: strings.Repeat("b", 32), Phase: "metadata", ReasonCode: "ready", EventSeq: 1}
	events := make([]playbackTraceEntry, 24)
	for i := range events {
		events[i] = e
	}
	body, _ := json.Marshal(map[string]any{"events": events, "dropped_events": 0})
	for i := 0; i < 31; i++ {
		r := httptest.NewRecorder()
		mux.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/api/v1/diagnostics/playback-trace", bytes.NewReader(body)))
		want := http.StatusAccepted
		if i == 30 {
			want = http.StatusTooManyRequests
		}
		if r.Code != want {
			t.Fatalf("batch %d status %d", i, r.Code)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		if len(line) > 4096 || !json.Valid([]byte(line)) {
			t.Fatal("report line would truncate")
		}
	}
}
