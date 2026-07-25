package webui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	if bytes.Contains(body, []byte(`value="recent"`)) {
		t.Error("最近录像应独立展示，不应再出现在归档筛选条件中")
	}
	if logsPosition, settingsPosition := bytes.Index(body, []byte(`data-page-link="logs"`)), bytes.Index(body, []byte(`data-page-link="settings"`)); logsPosition < 0 || settingsPosition < 0 || logsPosition > settingsPosition {
		t.Error("侧边导航应将日志中心置于设置中心之前")
	}
	for _, marker := range []string{
		"theme-select", `value="red"`, `data-view="multi"`, `data-period="month"`,
		"session-player-view", "composite-grid", "camera-switcher", "session-timeline",
		"event-marker-layer", "internet-indicator", "temperature-value", "cpu-value",
		"memory-value", "system-disk-value", "nav-toggle", "settings-page", "wifi-scan",
		"ap-form", "storage-device-list", "shutdown-button", "appearance-toggle", "appearance-menu",
		"settings-font-size", "wifi-mac-address", "wifi-ip-address", "ap-mac-address",
		"logout-button", "player-quality", "playback-throughput", "playback-buffer",
		"export-open", "export-progress", "password-change-open", "usb-gadget-disconnect", "usb-gadget-reconnect", "usb-gadget-warm-restart",
		"storage-music-enabled", "storage-teslacam-percent", "storage-confirm-otg-disconnected",
		"password-change-form", "export-layout", "export-camera", "export-quality-help", "update-check", "update-install", "update-notice", "logs-page", "logs-output", "data-log-source=\"usb\"", "logs-radxa-adbd", "logs-radxa-usbnet", "media-page", "media-boombox-upload", "media-wrap-upload", "media-lightshow-upload", "media-music-upload", "media-music-breadcrumb", "media-music-pagination", "media-confirm-otg-disconnected", "wifi-ipv4-modal", "wifi-ipv4-form", "saved-network-toggle", "available-network-toggle", "maintenance-toggle", "logs-auto-refresh", "直接写入", "recent-recordings-section", "recent-video-grid", "app.css?v=20260725-2", "app.js?v=20260725-3",
		"toast-region",
		"自动（当前：高清）", "流畅（480p · 20 FPS）", "标清（720p · 24 FPS）", "高清（720p · 30 FPS）", "<option value=\"original\">原画</option>",
	} {
		if !bytes.Contains(body, []byte(marker)) {
			t.Errorf("页面缺少管理功能标记 %q", marker)
		}
	}
}

func TestHandlerServesProtectedDiagnosticsCenter(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "http://teslabox.local/logs", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "日志中心") {
		t.Fatalf("page status = %d, body = %s", page.Code, page.Body.String())
	}

	script := httptest.NewRecorder()
	handler.ServeHTTP(script, httptest.NewRequest(http.MethodGet, "http://teslabox.local/app.js", nil))
	for _, marker := range []string{"loadLogs", "/api/v1/logs?source=", "downloadDiagnosticReport", "/api/v1/diagnostics/report", "reportPlaybackDiagnostic", "/api/v1/diagnostics/client-event", "playback_stall", "host_detected", "枚举未完成", "enumeration_complete", "bus_suspend_known", "gadget_resume_pending", "等待冷启动", "可能已断开", "主机休眠或 OTG 已拔出", "USB 总线已挂起 · Gadget 仍保持就绪", "设备在线", "/api/v1/gadget/disconnect", "confirm_physical_disconnect", "/api/v1/gadget/reconnect", "confirm_physical_connection", "准备下次冷启动恢复", "/api/v1/gadget/reboot-recover", "温重启恢复", "温重启命令已提交", "setLogsNotice", "showToast", "toast-dismiss", "setUpdateNotice", "scheduleLogsRefresh", "toggleLogsAutoRefresh", "renderGadgetServiceStates", "radxa-adbd.service", "loadMediaAssets", "/api/v1/media-assets", "/api/v1/media-assets/external-delete", "deleteExternalMediaAsset", "renderMusicBrowser", "musicDirectoryEntries", "applyMediaAssets", "openWiFiIPv4Modal", "toggleNetworkPanel", "toggleMaintenanceAutoRecovery", "/api/v1/network/wifi/ipv4", "update_auto_recovery", "renderSavedNetworks(wifi.saved_networks || [], available)", "button.disabled = !state.wifi?.available", "未检测到可用的无线网卡"} {
		if !strings.Contains(script.Body.String(), marker) {
			t.Errorf("日志中心脚本缺少标记 %q", marker)
		}
	}

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "http://teslabox.local/api/v1/logs?source=usb", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}
}

