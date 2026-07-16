package acceptance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMonitorRunPassesWhenHostRangeSnapshotAndVideoGrowthStayHealthy(t *testing.T) {
	t.Parallel()
	storageCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/storage":
			storageCalls++
			snapshotID := "snapshot-1"
			if storageCalls > 1 {
				snapshotID = "snapshot-2"
			}
			writeTestJSON(t, w, map[string]any{
				"phase":          "GADGET_ACTIVE",
				"gadget_status":  map[string]any{"bound": true, "host_connected": true, "current_speed": "high-speed", "udc_state": "configured"},
				"media_snapshot": map[string]any{"id": snapshotID},
				"video_count":    131 + storageCalls,
				"snapshot_space": map[string]any{"ready": true},
			})
		case r.URL.Path == "/api/v1/videos":
			writeTestJSON(t, w, map[string]any{"items": []map[string]any{{"id": "video-1"}}})
		case r.URL.Path == "/api/v1/videos/video-1/content":
			if r.Header.Get("Range") != "bytes=0-1023" {
				t.Errorf("Range = %q", r.Header.Get("Range"))
			}
			w.Header().Set("Content-Range", "bytes 0-1023/4096")
			w.Header().Set("Content-Type", "video/mp4")
			w.WriteHeader(http.StatusPartialContent)
			if _, err := w.Write(make([]byte, 1024)); err != nil {
				t.Errorf("write range response: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	monitor, err := NewMonitor(server.Client(), Options{
		BaseURL: server.URL, Duration: 2 * time.Second, Interval: time.Second,
		RequireHost: true, RequireGrowth: true, RequireSnapshotRotation: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	current := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	monitor.now = func() time.Time { return current }
	monitor.wait = func(_ context.Context, duration time.Duration) error {
		current = current.Add(duration)
		return nil
	}

	report, err := monitor.Run(t.Context(), nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !report.Passed || report.Checks != 3 || report.RangeChecks != 3 || report.InitialVideoCount != 132 || report.MaxVideoCount != 134 || len(report.SnapshotIDs) != 2 {
		t.Fatalf("report = %#v", report)
	}
}

func TestMonitorRunReportsAcceptanceFailuresWithoutStoppingEarly(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/storage":
			writeTestJSON(t, w, map[string]any{
				"phase":          "DEGRADED",
				"gadget_status":  map[string]any{"bound": true, "host_connected": false},
				"media_snapshot": map[string]any{"id": "snapshot-1"},
				"video_count":    10,
				"snapshot_space": map[string]any{"ready": true},
			})
		case r.URL.Path == "/api/v1/videos":
			writeTestJSON(t, w, map[string]any{"items": []map[string]any{{"id": "video-1"}}})
		case r.URL.Path == "/api/v1/videos/video-1/content":
			w.WriteHeader(http.StatusOK)
			if _, err := w.Write([]byte("not partial")); err != nil {
				t.Errorf("write response: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	monitor, err := NewMonitor(server.Client(), Options{
		BaseURL: server.URL, Duration: time.Second, Interval: time.Second,
		RequireHost: true, RequireGrowth: true, RequireSnapshotRotation: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	current := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	monitor.now = func() time.Time { return current }
	monitor.wait = func(_ context.Context, duration time.Duration) error {
		current = current.Add(duration)
		return nil
	}

	report, err := monitor.Run(t.Context(), nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if report.Passed || report.Checks != 2 || len(report.Failures) < 5 {
		t.Fatalf("report = %#v", report)
	}
}

func TestNewMonitorRejectsUnsafeOrBusyLoopConfiguration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		options Options
	}{
		{name: "relative URL", options: Options{BaseURL: "localhost:8080", Duration: time.Minute, Interval: time.Second}},
		{name: "URL with credentials", options: Options{BaseURL: "http://user:pass@localhost", Duration: time.Minute, Interval: time.Second}},
		{name: "zero interval", options: Options{BaseURL: "http://localhost", Duration: time.Minute}},
		{name: "interval exceeds duration", options: Options{BaseURL: "http://localhost", Duration: time.Second, Interval: time.Minute}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewMonitor(http.DefaultClient, test.options); err == nil {
				t.Fatal("NewMonitor() error = nil")
			}
		})
	}
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, payload any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Errorf("encode JSON: %v", err)
	}
}
