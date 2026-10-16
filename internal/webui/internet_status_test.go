package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInternetStatusResourceIsJavaScript(t *testing.T) {
	handler := newTestHandler(t)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://teslabox.local/internet-status.js", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Header().Get("Content-Type"), "javascript") || !strings.Contains(recorder.Body.String(), "TeslaBoxInternetStatus") {
		t.Fatalf("资源没有正确注册: %d %s", recorder.Code, recorder.Header().Get("Content-Type"))
	}
}
