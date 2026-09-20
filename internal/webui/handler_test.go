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
	if bytes.Contains(body, []byte("每一段旅程")) || bytes.Contains(body, []byte("每分钟文件自动衔接")) {
		t.Error("首页不应保留旧的营销标题和说明")
	}
	if bytes.Contains(body, []byte("storage-defragment")) || bytes.Contains(body, []byte("离线整理")) {
		t.Error("设置页不应继续暴露已撤下的离线整理入口")
	}
	if logsPosition, settingsPosition := bytes.Index(body, []byte(`data-page-link="logs"`)), bytes.Index(body, []byte(`data-page-link="settings"`)); logsPosition < 0 || settingsPosition < 0 || logsPosition > settingsPosition {
		t.Error("侧边导航应将日志中心置于设置中心之前")
	}
	for _, marker := range []string{
		"theme-select", `value="red"`, `data-view="multi"`, `data-period="month"`,
		"session-player-view", "composite-grid", "camera-switcher", "session-timeline", "play-icon-play", "play-icon-pause",
		"event-marker-layer", "internet-indicator", "temperature-value", "cpu-value",
		"memory-value", "system-disk-value", "nav-toggle", "settings-page", "wifi-scan",
		"ap-form", "ap-interface-role", "ap-active-band", "ap-active-channel", "ap-capability-notice", "ap-channel-detail", "ap-repair-interface", "storage-device-list", "shutdown-button", "appearance-toggle", "appearance-menu",
		"settings-font-size", "wifi-mac-address", "wifi-ip-address", "ap-mac-address", "onboarding-modal", "存储初始化引导", "page-guide",
		"logout-button", "player-quality", "playback-throughput", "playback-buffer", "tesla-browser-compatibility", "player-loading", "update-release-markdown",
		"export-open", "export-progress", "password-change-open", "usb-gadget-disconnect", "usb-gadget-reconnect", "usb-gadget-warm-restart",
		"storage-music-enabled", "storage-teslacam-percent", "storage-confirm-otg-disconnected",
		"status-title", "SYSTEM MONITOR", "TeslaBox 运行状态", "hero-actions", "recording-refresh-interval", "录像自动刷新", `value="0">关`, `value="15">15 秒`, `value="30">30 秒`, `value="60">60 秒`,
		"password-change-form", "export-layout", "export-camera", "export-quality-help", "export-overlay-mode-field", "行车记录仪模式", "画面上方信息栏", "悬浮信息栏", "update-check", "update-install", "update-notice", "logs-page", "logs-output", "logs-summary", "中文诊断摘要", "data-log-source=\"usb\"", "logs-radxa-adbd", "logs-radxa-usbnet", "media-page", "media-boombox-upload", "media-wrap-upload", "media-lightshow-upload", "media-music-breadcrumb", "media-music-pagination", "media-confirm-otg-disconnected", "wifi-ipv4-modal", "wifi-ipv4-form", "saved-network-toggle", "available-network-toggle", "maintenance-toggle", "storage-reclaim", "snapshot-reserve-repair", "snapshot-reserve-cancel", "storage-fragmentation-check", "storage-capacity-migrate", "logs-auto-refresh", "直接写入", "recent-recordings-section", "recent-video-grid", "recording-select-all", "recording-delete-selected", "删除录像", "车机虚拟盘可用", "录像索引", "车机兼容", "teslabox-logo.svg", "login-brand", "app.css?v=20260906-2", "app.js?v=20260906-1",
		"toast-region", "network-loading",
		"自动（当前：高清）", "流畅（480p · 20 FPS）", "标清（720p · 24 FPS）", "高清（720p · 30 FPS）", "高保真合成（多路 H.265 · 1080 高度 · 30 FPS）", "多路原画", "player.html",
	} {
		if !bytes.Contains(body, []byte(marker)) {
			t.Errorf("页面缺少管理功能标记 %q", marker)
		}
	}
}

