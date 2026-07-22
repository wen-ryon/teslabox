package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wen-ryon/teslabox/internal/diagnostics"
)

type fakeDiagnostics struct {
	source string
	limit  int
}

func (d *fakeDiagnostics) Logs(_ context.Context, source string, limit int) (diagnostics.Report, error) {
	d.source = source
	d.limit = limit
	return diagnostics.Report{Source: source, Lines: []string{"kernel: usb gadget configured"}}, nil
}

func TestDiagnosticsRouteUsesAllowlistedSourceAndBoundedLimit(t *testing.T) {
	t.Parallel()
	manager := &fakeDiagnostics{}
	handler := NewHandler(
		fakeController{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDiagnostics(manager),
	)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/logs?source=usb&lines=9999", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if manager.source != diagnostics.SourceUSB || manager.limit != 500 {
		t.Fatalf("request = source %q, limit %d", manager.source, manager.limit)
	}
	var report diagnostics.Report
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Lines) != 1 {
		t.Fatalf("report = %#v", report)
	}
}

func TestDiagnosticsRouteRejectsUnknownSource(t *testing.T) {
	t.Parallel()
	handler := NewHandler(
		fakeController{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDiagnostics(&fakeDiagnostics{}),
	)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/logs?source=../../etc/passwd", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}
