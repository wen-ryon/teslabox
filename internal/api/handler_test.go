package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/service"
	"github.com/wen-ryon/teslabox/internal/teslacam"
)

type fakeController struct {
	state service.Snapshot
}

func (c fakeController) Snapshot() service.Snapshot {
	return c.state
}

func (c fakeController) Refresh(context.Context) error {
	return nil
}

func TestVideoListFiltersAndPaginates(t *testing.T) {
	controller := fakeController{state: service.Snapshot{Videos: []teslacam.Video{
		{ID: "1", ClipType: "recent", Camera: "front", RecordedAt: time.Date(2026, 7, 15, 10, 0, 0, 0, time.Local)},
		{ID: "2", ClipType: "saved", Camera: "front", RecordedAt: time.Date(2026, 7, 15, 9, 0, 0, 0, time.Local)},
		{ID: "3", ClipType: "recent", Camera: "back", RecordedAt: time.Date(2026, 7, 14, 9, 0, 0, 0, time.Local)},
	}}}
	handler := NewHandler(controller, slog.New(slog.NewTextHandler(io.Discard, nil)))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/videos?clip_type=recent&date=2026-07-15&limit=1", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	var payload videoListResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Total != 1 || len(payload.Items) != 1 || payload.Items[0].ID != "1" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestStorageStatusIncludesTeslaUSBPhase(t *testing.T) {
	t.Parallel()
	controller := fakeController{state: service.Snapshot{Phase: service.PhasePreparingBackingStore}}
	handler := NewHandler(controller, slog.New(slog.NewTextHandler(io.Discard, nil)))

	request := httptest.NewRequest(http.MethodGet, "/api/v1/storage", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	var payload service.Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Phase != service.PhasePreparingBackingStore {
		t.Fatalf("Phase = %q", payload.Phase)
	}
}
