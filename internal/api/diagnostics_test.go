package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wen-ryon/teslabox/internal/diagnostics"
)

type fakeDiagnostics struct {
	source string
	limit  int
	bundle diagnostics.Bundle
}

func (d *fakeDiagnostics) Logs(_ context.Context, source string, limit int) (diagnostics.Report, error) {
	d.source = source
	d.limit = limit
	return diagnostics.Report{Source: source, Lines: []string{"kernel: usb gadget configured"}}, nil
}

func (d *fakeDiagnostics) Bundle(context.Context) (diagnostics.Bundle, error) {
	if d.bundle.Filename == "" {
		return diagnostics.Bundle{Filename: "teslabox-diagnostics.txt", Content: []byte("diagnostics")}, nil
	}
	return d.bundle, nil
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

func TestDiagnosticsRouteUsesCompactDefaultLimit(t *testing.T) {
	t.Parallel()
	manager := &fakeDiagnostics{}
	handler := NewHandler(
		fakeController{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDiagnostics(manager),
	)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/logs?source=media", nil))
	if response.Code != http.StatusOK || manager.limit != 50 {
		t.Fatalf("status = %d, limit = %d, body = %s", response.Code, manager.limit, response.Body.String())
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

func TestDiagnosticsBundleIsDownloadedAsPlainText(t *testing.T) {
	t.Parallel()
	manager := &fakeDiagnostics{bundle: diagnostics.Bundle{
		Filename: "teslabox-diagnostics-20260724.txt",
		Content:  []byte("USB configured\nFFmpeg failed\n"),
	}}
	handler := NewHandler(
		fakeController{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDiagnostics(manager),
	)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/diagnostics/report", nil))
	if response.Code != http.StatusOK || response.Body.String() != string(manager.bundle.Content) {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Header().Get("Content-Disposition"), manager.bundle.Filename) {
		t.Fatalf("content disposition = %q", response.Header().Get("Content-Disposition"))
	}
	if !strings.Contains(response.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("content type = %q", response.Header().Get("Content-Type"))
	}
}

func TestClientPlaybackDiagnosticIsValidatedAndLogged(t *testing.T) {
	t.Parallel()
	var output strings.Builder
	handler := NewHandler(
		fakeController{},
		slog.New(slog.NewJSONHandler(&output, nil)),
		WithDiagnostics(&fakeDiagnostics{}),
	)
	body := `{"event":"load_failed","reason":"六路合成加载失败","display":"composite6","quality":"smooth","effective_quality":"smooth","stalls":11,"rtt_ms":300}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/diagnostics/client-event", strings.NewReader(body))
	request.Header.Set("User-Agent", "Mobile Safari")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	logged := output.String()
	for _, expected := range []string{"客户端播放诊断", "load_failed", "composite6", "Mobile Safari"} {
		if !strings.Contains(logged, expected) {
			t.Fatalf("log missing %q: %s", expected, logged)
		}
	}
}
