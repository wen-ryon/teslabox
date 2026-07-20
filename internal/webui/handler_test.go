package webui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/webauth"
)

func TestHandlerServesEmbeddedPageWithSecurityHeaders(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	request := httptest.NewRequest(http.MethodGet, "http://teslabox.local/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	response := recorder.Result()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("status = %d, content-type = %q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	if response.Header.Get("Content-Security-Policy") == "" || response.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("security headers = %#v", response.Header)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("TeslaBox 管理中心")) {
		t.Fatalf("body does not contain page title: %q", body)
	}
	if bytes.Contains(body, []byte(`id="camera"`)) || bytes.Contains(body, []byte(`name="camera"`)) {
		t.Error("页面不应再包含摄像头筛选")
	}
	if bytes.Contains(body, []byte(`value="track"`)) || bytes.Contains(body, []byte(`data-display=`)) {
		t.Error("页面不应显示无数据的赛道筛选或独立播放模式切换")
	}
	for _, marker := range []string{
		"theme-select", `value="red"`, `data-view="multi"`, `data-period="month"`,
		"session-player-view", "composite-grid", "camera-switcher", "session-timeline",
		"event-marker-layer", "internet-indicator", "temperature-value", "cpu-value",
		"memory-value", "system-disk-value", "nav-toggle", "settings-page", "wifi-scan",
		"ap-form", "storage-device-list", "shutdown-button", "appearance-toggle", "appearance-menu",
		"settings-font-size", "wifi-mac-address", "wifi-ip-address", "ap-mac-address",
		"logout-button", "player-quality", "playback-throughput", "playback-buffer",
		"app.js?v=20260720-4",
	} {
		if !bytes.Contains(body, []byte(marker)) {
			t.Errorf("页面缺少管理功能标记 %q", marker)
		}
	}
}

func TestHandlerServesCompositeLayoutGeometry(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	request := httptest.NewRequest(http.MethodGet, "http://teslabox.local/app.css", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	for _, marker := range []string{
		`.composite-grid[data-layout="composite4b"]`, `grid-template-rows: 3fr 1fr`,
		`.composite-grid[data-layout="composite3"]`, `grid-template-rows: 2fr 1fr`, `width: 100vw !important`,
	} {
		if !strings.Contains(recorder.Body.String(), marker) {
			t.Errorf("播放器布局样式缺少标记 %q", marker)
		}
	}

	scriptRequest := httptest.NewRequest(http.MethodGet, "http://teslabox.local/app.js", nil)
	scriptRecorder := httptest.NewRecorder()
	handler.ServeHTTP(scriptRecorder, scriptRequest)
	if scriptRecorder.Code != http.StatusOK {
		t.Fatalf("script status = %d, body = %s", scriptRecorder.Code, scriptRecorder.Body.String())
	}
	for _, marker := range []string{
		"applyCompositeFrameGeometry", "layoutFactors", "composite6:3/2", "composite4a:1",
		"composite4b:3/4", "composite3:2/3", "fitCompositeFrame", "fitFullscreenCompositeGrid", "isPlayerFullscreen",
	} {
		if !strings.Contains(scriptRecorder.Body.String(), marker) {
			t.Errorf("播放器布局脚本缺少标记 %q", marker)
		}
	}
}

func TestHandlerServesPersistentAppearancePreferences(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	request := httptest.NewRequest(http.MethodGet, "http://teslabox.local/app.js", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	for _, marker := range []string{
		"localStorage", "teslabox-theme", "teslabox-settings-font", "teslabox-player-layout",
		"teslabox-playback-quality", "四路合成 A", "四路合成 B", "三路合成",
	} {
		if !strings.Contains(recorder.Body.String(), marker) {
			t.Errorf("外观设置脚本缺少持久化标记 %q", marker)
		}
	}
}

func TestHandlerProtectsAPIAndAcceptsSignedLoginSession(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "http://teslabox.local/api/v1/storage", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	loginRequest := httptest.NewRequest(http.MethodPost, "http://teslabox.local/api/v1/auth/login", strings.NewReader(`{"password":"owner-password"}`))
	loginRequest.Header.Set("Content-Type", "application/json")
	loginRequest.Header.Set("Origin", "http://teslabox.local")
	loginRequest.RemoteAddr = "192.168.8.2:1234"
	login := httptest.NewRecorder()
	handler.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusOK || len(login.Result().Cookies()) != 1 {
		t.Fatalf("login status = %d, cookies = %#v, body = %s", login.Code, login.Result().Cookies(), login.Body.String())
	}

	storageRequest := httptest.NewRequest(http.MethodGet, "http://teslabox.local/api/v1/storage", nil)
	storageRequest.AddCookie(login.Result().Cookies()[0])
	storage := httptest.NewRecorder()
	handler.ServeHTTP(storage, storageRequest)
	if storage.Code != http.StatusOK || !strings.Contains(storage.Body.String(), `"source":"api"`) {
		t.Fatalf("storage status = %d, body = %s", storage.Code, storage.Body.String())
	}
}

func TestHandlerRejectsCrossOriginLogin(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	request := httptest.NewRequest(http.MethodPost, "http://teslabox.local/api/v1/auth/login", strings.NewReader(`{"password":"owner-password"}`))
	request.Header.Set("Origin", "http://attacker.invalid")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	digest := sha256.Sum256([]byte("owner-password"))
	authenticator, err := webauth.New(webauth.Config{
		PasswordSHA256: hex.EncodeToString(digest[:]),
		SessionSecret:  strings.Repeat("ab", 32),
		SessionTTL:     time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	apiHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"source":"api"}`); err != nil {
			t.Errorf("write API response: %v", err)
		}
	})
	return NewHandler(apiHandler, authenticator, slog.New(slog.NewTextHandler(io.Discard, nil)))
}