func TestEmbeddedUIIncludesFanSettings(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"assets/index.html", "assets/app.js", "assets/app.css"} {
		content, err := assets.ReadFile(name)
		if err != nil {
			t.Fatalf("读取 %s: %v", name, err)
		}
		if !bytes.Contains(content, []byte("fan-")) {
			t.Fatalf("%s 缺少风扇设置资源", name)
		}
	}
}

func TestHandlerServesTeslaBoxLogo(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://teslabox.local/teslabox-logo.svg", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "image/svg+xml" {
		t.Fatalf("content-type = %q", contentType)
	}
	if !strings.Contains(recorder.Body.String(), "带有六路蓝色镜头和录制指示灯的行车记录仪") {
		t.Fatal("Logo 资源内容不完整")
	}
}

func TestHandlerServesMarkdownReleaseNotes(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	for _, asset := range []struct {
		path    string
		markers []string
	}{
		{path: "/", markers: []string{`id="update-release-notes"`, "update-release-markdown"}},
		{path: "/app.js", markers: []string{"renderReleaseNotes", "appendReleaseNoteInline", "document.createTextNode", "update-history-notes update-release-markdown"}},
		{path: "/app.css", markers: []string{".update-release-markdown h1", ".update-release-markdown ul", ".update-history-notes"}},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://teslabox.local"+asset.path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", asset.path, recorder.Code, recorder.Body.String())
		}
		for _, marker := range asset.markers {
			if !strings.Contains(recorder.Body.String(), marker) {
				t.Errorf("%s 缺少 Markdown 更新说明标记 %q", asset.path, marker)
			}
		}
	}
}

func TestHandlerKeepsUpdateNotesInDocumentFlow(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://teslabox.local/app.css", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("app.css status = %d", recorder.Code)
	}
	stylesheet := recorder.Body.String()
	for _, marker := range []string{
		"html[data-ui-font] .update-release-markdown h1",
		"html[data-ui-font] .update-release-markdown h2",
	} {
		if !strings.Contains(stylesheet, marker) {
			t.Errorf("app.css 缺少更新说明独立字号标记 %q", marker)
		}
	}
	if strings.Contains(stylesheet, ".update-release-notes { max-height:") {
		t.Fatal("更新说明不应再使用内部滚动高度限制")
	}
}