func TestUpdateFailureNeverReconnectsGadgetAutomatically(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://teslabox.local/app.js", nil))
	script := recorder.Body.String()
	start := strings.Index(script, "async function installUpdate()")
	if start < 0 {
		t.Fatal("找不到在线更新函数起点")
	}
	end := strings.Index(script[start:], "async function refreshUpdateStatus()")
	if end < 0 {
		t.Fatal("找不到在线更新函数边界")
	}
	installScript := script[start : start+end]
	if strings.Contains(installScript, "/api/v1/gadget/reconnect") {
		t.Fatal("更新失败路径不得在 OTG 已拔出时自动恢复 Gadget")
	}
	for _, marker := range []string{"setUpdateNotice", "/api/v1/gadget/disconnect", "USB Gadget 会继续保持安全断开"} {
		if !strings.Contains(installScript, marker) {
			t.Errorf("在线更新流程缺少安全标记 %q", marker)
		}
	}
}

func TestHandlerServesCustomExportWorkflow(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	request := httptest.NewRequest(http.MethodGet, "http://teslabox.local/app.js", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	for _, marker := range []string{
		"createExportTask", `request("/api/v1/exports"`, "scheduleExportPoll", "cancelExportTask",
		"start_seconds", "duration_seconds", "releaseVideoElement(sessionVideo)",
		`layout === "single"`, "originalOption.disabled", "layout, cameras",
	} {
		if !strings.Contains(recorder.Body.String(), marker) {
			t.Errorf("自定义导出脚本缺少标记 %q", marker)
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
		"applyCompositeFrameGeometry", "applyServerCompositeFrameGeometry", "compositeLayoutFactor", "composite6:3/2", "composite4a:1",
		"composite4b:3/4", "composite3:2/3", "fitCompositeFrame", "fitFullscreenCompositeGrid", "isPlayerFullscreen",
		"if (!composite || serverComposite) resetCompositeFrameGeometry();",
		`resolvePlaybackQuality(playbackQuality, activeDisplay)`, "isAutomaticPlaybackQuality", "refreshPlaybackQualityControl", "shouldAutomaticallyDowngrade", "isLocalAccessPath",
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
		`sessionVideo.getAttribute("src") !== source`,
	} {
		if !strings.Contains(recorder.Body.String(), marker) {
			t.Errorf("外观设置脚本缺少持久化标记 %q", marker)
		}
	}
}

func TestHandlerServesPlaybackFailureFallback(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	request := httptest.NewRequest(http.MethodGet, "http://teslabox.local/app.js", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	for _, marker := range []string{
		"requestPlaybackFallback", "playbackFallbackLadder", "effectivePlaybackQuality", "usesServerTranscode", "loadServerSingle", "timeline_start_seconds",
		"waitForMetadata(sessionVideo, 15000)", "function waitForMetadata(video, timeout = 4000)",
		"原画不可用，已自动切换到高清合成", "已自动降至", "已自动切换到单路原画", "播放连续卡顿超过 8 秒", `sessionVideo.addEventListener("error"`,
		"refreshRecordings", `request("/api/v1/scan"`, "timelinePositionPending", "eventCameraView", "track-segment continuous",
		"pendingCompositeTracks", "releaseCompositeTrackMap", "token !== loadToken", `element.preload = "metadata"`,
		"原画多路视频加载失败，请重试或切换“自动”画质", "finishSessionVideoLoad",
		"reportCompositeLoadFailures", "showCompositeTrackLoadFailure", "已保留其余",
	} {
		if !strings.Contains(recorder.Body.String(), marker) {
			t.Errorf("播放器缺少失败自动降级标记 %q", marker)
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

	deleteRequest := httptest.NewRequest(http.MethodDelete, "http://teslabox.local/api/v1/exports/task", nil)
	deleteRequest.AddCookie(login.Result().Cookies()[0])
	deleted := httptest.NewRecorder()
	handler.ServeHTTP(deleted, deleteRequest)
	if deleted.Code != http.StatusOK {
		t.Fatalf("DELETE protected API status = %d, body = %s", deleted.Code, deleted.Body.String())
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

func TestHandlerForcesFreshInstallPasswordChange(t *testing.T) {
	authenticator, err := webauth.New(webauth.Config{
		StatePath: filepath.Join(t.TempDir(), "web-auth.json"), SessionTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	apiHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"source":"api"}`)
	})
	handler := NewHandler(apiHandler, authenticator, slog.New(slog.NewTextHandler(io.Discard, nil)))

	loginRequest := httptest.NewRequest(http.MethodPost, "http://teslabox.local/api/v1/auth/login", strings.NewReader(`{"password":"teslabox"}`))
	loginRequest.Header.Set("Origin", "http://teslabox.local")
	loginRequest.RemoteAddr = "192.168.8.2:1234"
	login := httptest.NewRecorder()
	handler.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusOK || !strings.Contains(login.Body.String(), `"must_change_password":true`) {
		t.Fatalf("login status = %d, body = %s", login.Code, login.Body.String())
	}
	oldCookie := login.Result().Cookies()[0]

	blockedRequest := httptest.NewRequest(http.MethodGet, "http://teslabox.local/api/v1/storage", nil)
	blockedRequest.AddCookie(oldCookie)
	blocked := httptest.NewRecorder()
	handler.ServeHTTP(blocked, blockedRequest)
	if blocked.Code != http.StatusPreconditionRequired || !strings.Contains(blocked.Body.String(), `"password_change_required":true`) {
		t.Fatalf("blocked status = %d, body = %s", blocked.Code, blocked.Body.String())
	}

	changeRequest := httptest.NewRequest(http.MethodPost, "http://teslabox.local/api/v1/auth/password", strings.NewReader(`{"current_password":"teslabox","new_password":"new memorable password"}`))
	changeRequest.Header.Set("Origin", "http://teslabox.local")
	changeRequest.RemoteAddr = "192.168.8.2:1234"
	changeRequest.AddCookie(oldCookie)
	changed := httptest.NewRecorder()
	handler.ServeHTTP(changed, changeRequest)
	if changed.Code != http.StatusOK || len(changed.Result().Cookies()) != 1 {
		t.Fatalf("change status = %d, cookies = %#v, body = %s", changed.Code, changed.Result().Cookies(), changed.Body.String())
	}

	storageRequest := httptest.NewRequest(http.MethodGet, "http://teslabox.local/api/v1/storage", nil)
	storageRequest.AddCookie(changed.Result().Cookies()[0])
	storage := httptest.NewRecorder()
	handler.ServeHTTP(storage, storageRequest)
	if storage.Code != http.StatusOK || !strings.Contains(storage.Body.String(), `"source":"api"`) {
		t.Fatalf("storage status = %d, body = %s", storage.Code, storage.Body.String())
	}
}

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	digest := sha256.Sum256([]byte("owner-password"))
	authenticator, err := webauth.New(webauth.Config{
		StatePath:      filepath.Join(t.TempDir(), "web-auth.json"),
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
