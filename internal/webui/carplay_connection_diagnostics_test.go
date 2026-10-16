package webui

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCarPlayConnectionDiagnosticsAuthPrivacyAndBudget(t *testing.T) {
	var logs bytes.Buffer
	auth := &carplayAuth{}
	handler := NewHandler(http.NotFoundHandler(), auth, slog.New(slog.NewJSONHandler(&logs, nil)), WithCarPlay(http.NotFoundHandler()))
	const body = `{"page_id":"page-test123","phase":"ws_close","reason":"none","seq":3,"attempt":1,"generation":2,"elapsed_ms":81,"status":0,"close_code":1006,"hidden":false,"dropped":0}`
	send := func(body, origin string) int {
		r := httptest.NewRequest("POST", "https://carplay.test/api/v1/diagnostics/carplay-connection", strings.NewReader(body))
		r.Header.Set("Origin", origin)
		r.Header.Set("Cookie", "secret-session")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	if send(body, "https://carplay.test") != 401 {
		t.Fatal("未登录允许写诊断日志")
	}
	auth.authenticated = true
	if send(body, "https://other.test") != 403 || logs.Len() != 0 {
		t.Fatal("跨来源诊断写入未阻止")
	}
	if send(strings.Replace(body, `"ws_close"`, `"secret-url"`, 1), "https://carplay.test") != 400 {
		t.Fatal("未知阶段进入日志")
	}
	if send(strings.Replace(body, `"seq":3`, `"url":"secret-url","seq":3`, 1), "https://carplay.test") != 400 {
		t.Fatal("额外字段未拒绝")
	}
	for range 120 {
		if send(body, "https://carplay.test") != 204 {
			t.Fatal("有效事件未接受")
		}
	}
	if send(body, "https://carplay.test") != 429 {
		t.Fatal("服务端日志预算无效")
	}
	if strings.Contains(logs.String(), "secret") || !strings.Contains(logs.String(), `"close_code":1006`) || !strings.Contains(logs.String(), `"page_id":"page-test123"`) {
		t.Fatal("诊断字段不完整或泄露请求内容")
	}
}

func TestCarPlayProxyDiagnosticsDoNotLeakBackendCredentials(t *testing.T) {
	var logs bytes.Buffer
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "private-backend-body")
	}))
	defer backend.Close()
	handler := newCarPlayProxy(carplayTokenFixture(t), strings.TrimPrefix(backend.URL, "http://"), slog.New(slog.NewJSONHandler(&logs, nil)))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "http://carplay.test/carplay/api/status", nil))
	if w.Code != 503 || !strings.Contains(logs.String(), `"phase":"token_window"`) || strings.Contains(logs.String(), "private-backend-body") || strings.Contains(logs.String(), strings.Repeat("a", 64)) {
		t.Fatalf("代理诊断缺失或泄露令牌: %s", logs.String())
	}
}