func TestHandlerKeepsAutomaticRecordingRefreshNonBlocking(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://teslabox.local/app.js", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	script := recorder.Body.String()
	for _, marker := range []string{
		`readPreference(recordingRefreshPreferenceKey, "60")`,
		`return operation();`,
		`if (automatic && recordingInteractionActive()) return`,
		`if (canAutomaticallyRefreshRecordings()) await loadSessions(true, true)`,
	} {
		if !strings.Contains(script, marker) {
			t.Errorf("录像自动刷新缺少非阻塞保护 %q", marker)
		}
	}
	if strings.Contains(script, `silent ? "正在同步录像索引…"`) {
		t.Fatal("后台录像刷新不应再显示阻塞式同步提示")
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
	for _, marker := range []string{"loadLogs", "/api/v1/logs?source=", "report.summaries", "logs-summary-list", "downloadDiagnosticReport", "/api/v1/diagnostics/report", "reportPlaybackDiagnostic", "/api/v1/diagnostics/client-event", "playback_stall", "host_detected", "枚举未完成", "enumeration_complete", "bus_suspend_known", "gadget_resume_pending", "等待冷启动", "可能已断开", "主机休眠或 OTG 已拔出", "USB 总线已挂起 · Gadget 仍保持就绪", "设备在线", "/api/v1/gadget/disconnect", "confirm_physical_disconnect", "/api/v1/gadget/reconnect", "confirm_physical_connection", "准备下次冷启动恢复", "/api/v1/gadget/reboot-recover", "温重启恢复", "温重启命令已提交", "setLogsNotice", "showToast", "toast-dismiss", "setUpdateNotice", "scheduleLogsRefresh", "toggleLogsAutoRefresh", "renderGadgetServiceStates", "radxa-adbd.service", "loadMediaAssets", "/api/v1/media-assets", "/api/v1/media-assets/external-delete", "deleteExternalMediaAsset", "deleteRecordingSession", "handleRecordingDeleteAction", "recordingDeleteMode", "selectableRecordingSessions", "删除此段最近录像", "/api/v1/video-sessions/delete", "session_ids", "confirm_otg_disconnected", "reclaimStorageSpace", "/api/v1/storage/reclaim", "repairSnapshotReserve", "/api/v1/storage/snapshot-reserve/repair", "cancelSnapshotReserveRepair", "/api/v1/storage/snapshot-reserve/cancel", "needs_maintenance_diagnosis", "exFAT 上次采样", "正在读取车机虚拟盘空间", "外层 XFS 可用", "media_volume_space", "renderMusicBrowser", "musicDirectoryEntries", "applyMediaAssets", "openWiFiIPv4Modal", "toggleNetworkPanel", "toggleMaintenanceAutoRecovery", "/api/v1/network/wifi/ipv4", "update_auto_recovery", "renderSavedNetworks(wifi.saved_networks || [], available)", "button.disabled = !state.wifi?.available", "requires_password", "旧配置没有可供后台服务读取的密码", "未检测到可用的无线网卡", "settingsLoadSequence", "startNetworkLoading", "requestResponse", "recordingRefreshPreferenceKey", "normalizeRecordingRefreshSeconds", "scheduleRecordingRefresh", "recordingInteractionActive", "loadSessions(true, true)", "loading: !silent", "maybeShowOnboarding", "/api/v1/onboarding", "dismissed_storage_stable_paths"} {
		if !strings.Contains(script.Body.String(), marker) {
			t.Errorf("日志中心脚本缺少标记 %q", marker)
		}
	}
	for _, marker := range []string{"repairAPInterface", "/api/v1/network/ap/repair", "desired_enabled", "status_message", "allowed_channels", "自动（按网卡能力选择）", "当前受限"} {
		if !strings.Contains(script.Body.String(), marker) {
			t.Errorf("热点状态脚本缺少标记 %q", marker)
		}
	}

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "http://teslabox.local/api/v1/logs?source=usb", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}
}

func TestHandlerServesTeslaBrowserCompatibilitySelectors(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)

	for _, asset := range []struct {
		path    string
		markers []string
	}{
		{
			path: "/",
			markers: []string{
				"车机兼容", `id="tesla-browser-compatibility"`, "保存在当前浏览器",
			},
		},
		{
			path: "/app.js",
			markers: []string{
				"applyTeslaBrowserCompatibility", "upgradeCompatibilitySelect", "renderCompatibilitySelect",
				`document.querySelectorAll("select")`, `select.dispatchEvent(new Event("change"`,
				"compatibilitySelectDocumentObserver", "closeOpenCompatibilitySelect",
			},
		},
		{
			path: "/app.css",
			markers: []string{
				`.compat-select-trigger`, `.compat-select-options`, `.compat-select-option[aria-selected="true"]`,
				`data-tesla-browser-compatibility="true"`,
			},
		},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://teslabox.local"+asset.path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", asset.path, recorder.Code, recorder.Body.String())
		}
		for _, marker := range asset.markers {
			if !strings.Contains(recorder.Body.String(), marker) {
				t.Errorf("%s 缺少车机兼容选择器标记 %q", asset.path, marker)
			}
		}
	}
}

func TestHandlerServesCenteredMobileTelemetryLayout(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://teslabox.local/app.css", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("app.css status = %d", recorder.Code)
	}
	stylesheet := recorder.Body.String()
	for _, marker := range []string{
		"grid-template-columns: repeat(7,68px)",
		"grid-template-columns: repeat(7,minmax(0,1fr))",
		"max-width: 360px",
		".telemetry-accelerator > small { display: flex",
		"contain: inline-size",
		"transform: translateY(1px)",
		"overflow: visible; transform: translateX(-2px)",
	} {
		if !strings.Contains(stylesheet, marker) {
			t.Errorf("app.css 缺少移动端 SEI 对称布局标记 %q", marker)
		}
	}
}

