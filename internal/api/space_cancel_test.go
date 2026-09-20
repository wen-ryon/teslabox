package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wen-ryon/teslabox/internal/service"
)

func TestVideoRejectsCanceledMediaLease(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	w := httptest.NewRecorder()
	serveVideo(w, httptest.NewRequest(http.MethodGet, "/", nil), service.Snapshot{MediaContext: ctx}, false)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d", w.Code)
	}
}
