package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wen-ryon/teslabox/internal/diagnostics"
)

type sourceFakeDiagnostics struct {
	fakeDiagnostics
	selected string
}

func (d *sourceFakeDiagnostics) BundleForSource(_ context.Context, source string) (diagnostics.Bundle, error) {
	d.selected = source
	return d.bundle, nil
}

func TestDiagnosticsCategoryDownloadRejectsUnknownAndSelectsCarPlay(t *testing.T) {
	manager := &sourceFakeDiagnostics{fakeDiagnostics: fakeDiagnostics{bundle: diagnostics.Bundle{Filename: "carplay.txt", Content: []byte("BRIDGE_RENDER")}}}
	handler := NewHandler(fakeController{}, slog.New(slog.NewTextHandler(io.Discard, nil)), WithDiagnostics(manager))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/diagnostics/report?source=carplay", nil))
	if response.Code != http.StatusOK || manager.selected != "carplay" || response.Body.String() != "BRIDGE_RENDER" {
		t.Fatalf("分类下载失败: %d %q %q", response.Code, manager.selected, response.Body.String())
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/diagnostics/report?source=../../etc/passwd", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("非法来源: %d", response.Code)
	}
}