func TestHandlerServesPlayerLoadingOverlay(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	for _, asset := range []struct {
		path    string
		markers []string
	}{
		{path: "/", markers: []string{`id="player-loading"`, `id="player-loading-message"`, "正在加载视频…"}},
		{path: "/app.js", markers: []string{"showPlayerLoading(token)", "finishPlayerLoadingFromMedia", `sessionVideo.addEventListener("loadeddata"`, `sessionVideo.addEventListener("canplay"`}},
		{path: "/app.css", markers: []string{".player-loading {", ".player-loading[hidden]", ".player-loading .loading-spinner"}},
	} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://teslabox.local"+asset.path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body = %s", asset.path, recorder.Code, recorder.Body.String())
		}
		for _, marker := range asset.markers {
			if !strings.Contains(recorder.Body.String(), marker) {
				t.Errorf("%s 缺少播放器加载动画标记 %q", asset.path, marker)
			}
		}
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
		"videoCoversAbsolutePosition(video, selection.absolute)", "duration-.05", "localTime >= -3",
		"start_seconds=${encodeURIComponent(streamStart.toFixed(3))}", "activeStreamTimelineStart+sessionVideo.currentTime",
		`layout === "single"`, "originalOption.disabled", "layout, cameras",
		"downloadSourceFiles", "selectedSourceVideos", "/api/v1/videos/${encodeURIComponent(video.id)}/download",
		"initializeExportRange", "updateExportRangeFromSlider", "localDateTimeInput", "export-range-start", "export-range-end",
		"已立即向浏览器发起", "允许多个文件下载", "RK3568 H.264 编码残块", "H.265（HEVC）MP4",
		"renderExportOverlayOptions", "overlay_mode", "output_bytes", "实际 ${formatBytes(task.output_bytes)}",
		"deleteExportHistoryTask", "/api/v1/exports/history/${encodeURIComponent(task.id)}", "删除后无法再次下载",
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
		`.composite-grid[data-layout="composite3"]`, `grid-template-rows: 2fr 1fr`, `.session-player-view:fullscreen`, `grid-template-rows: auto auto auto`, `align-content: center`, `.session-player-view.player-fullscreen-fallback`, `var(--fullscreen-stage-width,100%)`, `var(--fullscreen-video-height,auto)`, `grid-template-columns: auto minmax(0,1fr) auto auto`, `.unified-controls > .play-toggle`, `.unified-controls > span`, `#current-time`, `#total-time`, `@media (orientation: portrait) and (max-width: 620px)`, `grid-column: 1 / -1; grid-row: 2`, `white-space: nowrap`, `:is(.player-actions,.active-camera-label,.composite-tile-label)`, `data-fullscreen-density="compact"`,
		`#confirm-modal { z-index: 70; }`,
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
		"composite4b:3/4", "composite3:2/3", "fitCompositeFrame", "fitFullscreenCompositeGrid", "fullscreenPlaybackAspect", "isPlayerFullscreen", "sessionPlayerView.requestFullscreen", "activatePlayerFullscreenFallback", "exitPlayerFullscreen", "updateFullscreenButton", "updatePlayerFullscreenDensity", `fullscreenDensity = shortEdge <= 620 ? "compact"`, `isCompositeDisplay(activeDisplay) && !usesServerComposite() ? compositeGrid : sessionVideo`, "不同合成布局的宽高比不同", "return false;",
		"if (!composite || serverComposite) resetCompositeFrameGeometry();",
		`resolvePlaybackQuality(playbackQuality, activeDisplay)`, "isAutomaticPlaybackQuality", "refreshPlaybackQualityControl", "shouldAutomaticallyDowngrade", "isLocalAccessPath",
	} {
		if !strings.Contains(scriptRecorder.Body.String(), marker) {
			t.Errorf("播放器布局脚本缺少标记 %q", marker)
		}
	}
	script := scriptRecorder.Body.String()
	switchStart := strings.Index(script, "async function setDisplayMode(display)")
	if switchStart < 0 {
		t.Fatal("播放器布局切换函数不完整")
	}
	switchEnd := strings.Index(script[switchStart:], "function applyDisplayState()")
	if switchEnd < 0 {
		t.Fatal("播放器布局切换函数不完整")
	}
	switchBody := script[switchStart : switchStart+switchEnd]
	resetPosition := strings.Index(switchBody, "resetCompositeFrameGeometry();")
	assignPosition := strings.Index(switchBody, "activeDisplay = display;")
	if resetPosition < 0 || assignPosition < 0 || resetPosition > assignPosition {
		t.Fatal("切换合成布局必须先清除上一布局尺寸，再更新当前布局")
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

func TestHandlerDefersSEIUntilMediaSeekCompletes(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	request := httptest.NewRequest(http.MethodGet, "http://teslabox.local/app.js", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	body := recorder.Body.String()
	compositeStart := strings.Index(body, "async function loadCompositePosition")
	compositeEnd := strings.Index(body, "async function loadServerComposite")
	if compositeStart < 0 || compositeEnd <= compositeStart {
		t.Fatal("播放器缺少原画多路加载函数")
	}
	composite := body[compositeStart:compositeEnd]
	seeked := strings.Index(composite, "const prepared = await Promise.all(preparations)")
	telemetry := strings.Index(composite, "startPlaybackTelemetryLoad(activeVideo)")
	if seeked < 0 || telemetry <= seeked {
		t.Fatal("SEI 必须在六路媒体完成定位后再加载")
	}

	for _, marker := range []string{
		"function seekVideoElement", `video.addEventListener("seeked", done)`,
		"timelineScrubbing || sessionVideo.seeking || timelinePositionPending", "timelineScrubbing = true",
		"timelineScrubbing = false", "resetPlaybackTelemetryLoad()",
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("播放器缺少安全拖动标记 %q", marker)
		}
	}
}

func TestHandlerRendersSEIAbovePlaybackWithoutCoveringVideo(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)

	assets := []struct {
		name    string
		path    string
		markers []string
	}{
		{
			name: "页面结构",
			path: "/",
			markers: []string{
				`id="playback-telemetry"`, `class="telemetry-main-row"`, `id="playback-gear"`, `class="telemetry-speed-reading"`, `id="playback-speed"`,
				`id="playback-left-blinker"`, `id="playback-right-blinker"`, `id="playback-brake"`,
				`class="telemetry-accelerator-gauge"`, `id="playback-accelerator"`, `class="telemetry-steering-icon"`, `id="playback-steering-wheel"`, `viewBox="48 54 234 234"`, `id="playback-autopilot"`,
			},
		},
		{
			name: "样式",
			path: "/app.css",
			markers: []string{
				`.playback-telemetry { --telemetry-red`, `.telemetry-main-row { display: grid`, `grid-template-columns: minmax(0,1fr) auto minmax(0,1fr)`, `.telemetry-brake[data-active="true"]`,
				`.telemetry-dashboard { display: grid`, `grid-template-columns: repeat(7,68px)`, `border-radius: 27px`, `background: #202020`,
				`.telemetry-dashboard small`, `font-size: 12px; font-weight: 700`, `.telemetry-gear > small, .telemetry-item > small { font-size: 13px; font-weight: 700`, `.telemetry-speed-reading strong { font-size: 30px; font-weight: 800`, `.telemetry-speed-reading small { align-self: auto; margin-top: 4px; font-size: 12px`, `.telemetry-item { box-sizing: border-box; width: 68px; min-width: 68px`, `.telemetry-steering-icon { width: 29px`, `.telemetry-brake-icon`, `.telemetry-accelerator-level`, `.telemetry-accelerator[data-active="true"] .telemetry-accelerator-gauge`, `.telemetry-steering { width: 68px; min-width: 68px; color: #fff`, `.telemetry-steering small strong { display: inline-block; width: 43px`,
				`.telemetry-steering[data-assisted="true"]`, `.telemetry-blinker svg { width: 30px; height: 30px`, `.telemetry-blinker[data-active="true"]`, `animation: telemetry-blinker-pulse 1s`, `.telemetry-assistance[data-mode="fsd"]`, `@media (max-width: 620px)`,
			},
		},
		{
			name: "遥测渲染",
			path: "/app.js",
			markers: []string{
				`function renderPlaybackTelemetrySample`, `normalizeAcceleratorPercent`,
				`Math.round(Math.abs(speedMPS)*3.6)`, `steeringSign`, `Math.abs(steering).toFixed(1)`, `playback-steering-wheel`,
				`metadata?.brake_applied === true`, `metadata?.left_blinker === true`, `metadata?.right_blinker === true`,
				`function playbackAutopilotState`, `mode:"fsd"`, `mode:"steering"`, `mode:"tacc"`,
			},
		},
		{
			name: "图标许可",
			path: "/third-party-notices.txt",
			markers: []string{
				"Lucide Icons", "ISC License", "Phosphor Icons", "MIT License",
			},
		},
	}

	for _, asset := range assets {
		asset := asset
		t.Run(asset.name, func(t *testing.T) {
			t.Parallel()
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://teslabox.local"+asset.path, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			for _, marker := range asset.markers {
				if !strings.Contains(recorder.Body.String(), marker) {
					t.Errorf("%s 缺少 SEI 仪表条标记 %q", asset.name, marker)
				}
			}
		})
	}

	index := httptest.NewRecorder()
	handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "http://teslabox.local/", nil))
	body := index.Body.String()
	telemetry := strings.Index(body, `id="playback-telemetry"`)
	assistance := strings.Index(body, `id="playback-autopilot"`)
	mainRow := strings.Index(body, `class="telemetry-main-row"`)
	video := strings.Index(body, `class="main-video-frame"`)
	if telemetry < 0 || video < 0 || telemetry >= video {
		t.Fatal("SEI 仪表条必须位于视频容器上方，不能覆盖视频画面")
	}
	if mainRow < telemetry || assistance <= mainRow || assistance >= video {
		t.Fatal("辅助驾驶状态必须位于主仪表的时速单元内")
	}
}

