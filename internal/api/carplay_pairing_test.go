package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/carplaypairing"
)

func TestPairingInputRejectsAmbiguousAndOversizedRequests(t *testing.T) {
	for _, body := range []string{`{}`, `{"phone_id":1}`, `{"phone_id":"x","phone_id":"y"}`, `{"phone_id":"x","extra":"y"}`, `{"phone_id":"x"} {}`, `[]`, `{"phone_id":"` + strings.Repeat("x", 512) + `"}`} {
		w := httptest.NewRecorder()
		_, ok := pairingInput(w, httptest.NewRequest("POST", "/", strings.NewReader(body)), "phone_id")
		if ok || w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), body) {
			t.Fatal("请求边界或错误脱敏失效", w.Code)
		}
	}
}

func TestPairingTaskOutlivesHTTPRequestAndCancelWaitsForCleanup(t *testing.T) {
	entered, cleanup := make(chan struct{}), make(chan struct{})
	m, _ := carplaypairing.NewManager(func(ctx context.Context, _ string, _ *carplaypairing.Confirmation) error {
		close(entered)
		<-ctx.Done()
		<-cleanup
		return ctx.Err()
	})
	parent, stop := context.WithCancel(context.Background())
	defer stop()
	mux := http.NewServeMux()
	registerCarPlayPairingRoutes(mux, m, parent)
	rctx, endRequest := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/api/v1/carplay/pairing/start", strings.NewReader(`{"phone_id":"02:11:22:33:44:55","handoff_fingerprint":"","confirm_handoff":"yes"}`)).WithContext(rctx)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatal(w.Code, w.Body.String())
	}
	var task carplaypairing.TaskStatus
	if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	<-entered
	endRequest()
	if !m.Status().Running {
		t.Fatal("HTTP 结束不能取消任务")
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/carplay/pairing/cancel", strings.NewReader(`{"request_id":"`+task.RequestID+`"}`)))
	if w.Code != http.StatusOK || !m.Status().Running {
		t.Fatal("未清理就释放任务", w.Code)
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/carplay/pairing/start", strings.NewReader(`{"phone_id":"02:11:22:33:44:55","handoff_fingerprint":"","confirm_handoff":"yes"}`)))
	if w.Code != http.StatusConflict {
		t.Fatal(w.Code)
	}
	close(cleanup)
	wait, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Wait(wait, task.RequestID); err != nil {
		t.Fatal(err)
	}
	if m.Status().State != "canceled" {
		t.Fatal(m.Status())
	}
}

func TestPairingUnavailableAndStaleTaskCannotConfirm(t *testing.T) {
	mux := http.NewServeMux()
	registerCarPlayPairingRoutes(mux, nil, context.Background())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/carplay/pairing", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"available":false`) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/carplay/pairing/start", strings.NewReader(`{"phone_id":"02:11:22:33:44:55"}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal(w.Code)
	}
	m, _ := carplaypairing.NewManager(func(context.Context, string, *carplaypairing.Confirmation) error { return nil })
	mux = http.NewServeMux()
	registerCarPlayPairingRoutes(mux, m, context.Background())
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/carplay/pairing/confirm", strings.NewReader(`{"request_id":"old","digits":"123456"}`)))
	if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "123456") {
		t.Fatal(w.Code, w.Body.String())
	}
}