func TestHandlerLetsLogOutputUseFullCardWidth(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)

	request := httptest.NewRequest(http.MethodGet, "/app.css", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("读取页面样式状态码 = %d，期望 %d", response.Code, http.StatusOK)
	}
	for _, marker := range []string{
		`.logs-card { width: 100%; min-width: 0;`,
		`.logs-output { display: block; width: 100%; min-width: 0; max-width: 100%;`,
		`white-space: pre-wrap; overflow-wrap: anywhere; word-break: break-all;`,
	} {
		if !strings.Contains(response.Body.String(), marker) {
			t.Errorf("日志正文自适应宽度样式缺少标记 %q", marker)
		}
	}
}

func TestHandlerKeepsSEIInSyncAcrossAutomaticSegmentTransition(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://teslabox.local/app.js", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	for _, marker := range []string{
		"function loadSessionPosition(position, autoplay, preferredMomentIndex = -1)",
		"selectMomentAtPosition(activeSession, position, preferredMomentIndex)",
		"loadSessionPosition(nextPosition, true, next)",
		"telemetryAbortController !== controller", "activeVideo?.id !== videoID",
		"telemetryRequestsByVideo", "function requestPlaybackTelemetry", "function scheduleAdjacentTelemetryPrefetch",
		"for (const index of [momentIndex+1, momentIndex-1])", "scheduleAdjacentTelemetryPrefetch()",
	} {
		if !strings.Contains(recorder.Body.String(), marker) {
			t.Errorf("播放器缺少跨分钟 SEI 同步标记 %q", marker)
		}
	}
}

func TestHandlerShowsEmergencyBrakingEventReasonInChinese(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://teslabox.local/app.js", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	for _, marker := range []string{`triggerType === "vehicle_emergency_braking"`, `reason === "vehicle_auto_emergency_braking"`, "自动紧急制动触发"} {
		if !strings.Contains(recorder.Body.String(), marker) {
			t.Errorf("自动紧急制动触发原因展示缺少标记 %q", marker)
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
