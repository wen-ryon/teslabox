"use strict";

const cameraOrder = ["left_pillar", "front", "right_pillar", "left_repeater", "back", "right_repeater"];
const compositeLayouts = {
  composite6: {label:"六路合成", cameras:cameraOrder},
  composite4a: {label:"四路合成 A", cameras:["front", "back", "left_repeater", "right_repeater"]},
  composite4b: {label:"四路合成 B", cameras:["front", "right_repeater", "back", "left_repeater"]},
  composite3: {label:"三路合成", cameras:["front", "right_repeater", "left_repeater"]},
};
const musicPageSize = 24;
const maximumExportDurationSeconds = 120;
const savedView = readPreference("teslabox-view", "multi");
const savedPeriod = readPreference("teslabox-period", "month");
const savedUIFont = normalizeUIFont(readPreference("teslabox-settings-font", "medium"));
const savedPlaybackQuality = normalizePlaybackQuality(readPreference("teslabox-playback-quality", "auto"));
const initialPage = window.location.pathname === "/media" ? "media" : window.location.pathname === "/settings" ? "settings" : window.location.pathname === "/logs" ? "logs" : "dashboard";
const state = {
  offset: 0, total: 0, loading: false, refreshing: false, authEnabled: true,
  view: savedView === "single" ? "single" : "multi",
  period: ["month", "week", "day"].includes(savedPeriod) ? savedPeriod : "month",
  page: initialPage, mustChangePassword: false,
  wifi: null, ap: null, storage: null, update: null, updateHistory: null, maintenance: null, mediaAssets: null, uiFont: savedUIFont,
  musicPath: "", musicPage: 0,
};
const $ = (id) => document.getElementById(id);

const loginView = $("login-view");
const appView = $("app-view");
const loginForm = $("login-form");
const loginError = $("login-error");
const timelineList = $("video-grid");
const notice = $("notice");
const toastRegion = $("toast-region");
const playerModal = $("player-modal");
const mainVideoFrame = document.querySelector(".main-video-frame");
const compositeGrid = $("composite-grid");
const sessionVideo = $("session-video");
let activeSession = null;
let activeCamera = "front";
let activeVideo = null;
let activeMomentIndex = -1;
let requestedPosition = 0;
let loadToken = 0;
let activeDisplay = normalizeDisplayMode(readPreference("teslabox-player-layout", "composite6"));
let recentSessions = [];
let archivedSessions = [];
const selectedRecordingSessions = new Set();
let recordingDeleteInProgress = false;
let recordingDeleteMode = false;
let compositeTracks = new Map();
let pendingCompositeTracks = new Map();
let lastCompositeSync = 0;
let pendingWiFi = null;
let pendingWiFiIPv4Network = null;
let pendingStorage = null;
let confirmResolver = null;
let playbackQuality = savedPlaybackQuality;
let effectivePlaybackQuality = resolvePlaybackQuality(playbackQuality, activeDisplay);
let playbackFallbackInProgress = false;
let playbackWasPlaying = false;
let playbackDiagnosticsTimer = 0;
let timelinePositionRequestID = 0;
let timelinePositionPending = false;
let playbackStalls = 0;
let isPlaybackBuffering = false;
let lastPlaybackProgressAt = 0;
let lastPlaybackTime = 0;
let mediaThroughputSamples = [];
let activeCompositeAspect = 0;
let exportTask = null;
let exportPollTimer = 0;
let exportMode = "clip";
let sourceExportSessionID = "";
const sourceExportMomentIDs = new Set();
const sourceExportCameras = new Set();
const telemetryByVideo = new Map();
let activeTelemetry = null;
let telemetryAbortController = null;
let passwordChangeForced = false;
let updatePollTimer = 0;
let logsRefreshTimer = 0;
let activeLogSource = "usb";
const observedMediaEntries = new Set();
const playbackDiagnosticLastSent = new Map();
let pendingRequestControl = null;
let pendingRequestControlTimer = 0;
let loadingSequence = 0;
const networkLoadingTasks = new Map();
const pageLoadingTasks = new Map();
const requestLoadingControls = new Map();
let networkLoadingTimer = 0;

applyTheme(normalizeTheme(readPreference("teslabox-theme", "blue")));
applyUIFont(state.uiFont);
applyViewState();
showPage(state.page, false);

function rememberRequestControl(control) {
  if (!(control instanceof HTMLButtonElement) || control.disabled) return;
  pendingRequestControl = control;
  window.clearTimeout(pendingRequestControlTimer);
  pendingRequestControlTimer = window.setTimeout(() => {
    if (pendingRequestControl === control) pendingRequestControl = null;
  }, 0);
}

function takeRequestControl() {
  const control = pendingRequestControl;
  pendingRequestControl = null;
  window.clearTimeout(pendingRequestControlTimer);
  return control instanceof HTMLButtonElement && control.isConnected ? control : null;
}

function setRequestControlLoading(control) {
  if (!control) return () => {};
  const current = requestLoadingControls.get(control);
  if (current) {
    current.count++;
    return () => clearRequestControlLoading(control);
  }
  requestLoadingControls.set(control, {
    count: 1,
    disabled: control.disabled,
    ariaBusy: control.getAttribute("aria-busy"),
    loadingClass: control.classList.contains("is-loading"),
  });
  control.disabled = true;
  control.classList.add("is-loading");
  control.setAttribute("aria-busy", "true");
  return () => clearRequestControlLoading(control);
}

function clearRequestControlLoading(control) {
  const current = requestLoadingControls.get(control);
  if (!current) return;
  current.count--;
  if (current.count > 0) return;
  requestLoadingControls.delete(control);
  if (!current.loadingClass) control.classList.remove("is-loading");
  if (current.ariaBusy === null) control.removeAttribute("aria-busy");
  else control.setAttribute("aria-busy", current.ariaBusy);
  if (!current.disabled) control.disabled = false;
}

function latestLoadingMessage(tasks, fallback) {
  const messages = [...tasks.values()];
  return messages.length ? messages[messages.length-1] : fallback;
}

function renderNetworkLoading() {
  const root = $("network-loading");
  if (networkLoadingTasks.size === 0) {
    window.clearTimeout(networkLoadingTimer);
    networkLoadingTimer = 0;
    root.hidden = true;
    return;
  }
  $("network-loading-message").textContent = latestLoadingMessage(networkLoadingTasks, "正在与 TeslaBox 通信…");
  if (networkLoadingTimer || !root.hidden) return;
  networkLoadingTimer = window.setTimeout(() => {
    networkLoadingTimer = 0;
    if (networkLoadingTasks.size > 0) root.hidden = false;
  }, 250);
}

function startNetworkLoading(message) {
  const token = ++loadingSequence;
  const releaseControl = setRequestControlLoading(takeRequestControl());
  networkLoadingTasks.set(token, message || "正在与 TeslaBox 通信…");
  renderNetworkLoading();
  return () => {
    networkLoadingTasks.delete(token);
    releaseControl();
    renderNetworkLoading();
  };
}

function renderPageLoading() {
  const root = $("page-loading");
  const loading = pageLoadingTasks.size > 0 && !appView.hidden;
  root.hidden = !loading;
  appView.toggleAttribute("aria-busy", loading);
  if (loading) $("page-loading-message").textContent = latestLoadingMessage(pageLoadingTasks, "正在读取设备状态…");
}

function startPageLoading(message) {
  const token = ++loadingSequence;
  pageLoadingTasks.set(token, message || "正在读取设备状态…");
  renderPageLoading();
  return () => {
    pageLoadingTasks.delete(token);
    renderPageLoading();
  };
}

async function withPageLoading(message, operation) {
  const finish = startPageLoading(message);
  try {
    return await operation();
  } finally {
    finish();
  }
}

async function requestResponse(path, options = {}) {
  const {loading = true, loadingMessage, ...fetchOptions} = options;
  const finish = loading ? startNetworkLoading(loadingMessage) : () => {};
  try {
    const response = await fetch(path, { credentials: "same-origin", ...fetchOptions });
    if (response.status === 401) {
      showLogin();
      throw new Error("登录状态已失效");
    }
    return response;
  } finally {
    finish();
  }
}

async function request(path, options = {}) {
  const response = await requestResponse(path, options);
  if (!response.ok) {
    let message = `请求失败（HTTP ${response.status}）`;
    try { message = (await response.json()).error || message; } catch (_) { /* 保留通用提示 */ }
    throw new Error(message);
  }
  if (response.status === 204) return null;
  return response.json();
}

document.addEventListener("click", (event) => {
  if (!(event.target instanceof Element)) return;
  rememberRequestControl(event.target.closest("button"));
}, true);
document.addEventListener("submit", (event) => rememberRequestControl(event.submitter));

function showLogin() {
  appView.hidden = true;
  loginView.hidden = false;
  window.setTimeout(() => $("password").focus(), 0);
}

function showApp() {
  loginView.hidden = true;
  appView.hidden = false;
  $("logout-button").hidden = !state.authEnabled;
}

async function initialize() {
  try {
    const session = await requestResponse("/api/v1/auth/session", {loading:false}).then((response) => response.json());
    state.authEnabled = session.enabled;
    state.mustChangePassword = Boolean(session.must_change_password);
    if (session.enabled && !session.authenticated) {
      showLogin();
      return;
    }
    showApp();
    if (state.mustChangePassword) {
      openPasswordModal(true);
      return;
    }
    await refreshAll();
    if (state.page === "media") await loadMediaAssets();
    if (state.page === "settings") await loadSettings();
    if (state.page === "logs") await loadLogs();
  } catch (error) {
    showApp();
    setNotice(`无法连接 TeslaBox：${error.message}`, true);
  }
}

loginForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  loginError.textContent = "";
  const button = loginForm.querySelector("button");
  button.disabled = true;
  try {
    const response = await requestResponse("/api/v1/auth/login", {
      method: "POST", credentials: "same-origin", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ password: $("password").value }),
      loadingMessage:"正在验证登录…",
    });
    const payload = await response.json();
    if (!response.ok) throw new Error(payload.error || "登录失败");
    $("password").value = "";
    state.mustChangePassword = Boolean(payload.must_change_password);
    showApp();
    if (state.mustChangePassword) {
      openPasswordModal(true);
      return;
    }
    await refreshAll();
  } catch (error) {
    loginError.textContent = error.message;
  } finally {
    button.disabled = false;
  }
});

$("logout-button").addEventListener("click", async () => {
  await requestResponse("/api/v1/auth/logout", { method: "POST", loadingMessage:"正在退出管理页面…" });
  closeAppearanceMenu();
  closePasswordModal(true);
  closePlayer();
  showLogin();
});
$("theme-select").addEventListener("change", (event) => applyTheme(event.target.value));
$("appearance-toggle").addEventListener("click", (event) => {
  event.stopPropagation();
  toggleAppearanceMenu();
});
$("appearance-menu").addEventListener("click", (event) => event.stopPropagation());
document.addEventListener("click", closeAppearanceMenu);
$("nav-toggle").addEventListener("click", openNavigation);
$("nav-close").addEventListener("click", closeNavigation);
$("nav-backdrop").addEventListener("click", closeNavigation);
document.querySelectorAll("[data-page-link]").forEach((element) => element.addEventListener("click", (event) => {
  event.preventDefault();
  showPage(element.dataset.pageLink, true);
}));
$("refresh-button").addEventListener("click", refreshRecordings);
$("settings-refresh").addEventListener("click", loadSettings);
$("logs-refresh").addEventListener("click", loadLogs);
$("logs-lines").addEventListener("change", loadLogs);
$("logs-auto-refresh").addEventListener("click", toggleLogsAutoRefresh);
$("logs-copy").addEventListener("click", copyLogs);
$("logs-download-report").addEventListener("click", downloadDiagnosticReport);
$("media-refresh").addEventListener("click", loadMediaAssets);
$("media-boombox-upload").addEventListener("click", () => uploadMediaFiles("boombox"));
$("media-wrap-upload").addEventListener("click", () => uploadMediaFiles("wrap"));
$("media-lightshow-upload").addEventListener("click", () => uploadMediaFiles("lightshow"));
$("media-music-upload").addEventListener("click", () => uploadMediaFiles("music"));
$("media-apply").addEventListener("click", applyMediaAssets);
$("media-music-back").addEventListener("click", () => {
  if (!state.musicPath) return;
  const parts = state.musicPath.split("/");
  parts.pop();
  state.musicPath = parts.join("/");
  state.musicPage = 0;
  renderMusicBrowser(musicAssets());
});
$("media-music-prev").addEventListener("click", () => {
  if (state.musicPage <= 0) return;
  state.musicPage -= 1;
  renderMusicBrowser(musicAssets());
});
$("media-music-next").addEventListener("click", () => {
  state.musicPage += 1;
  renderMusicBrowser(musicAssets());
});
$("media-music-breadcrumb").addEventListener("click", (event) => {
  if (!(event.target instanceof Element)) return;
  const target = event.target.closest("button[data-music-path]");
  if (!target) return;
  state.musicPath = target.dataset.musicPath || "";
  state.musicPage = 0;
  renderMusicBrowser(musicAssets());
});
$("usb-gadget-disconnect").addEventListener("click", disconnectGadget);
$("usb-gadget-reconnect").addEventListener("click", reconnectGadget);
$("usb-gadget-warm-restart").addEventListener("click", warmRestartGadget);
document.querySelectorAll("[data-log-source]").forEach((button) => button.addEventListener("click", () => {
  activeLogSource = button.dataset.logSource;
  document.querySelectorAll("[data-log-source]").forEach((item) => item.classList.toggle("active", item === button));
  loadLogs();
}));
$("settings-font-size").addEventListener("change", (event) => applyUIFont(event.target.value));
$("wifi-scan").addEventListener("click", scanWiFi);
$("saved-network-toggle").addEventListener("click", () => toggleNetworkPanel("saved-network-panel", "saved-network-toggle"));
$("available-network-toggle").addEventListener("click", () => toggleNetworkPanel("available-network-panel", "available-network-toggle"));
$("ap-band").addEventListener("change", () => populateAPChannels($("ap-band").value, 0));
$("ap-form").addEventListener("submit", saveAPSettings);
$("export-settings-save").addEventListener("click", saveExportSettings);
$("export-history-refresh").addEventListener("click", loadExportSettings);
$("export-delete-after-download").addEventListener("change", renderExportRetentionControls);
$("wifi-connect-form").addEventListener("submit", connectNewWiFi);
$("wifi-ipv4-form").addEventListener("submit", saveWiFiIPv4Settings);
$("wifi-ipv4-mode").addEventListener("change", renderWiFiIPv4Fields);
$("storage-refresh").addEventListener("click", loadStorageDevices);
$("storage-init-form").addEventListener("submit", initializeStorageDevice);
$("storage-music-enabled").addEventListener("change", renderStoragePartitionPreview);
$("storage-teslacam-percent").addEventListener("input", renderStoragePartitionPreview);
$("maintenance-auto-recovery").addEventListener("click", toggleMaintenanceAutoRecovery);
$("storage-reclaim").addEventListener("click", reclaimStorageSpace);
$("password-change-open").addEventListener("click", () => openPasswordModal(false));
$("password-change-form").addEventListener("submit", changeManagementPassword);
document.querySelectorAll("[data-close-password]").forEach((element) => element.addEventListener("click", () => closePasswordModal(false)));
document.querySelectorAll("[data-close-wifi]").forEach((element) => element.addEventListener("click", closeWiFiModal));
document.querySelectorAll("[data-close-wifi-ipv4]").forEach((element) => element.addEventListener("click", closeWiFiIPv4Modal));
document.querySelectorAll("[data-close-storage]").forEach((element) => element.addEventListener("click", closeStorageModal));
document.querySelectorAll("[data-close-confirm]").forEach((element) => element.addEventListener("click", () => resolveConfirmation(false)));
$("confirm-submit").addEventListener("click", () => resolveConfirmation(true));
$("reboot-button").addEventListener("click", () => requestPower("reboot"));
$("shutdown-button").addEventListener("click", () => requestPower("shutdown"));
$("update-check").addEventListener("click", checkUpdate);
$("update-history-load").addEventListener("click", loadUpdateHistory);
$("update-download").addEventListener("click", downloadUpdate);
$("update-install").addEventListener("click", installUpdate);
$("filter-form").addEventListener("submit", (event) => { event.preventDefault(); loadSessions(false); });
$("recording-select-all").addEventListener("click", toggleAllRecordingSelections);
$("recording-delete-selected").addEventListener("click", handleRecordingDeleteAction);
$("play-toggle").addEventListener("click", togglePlayback);
$("session-timeline").addEventListener("input", previewTimelinePosition);
$("session-timeline").addEventListener("change", seekTimelinePosition);
$("fullscreen-button").addEventListener("click", enterFullscreen);
$("export-open").addEventListener("click", openExportModal);
$("export-form").addEventListener("submit", createExportTask);
$("export-cancel").addEventListener("click", cancelExportTask);
$("export-layout").addEventListener("change", renderExportOptions);
$("export-mode-clip").addEventListener("click", () => setExportMode("clip"));
$("export-mode-source").addEventListener("click", () => setExportMode("source"));
$("export-source-current").addEventListener("click", selectCurrentSourceMoment);
$("export-source-all").addEventListener("click", selectAllSourceMoments);
$("export-source-clear").addEventListener("click", clearSourceMomentSelection);
$("export-source-cameras-all").addEventListener("click", selectAllSourceCameras);
$("export-source-cameras-layout").addEventListener("click", selectSourceCamerasForCurrentLayout);
$("export-source-direct").addEventListener("click", downloadSourceFiles);
document.querySelectorAll("[data-close-export]").forEach((element) => element.addEventListener("click", closeExportModal));
refreshPlaybackQualityControl();
$("player-quality").addEventListener("change", (event) => setPlaybackQuality(event.target.value));
document.querySelectorAll("[data-view]").forEach((button) => button.addEventListener("click", () => setView(button.dataset.view)));
document.querySelectorAll("[data-period]").forEach((button) => button.addEventListener("click", () => setPeriod(button.dataset.period)));
document.querySelectorAll("[data-close-player]").forEach((element) => element.addEventListener("click", closePlayer));
document.addEventListener("keydown", (event) => {
  if (event.key !== "Escape") return;
  if (!$("confirm-modal").hidden) resolveConfirmation(false);
  else if (!$("password-modal").hidden) closePasswordModal(false);
  else if (!$("export-modal").hidden) closeExportModal();
  else if (!$("wifi-modal").hidden) closeWiFiModal();
  else if (!$("appearance-menu").hidden) closeAppearanceMenu();
  else if (!$("nav-drawer").hidden) closeNavigation();
  else if (!playerModal.hidden) closePlayer();
});
sessionVideo.addEventListener("timeupdate", updateUnifiedTimeline);
sessionVideo.addEventListener("ended", advanceToNextSegment);
sessionVideo.addEventListener("play", () => {
  playbackWasPlaying = true;
  lastPlaybackProgressAt = Date.now();
  lastPlaybackTime = sessionVideo.currentTime;
  updatePlayButton();
});
sessionVideo.addEventListener("pause", updatePlayButton);
sessionVideo.addEventListener("waiting", markPlaybackStall);
sessionVideo.addEventListener("playing", () => { isPlaybackBuffering = false; });
sessionVideo.addEventListener("error", handlePlaybackError);
navigator.connection?.addEventListener?.("change", updatePlaybackDiagnostics);
window.addEventListener("resize", handlePlayerViewportChange);
document.addEventListener("fullscreenchange", handlePlayerViewportChange);
document.addEventListener("webkitfullscreenchange", handlePlayerViewportChange);

async function refreshAll() {
  await withPageLoading("正在同步设备状态与录像索引…", async () => {
    await Promise.allSettled([loadStatus(), loadSessions(false)]);
  });
}

async function refreshRecordings() {
	if (state.refreshing) return;
	state.refreshing = true;
	const button = $("refresh-button");
	const label = button.textContent;
	button.disabled = true;
	button.classList.add("is-loading");
	button.setAttribute("aria-busy", "true");
	button.textContent = "正在刷新…";
  try {
    setNotice("正在读取车机刚写入的录像…", false, false);
    await request("/api/v1/scan", {method:"POST"});
    await refreshAll();
  } catch (error) {
    setNotice(`刷新录像失败：${error.message}`, true);
	} finally {
		button.disabled = false;
		button.classList.remove("is-loading");
		button.removeAttribute("aria-busy");
		button.textContent = label;
		state.refreshing = false;
  }
}

function openNavigation() {
  closeAppearanceMenu();
  $("nav-drawer").hidden = false;
  $("nav-backdrop").hidden = false;
  $("nav-toggle").setAttribute("aria-expanded", "true");
}

function closeNavigation() {
  $("nav-drawer").hidden = true;
  $("nav-backdrop").hidden = true;
  $("nav-toggle").setAttribute("aria-expanded", "false");
}

function openPasswordModal(forced) {
  passwordChangeForced = Boolean(forced);
  $("password-modal").hidden = false;
  $("password-modal-title").textContent = forced ? "请先修改默认密码" : "修改管理密码";
  $("password-modal-detail").textContent = forced
    ? "当前仍在使用首次启动默认密码 teslabox。修改成功前，设备管理和录像接口不会开放。"
    : "新密码保存后，其他浏览器中的旧登录会话会立即失效。";
  $("password-change-close").hidden = passwordChangeForced;
  $("password-change-error").textContent = "";
  $("current-password").value = "";
  $("new-password").value = "";
  $("confirm-new-password").value = "";
  window.setTimeout(() => $("current-password").focus(), 0);
}

function closePasswordModal(force) {
  if (passwordChangeForced && !force) return;
  $("password-modal").hidden = true;
  passwordChangeForced = false;
}

async function changeManagementPassword(event) {
  event.preventDefault();
  const currentPassword = $("current-password").value;
  const newPassword = $("new-password").value;
  const confirmation = $("confirm-new-password").value;
  const errorElement = $("password-change-error");
  errorElement.textContent = "";
  if (newPassword !== confirmation) {
    errorElement.textContent = "两次输入的新密码不一致";
    return;
  }
  if (newPassword.length < 8 || new TextEncoder().encode(newPassword).length > 128) {
    errorElement.textContent = "新密码必须为 8 到 128 个字节";
    return;
  }
  const button = $("password-change-submit");
  button.disabled = true;
  try {
    await request("/api/v1/auth/password", {
      method:"POST", headers:{"Content-Type":"application/json"},
      body:JSON.stringify({current_password:currentPassword, new_password:newPassword}),
    });
    state.mustChangePassword = false;
    $("password-status").textContent = "密码已更新";
    closePasswordModal(true);
    await refreshAll();
    if (state.page === "settings") await loadSettings();
    if (state.page === "logs") await loadLogs();
  } catch (error) {
    errorElement.textContent = error.message;
  } finally {
    button.disabled = false;
  }
}

function toggleAppearanceMenu() {
  const shouldOpen = $("appearance-menu").hidden;
  closeNavigation();
  $("appearance-menu").hidden = !shouldOpen;
  $("appearance-toggle").setAttribute("aria-expanded", String(shouldOpen));
}

function closeAppearanceMenu() {
  $("appearance-menu").hidden = true;
  $("appearance-toggle").setAttribute("aria-expanded", "false");
}

function showPage(page, updateHistory) {
  state.page = ["media", "settings", "logs"].includes(page) ? page : "dashboard";
  $("dashboard-page").hidden = state.page !== "dashboard";
  $("media-page").hidden = state.page !== "media";
  $("settings-page").hidden = state.page !== "settings";
  $("logs-page").hidden = state.page !== "logs";
  document.querySelectorAll("[data-page-link]").forEach((element) => element.classList.toggle("active", element.dataset.pageLink === state.page));
  closeNavigation();
  closeAppearanceMenu();
  const pagePath = state.page === "media" ? "/media" : state.page === "settings" ? "/settings" : state.page === "logs" ? "/logs" : "/";
  if (updateHistory) window.history.pushState({page:state.page}, "", pagePath);
  if (state.page === "media" && !appView.hidden) loadMediaAssets();
  if (state.page === "settings" && !appView.hidden) loadSettings();
  if (state.page === "logs" && !appView.hidden) loadLogs();
  scheduleLogsRefresh();
  window.scrollTo({top:0, behavior:"smooth"});
}

window.addEventListener("popstate", () => {
  const page = window.location.pathname === "/media" ? "media" : window.location.pathname === "/settings" ? "settings" : window.location.pathname === "/logs" ? "logs" : "dashboard";
  showPage(page, false);
});

async function loadLogs({background = false} = {}) {
  if (state.page !== "logs" || appView.hidden) return;
  const load = async () => {
    $("logs-refresh").disabled = true;
    setLogsNotice("", false);
    try {
      const lineCount = Number($("logs-lines").value) || 200;
      const [report, storage] = await Promise.all([
        request(`/api/v1/logs?source=${encodeURIComponent(activeLogSource)}&lines=${lineCount}`, {loading:!background}),
        request("/api/v1/storage", {loading:!background}),
      ]);
      renderLogDiagnostics(storage);
      renderLogs(report);
    } catch (error) {
      setLogsNotice(error.message, true);
    } finally {
      $("logs-refresh").disabled = false;
      scheduleLogsRefresh();
    }
  };
  if (background) return load();
  return withPageLoading("正在读取日志与 USB Gadget 状态…", load);
}

async function downloadDiagnosticReport() {
  const button = $("logs-download-report");
  const originalLabel = button.textContent;
  button.disabled = true;
  button.textContent = "正在收集…";
  try {
    const response = await requestResponse("/api/v1/diagnostics/report", {cache:"no-store", loadingMessage:"正在生成完整诊断报告…"});
    if (!response.ok) {
      let message = `生成诊断报告失败（HTTP ${response.status}）`;
      try { message = (await response.json()).error || message; } catch (_) { /* 保留通用提示 */ }
      throw new Error(message);
    }
    const disposition = response.headers.get("Content-Disposition") || "";
    const filenameMatch = disposition.match(/filename="?([^";]+)"?/i);
    const filename = filenameMatch?.[1] || `teslabox-diagnostics-${Date.now()}.txt`;
    const url = URL.createObjectURL(await response.blob());
    const link = document.createElement("a");
    link.href = url;
    link.download = filename;
    document.body.append(link);
    link.click();
    link.remove();
    window.setTimeout(() => URL.revokeObjectURL(url), 1000);
    showToast("完整诊断报告已下载，可以直接发给维护者。", "success");
  } catch (error) {
    showToast(error.message, "error");
  } finally {
    button.disabled = false;
    button.textContent = originalLabel;
  }
}

async function loadMediaAssets() {
  if (state.page !== "media" || appView.hidden) return;
  return withPageLoading("正在读取媒体中心内容…", async () => {
    $("media-refresh").disabled = true;
    setMediaNotice("", false);
    try {
      const payload = await request("/api/v1/media-assets");
      state.mediaAssets = payload;
      renderMediaAssets(payload);
    } catch (error) {
      setMediaNotice(error.message, true);
      renderMediaUnavailable(error.message);
    } finally {
      $("media-refresh").disabled = false;
    }
  });
}

function renderMediaAssets(payload) {
  const assets = Array.isArray(payload.assets) ? payload.assets : [];
  const categories = ["boombox", "wrap", "lightshow"];
  categories.forEach((category) => renderMediaCategory(category, assets.filter((asset) => asset.category === category)));
  renderMusicBrowser(assets.filter((asset) => asset.category === "music"));
  const status = $("media-apply-status");
  status.className = `badge${payload.needs_apply ? " error" : " ok"}`;
  status.textContent = payload.needs_apply ? "有待应用变更" : "已同步";
  $("media-apply").disabled = !payload.needs_apply || !payload.image_available;
  if (!payload.image_available) {
    $("media-apply-detail").textContent = payload.message || "TeslaCam 虚拟盘尚未初始化。";
  } else if (payload.needs_apply) {
    $("media-apply-detail").textContent = `${payload.message ? `${payload.message} ` : ""}已暂存变更。应用时会暂停 USB Gadget，写入完成后需要插回 OTG 并执行温重启恢复。`;
  } else {
    $("media-apply-detail").textContent = `${payload.message ? `${payload.message} ` : ""}当前没有待应用变更。上传或删除文件后，会在这里集中写入虚拟盘。`;
  }
}

function renderMediaUnavailable(message) {
  ["boombox", "wrap", "lightshow"].forEach((category) => {
    const list = $(`media-${category}-list`);
    list.replaceChildren();
    const empty = document.createElement("p");
    empty.className = "empty-state";
    empty.textContent = message;
    list.append(empty);
  });
  $("media-music-count").textContent = "0 首";
  $("media-music-breadcrumb").replaceChildren();
  const musicEmpty = document.createElement("p");
  musicEmpty.className = "empty-state";
  musicEmpty.textContent = message;
  $("media-music-list").replaceChildren(musicEmpty);
  $("media-music-page-summary").textContent = "无法读取";
  $("media-music-back").disabled = true;
  $("media-music-prev").disabled = true;
  $("media-music-next").disabled = true;
  $("media-apply-status").textContent = "不可用";
  $("media-apply-status").className = "badge error";
  $("media-apply").disabled = true;
}

function renderMediaCategory(category, assets) {
  const list = $(`media-${category}-list`);
  const count = $(`media-${category}-count`);
  count.textContent = category === "wrap" ? `${assets.length} / 10` : `${assets.length} 个`;
  list.replaceChildren();
  if (!assets.length) {
    const empty = document.createElement("p");
    empty.className = "empty-state";
    empty.textContent = "暂无可识别媒体文件。";
    list.append(empty);
    return;
  }
  assets.forEach((asset) => list.append(createMediaAssetRow(category, asset)));
}

function musicAssets() {
  const assets = state.mediaAssets?.assets;
  return Array.isArray(assets) ? assets.filter((asset) => asset.category === "music") : [];
}

function renderMusicBrowser(assets) {
  const list = $("media-music-list");
  $("media-music-count").textContent = `${assets.length} 首`;
  renderMusicBreadcrumb();
  $("media-music-back").disabled = !state.musicPath;
  const entries = musicDirectoryEntries(assets, state.musicPath);
  const pageCount = Math.max(1, Math.ceil(entries.length / musicPageSize));
  state.musicPage = Math.min(Math.max(state.musicPage, 0), pageCount - 1);
  const start = state.musicPage * musicPageSize;
  const visible = entries.slice(start, start + musicPageSize);
  list.replaceChildren();
  if (!visible.length) {
    const empty = document.createElement("p");
    empty.className = "empty-state";
    empty.textContent = state.musicPath ? "此目录没有可识别的歌曲。" : "暂无可识别歌曲。";
    list.append(empty);
  }
  visible.forEach((entry) => {
    if (entry.kind === "directory") {
      const row = document.createElement("button");
      row.type = "button";
      row.className = "media-directory-row";
      const main = document.createElement("span");
      main.className = "media-asset-main";
      const name = document.createElement("strong");
      name.textContent = `文件夹 · ${entry.name}`;
      const detail = document.createElement("small");
      detail.textContent = `${entry.songCount} 首歌曲`;
      main.append(name, detail);
      const arrow = document.createElement("span");
      arrow.className = "media-directory-arrow";
      arrow.textContent = "›";
      row.append(main, arrow);
      row.addEventListener("click", () => {
        state.musicPath = entry.path;
        state.musicPage = 0;
        renderMusicBrowser(musicAssets());
      });
      list.append(row);
      return;
    }
    list.append(createMediaAssetRow("music", entry.asset));
  });
  $("media-music-page-summary").textContent = entries.length ? `第 ${state.musicPage + 1} / ${pageCount} 页 · ${entries.length} 项` : "0 项";
  $("media-music-prev").disabled = state.musicPage === 0;
  $("media-music-next").disabled = state.musicPage >= pageCount - 1;
}

function renderMusicBreadcrumb() {
  const breadcrumb = $("media-music-breadcrumb");
  breadcrumb.replaceChildren();
  const parts = state.musicPath ? state.musicPath.split("/") : [];
  const items = ["MUSIC", ...parts];
  let currentPath = "";
  items.forEach((name, index) => {
    if (index > 0) currentPath = currentPath ? `${currentPath}/${name}` : name;
    const button = document.createElement("button");
    button.type = "button";
    button.className = "ghost compact";
    button.dataset.musicPath = currentPath;
    button.textContent = name;
    breadcrumb.append(button);
    if (index < items.length - 1) breadcrumb.append(document.createTextNode("/"));
  });
}

function musicDirectoryEntries(assets, currentPath) {
  const prefix = currentPath ? currentPath.split("/") : [];
  const directories = new Map();
  const files = [];
  assets.forEach((asset) => {
    const file = Array.isArray(asset.files) ? asset.files[0] : null;
    const relative = String(file?.name || asset.name || "").replaceAll("\\", "/");
    const parts = relative.split("/").filter(Boolean);
    if (parts.length <= prefix.length || prefix.some((part, index) => part !== parts[index])) return;
    if (parts.length === prefix.length + 1) {
      files.push({kind:"file", name:parts[parts.length - 1], asset});
      return;
    }
    const name = parts[prefix.length];
    const path = [...prefix, name].join("/");
    const directory = directories.get(path) || {kind:"directory", name, path, songCount:0};
    directory.songCount += 1;
    directories.set(path, directory);
  });
  const order = (left, right) => left.name.localeCompare(right.name, "zh-CN", {numeric:true, sensitivity:"base"});
  return [...directories.values()].sort(order).concat(files.sort(order));
}

function createMediaAssetRow(category, asset) {
  const row = document.createElement("article");
  row.className = "media-asset-row";
  const main = document.createElement("div");
  main.className = "media-asset-main";
  const name = document.createElement("strong");
  name.textContent = asset.name || "未命名媒体";
  const detail = document.createElement("small");
  const files = Array.isArray(asset.files) ? asset.files : [];
  detail.textContent = files.map((file) => `${file.name} · ${formatBytes(file.size_bytes)}`).join(" · ") || "文件详情不可用";
  main.append(name, detail);
  if (asset.preview_available && !asset.pending_delete) {
    const previewPath = `/api/v1/media-assets/${encodeURIComponent(asset.id)}/preview`;
    if (category === "wrap") {
      const preview = document.createElement("img");
      preview.className = "media-wrap-preview";
      preview.loading = "lazy";
      preview.alt = `${asset.name || "皮肤"} 预览`;
      preview.src = previewPath;
      main.append(preview);
    } else if (category === "boombox") {
      const preview = document.createElement("audio");
      preview.className = "media-audio-preview";
      preview.controls = true;
      preview.preload = "none";
      preview.src = previewPath;
      main.append(preview);
    }
  }
  const actions = document.createElement("div");
  actions.className = "media-asset-actions";
  const stateBadge = document.createElement("span");
  stateBadge.className = `badge${asset.pending_delete ? " error" : asset.applied ? " ok" : ""}`;
  stateBadge.textContent = asset.external ? "直接写入" : asset.pending_delete ? "待删除" : asset.pending ? "待应用" : "已应用";
  actions.append(stateBadge);
  const remove = document.createElement("button");
  remove.type = "button";
  remove.className = "ghost compact";
  remove.textContent = "删除";
  remove.addEventListener("click", () => deleteMediaAsset(asset));
  actions.append(remove);
  row.append(main, actions);
  return row;
}

async function uploadMediaFiles(category) {
  const configuration = {
    boombox: {input:"media-boombox-files", button:"media-boombox-upload", role:"file"},
    wrap: {input:"media-wrap-files", button:"media-wrap-upload", role:"file"},
    music: {input:"media-music-files", button:"media-music-upload", role:"file"},
  }[category];
  const form = new FormData();
  let button;
  if (category === "lightshow") {
    const sequence = $("media-lightshow-sequence").files[0];
    const audio = $("media-lightshow-audio").files[0];
    if (!sequence || !audio) {
      setMediaNotice("请同时选择 .fseq 序列文件和 MP3/WAV 音频文件", true);
      return;
    }
    form.append("sequence", sequence, sequence.name);
    form.append("audio", audio, audio.name);
    button = $("media-lightshow-upload");
  } else {
    const files = configuration ? $(configuration.input).files : [];
    if (!files?.length) {
      setMediaNotice("请先选择文件", true);
      return;
    }
    for (const file of files) form.append(configuration.role, file, file.name);
    button = $(configuration.button);
  }
  button.disabled = true;
  const original = button.textContent;
  button.textContent = "暂存中…";
  try {
    const result = await request(`/api/v1/media-assets/upload?category=${encodeURIComponent(category)}`, {method:"POST", body:form});
    const count = Array.isArray(result.assets) ? result.assets.length : 0;
    if (category === "music") {
      state.musicPath = "";
      state.musicPage = 0;
    }
    setMediaNotice(state.maintenance?.auto_recovery ? `已安全暂存 ${count} 个媒体资源；可直接应用，TeslaBox 会自动维护恢复。` : `已安全暂存 ${count} 个媒体资源；待确认 OTG 已断开后再应用。`, false);
    if (category === "lightshow") {
      $("media-lightshow-sequence").value = "";
      $("media-lightshow-audio").value = "";
    } else if (configuration) {
      $(configuration.input).value = "";
    }
    await loadMediaAssets();
  } catch (error) {
    setMediaNotice(error.message, true);
  } finally {
    button.disabled = false;
    button.textContent = original;
  }
}

async function deleteMediaAsset(asset) {
  if (asset.external) {
    await deleteExternalMediaAsset(asset);
    return;
  }
  const confirmed = await askConfirmation(
    "删除媒体文件",
    asset.applied ? "文件会标记为待删除，确认应用变更后才从车机虚拟盘移除。" : "这会丢弃尚未应用到车机的暂存文件。",
    "确认删除", true,
  );
  if (!confirmed) return;
  try {
    await request(`/api/v1/media-assets/${encodeURIComponent(asset.id)}/delete`, {method:"POST"});
    setMediaNotice(asset.applied ? "已标记删除；请在 OTG 断开后应用变更。" : "已删除暂存文件。", false);
    await loadMediaAssets();
  } catch (error) {
    setMediaNotice(error.message, true);
  }
}

async function deleteExternalMediaAsset(asset) {
  const automatic = Boolean(state.maintenance?.auto_recovery);
  if (!automatic && !$("media-confirm-otg-disconnected").checked) {
    setMediaNotice("请先物理拔掉 OTG 线，并勾选确认项。", true);
    return;
  }
  const confirmed = await askConfirmation(
    "删除直接写入的媒体",
    automatic ? "文件会立刻从虚拟盘删除。TeslaBox 会自动断开 USB Gadget，并在完成后自动温重启恢复。" : "文件会立刻从虚拟盘删除。TeslaBox 将暂停 USB Gadget；完成后请插回 OTG，并到日志中心执行温重启恢复。",
    "确认删除", true,
  );
  if (!confirmed) return;
  try {
    await postJSON("/api/v1/media-assets/external-delete", {
      id: asset.id,
      category: asset.category,
      files: Array.isArray(asset.files) ? asset.files : [],
      confirm_otg_disconnected: !automatic,
    });
    $("media-confirm-otg-disconnected").checked = false;
    setMediaNotice(automatic ? "已删除直接写入的媒体，自动温重启恢复已提交。" : "已从虚拟盘删除直接写入的媒体。请插回 OTG 后到日志中心执行温重启恢复。", false);
    if (!automatic) await Promise.allSettled([loadMediaAssets(), loadStatus()]);
  } catch (error) {
    setMediaNotice(error.message, true);
  }
}

async function applyMediaAssets() {
  const automatic = Boolean(state.maintenance?.auto_recovery);
  if (!automatic && !$("media-confirm-otg-disconnected").checked) {
    setMediaNotice("请先物理拔掉 OTG 线，并勾选确认项。", true);
    return;
  }
  const confirmed = await askConfirmation(
    "安全应用媒体变更",
    automatic ? "TeslaBox 会先自动断开 USB Gadget，再写入虚拟盘；完成后将自动温重启恢复。车机或电脑会短暂失去 USB 存储。" : "TeslaBox 将暂停 USB Gadget 并写入虚拟盘。本次运行不会重新绑定 OTG；完成后请插回 OTG，再到日志中心执行温重启恢复。",
    "确认应用", true,
  );
  if (!confirmed) return;
  const button = $("media-apply");
  button.disabled = true;
  try {
    await postJSON("/api/v1/media-assets/apply", {confirm_otg_disconnected:!automatic});
    $("media-confirm-otg-disconnected").checked = false;
    setMediaNotice(automatic ? "媒体变更已写入虚拟盘，自动温重启恢复已提交。" : "媒体变更已写入虚拟盘。请插回 OTG 后到日志中心执行温重启恢复。", false);
    if (!automatic) await Promise.allSettled([loadMediaAssets(), loadStatus()]);
  } catch (error) {
    setMediaNotice(error.message, true);
  } finally {
    button.disabled = false;
  }
}

function setMediaNotice(message, error) {
  presentNotice($("media-notice"), message, error);
}

function setLogsNotice(message, error) {
  presentNotice($("logs-notice"), message, error);
}

function renderLogDiagnostics(storage) {
  const gadget = storage.gadget_status || {};
  const enumerationComplete = Boolean(gadget.enumeration_complete ?? gadget.host_connected);
  const busSuspended = Boolean(gadget.bus_suspend_known && gadget.bus_suspended);
  const badge = $("logs-usb-status");
  const warning = $("logs-usb-warning");
  const disconnect = $("usb-gadget-disconnect");
  const reconnect = $("usb-gadget-reconnect");
	const warmRestart = $("usb-gadget-warm-restart");
  badge.className = "badge";
  warning.hidden = true;
  warning.textContent = "";
	disconnect.hidden = Boolean(storage.gadget_paused) || !gadget.bound;
	reconnect.hidden = !storage.gadget_paused || Boolean(storage.gadget_resume_pending);
	warmRestart.hidden = !storage.gadget_paused || Boolean(storage.gadget_resume_pending);
	if (storage.gadget_paused) {
		badge.textContent = storage.gadget_resume_pending ? "等待冷启动" : "已安全断开";
		badge.classList.add("ok");
		warning.hidden = false;
		warning.textContent = storage.gadget_resume_pending
			? "已准备下次冷启动恢复。本次运行不会重新绑定 UDC；请保持 OTG 已插入，正常关机后断开主电源，再重新上电。"
			: "USB Gadget 已从 UDC 解绑。同一开机周期禁止重新绑定；请先物理插入 OTG 线，再执行温重启恢复，冷启动可作为备用方案。";
  } else if (enumerationComplete && busSuspended) {
    badge.textContent = "可能已断开";
    badge.classList.add("error");
    warning.hidden = false;
		warning.textContent = "控制器仍保留 configured/high-speed，但 USB 总线已经挂起。它可能是主机正常休眠，也可能是 OTG 已拔出；当前内核无法无损区分，TeslaBox 不会为确认状态而运行时重绑。";
  } else if (enumerationComplete) {
    badge.textContent = "已完成枚举";
    badge.classList.add("ok");
    warning.hidden = false;
		warning.textContent = "该状态只表示 UDC 最近一次完成过枚举。部分 ROCK 3A 内核拔线后仍会保留 configured/high-speed，因此它不能证明物理 OTG 当前仍连接。";
  } else if (gadget.host_detected) {
    badge.textContent = "枚举未完成";
    badge.classList.add("error");
    warning.hidden = false;
    warning.textContent = `已检测到 USB 主机，但状态停留在 ${gadget.udc_state || "未知阶段"}，车机或电脑尚未完成设备描述符与配置枚举。请立即刷新 USB 枚举日志后再断线或重启。`;
  } else if (gadget.bound) {
    badge.textContent = "等待主机";
  } else {
    badge.textContent = "Gadget 未就绪";
    badge.classList.add("error");
  }
  $("logs-usb-state").textContent = gadget.udc_state || "not attached";
  $("logs-usb-speed").textContent = gadget.current_speed || "—";
  $("logs-udc").textContent = gadget.udc || gadget.selected_udc || "—";
  $("logs-backing").textContent = gadget.image_path || "未绑定";
}

async function disconnectGadget() {
  const confirmed = await askConfirmation(
    "安全断开 USB Gadget",
		"请先确认车机或电脑上的 OTG 线已经物理拔出。继续后 TeslaBox 会解绑 UDC 并保持暂停；本次开机不会再重新绑定。",
    "确认已拔线并断开", true,
  );
  if (!confirmed) return;
  try {
    await postJSON("/api/v1/gadget/disconnect", {confirm_physical_disconnect:true});
		setLogsNotice("USB Gadget 已安全断开；请先插入 OTG 线，再准备下次冷启动恢复", false);
    await Promise.allSettled([loadLogs(), loadStatus()]);
  } catch (error) {
    setLogsNotice(error.message, true);
  }
}

async function reconnectGadget() {
	const confirmed = await askConfirmation(
		"准备下次冷启动恢复",
		"请先把 OTG 线物理插入车机或电脑。确认后本次运行仍保持 UDC 解绑；请正常关机，断开主电源后重新上电，TeslaBox 只会在新的系统开机周期尝试一次绑定。",
		"确认已插线并准备", true,
	);
  if (!confirmed) return;
  try {
    await postJSON("/api/v1/gadget/reconnect", {confirm_physical_connection:true});
		setLogsNotice("已准备下次冷启动恢复；请保持 OTG 已插入，关机断电后重新上电", false);
    await Promise.allSettled([loadLogs(), loadStatus()]);
  } catch (error) {
    setLogsNotice(error.message, true);
  }
}

async function warmRestartGadget() {
	const confirmed = await askConfirmation(
		"温重启恢复 USB Gadget",
		"请先把 OTG 线物理插入车机或电脑。确认后 TeslaBox 会保存恢复状态并自动重启；本次运行不会直接重新绑定 UDC。",
		"确认已插线并重启", true,
	);
	if (!confirmed) return;
	try {
		await postJSON("/api/v1/gadget/reboot-recover", {confirm_physical_connection:true});
		setLogsNotice("温重启命令已提交，请等待设备重新上线", false);
	} catch (error) {
		setLogsNotice(error.message, true);
	}
}

function renderLogs(report) {
  const labels = {usb:"USB 枚举", media:"视频与转码", application:"TeslaBox 服务", network:"网络与热点"};
  const lines = Array.isArray(report.lines) ? report.lines : [];
  $("logs-output").textContent = lines.length ? lines.join("\n") : "当前来源暂无日志。";
  const details = [`${labels[report.source] || report.source || "系统"} · ${lines.length} 行`, `生成于 ${formatDate(report.generated_at)}`];
  if (report.limited) details.push("仅显示最新日志");
  if (report.redacted) details.push(`已脱敏 ${report.redacted} 行`);
  $("logs-meta").textContent = details.join(" · ");
  if (Array.isArray(report.service_states)) renderGadgetServiceStates(report.service_states);
}

function renderGadgetServiceStates(services) {
  const states = new Map(services.map((service) => [service.name, service]));
  renderGadgetServiceState("logs-radxa-adbd", states.get("radxa-adbd.service"));
  renderGadgetServiceState("logs-radxa-usbnet", states.get("radxa-usbnet.service"));
}

function renderGadgetServiceState(elementID, service) {
  const element = $(elementID);
  if (!service || service.load_state === "not-found" || service.load_state === "unknown") {
    element.textContent = "未安装";
    return;
  }
  if (String(service.unit_file_state || "").startsWith("masked") && service.active_state === "inactive") {
    element.textContent = "已屏蔽 · 未运行";
    return;
  }
  element.textContent = `${service.unit_file_state || "状态未知"} · ${service.active_state || "未知"}`;
}

function scheduleLogsRefresh() {
  window.clearTimeout(logsRefreshTimer);
  logsRefreshTimer = 0;
  if (state.page !== "logs" || appView.hidden || !logsAutoRefreshEnabled()) return;
  logsRefreshTimer = window.setTimeout(() => loadLogs({background:true}), 10000);
}

function logsAutoRefreshEnabled() {
  return $("logs-auto-refresh").getAttribute("aria-pressed") === "true";
}

function toggleLogsAutoRefresh() {
  const button = $("logs-auto-refresh");
  const enabled = !logsAutoRefreshEnabled();
  button.setAttribute("aria-pressed", String(enabled));
  button.lastElementChild.textContent = enabled ? "自动刷新：每 10 秒" : "自动刷新：已关闭";
  scheduleLogsRefresh();
}

async function copyLogs() {
  const value = $("logs-output").textContent || "";
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(value);
    } else {
      const textarea = document.createElement("textarea");
      textarea.value = value;
      textarea.style.position = "fixed";
      textarea.style.opacity = "0";
      document.body.appendChild(textarea);
      textarea.select();
      document.execCommand("copy");
      textarea.remove();
    }
    setLogsNotice("日志已复制。", false);
  } catch (_) {
    setLogsNotice("浏览器不允许自动复制，请选中日志内容后手动复制。", true);
  }
}

async function postJSON(path, body) {
  return request(path, {
    method: "POST", headers: {"Content-Type":"application/json"}, body: JSON.stringify(body),
  });
}

async function loadSettings() {
  return withPageLoading("正在读取网络、存储与系统设置…", async () => {
    setSettingsNotice("正在读取网络设置…", false, false);
    const [wifiResult, apResult, storageResult, updateResult, maintenanceResult, exportSettingsResult, exportHistoryResult] = await Promise.allSettled([
      request("/api/v1/network/wifi"), request("/api/v1/network/ap"), request("/api/v1/storage/devices"), request("/api/v1/update"), request("/api/v1/maintenance"), request("/api/v1/export-settings"), request("/api/v1/exports/history"),
    ]);
    if (wifiResult.status === "fulfilled") {
      state.wifi = wifiResult.value;
      renderWiFiSettings(state.wifi);
    } else {
      renderWiFiUnavailable(wifiResult.reason.message);
    }
    if (apResult.status === "fulfilled") {
      state.ap = apResult.value;
      renderAPSettings(state.ap);
    } else {
      renderAPUnavailable(apResult.reason.message);
    }
    if (storageResult.status === "fulfilled") {
      state.storage = storageResult.value;
      renderStorageDevices(state.storage);
    } else {
      renderStorageUnavailable(storageResult.reason.message);
    }
    if (updateResult.status === "fulfilled") {
      state.update = updateResult.value;
      renderUpdate(state.update);
    } else {
      renderUpdateUnavailable(updateResult.reason.message);
    }
    if (maintenanceResult.status === "fulfilled") {
      state.maintenance = maintenanceResult.value;
      renderMaintenanceSettings(state.maintenance);
    } else {
      renderMaintenanceUnavailable(maintenanceResult.reason.message);
    }
    if (exportSettingsResult.status === "fulfilled" && exportHistoryResult.status === "fulfilled") {
      state.exportSettings = exportSettingsResult.value;
      state.exportHistory = exportHistoryResult.value;
      renderExportSettings();
    } else {
      renderExportSettingsUnavailable(exportSettingsResult.status === "rejected" ? exportSettingsResult.reason.message : exportHistoryResult.reason.message);
    }
    if (wifiResult.status === "fulfilled" && apResult.status === "fulfilled" && storageResult.status === "fulfilled" && updateResult.status === "fulfilled") setSettingsNotice("设置已更新", false, false);
    else setSettingsNotice("部分设置暂不可用，请检查设备状态", true);
  });
}

async function loadExportSettings() {
  const [settingsResult, historyResult] = await Promise.allSettled([
    request("/api/v1/export-settings"), request("/api/v1/exports/history"),
  ]);
  if (settingsResult.status !== "fulfilled" || historyResult.status !== "fulfilled") {
    const reason = settingsResult.status === "rejected" ? settingsResult.reason : historyResult.reason;
    renderExportSettingsUnavailable(reason.message);
    setSettingsNotice(`导出设置不可用：${reason.message}`, true);
    return;
  }
  state.exportSettings = settingsResult.value;
  state.exportHistory = historyResult.value;
  renderExportSettings();
}

function renderExportSettings() {
  const settings = state.exportSettings || {delete_after_download:false, retain_count:8};
  const deleteAfterDownload = Boolean(settings.delete_after_download);
  $("export-delete-after-download").checked = deleteAfterDownload;
  const retainSelect = $("export-retain-count");
  if ([...retainSelect.options].some((option) => Number(option.value) === Number(settings.retain_count))) retainSelect.value = String(settings.retain_count);
  else retainSelect.value = "8";
  $("export-settings-save").disabled = false;
  $("export-history-refresh").disabled = false;
  $("export-retention-status").textContent = deleteAfterDownload ? "下载即清空" : `保留 ${retainSelect.value} 个`;
  $("export-retention-status").className = "badge ok";
  renderExportRetentionControls();
  const history = Array.isArray(state.exportHistory) ? state.exportHistory : [];
  const root = $("export-history-list");
  root.replaceChildren();
  if (history.length === 0) {
    const empty = document.createElement("p");
    empty.className = "empty-state";
    empty.textContent = deleteAfterDownload ? "下载即清空已开启；后续导出完整下载后不会保留在历史中。" : "暂时没有可再次下载的导出文件。";
    root.append(empty);
    return;
  }
  history.forEach((task) => {
    const row = document.createElement("div");
    row.className = "export-history-row";
    const copy = document.createElement("div");
    const title = document.createElement("strong");
    const source = task.request?.mode === "source";
    title.textContent = source ? "多路原画压缩包" : exportTaskLabel(task.request);
    const detail = document.createElement("small");
    const completedAt = task.completed_at || task.created_at;
    detail.textContent = `${formatDate(completedAt)}${task.estimated_bytes ? ` · 约 ${formatBytes(task.estimated_bytes)}` : ""}`;
    copy.append(title, detail);
    const download = document.createElement("a");
    download.className = "secondary compact";
    download.href = `/api/v1/exports/${encodeURIComponent(task.id)}/download`;
    download.download = "";
    download.textContent = "再次下载";
    row.append(copy, download);
    root.append(row);
  });
}

function renderExportRetentionControls() {
  const deleteAfterDownload = $("export-delete-after-download").checked;
  $("export-retain-count").disabled = deleteAfterDownload;
  $("export-settings-detail").textContent = deleteAfterDownload
    ? "仅在浏览器完整接收文件后删除；网络中断或关闭页面时会保留，方便再次下载。"
    : `将保留最近 ${$("export-retain-count").value} 个已完成导出，超过后自动清理最旧文件。`;
}

function renderExportSettingsUnavailable(message) {
  state.exportSettings = null;
  state.exportHistory = [];
  $("export-settings-save").disabled = true;
  $("export-history-refresh").disabled = true;
  $("export-retention-status").textContent = "不可用";
  $("export-retention-status").className = "badge error";
  $("export-settings-detail").textContent = `导出设置暂不可用：${message}`;
  $("export-history-list").replaceChildren();
}

async function saveExportSettings() {
  const button = $("export-settings-save");
  button.disabled = true;
  try {
    state.exportSettings = await request("/api/v1/export-settings", {
      method:"POST", headers:{"Content-Type":"application/json"},
      body:JSON.stringify({
        delete_after_download:$("export-delete-after-download").checked,
        retain_count:Number($("export-retain-count").value),
      }),
    });
    await loadExportSettings();
    setSettingsNotice("导出文件保留设置已保存", false);
  } catch (error) {
    setSettingsNotice(error.message, true);
  } finally {
    button.disabled = false;
  }
}

function exportTaskLabel(request) {
  const labels = {composite6:"六路合成视频", composite4a:"四路合成 A 视频", composite4b:"四路合成 B 视频", composite3:"三路合成视频"};
  if (request?.layout === "single") return `${cameraLabel(request.cameras?.[0] || "front")}单路视频`;
  return labels[request?.layout] || "导出视频";
}

function renderMaintenanceSettings(settings) {
  const enabled = Boolean(settings.auto_recovery);
  const toggle = $("maintenance-auto-recovery");
  $("storage-reclaim").disabled = false;
  toggle.disabled = false;
  toggle.setAttribute("aria-pressed", String(enabled));
  $("maintenance-toggle-title").textContent = enabled ? "媒体自动恢复已开启" : "媒体自动恢复已禁用";
  $("maintenance-toggle-detail").textContent = enabled ? "点击后切换为手动维护" : "点击后启用";
  $("maintenance-status").textContent = enabled ? "媒体自动恢复" : "媒体手动恢复";
  $("maintenance-status").className = `badge${enabled ? " ok" : ""}`;
  $("media-confirm-otg-row").hidden = enabled;
  $("media-maintenance-warning").textContent = enabled
    ? "自动维护恢复已启用。应用媒体或删除直接写入文件时，TeslaBox 会自动解绑 USB Gadget、完成写入后温重启；车机或电脑会短暂失去 USB 存储。"
    : "媒体变更默认需要先物理拔掉连接车机或电脑的 OTG 线。TeslaBox 会暂停 USB Gadget、写入虚拟盘并保持暂停；完成后请插回 OTG，再到日志中心执行温重启恢复。在线更新仍会自动重启恢复。";
}

function renderMaintenanceUnavailable(message) {
  state.maintenance = null;
  const toggle = $("maintenance-auto-recovery");
  toggle.setAttribute("aria-pressed", "false");
  toggle.disabled = true;
  $("storage-reclaim").disabled = true;
  $("maintenance-toggle-title").textContent = "媒体自动恢复不可用";
  $("maintenance-toggle-detail").textContent = "请检查设备状态";
  $("maintenance-status").textContent = "不可用";
  $("maintenance-status").className = "badge error";
  $("media-confirm-otg-row").hidden = false;
  $("media-maintenance-warning").textContent = "自动维护设置暂不可用。应用变更前必须先物理拔掉连接车机或电脑的 OTG 线，并在完成后手动温重启恢复。";
  setSettingsNotice(`自动维护设置不可用：${message}`, true);
}

async function toggleMaintenanceAutoRecovery() {
  const toggle = $("maintenance-auto-recovery");
  const enabled = !Boolean(state.maintenance?.auto_recovery);
  if (enabled) {
    const confirmed = await askConfirmation(
      "启用媒体自动恢复",
      "以后应用媒体或删除直接写入文件时，TeslaBox 会自动断开 USB Gadget 并温重启。车机或电脑会短暂失去存储设备；仅在已验证可稳定重新枚举时启用。",
      "确认启用", true,
    );
    if (!confirmed) return;
  }
  toggle.disabled = true;
  try {
    state.maintenance = await postJSON("/api/v1/maintenance", {auto_recovery:enabled, confirm_risk:enabled});
    renderMaintenanceSettings(state.maintenance);
    setSettingsNotice(enabled ? "媒体自动恢复已启用" : "媒体已恢复为手动维护模式", false);
  } catch (error) {
    setSettingsNotice(error.message, true);
    await loadSettings();
  } finally {
    toggle.disabled = false;
  }
}

async function reclaimStorageSpace() {
  const automatic = Boolean(state.maintenance?.auto_recovery);
  const lifecycle = automatic
    ? "TeslaBox 会自动断开 USB Gadget，完成后温重启恢复。"
    : "请先物理拔掉车机或电脑的 OTG 线；完成后请插回 OTG，并在日志中心执行温重启恢复。";
  const confirmed = await askConfirmation(
    "回收虚拟盘旧空间",
    `系统会先检查 exFAT，只对分配位图确认空闲的簇执行外层打洞。${lifecycle} 操作可能持续数分钟，请勿断电或拔出数据盘。`,
    "确认回收",
    true,
  );
  if (!confirmed) return;
  const button = $("storage-reclaim");
  const originalLabel = button.textContent;
  button.disabled = true;
  button.textContent = "正在回收…";
  setSettingsNotice("正在检查 exFAT 分配位图并回收外层空间，请勿断电…", false, false);
  try {
    const result = await postJSON("/api/v1/storage/reclaim", {
      confirm_otg_disconnected: !automatic,
    });
    const summary = `已检查 ${formatNumber(result.free_clusters || 0)} 个空闲簇，外层实际释放 ${formatBytes(result.reclaimed_bytes || 0)}。`;
    setSettingsNotice(
      automatic ? `${summary} 自动温重启恢复已提交。` : `${summary} 请插回 OTG 后到日志中心执行温重启恢复。`,
      false,
    );
    if (!automatic) await loadStatus();
  } catch (error) {
    setSettingsNotice(error.message, true);
  } finally {
    button.disabled = false;
    button.textContent = originalLabel;
  }
}

function renderUpdate(update) {
  const labels = {
    disabled:"未配置", idle:"可检查", checking:"检查中", available:"发现更新", current:"已是最新",
    downloading:"下载中", downloaded:"等待安装", installing:"正在安装", failed:"更新失败",
  };
  if (update.selected_action === "rollback" && update.state === "available") labels.available = "已选择回退";
  $("update-status").textContent = labels[update.state] || "状态未知";
  $("update-status").className = `badge${["current", "downloaded"].includes(update.state) ? " ok" : update.state === "failed" ? " error" : ""}`;
  $("update-current-version").textContent = update.current_version || "dev";
  $("update-latest-version").textContent = update.latest_version || "—";
  $("update-target-label").firstChild.textContent = update.selected_action === "rollback" ? "回退目标" : "最新版本";
  const detail = update.configured
    ? `更新通道：${update.channel || "stable"}${update.current_commit && update.current_commit !== "unknown" ? ` · 提交 ${update.current_commit.slice(0, 12)}` : ""}`
    : "尚未配置 Gitee 更新清单地址和设备公钥。";
  const installDetail = update.install_result === "success" ? "上次更新已成功安装。" : "";
  $("update-version-detail").textContent = update.error || installDetail || detail;
  $("update-release-notes").hidden = !update.release_notes;
  $("update-release-notes").textContent = update.release_notes || "";
  const downloading = update.state === "downloading";
  $("update-progress-wrap").hidden = !downloading;
  $("update-progress").value = Number(update.progress_percent || 0);
  $("update-progress-detail").textContent = `${Number(update.progress_percent || 0)}% · ${formatBytes(update.downloaded_bytes || 0)} / ${formatBytes(update.total_bytes || 0)}`;
  $("update-check").disabled = !update.configured || ["checking", "downloading", "installing"].includes(update.state);
  $("update-download").hidden = update.state !== "available";
  $("update-install").hidden = update.state !== "downloaded";
  $("update-download").textContent = update.selected_action === "rollback" ? "下载回退版本" : "下载更新";
  $("update-install").textContent = update.selected_action === "rollback" ? "回退并重启" : "安装并重启";
  $("update-history-section").hidden = !update.history_configured;
  if (["downloading", "installing"].includes(update.state)) scheduleUpdatePoll();
  else clearUpdatePoll();
}

function renderUpdateUnavailable(message) {
  clearUpdatePoll();
  $("update-status").textContent = "不可用";
  $("update-status").className = "badge error";
  $("update-version-detail").textContent = message;
  $("update-check").disabled = true;
  $("update-download").hidden = true;
  $("update-install").hidden = true;
  $("update-history-section").hidden = true;
}

function setUpdateNotice(message, error) {
  const notice = $("update-notice");
  presentNotice(notice, message, error);
  notice.hidden = true;
}

function scheduleUpdatePoll() {
  clearUpdatePoll();
  updatePollTimer = window.setTimeout(async () => {
    try {
      state.update = await request("/api/v1/update", {loading:false});
      renderUpdate(state.update);
    } catch (_) {
      if (state.update?.state === "installing") {
        $("update-version-detail").textContent = "服务正在重启，页面恢复后请重新检查版本。";
        scheduleUpdatePoll();
      }
    }
  }, 1000);
}

function clearUpdatePoll() {
  if (updatePollTimer) window.clearTimeout(updatePollTimer);
  updatePollTimer = 0;
}

async function checkUpdate() {
  $("update-check").disabled = true;
  try {
    state.update = await postJSON("/api/v1/update/check", {});
    renderUpdate(state.update);
    setUpdateNotice(state.update.available ? `发现 ${state.update.latest_version} 更新` : "当前已是最新版本", false);
  } catch (error) {
    setUpdateNotice(error.message, true);
    await refreshUpdateStatus();
  }
}

async function loadUpdateHistory() {
  const button = $("update-history-load");
  button.disabled = true;
  try {
    state.updateHistory = await request("/api/v1/update/history");
    renderUpdateHistory(state.updateHistory);
  } catch (error) {
    $("update-history-detail").hidden = false;
    $("update-history-detail").textContent = error.message;
    $("update-history-list").hidden = true;
  } finally {
    button.disabled = false;
  }
}

function renderUpdateHistory(history) {
  $("update-history-section").hidden = !history.configured;
  if (!history.configured) return;
  const detail = $("update-history-detail");
  const list = $("update-history-list");
  const releases = Array.isArray(history.releases) ? history.releases : [];
  detail.hidden = false;
  detail.textContent = history.generated_at ? `版本目录更新于 ${formatDate(history.generated_at)}` : "历史版本目录已校验";
  list.replaceChildren();
  if (!releases.length) {
    detail.textContent = "没有可直接安装的历史版本。维护者需要把兼容版本加入签名目录。";
    list.hidden = true;
    return;
  }
  releases.forEach((release) => {
    const item = document.createElement("article");
    item.className = "update-history-item";
    const copy = document.createElement("div");
    const title = document.createElement("strong");
    title.textContent = release.version;
    const metadata = document.createElement("small");
    metadata.textContent = `${release.action === "rollback" ? "可回退" : "可安装"}${release.published_at ? ` · 发布于 ${formatDate(release.published_at)}` : ""}`;
    copy.append(title, metadata);
    if (release.release_notes) {
      const notes = document.createElement("p");
      notes.textContent = release.release_notes;
      copy.append(notes);
    }
    const button = document.createElement("button");
    button.type = "button";
    button.className = "secondary";
    button.textContent = release.action === "rollback" ? "选择回退" : "选择安装";
    button.addEventListener("click", () => selectUpdateVersion(release));
    item.append(copy, button);
    list.append(item);
  });
  list.hidden = false;
}

async function selectUpdateVersion(release) {
  const rollback = release.action === "rollback";
  const confirmed = await askConfirmation(
    rollback ? "选择主程序回退版本" : "选择指定主程序版本",
    `${rollback ? "将回退" : "将安装"} TeslaBox 主程序至 ${release.version}。内核、无线驱动、FFmpeg 和 systemd 不会回退；确认后仍需下载并安装。`,
    rollback ? "确认选择回退版本" : "确认选择版本", rollback,
  );
  if (!confirmed) return;
  try {
    state.update = await postJSON("/api/v1/update/select", {version:release.version});
    renderUpdate(state.update);
    setUpdateNotice(`${rollback ? "已选择回退" : "已选择"} ${release.version}，请继续下载并安装`, false);
  } catch (error) {
    setUpdateNotice(error.message, true);
  }
}

async function downloadUpdate() {
  try {
    state.update = await postJSON("/api/v1/update/download", {});
    renderUpdate(state.update);
    setUpdateNotice(state.update.selected_action === "rollback" ? "回退包正在后台下载并校验" : "更新包正在后台下载并校验", false);
  } catch (error) {
    setUpdateNotice(error.message, true);
  }
}

async function installUpdate() {
  const automatic = Boolean(state.maintenance?.update_auto_recovery);
  const rollback = state.update?.selected_action === "rollback";
  const confirmed = await askConfirmation(
    rollback ? "回退 TeslaBox 主程序" : "安装系统更新",
		automatic ? `将${rollback ? "回退" : "安装"} ${state.update?.latest_version || "已下载版本"}。TeslaBox 会自动断开 USB Gadget，安装健康检查通过后自动重启恢复；车机或电脑会短暂失去 USB 存储。内核、驱动、FFmpeg 和 systemd 不会回退。` : `将${rollback ? "回退" : "安装"} ${state.update?.latest_version || "已下载版本"} 并重启 TeslaBox 服务。请先物理拔掉车机 OTG。即使页面仍显示 configured，也可以确认继续：TeslaBox 会先真正解绑 UDC，再提交更新。更新后请插好 OTG，在日志中心准备下次冷启动，再关机断电重启。`,
		automatic ? `确认自动维护${rollback ? "回退" : "安装"}` : `确认已拔线并${rollback ? "回退" : "安装"}`, true,
  );
  if (!confirmed) return;
  try {
    if (!automatic) {
      const storage = await request("/api/v1/storage");
      if (storage.gadget_status?.bound) {
        setUpdateNotice("已确认 OTG 物理断开，正在安全解绑 USB Gadget…", false);
        await postJSON("/api/v1/gadget/disconnect", {confirm_physical_disconnect:true});
      }
    }
    state.update = await postJSON("/api/v1/update/install", {});
    renderUpdate(state.update);
		setUpdateNotice(automatic ? `${rollback ? "回退" : "安装"}请求已提交。新程序健康检查通过后，设备会自动重启恢复 USB Gadget。` : `${rollback ? "回退" : "安装"}请求已提交。服务重启后 Gadget 会继续保持安全断开；请插好 OTG，在日志中心准备下次冷启动，再关机断电重启。`, false);
	} catch (error) {
		setUpdateNotice(automatic ? `${error.message}。自动维护未完成，请在日志中心确认 USB Gadget 状态。` : `${error.message}。USB Gadget 会继续保持安全断开；请先插入 OTG 线，再到日志中心准备下次冷启动。`, true);
  }
}

async function refreshUpdateStatus() {
  try {
    state.update = await request("/api/v1/update");
    renderUpdate(state.update);
  } catch (error) {
    renderUpdateUnavailable(error.message);
  }
}

function renderWiFiSettings(wifi) {
  const available = Boolean(wifi.available);
  $("wifi-interface").textContent = available ? wifi.interface : "不可用";
  $("wifi-mac-address").textContent = wifi.mac_address || "—";
  $("wifi-ip-address").textContent = available ? wifi.ip_address || "未获取" : "—";
  $("wifi-status").textContent = !available ? "不可用" : wifi.current_connection ? `已连接 · ${wifi.current_connection}` : "未连接";
  $("wifi-status").className = `hardware-value${available && wifi.current_connection ? " ok" : " error"}`;
  const warnings = [];
  if (wifi.single_radio_notice) warnings.push(wifi.single_radio_notice);
  if (wifi.recovery?.last_error) {
    const retry = wifi.recovery.next_attempt_at ? `，下次尝试 ${new Date(wifi.recovery.next_attempt_at).toLocaleTimeString("zh-CN", {hour12:false})}` : "";
    warnings.push(`上游连接协调：${wifi.recovery.last_error}${retry}`);
  } else if (wifi.recovery?.last_success_at) {
    warnings.push(`上游连接协调最近成功：${new Date(wifi.recovery.last_success_at).toLocaleString("zh-CN", {hour12:false})}`);
  }
  $("wifi-warning").hidden = warnings.length === 0;
  $("wifi-warning").textContent = warnings.join(" ");
  renderSavedNetworks(wifi.saved_networks || [], available);
  renderAvailableNetworks(available ? wifi.available_networks || [] : []);
  $("wifi-scan").disabled = !available;
  $("wifi-scan").title = available ? "手动扫描附近 WiFi" : "未检测到可用的无线网卡";
}

function renderWiFiUnavailable(message) {
  $("wifi-interface").textContent = "不可用";
  $("wifi-mac-address").textContent = "—";
  $("wifi-ip-address").textContent = "—";
  $("wifi-status").textContent = "不可用";
  $("wifi-status").className = "hardware-value error";
  $("wifi-warning").hidden = false;
  $("wifi-warning").textContent = message;
  renderSavedNetworks([], false);
  renderAvailableNetworks([]);
  $("wifi-scan").disabled = true;
  $("wifi-scan").title = "无线管理暂不可用";
}

function renderSavedNetworks(networks, wifiAvailable = true) {
  const list = $("saved-network-list");
  list.replaceChildren();
  if (!networks.length) {
    const empty = document.createElement("p");
    empty.className = "empty-state";
    empty.textContent = "暂无已保存的客户端 WiFi";
    list.append(empty);
    return;
  }
  networks.forEach((network) => {
    const row = document.createElement("div");
    row.className = "network-row";
    const identity = document.createElement("div");
    identity.className = "network-identity";
    const title = document.createElement("strong");
    title.textContent = network.ssid || network.name;
    const detail = document.createElement("small");
    detail.textContent = `${network.active ? "当前已连接" : network.requires_password ? "需要重新输入密码" : "已保存"} · ${network.auto_connect ? "自动连接" : "手动连接"} · ${formatWiFiIPv4Summary(network.ipv4)}`;
    identity.append(title, detail);
    const actions = document.createElement("div");
    actions.className = "network-actions";
    const autoConnect = document.createElement("label");
    autoConnect.className = "auto-connect-control";
    const autoConnectInput = document.createElement("input");
    autoConnectInput.type = "checkbox";
    autoConnectInput.checked = Boolean(network.auto_connect);
    autoConnectInput.setAttribute("aria-label", `${network.ssid || network.name} 自动连接`);
    autoConnectInput.addEventListener("change", () => updateWiFiAutoConnect(network, autoConnectInput));
    const autoConnectText = document.createElement("span");
    autoConnectText.textContent = "自动连接";
    autoConnect.append(autoConnectInput, autoConnectText);
    const connect = document.createElement("button");
    connect.type = "button";
    connect.className = "secondary";
    connect.textContent = network.active ? "已连接" : "连接";
    connect.disabled = !wifiAvailable || network.active;
    connect.title = wifiAvailable ? "" : "未检测到可用的无线网卡";
    connect.addEventListener("click", () => selectSavedWiFi(network));
    const ipv4 = document.createElement("button");
    ipv4.type = "button";
    ipv4.className = "ghost";
    ipv4.textContent = "IP 设置";
    ipv4.addEventListener("click", () => openWiFiIPv4Modal(network));
    const forget = document.createElement("button");
    forget.type = "button";
    forget.className = "ghost";
    forget.textContent = "忘记";
    forget.disabled = network.active;
    forget.addEventListener("click", () => forgetWiFi(network));
    actions.append(autoConnect, connect, ipv4, forget);
    row.append(identity, actions);
    list.append(row);
  });
}

function toggleNetworkPanel(panelID, toggleID) {
  const panel = $(panelID);
  const toggle = $(toggleID);
  const expanded = panel.hidden;
  panel.hidden = !expanded;
  toggle.setAttribute("aria-expanded", String(expanded));
  toggle.textContent = expanded ? "收起" : "展开";
}

function renderAvailableNetworks(networks) {
  const list = $("available-network-list");
  list.replaceChildren();
  if (!networks.length) {
    const row = document.createElement("tr");
    const cell = document.createElement("td");
    cell.colSpan = 5;
    cell.className = "empty-state";
    cell.textContent = "暂无扫描结果，请点击扫描网络";
    row.append(cell);
    list.append(row);
    return;
  }
  networks.forEach((network) => {
    const row = document.createElement("tr");
    const ssid = document.createElement("td");
    ssid.textContent = network.ssid;
    const signal = document.createElement("td");
    const meter = document.createElement("span");
    meter.className = "signal-meter";
    meter.style.setProperty("--signal", `${Math.max(0,Math.min(100,Number(network.signal || 0)))}%`);
    const bar = document.createElement("i");
    const signalText = document.createElement("span");
    signalText.textContent = `${network.signal}%`;
    meter.append(bar, signalText);
    signal.append(meter);
    const band = document.createElement("td");
    band.textContent = network.band;
    const security = document.createElement("td");
    security.textContent = network.security;
    const action = document.createElement("td");
    const button = document.createElement("button");
    button.type = "button";
    button.className = network.saved ? "secondary" : "primary";
    button.textContent = network.saved ? "连接已保存" : "连接";
    button.disabled = !supportedWiFiSecurity(network.security);
    button.title = button.disabled ? "第一版暂不支持该加密类型" : "";
    button.addEventListener("click", () => openAvailableWiFi(network));
    action.append(button);
    row.append(ssid, signal, band, security, action);
    list.append(row);
  });
}

async function scanWiFi() {
  const button = $("wifi-scan");
  button.disabled = true;
  button.textContent = "扫描中…";
  try {
    const response = await postJSON("/api/v1/network/wifi/scan", {});
    state.wifi.available_networks = response.items || [];
    renderAvailableNetworks(state.wifi.available_networks);
    setSettingsNotice(`扫描完成，发现 ${state.wifi.available_networks.length} 个网络`, false);
  } catch (error) {
    setSettingsNotice(error.message, true);
  } finally {
    button.disabled = !state.wifi?.available;
    button.textContent = "扫描网络";
  }
}

async function connectSavedWiFi(network) {
  const accessPoint = (state.wifi?.available_networks || []).find((item) => item.ssid === network.ssid);
  await runWiFiConnect({
    uuid:network.uuid,
    bssid:accessPoint?.bssid || "",
    channel:Number(accessPoint?.channel || 0),
  });
}

function selectSavedWiFi(network) {
  if (!network.requires_password) {
    connectSavedWiFi(network);
    return;
  }
  const accessPoint = (state.wifi?.available_networks || []).find((item) => item.ssid === network.ssid);
  pendingWiFi = {
    ...accessPoint,
    uuid:network.uuid,
    ssid:network.ssid,
    security:accessPoint?.security || network.security || "WPA2",
  };
  $("wifi-modal-title").textContent = `重新连接 ${network.ssid}`;
  $("wifi-modal-detail").textContent = "旧配置没有可供后台服务读取的密码，请重新输入一次；保存后会恢复开机自动连接。";
  $("wifi-password-label").hidden = false;
  $("wifi-password").required = true;
  $("wifi-password").value = "";
  $("wifi-autoconnect").checked = Boolean(network.auto_connect);
  $("wifi-modal").hidden = false;
  window.setTimeout(() => $("wifi-password").focus(), 0);
}

async function updateWiFiAutoConnect(network, input) {
  const enabled = input.checked;
  input.disabled = true;
  try {
    await postJSON("/api/v1/network/wifi/autoconnect", {uuid:network.uuid, enabled});
    network.auto_connect = enabled;
    setSettingsNotice(enabled ? "已开启自动连接" : "已关闭自动连接", false);
  } catch (error) {
    input.checked = !enabled;
    setSettingsNotice(error.message, true);
  } finally {
    input.disabled = false;
  }
}

function openAvailableWiFi(network) {
  const saved = (state.wifi?.saved_networks || []).find((item) => item.ssid === network.ssid);
  if (saved) {
    if (saved.requires_password) {
      selectSavedWiFi(saved);
    } else {
      connectSavedWiFi(saved);
    }
    return;
  }
  pendingWiFi = network;
  $("wifi-modal-title").textContent = `连接 ${network.ssid}`;
  $("wifi-modal-detail").textContent = `${network.band} · 信号 ${network.signal}% · ${network.security}`;
  const needsPassword = network.security !== "OPEN";
  $("wifi-password-label").hidden = !needsPassword;
  $("wifi-password").required = needsPassword;
  $("wifi-password").value = "";
  $("wifi-autoconnect").checked = true;
  $("wifi-modal").hidden = false;
  if (needsPassword) window.setTimeout(() => $("wifi-password").focus(), 0);
}

function closeWiFiModal() {
  pendingWiFi = null;
  $("wifi-modal").hidden = true;
  $("wifi-password").value = "";
}

function formatWiFiIPv4Summary(settings) {
  if (!settings || settings.mode !== "static") return "DHCP";
  const address = settings.address && settings.subnet_mask ? `${settings.address} / ${settings.subnet_mask}` : "静态 IPv4";
  return `静态 ${address}`;
}

function openWiFiIPv4Modal(network) {
  pendingWiFiIPv4Network = network;
  const settings = network.ipv4 || {mode:"dhcp", dns:[]};
  $("wifi-ipv4-modal-title").textContent = `${network.ssid || network.name} 的 IPv4 设置`;
  $("wifi-ipv4-modal-detail").textContent = network.active ? "当前正在连接此网络，保存后会立即重新获取网络地址。" : "此配置会在下次连接该 WiFi 时自动生效。";
  $("wifi-ipv4-mode").value = settings.mode === "static" ? "static" : "dhcp";
  $("wifi-ipv4-address").value = settings.address || "";
  $("wifi-ipv4-mask").value = settings.subnet_mask || "";
  $("wifi-ipv4-gateway").value = settings.gateway || "";
  $("wifi-ipv4-dns").value = Array.isArray(settings.dns) ? settings.dns.join(", ") : "";
  renderWiFiIPv4Fields();
  $("wifi-ipv4-modal").hidden = false;
}

function closeWiFiIPv4Modal() {
  pendingWiFiIPv4Network = null;
  $("wifi-ipv4-modal").hidden = true;
}

function renderWiFiIPv4Fields() {
  const isStatic = $("wifi-ipv4-mode").value === "static";
  $("wifi-ipv4-static-fields").hidden = !isStatic;
  ["wifi-ipv4-address", "wifi-ipv4-mask", "wifi-ipv4-gateway"].forEach((id) => { $(id).required = isStatic; });
}

async function saveWiFiIPv4Settings(event) {
  event.preventDefault();
  if (!pendingWiFiIPv4Network) return;
  const network = pendingWiFiIPv4Network;
  const mode = $("wifi-ipv4-mode").value;
  const payload = {
    uuid:network.uuid,
    mode,
    address:$("wifi-ipv4-address").value.trim(),
    subnet_mask:$("wifi-ipv4-mask").value.trim(),
    gateway:$("wifi-ipv4-gateway").value.trim(),
    dns:$("wifi-ipv4-dns").value.split(/[，,;\s]+/).map((value) => value.trim()).filter(Boolean),
  };
  if (network.active) {
    $("wifi-ipv4-modal").hidden = true;
    const confirmed = await askConfirmation(
      "应用当前 WiFi 的 IPv4 设置",
      "保存后 WiFi 会立即重新连接，当前页面可能断开。请记住新的 IP 地址，或通过 TeslaBox 热点重新进入设置中心。",
      "确认保存", true,
    );
    if (!confirmed) {
      $("wifi-ipv4-modal").hidden = false;
      return;
    }
  }
  closeWiFiIPv4Modal();
  setSettingsNotice(network.active ? "正在应用 IPv4 设置，页面可能短暂断开…" : "正在保存 WiFi IPv4 设置…", false);
  try {
    await postJSON("/api/v1/network/wifi/ipv4", payload);
    setSettingsNotice(network.active ? "IPv4 设置已应用；如页面断开，请使用新 IP 或 TeslaBox 热点重新访问。" : "WiFi IPv4 设置已保存，将在下次连接时生效。", false);
    if (!network.active) await loadSettings();
  } catch (error) {
    setSettingsNotice(error.message, true);
  }
}

async function connectNewWiFi(event) {
  event.preventDefault();
  if (!pendingWiFi) return;
  const network = pendingWiFi;
  const password = $("wifi-password").value;
  closeWiFiModal();
  await runWiFiConnect({
    uuid:network.uuid || "", ssid:network.ssid, bssid:network.bssid, channel:Number(network.channel || 0),
    security:network.security, password, auto_connect:$("wifi-autoconnect").checked,
  });
}

async function runWiFiConnect(payload) {
  setSettingsNotice("正在连接 WiFi；若需要同步信道，热点会短暂重启…", false);
  try {
    await postJSON("/api/v1/network/wifi/connect", payload);
    setSettingsNotice("WiFi 已连接", false);
    await loadSettings();
  } catch (error) {
    setSettingsNotice(error.message, true);
  }
}

async function forgetWiFi(network) {
  const confirmed = await askConfirmation("忘记网络", `确定删除“${network.ssid || network.name}”的已保存配置吗？页面不会再自动连接该网络。`, "忘记网络", true);
  if (!confirmed) return;
  try {
    await postJSON("/api/v1/network/wifi/forget", {uuid:network.uuid});
    setSettingsNotice("已忘记网络", false);
    await loadSettings();
  } catch (error) {
    setSettingsNotice(error.message, true);
  }
}

function renderAPSettings(ap) {
  const available = Boolean(ap.available);
  $("ap-status").textContent = !available ? "不可用" : ap.enabled ? "运行中" : "已停用";
  $("ap-status").className = `hardware-value${ap.enabled ? " ok" : " error"}`;
  $("ap-interface").textContent = ap.interface || "—";
  $("ap-mac-address").textContent = ap.mac_address || "—";
  $("ap-ssid").value = ap.ssid || "";
  $("ap-band").value = ap.band || "a";
  populateAPChannels($("ap-band").value, Number(ap.channel || 0));
  $("ap-enabled").checked = Boolean(ap.enabled);
  $("ap-autoconnect").checked = Boolean(ap.auto_connect);
  $("ap-address").textContent = ap.address || "—";
  $("ap-sharing").textContent = ap.sharing ? "已开启" : "未开启";
  $("ap-form").querySelectorAll("input,select,button").forEach((element) => { element.disabled = !available; });
}

function renderAPUnavailable(message) {
  $("ap-status").textContent = "不可用";
  $("ap-status").className = "hardware-value error";
  $("ap-interface").textContent = "—";
  $("ap-mac-address").textContent = "—";
  $("ap-address").textContent = message;
  $("ap-form").querySelectorAll("input,select,button").forEach((element) => { element.disabled = true; });
}

function populateAPChannels(band, selected) {
  const select = $("ap-channel");
  const channels = band === "bg" ? [0,1,2,3,4,5,6,7,8,9,10,11,12,13] : [0,36,40,44,48,149,153,157,161,165];
  select.replaceChildren();
  channels.forEach((channel) => {
    const option = document.createElement("option");
    option.value = String(channel);
    option.textContent = channel === 0 ? "自动" : String(channel);
    option.selected = channel === selected;
    select.append(option);
  });
}

async function saveAPSettings(event) {
  event.preventDefault();
  const enabled = $("ap-enabled").checked;
  const confirmed = await askConfirmation(
    enabled ? "应用热点设置" : "关闭无线热点",
    enabled
      ? "保存后热点会重新启动，当前连接可能断开。请确认已经记住新的热点名称和密码。"
      : "关闭后会同时关闭开机自动启动，通过该热点打开的页面会断开。需要重新开启时，请通过其他网络访问设置中心。",
    enabled ? "保存并应用" : "确认关闭",
    !enabled,
  );
  if (!confirmed) return;
  const payload = {
    enabled, ssid:$("ap-ssid").value, password:$("ap-password").value,
    band:$("ap-band").value, channel:Number($("ap-channel").value), auto_connect:enabled && $("ap-autoconnect").checked,
  };
  setSettingsNotice(enabled ? "正在应用热点设置，页面可能断开…" : "正在提交关闭指令…", false);
  try {
    await postJSON("/api/v1/network/ap", payload);
    $("ap-password").value = "";
    if (!enabled) {
      $("ap-status").textContent = "正在关闭";
      $("ap-status").className = "hardware-value";
      $("ap-enabled").checked = false;
      $("ap-autoconnect").checked = false;
      setSettingsNotice("关闭指令已提交，热点将在 1 秒后关闭", false);
      return;
    }
    setSettingsNotice("热点设置已应用", false);
    await loadSettings();
  } catch (error) {
    setSettingsNotice(error.message, true);
  }
}

async function loadStorageDevices() {
  const button = $("storage-refresh");
  button.disabled = true;
  try {
    state.storage = await request("/api/v1/storage/devices");
    renderStorageDevices(state.storage);
    setSettingsNotice("存储设备列表已更新", false);
  } catch (error) {
    renderStorageUnavailable(error.message);
    setSettingsNotice(error.message, true);
  } finally {
    button.disabled = false;
  }
}

function renderStorageDevices(storage) {
  const devices = storage.devices || [];
  const list = $("storage-device-list");
  list.replaceChildren();
  const ready = devices.filter((device) => device.initialized).length;
  $("storage-status").textContent = ready ? `${ready} 个已初始化` : devices.length ? "需要初始化" : "未检测到设备";
  $("storage-status").className = `badge${ready ? " ok" : devices.length ? " error" : ""}`;
  if (!devices.length) {
    const empty = document.createElement("p");
    empty.className = "empty-state";
    empty.textContent = "未检测到具有稳定 by-id 的 USB 存储整盘";
    list.append(empty);
    return;
  }
  devices.forEach((device) => {
    const row = document.createElement("article");
    row.className = "storage-device-row";
    const identity = document.createElement("div");
    identity.className = "storage-device-identity";
    const title = document.createElement("strong");
    title.textContent = device.model || "USB 存储设备";
    const detail = document.createElement("small");
    detail.textContent = `${formatBytes(device.size_bytes)} · 序列号 ${device.serial || "不可用"}`;
    const path = document.createElement("code");
    path.textContent = device.stable_path;
    identity.append(title, detail, path);
    const stateBox = document.createElement("div");
    stateBox.className = "storage-device-actions";
    const badge = document.createElement("span");
    badge.className = `badge${device.active || device.initialized ? " ok" : " error"}`;
    badge.textContent = storageStateLabel(device);
    const reason = document.createElement("small");
    reason.textContent = device.last_error || device.reason || "—";
    stateBox.append(badge, reason);
    if (device.can_initialize || device.can_resume || device.can_reinitialize) {
      const button = document.createElement("button");
      button.type = "button";
      button.className = device.can_resume || device.can_reinitialize ? "secondary compact" : "danger-button compact";
      button.textContent = device.can_resume ? "继续初始化" : device.can_reinitialize ? "重新初始化" : "初始化设备";
      button.addEventListener("click", () => openStorageModal(device));
      stateBox.append(button);
    }
    if (device.can_cancel) {
      const cancel = document.createElement("button");
      cancel.type = "button";
      cancel.className = "ghost compact";
      cancel.textContent = "放弃未完成任务";
      cancel.addEventListener("click", () => cancelStorageInitialization(device));
      stateBox.append(cancel);
    }
    row.append(identity, stateBox);
    list.append(row);
  });
}

function renderStorageUnavailable(message) {
  $("storage-status").textContent = "不可用";
  $("storage-status").className = "badge error";
  const list = $("storage-device-list");
  list.replaceChildren();
  const error = document.createElement("p");
  error.className = "empty-state";
  error.textContent = message;
  list.append(error);
}

function storageStateLabel(device) {
  if (device.active) return "当前数据盘";
  if (device.initialized) return "已初始化";
  if (device.state === "initializing") return "初始化中断";
  if (device.state === "failed") return "初始化失败";
  if (device.state === "unsafe") return "正在使用";
  if (device.state === "unsupported") return "不支持";
  return "未初始化";
}

function openStorageModal(device) {
  pendingStorage = device;
  const reinitialize = Boolean(device.initialized && device.can_reinitialize);
  $("storage-modal-title").textContent = device.can_resume ? "继续初始化存储设备" : reinitialize ? "重新初始化存储设备" : "初始化存储设备";
  const imageSize = device.planned_image_size_bytes || device.size_bytes;
  $("storage-modal-detail").textContent = `${device.model || "USB 存储设备"}，物理容量 ${formatBytes(device.size_bytes)}，预计向车机提供 ${formatBytes(imageSize)}。目标序列号：${device.serial}。该操作会擦除整块设备。`;
  $("storage-music-enabled").checked = false;
  $("storage-teslacam-percent").value = "80";
  $("storage-confirm-serial").value = "";
  $("storage-confirm-otg-disconnected").checked = false;
  $("storage-confirm-erase").checked = false;
  renderStoragePartitionPreview();
  $("storage-modal").hidden = false;
  $("storage-confirm-serial").focus();
}

function renderStoragePartitionPreview() {
  const enabled = $("storage-music-enabled").checked;
  $("storage-partition-options").hidden = !enabled;
  if (!pendingStorage || !enabled) return;
  const percent = Number($("storage-teslacam-percent").value || 80);
  const total = Number(pendingStorage.planned_image_size_bytes || pendingStorage.size_bytes || 0);
  const teslaCam = Math.floor(total * percent / 100);
  $("storage-partition-preview").textContent = `TeslaCam ${percent}%（约 ${formatBytes(teslaCam)}） · MUSIC ${100 - percent}%（约 ${formatBytes(total - teslaCam)}）`;
}

function closeStorageModal() {
  $("storage-modal").hidden = true;
  pendingStorage = null;
}

async function initializeStorageDevice(event) {
  event.preventDefault();
  if (!pendingStorage) return;
  const button = $("storage-init-submit");
  button.disabled = true;
  button.textContent = "初始化中，请勿拔出…";
  try {
    await postJSON("/api/v1/storage/initialize", {
      stable_path:pendingStorage.stable_path,
      confirm_serial:$("storage-confirm-serial").value,
      confirm_erase:$("storage-confirm-erase").checked,
      confirm_otg_disconnected:$("storage-confirm-otg-disconnected").checked,
      reinitialize:Boolean(pendingStorage.initialized && pendingStorage.can_reinitialize),
      music_enabled:$("storage-music-enabled").checked,
      teslacam_percent:Number($("storage-teslacam-percent").value || 80),
    });
    closeStorageModal();
    setSettingsNotice("存储设备初始化完成；请插好 OTG 后到日志中心执行温重启恢复", false);
    await loadStorageDevices();
    await refreshAll();
  } catch (error) {
    setSettingsNotice(error.message, true);
  } finally {
    button.disabled = false;
    button.textContent = "擦除并初始化";
  }
}

async function cancelStorageInitialization(device) {
  const confirmed = await askConfirmation(
    "放弃未完成初始化",
    `将卸载 ${device.model || "目标设备"} 并恢复原活动数据盘选择，不会再次擦除设备。`,
    "确认放弃", true,
  );
  if (!confirmed) return;
  try {
    await postJSON("/api/v1/storage/initialize/cancel", {
      stable_path:device.stable_path, confirm_serial:device.serial,
    });
    setSettingsNotice("已放弃未完成初始化任务", false);
    await loadStorageDevices();
  } catch (error) {
    setSettingsNotice(error.message, true);
  }
}

async function requestPower(action) {
  const shutdown = action === "shutdown";
  const confirmed = await askConfirmation(
    shutdown ? "关闭 TeslaBox" : "重启 TeslaBox",
    shutdown ? "设备将停止服务并断开车机 USB。关机后必须重新断电上电才能启动。" : "设备将停止服务并断开车机 USB，预计稍后恢复。",
    shutdown ? "确认关机" : "确认重启", true,
  );
  if (!confirmed) return;
  try {
    await postJSON("/api/v1/system/power", {action});
    setSettingsNotice(shutdown ? "关机命令已提交" : "重启命令已提交，请稍后重新连接", false);
  } catch (error) {
    setSettingsNotice(error.message, true);
  }
}

function askConfirmation(title, message, label, dangerous) {
  if (confirmResolver) confirmResolver(false);
  $("confirm-title").textContent = title;
  $("confirm-message").textContent = message;
  $("confirm-submit").textContent = label;
  $("confirm-submit").className = dangerous ? "danger-button" : "primary";
  $("confirm-modal").hidden = false;
  return new Promise((resolve) => { confirmResolver = resolve; });
}

function resolveConfirmation(value) {
  $("confirm-modal").hidden = true;
  const resolve = confirmResolver;
  confirmResolver = null;
  if (resolve) resolve(value);
}

function supportedWiFiSecurity(security) {
  const value = String(security || "").toUpperCase();
  return value === "OPEN" || (value.includes("WPA") && !value.includes("EAP") && !value.includes("802.1X"));
}

function setSettingsNotice(message, error, toast = true) {
  presentNotice($("settings-notice"), message, error, toast);
}

async function loadStatus() {
  try {
    const [storageResult, systemResult] = await Promise.allSettled([
      request("/api/v1/storage"), request("/api/v1/system"),
    ]);
    if (storageResult.status === "rejected") throw storageResult.reason;
    const storage = storageResult.value;
    renderStatus(storage);
    $("live-indicator").classList.add("online");
    $("live-indicator").querySelector("span").textContent = "设备在线";
    if (systemResult.status === "fulfilled") renderSystemStatus(systemResult.value);
    else renderSystemUnavailable();
  } catch (error) {
    $("live-indicator").classList.remove("online");
    $("live-indicator").querySelector("span").textContent = "设备离线";
    setNotice(error.message, true);
  }
}

function renderStatus(storage) {
  const gadget = storage.gadget_status || {};
  const enumerationComplete = Boolean(gadget.enumeration_complete ?? gadget.host_connected);
  const busSuspended = Boolean(gadget.bus_suspend_known && gadget.bus_suspended);
  const badge = $("usb-badge");
  badge.className = "badge";
	if (storage.gadget_paused) {
		badge.textContent = storage.gadget_resume_pending ? "等待冷启动" : "已安全断开";
		badge.classList.add("ok");
		$("usb-title").textContent = storage.gadget_resume_pending ? "已准备下次冷启动恢复" : "USB Gadget 保持暂停";
  } else if (enumerationComplete && busSuspended) {
    badge.textContent = "可能已断开";
    badge.classList.add("error");
    $("usb-title").textContent = "主机休眠或 OTG 已拔出";
  } else if (enumerationComplete) {
    badge.textContent = "已完成枚举";
    badge.classList.add("ok");
    $("usb-title").textContent = gadget.current_speed || "USB 枚举完成";
  } else if (gadget.host_detected) {
    badge.textContent = "枚举未完成";
    badge.classList.add("error");
    $("usb-title").textContent = "USB 主机尚未完成识别";
  } else if (gadget.bound) {
    badge.textContent = "等待车机";
    $("usb-title").textContent = "Gadget 已就绪";
  } else {
    badge.textContent = "未就绪";
    badge.classList.add("error");
    $("usb-title").textContent = "USB Gadget 未绑定";
  }
	if (storage.gadget_resume_pending) {
		$("usb-detail").textContent = "本次运行保持解绑 · 请关机断电后重新上电";
	} else if (busSuspended) {
    $("usb-detail").textContent = "USB 总线已挂起 · Gadget 仍保持就绪";
  } else {
    const usbSemantics = enumerationComplete && !storage.gadget_paused ? "内核可能保留该枚举状态" : "";
    $("usb-detail").textContent = [gadget.udc_state || "状态未知", storage.phase || "UNKNOWN", usbSemantics].filter(Boolean).join(" · ");
  }
  $("video-count").textContent = `${formatNumber(storage.video_count || 0)} 个`;

  const outerSpace = storage.snapshot_space || {};
  const volumeSpace = storage.media_snapshot?.volume_space || {};
  const hasVolumeSpace = Number(volumeSpace.total_bytes || 0) > 0;
  const displayedSpace = hasVolumeSpace ? volumeSpace : outerSpace;
  const freePercent = Number(displayedSpace.free_percent || 0);
  $("space-value").textContent = formatBytes(displayedSpace.free_bytes || 0);
  $("space-progress").style.width = `${Math.max(0, Math.min(100, 100-freePercent))}%`;
  $("space-progress").style.background = freePercent < 10 ? "var(--warning)" : "var(--ok)";
  const outerPercent = Number(outerSpace.free_percent || 0);
  $("space-detail").textContent = hasVolumeSpace
    ? `exFAT 可用 ${freePercent.toFixed(1)}% · 外层 XFS 可用 ${formatBytes(outerSpace.free_bytes || 0)}（${outerPercent.toFixed(1)}%）`
    : `等待虚拟盘快照 · 外层 XFS 可用 ${formatBytes(outerSpace.free_bytes || 0)}（${outerPercent.toFixed(1)}%）`;

  const snapshot = storage.media_snapshot;
  $("snapshot-state").className = "badge";
  $("snapshot-state").textContent = snapshot ? "可播放" : "尚未建立";
  $("snapshot-state").classList.add(snapshot ? "ok" : "error");
  $("snapshot-detail").textContent = storage.snapshot_error || (snapshot ? `当前快照内的单路 MP4 · 更新于 ${formatDate(snapshot.created_at)}` : "等待首次快照");
}

function renderSystemStatus(system) {
  const indicator = $("internet-indicator");
  indicator.className = `internet-indicator ${system.internet_available ? "online" : "offline"}`;
  indicator.querySelector("span").textContent = system.internet_available ? "互联网" : "无互联网";

  const temperature = Number(system.temperature_c);
  const temperatureValue = $("temperature-value");
  temperatureValue.className = "";
  if (Number.isFinite(temperature)) {
    temperatureValue.textContent = `${temperature.toFixed(1)}°C`;
    const level = temperature >= 85 ? "danger" : temperature >= 70 ? "warning" : "normal";
    temperatureValue.classList.toggle("metric-warning", level === "warning");
    temperatureValue.classList.toggle("metric-danger", level === "danger");
    $("temperature-detail").textContent = ({normal:"温度正常", warning:"温度偏高", danger:"温度过高"})[level];
  } else {
    temperatureValue.textContent = "—";
    $("temperature-detail").textContent = "传感器不可用";
  }
  renderUsage("cpu", system.cpu_used_percent, system.uptime_seconds);
  renderCapacityUsage("memory", system.memory);
  renderCapacityUsage("system-disk", system.system_disk);
}

function renderSystemUnavailable() {
  const indicator = $("internet-indicator");
  indicator.className = "internet-indicator unknown";
  indicator.querySelector("span").textContent = "网络状态未知";
  for (const prefix of ["cpu", "memory", "system-disk"]) {
    $(`${prefix}-value`).textContent = "—";
    $(`${prefix}-progress`).style.width = "0";
    $(`${prefix}-detail`).textContent = "状态接口不可用";
  }
  $("temperature-value").textContent = "—";
  $("temperature-detail").textContent = "状态接口不可用";
}

function renderUsage(prefix, percentValue, uptimeSeconds) {
  const percent = Number(percentValue);
  if (!Number.isFinite(percent)) {
    $(`${prefix}-value`).textContent = "—";
    $(`${prefix}-progress`).style.width = "0";
    $(`${prefix}-detail`).textContent = "等待下一次采样";
    return;
  }
  renderPercent(prefix, percent);
  $(`${prefix}-detail`).textContent = `已运行 ${formatUptime(uptimeSeconds)}`;
}

function renderCapacityUsage(prefix, usage) {
  if (!usage || !Number(usage.total_bytes)) {
    $(`${prefix}-value`).textContent = "—";
    $(`${prefix}-progress`).style.width = "0";
    $(`${prefix}-detail`).textContent = "状态不可用";
    return;
  }
  renderPercent(prefix, Number(usage.used_percent || 0));
  $(`${prefix}-detail`).textContent = `${formatBytes(usage.used_bytes)} / ${formatBytes(usage.total_bytes)}`;
}

function renderPercent(prefix, value) {
  const percent = Math.max(0, Math.min(100, value));
  $(`${prefix}-value`).textContent = `${percent.toFixed(1)}%`;
  $(`${prefix}-progress`).style.width = `${percent}%`;
  $(`${prefix}-progress`).style.background = percent >= 90 ? "var(--danger)" : percent >= 75 ? "var(--warning)" : "var(--ok)";
}

async function loadSessions(silent = false) {
  if (state.loading) return;
  return withPageLoading(silent ? "正在同步录像索引…" : "正在整理完整录像时间轴…", async () => {
    state.loading = true;
    if (!silent) {
      timelineList.replaceChildren();
      setNotice("正在整理完整录像时间轴…", false, false);
    }
    try {
      const archiveParams = new URLSearchParams({ limit: "1000" });
      for (const id of ["clip-type", "date", "event"]) {
        const value = $(id).value.trim();
        if (value) archiveParams.set(id.replace("-", "_"), value);
      }
      const [recentPayload, archivePayload] = await Promise.all([
        request("/api/v1/video-sessions?clip_type=recent&limit=1000"),
        request(`/api/v1/video-sessions?${archiveParams}`),
      ]);
      recentSessions = (recentPayload.items || []).filter((session) => session.clip_type === "recent");
      archivedSessions = (archivePayload.items || []).filter((session) => ["saved", "sentry"].includes(session.clip_type));
      reconcileRecordingSelection();
      state.total = archivedSessions.length;
      renderArchiveTimeline();
      $("result-count").textContent = `${formatNumber(archivedSessions.length)} 段已保存/哨兵录像`;
      $("load-more").hidden = true;
      if (!silent) setNotice(archivedSessions.length === 0 ? "当前筛选条件下没有已保存或哨兵录像" : "", false, false);
    } catch (error) {
      setNotice(error.message, true);
    } finally {
      state.loading = false;
    }
  });
}

function renderArchiveTimeline() {
  const recentSection = $("recent-recordings-section");
  const recentList = $("recent-video-grid");
  recentList.replaceChildren();
  recentSessions.forEach((session) => recentList.append(createSessionTimeline(session)));
  recentSection.hidden = recentSessions.length === 0;

  timelineList.replaceChildren();
  const buckets = archiveSessions(archivedSessions, state.period);
  buckets.forEach((bucket) => {
    const group = document.createElement("section");
    group.className = "archive-group";
    const heading = document.createElement("h3");
    heading.className = "archive-heading";
    heading.textContent = bucket.label;
    const content = document.createElement("div");
    content.className = "archive-content";
    bucket.sessions.forEach((session) => content.append(createSessionTimeline(session)));
    group.append(heading, content);
    timelineList.append(group);
  });
  updateRecordingBulkActions();
}

function createSessionTimeline(session) {
  const card = document.createElement("article");
  card.className = `session-card clip-${clipTypeClass(session.clip_type)}`;
  const selectable = isSelectableRecordingSession(session);
  const selected = recordingDeleteMode && selectable && selectedRecordingSessions.has(session.id);
  card.classList.toggle("selection-enabled", recordingDeleteMode && selectable);
  card.classList.toggle("selected", selected);
  if (recordingDeleteMode && selectable) {
    card.setAttribute("aria-label", `选择 ${formatSessionRange(session.start_at, session.end_at)}`);
    card.addEventListener("click", (event) => {
      if (recordingDeleteInProgress || isSessionCardAction(event.target)) return;
      toggleRecordingSessionSelection(session.id);
    });
  }
  const head = document.createElement("div");
  head.className = "session-head";
  if (session.thumbnail_url) {
    head.classList.add("has-thumbnail");
    const thumbnail = document.createElement("div");
    thumbnail.className = "session-thumbnail";
    const image = document.createElement("img");
    image.src = session.thumbnail_url;
    image.alt = `${clipLabel(session.clip_type)}缩略图`;
    image.loading = "lazy";
    image.decoding = "async";
    image.addEventListener("error", () => {
      thumbnail.remove();
      head.classList.remove("has-thumbnail");
    }, {once:true});
    thumbnail.append(image);
    head.append(thumbnail);
  }
  const heading = document.createElement("div");
  if (recordingDeleteMode && selectable) {
    const selector = document.createElement("p");
    selector.className = "session-select";
    selector.textContent = selected ? "已选择 · 点击卡片任意空白区域取消" : "删除模式 · 点击卡片任意空白区域选择";
    heading.append(selector);
  }
  const title = document.createElement("h3");
  title.className = "session-title";
  title.textContent = formatSessionRange(session.start_at, session.end_at);
  const meta = document.createElement("p");
  meta.className = "session-meta";
  const clipBadge = document.createElement("span");
  clipBadge.className = `clip-badge clip-${clipTypeClass(session.clip_type)}`;
  clipBadge.textContent = clipLabel(session.clip_type);
  meta.append(clipBadge, document.createTextNode(`完整时长 ${formatDuration(session.duration_seconds)}${session.event && !session.event_metadata ? ` · ${session.event}` : ""}`));
  const duration = document.createElement("span");
  duration.className = "duration-pill";
  duration.textContent = formatDuration(session.duration_seconds);
  const aside = document.createElement("div");
  aside.className = "session-head-aside";
  const asideMeta = document.createElement("div");
  asideMeta.className = "session-aside-meta";
  const location = eventPlace(session.event_metadata) || eventCoordinates(session.event_metadata);
  if (location) {
    const locationLabel = document.createElement("span");
    locationLabel.className = "session-location";
    locationLabel.textContent = location;
    asideMeta.append(locationLabel);
  }
  asideMeta.append(duration);
  aside.append(asideMeta);
  heading.append(title, meta);
  const eventSummary = createEventSummary(session.event_metadata);
  if (eventSummary) heading.append(eventSummary);

  const open = document.createElement("button");
  open.className = "primary session-open";
  open.type = "button";
  open.textContent = "打开完整录像";
  open.addEventListener("click", () => {
    const display = state.view === "multi" ? "composite" : "single";
    openSessionPlayer(session, preferredCamera(session), 0, display);
  });
  const actions = document.createElement("div");
  actions.className = "session-actions";
  actions.append(open);
  if (session.clip_type === "recent") {
    const remove = document.createElement("button");
    remove.className = "ghost session-delete";
    remove.type = "button";
    remove.disabled = recordingDeleteInProgress;
    remove.textContent = "删除此段最近录像";
    remove.addEventListener("click", () => deleteRecordingSession(session, remove));
    actions.append(remove);
  }
  if (!session.complete) {
    const status = document.createElement("span");
    status.className = "session-warning";
    status.textContent = "部分分钟存在视角缺失";
    aside.append(status);
  }
  aside.append(actions);
  head.append(heading, aside);

  const body = document.createElement("div");
  body.className = "session-timeline-body";
  if (state.view === "multi") {
    body.append(createTrackRow(session, "六路合并", ""));
  } else {
    const cameras = cameraOrder.filter((camera) => session.cameras.includes(camera));
    cameras.forEach((camera) => body.append(createTrackRow(session, cameraLabel(camera), camera)));
  }

  card.append(head, body);
  return card;
}

async function deleteRecordingSession(session, button) {
  await deleteRecordingSessions([session], button);
}

function handleRecordingDeleteAction() {
  if (recordingDeleteInProgress) return;
  const selected = selectableRecordingSessions().filter((session) => selectedRecordingSessions.has(session.id));
  if (selected.length > 0) {
    void deleteSelectedRecordingSessions();
    return;
  }
  recordingDeleteMode = !recordingDeleteMode;
  if (!recordingDeleteMode) selectedRecordingSessions.clear();
  renderArchiveTimeline();
}

async function deleteSelectedRecordingSessions() {
  const selected = selectableRecordingSessions().filter((session) => selectedRecordingSessions.has(session.id));
  if (selected.length === 0) {
    setNotice("请先选择要删除的录像", true);
    return;
  }
  await deleteRecordingSessions(selected, $("recording-delete-selected"));
}

async function deleteRecordingSessions(sessions, button) {
  if (recordingDeleteInProgress || sessions.length === 0) return;
  let maintenance;
  try {
    maintenance = await request("/api/v1/maintenance");
    state.maintenance = maintenance;
  } catch (error) {
    setNotice(`无法确认 Gadget 维护策略：${error.message}`, true);
    return;
  }
  const automatic = Boolean(maintenance.auto_recovery);
  const recentCount = sessions.filter((session) => session.clip_type === "recent").length;
  const segmentCount = sessions.reduce((total, session) => total+Number(session.segment_count || 0), 0);
  const totalSize = sessions.reduce((total, session) => total+Number(session.total_size || 0), 0);
  const single = sessions.length === 1;
  const scope = single
    ? (recentCount ? "这段最近录像" : "这段录像及其整个事件目录")
    : `已选择 ${formatNumber(sessions.length)} 段录像${recentCount ? `（包含最近录像 ${formatNumber(recentCount)} 段）` : ""}`;
  const lifecycle = automatic
    ? "TeslaBox 会只断开一次 USB Gadget，在同一个维护窗口内完成批量删除和空间回收，然后只安排一次温重启恢复。"
    : "请先物理拔掉车机或电脑的 OTG 线；删除和空间回收完成后 Gadget 会保持暂停，请插回 OTG 并只执行一次温重启恢复。";
  const confirmed = await askConfirmation(
    single && recentCount ? "删除最近录像" : single ? "删除录像" : "批量删除录像",
    `${scope}，共 ${formatNumber(segmentCount)} 个录像片段、约 ${formatBytes(totalSize)}。${lifecycle} 删除完成后会依据 exFAT 空闲位图自动返还外层数据盘空间，无需再到设置中心点“回收历史旧空间”。`,
    single && recentCount ? "确认删除" : single ? "确认删除" : `确认删除 ${formatNumber(sessions.length)} 段`,
    true,
  );
  if (!confirmed) return;

  recordingDeleteInProgress = true;
  button.disabled = true;
  button.textContent = "删除并恢复空间中…";
  renderArchiveTimeline();
  try {
    if (activeSession && sessions.some((session) => session.id === activeSession.id)) closePlayer();
    const result = await postJSON("/api/v1/video-sessions/delete", {
      session_ids: sessions.map((session) => session.id),
      confirm_otg_disconnected: !automatic,
    });
    const eventSummary = result.deleted_events ? `，包含 ${formatNumber(result.deleted_events)} 个事件目录` : "";
    const reclaim = result.space_reclaim || {};
    const reclaimSummary = result.reclaim_error
      ? ` 外层空间自动恢复失败：${result.reclaim_error}；录像已经删除，可稍后使用“回收历史旧空间”补做。`
      : ` 外层数据盘实际恢复 ${formatBytes(reclaim.reclaimed_bytes || 0)}；若显示 0，通常表示 discard 已在删除时完成释放或这些空闲簇此前已经回收。`;
    const summary = `已处理 ${formatNumber(sessions.length)} 段录像，删除 ${formatNumber(result.deleted_files || 0)} 个文件${eventSummary}，释放虚拟盘内部 ${formatBytes(result.deleted_bytes || 0)}。${reclaimSummary}`;
    const deletedIDs = new Set(sessions.map((session) => session.id));
    recentSessions = recentSessions.filter((session) => !deletedIDs.has(session.id));
    archivedSessions = archivedSessions.filter((session) => !deletedIDs.has(session.id));
    state.total = archivedSessions.length;
    $("result-count").textContent = `${formatNumber(archivedSessions.length)} 段已保存/哨兵录像`;
    selectedRecordingSessions.clear();
    recordingDeleteMode = false;
    setNotice(
      automatic ? `${summary} 已提交一次温重启恢复。` : `${summary} 请插回 OTG 后到日志中心执行一次温重启恢复。`,
      Boolean(result.reclaim_error),
    );
    if (!automatic) await Promise.allSettled([loadStatus(), loadSessions(true)]);
  } catch (error) {
    setNotice(error.message, true);
  } finally {
    recordingDeleteInProgress = false;
    renderArchiveTimeline();
  }
}

function isSelectableRecordingSession(session) {
  return session.clip_type === "saved" || session.clip_type === "sentry";
}

function selectableRecordingSessions() {
  return archivedSessions.filter(isSelectableRecordingSession);
}

function isSessionCardAction(target) {
  return target instanceof Element && Boolean(target.closest("button, a, input, select, textarea, label"));
}

function toggleRecordingSessionSelection(sessionID) {
  if (selectedRecordingSessions.has(sessionID)) selectedRecordingSessions.delete(sessionID);
  else selectedRecordingSessions.add(sessionID);
  renderArchiveTimeline();
}

function reconcileRecordingSelection() {
  const available = new Set(selectableRecordingSessions().map((session) => session.id));
  for (const id of selectedRecordingSessions) {
    if (!available.has(id)) selectedRecordingSessions.delete(id);
  }
}

function toggleAllRecordingSelections() {
  if (!recordingDeleteMode || recordingDeleteInProgress) return;
  const sessions = selectableRecordingSessions();
  const allSelected = sessions.length > 0 && sessions.every((session) => selectedRecordingSessions.has(session.id));
  if (allSelected) sessions.forEach((session) => selectedRecordingSessions.delete(session.id));
  else sessions.forEach((session) => selectedRecordingSessions.add(session.id));
  renderArchiveTimeline();
}

function updateRecordingBulkActions() {
  const sessions = selectableRecordingSessions();
  const selected = sessions.filter((session) => selectedRecordingSessions.has(session.id));
  const toolbar = $("recording-bulk-actions");
  const selectAll = $("recording-select-all");
  const deleteButton = $("recording-delete-selected");
  toolbar.hidden = !recordingDeleteMode || sessions.length === 0;
  selectAll.hidden = !recordingDeleteMode;
  selectAll.disabled = recordingDeleteInProgress || sessions.length === 0;
  selectAll.textContent = selected.length === sessions.length && sessions.length > 0 ? "取消全选" : "全选已保存/哨兵";
  deleteButton.disabled = recordingDeleteInProgress;
  deleteButton.textContent = recordingDeleteInProgress
    ? "删除并恢复空间中…"
    : selected.length > 0
      ? "删除已选并恢复空间"
      : recordingDeleteMode
        ? "取消删除"
        : "删除录像";
  const selectedCount = $("recording-selected-count");
  selectedCount.hidden = !recordingDeleteMode;
  selectedCount.textContent = selected.length === 0
    ? `删除模式：点击已保存或哨兵录像选择，最近录像仅支持单独删除`
    : `已选择 ${formatNumber(selected.length)} / ${formatNumber(sessions.length)} 段`;
}

function createTrackRow(session, labelText, camera) {
  const row = document.createElement("div");
  row.className = "track-row";
  const label = document.createElement("span");
  label.className = "track-label";
  label.textContent = labelText;
  const track = document.createElement("div");
  track.className = "session-track";
  const hasCamera = !camera || session.cameras.includes(camera);
  const segment = document.createElement("button");
  segment.type = "button";
  segment.className = `track-segment continuous${hasCamera ? "" : " missing"}${session.complete ? "" : " incomplete"}`;
  segment.title = "点击时间轴可跳转到对应录像位置";
  segment.setAttribute("aria-label", segment.title);
  segment.addEventListener("click", (event) => {
    const bounds = track.getBoundingClientRect();
    const ratio = bounds.width > 0 ? Math.max(0, Math.min(1, (event.clientX-bounds.left)/bounds.width)) : 0;
    const position = Number(session.duration_seconds || 0)*ratio;
    openSessionPlayer(session, camera || preferredCamera(session), position, camera ? "single" : "composite");
  });
  track.append(segment);
  const eventPosition = sessionEventPosition(session);
  const eventCamera = eventCameraView(session.event_metadata);
  if (eventPosition !== null && (!camera || eventCamera === camera)) {
    const marker = document.createElement("button");
    marker.type = "button";
    marker.className = "event-track-marker";
    marker.style.left = `${Math.max(.5, Math.min(99.5, eventPosition/session.duration_seconds*100))}%`;
    marker.title = eventMarkerTitle(session.event_metadata);
    marker.setAttribute("aria-label", marker.title);
    marker.addEventListener("click", () => {
      openSessionPlayer(session, camera || preferredCamera(session), eventPosition, camera ? "single" : "composite");
    });
    track.append(marker);
  }
  row.append(label, track);
  return row;
}

async function openSessionPlayer(summary, camera, position, display) {
  try {
    const session = await request(`/api/v1/video-sessions/${encodeURIComponent(summary.id)}`);
    closePlayerMedia();
    activeSession = session;
    playbackWasPlaying = false;
    activeCamera = session.cameras.includes(camera) ? camera : preferredCamera(session);
    activeDisplay = display === "composite" ? normalizeDisplayMode(readPreference("teslabox-player-layout", "composite6")) : "single";
    resetPlaybackFallbackState();
    $("player-title").textContent = formatSessionRange(session.start_at, session.end_at);
    renderPlayerMeta(session);
    $("session-timeline").max = String(session.duration_seconds);
    $("session-timeline").value = String(position);
    $("total-time").textContent = formatDuration(session.duration_seconds);
    $("current-time").textContent = formatDuration(position);
    renderCameraSwitcher();
    renderSegmentMarkers();
    renderEventMarker();
    applyDisplayState();
    playerModal.hidden = false;
    document.body.style.overflow = "hidden";
    resetPlaybackDiagnostics();
    startPlaybackDiagnostics();
    await loadSessionPosition(position, false);
  } catch (error) {
    setNotice(`无法打开录像：${error.message}`, true);
  }
}

function renderCameraSwitcher() {
  const switcher = $("camera-switcher");
  switcher.replaceChildren();
  Object.entries(compositeLayouts).forEach(([display, layout]) => {
    const button = document.createElement("button");
    button.type = "button";
    button.textContent = layout.label;
    button.classList.toggle("active", activeDisplay === display);
    button.addEventListener("click", () => setDisplayMode(display));
    switcher.append(button);
  });
  cameraOrder.forEach((camera) => {
    const button = document.createElement("button");
    button.type = "button";
    button.textContent = cameraLabel(camera);
    button.classList.toggle("active", activeDisplay === "single" && camera === activeCamera);
    button.classList.toggle("unavailable", !activeSession.cameras.includes(camera));
    button.disabled = !activeSession.cameras.includes(camera);
    button.addEventListener("click", () => switchCamera(camera));
    switcher.append(button);
  });
  $("active-camera-label").textContent = cameraLabel(activeCamera);
}

function renderPlayerMeta(session) {
  const meta = $("player-meta");
  const badge = document.createElement("span");
  badge.className = `clip-badge clip-${clipTypeClass(session.clip_type)}`;
  badge.textContent = clipLabel(session.clip_type);
  meta.replaceChildren(badge, document.createTextNode(`${session.segment_count} 个片段 · 完整时长 ${formatDuration(session.duration_seconds)}`));
  if (session.event_metadata) {
    const context = document.createElement("span");
    context.className = "player-event-context";
    const dot = document.createElement("i");
    dot.className = "event-dot";
    context.append(dot, document.createTextNode(eventMetadataText(session.event_metadata)));
    meta.append(context);
  }
}

function renderSegmentMarkers() {
  const markers = $("segment-markers");
  markers.replaceChildren();
  activeSession.moments.forEach((moment) => {
    const marker = document.createElement("i");
    marker.style.flexGrow = String(Math.max(1, timelineMomentDuration(moment)));
    markers.append(marker);
  });
}

function renderEventMarker() {
  const layer = $("event-marker-layer");
  layer.replaceChildren();
  const position = sessionEventPosition(activeSession);
  if (position === null) return;
  if (activeDisplay === "single" && eventCameraView(activeSession.event_metadata) !== activeCamera) return;
  const marker = document.createElement("button");
  marker.type = "button";
  marker.className = "player-event-marker";
  marker.style.left = `${Math.max(.5, Math.min(99.5, position/activeSession.duration_seconds*100))}%`;
  marker.title = eventMarkerTitle(activeSession.event_metadata);
  marker.setAttribute("aria-label", marker.title);
  marker.addEventListener("click", () => {
    $("session-timeline").value = String(position);
    seekTimelinePosition();
  });
  layer.append(marker);
}

async function loadSessionPosition(position, autoplay) {
  if (!activeSession) return false;
  const requestID = ++timelinePositionRequestID;
  const token = ++loadToken;
  timelinePositionPending = true;
  requestedPosition = Math.max(0, Math.min(Number(position), Number(activeSession.duration_seconds)));
  try {
    if (isCompositeDisplay(activeDisplay)) return await loadCompositePosition(requestedPosition, autoplay, token);
    const selection = selectVideoAtPosition(activeSession, activeCamera, requestedPosition);
    $("session-timeline").value = String(requestedPosition);
    $("current-time").textContent = formatDuration(requestedPosition);
    if (!selection) {
      showPlayerMessage("这个时刻缺少当前视角录像");
      releaseCompositeAuxiliaryTracks();
      releaseVideoElement(sessionVideo);
      return false;
    }
    hidePlayerMessage();
    activeMomentIndex = selection.momentIndex;
    activeVideo = selection.video;
    setPlaybackTelemetryVideo(activeVideo, token);
    if (usesServerTranscode()) return await loadServerSingle(selection, autoplay, token);
    $("segment-status").textContent = `第 ${activeMomentIndex+1} / ${activeSession.moments.length} 分钟 · ${cameraLabel(activeCamera)}`;
    $("download-link").href = `/api/v1/videos/${encodeURIComponent(activeVideo.id)}/download`;
    const source = `/api/v1/videos/${encodeURIComponent(activeVideo.id)}/content`;
    delete sessionVideo.dataset.compositeKey;
    if (sessionVideo.dataset.videoId !== activeVideo.id || sessionVideo.getAttribute("src") !== source ||
        sessionVideo.readyState < 1 || sessionVideo.error) {
      sessionVideo.pause();
      sessionVideo.dataset.loadingToken = String(token);
      sessionVideo.dataset.videoId = activeVideo.id;
      sessionVideo.src = source;
      sessionVideo.load();
      const loaded = await waitForMetadata(sessionVideo, 8000);
      if (token !== loadToken) return false;
      finishSessionVideoLoad(token);
      if (!loaded) {
        reportPlaybackDiagnostic("load_failed", "单路原画视频元数据加载失败");
        releaseVideoElement(sessionVideo);
        showPlayerMessage("单路视频加载失败，请稍后重试");
        return false;
      }
    }
    if (token !== loadToken) return false;
    const localTime = Math.max(0, selection.localTime);
    if (Number.isFinite(sessionVideo.duration)) {
      sessionVideo.currentTime = Math.min(localTime, Math.max(0, sessionVideo.duration-.05));
    } else {
      sessionVideo.currentTime = localTime;
    }
    if (autoplay) sessionVideo.play().catch(() => showPlayerMessage("浏览器阻止了自动续播，请点播放继续"));
    return true;
  } finally {
    if (requestID === timelinePositionRequestID) timelinePositionPending = false;
  }
}

async function loadCompositePosition(position, autoplay, token) {
  const selection = selectMomentAtPosition(activeSession, position);
  if (!selection || selection.moment.videos.length === 0) {
    showPlayerMessage("这个时刻没有可播放的摄像头画面");
    releaseCompositeAuxiliaryTracks();
    releaseVideoElement(sessionVideo);
    return false;
  }
  hidePlayerMessage();
  activeMomentIndex = selection.momentIndex;
  const layout = compositeLayouts[activeDisplay] || compositeLayouts.composite6;
  const layoutVideos = layout.cameras
    .map((camera) => selection.moment.videos.find((video) => video.camera === camera))
    .filter(Boolean);
  if (layoutVideos.length === 0) {
    showPlayerMessage("这个时刻缺少当前布局需要的摄像头画面");
    releaseCompositeAuxiliaryTracks();
    releaseVideoElement(sessionVideo);
    return false;
  }
  const masterVideo = layoutVideos.find((video) => video.camera === "front") || layoutVideos[0];
  activeCamera = masterVideo.camera;
  activeVideo = masterVideo;
  setPlaybackTelemetryVideo(activeVideo, token);
  if (usesServerComposite()) {
    return await loadServerComposite(selection, layout, masterVideo, autoplay, token);
  }
  delete sessionVideo.dataset.compositeKey;
  // 换段或拖动时先释放上一批 Range 请求，避免旧请求占满浏览器连接并污染新一批播放器。
  releaseCompositeAuxiliaryTracks();
  resetCompositePlaceholders();
  const nextTracks = new Map();
  layoutVideos.forEach((video) => {
    const element = video.id === masterVideo.id ? sessionVideo : compositeVideoElement(video.camera);
    element.muted = true;
    element.preload = "metadata";
    attachCompositeElement(video.camera, element);
    nextTracks.set(video.camera, {element, video, lastHardSync: 0});
  });
  pendingCompositeTracks = nextTracks;
  const preparations = [...nextTracks.values()].map((track) =>
    prepareVideoElement(track.element, track.video, selection.absolute, token));
  const prepared = await Promise.all(preparations);
  // 新一次拖动会接管并释放本批辅助元素，过期任务不得再暂停或清理当前播放器。
  if (token !== loadToken) return false;
  if (pendingCompositeTracks === nextTracks) pendingCompositeTracks = new Map();
  const trackEntries = [...nextTracks.entries()];
  const failedTracks = trackEntries.filter((_, index) => !prepared[index]);
  const masterLoaded = prepared[trackEntries.findIndex(([camera]) => camera === masterVideo.camera)];
  if (!masterLoaded || (failedTracks.length > 0 && canAutomaticallyFallback())) {
    reportCompositeLoadFailures(failedTracks);
    releaseCompositeTrackMap(nextTracks);
    releaseVideoElement(sessionVideo);
    if (canAutomaticallyFallback()) {
      showPlayerMessage("原画多路视频加载失败，正在尝试压缩画质");
      requestPlaybackFallback("原画多路视频加载失败");
    } else {
      showPlayerMessage("原画多路视频加载失败，请重试或切换“自动”画质");
    }
    return false;
  }
  if (failedTracks.length > 0) {
    reportCompositeLoadFailures(failedTracks);
    failedTracks.forEach(([camera, track]) => {
      releaseVideoElement(track.element);
      nextTracks.delete(camera);
      showCompositeTrackLoadFailure(camera);
    });
    showPlayerMessage(`部分原画视角加载失败，已保留其余 ${nextTracks.size} 路；切换布局或再次拖动可重试`);
  }
  applyCompositeFrameGeometry(nextTracks.get(masterVideo.camera)?.element);
  clearUnusedCompositeTracks(nextTracks);
  compositeTracks = nextTracks;
  $("segment-status").textContent = `第 ${activeMomentIndex+1} / ${activeSession.moments.length} 分钟 · ${layout.label} · ${nextTracks.size} 路可用 · ${qualityLabel(effectivePlaybackQuality)}`;
  $("download-link").href = `/api/v1/videos/${encodeURIComponent(masterVideo.id)}/download`;
  renderCameraSwitcher();
  if (autoplay) await playCompositeTracks();
  return true;
}

async function loadServerComposite(selection, layout, masterVideo, autoplay, token) {
  releaseCompositeAuxiliaryTracks();
  const source = `/api/v1/video-groups/${encodeURIComponent(selection.moment.id)}/composite?layout=${encodeURIComponent(activeDisplay)}&quality=${encodeURIComponent(effectivePlaybackQuality)}`;
  const sourceKey = `${selection.moment.id}:${activeDisplay}:${effectivePlaybackQuality}`;
  if (sessionVideo.dataset.compositeKey !== sourceKey || sessionVideo.readyState < 1 || sessionVideo.error) {
    sessionVideo.pause();
    sessionVideo.dataset.loadingToken = String(token);
    sessionVideo.dataset.compositeKey = sourceKey;
    sessionVideo.dataset.videoId = masterVideo.id;
    sessionVideo.src = source;
    sessionVideo.load();
    const loaded = await waitForMetadata(sessionVideo, 15000);
    if (token !== loadToken) return false;
    finishSessionVideoLoad(token);
    if (!loaded) {
      reportPlaybackDiagnostic("load_failed", "板端合成视频元数据加载失败");
      releaseVideoElement(sessionVideo);
      requestPlaybackFallback("板端合成视频加载失败");
      return false;
    }
  }
  if (token !== loadToken) return false;
  const localTime = Math.max(0, (selection.absolute-new Date(selection.moment.recorded_at).getTime())/1000);
  sessionVideo.currentTime = Number.isFinite(sessionVideo.duration)
    ? Math.min(localTime, Math.max(0, sessionVideo.duration-.05))
    : localTime;
  compositeTracks = new Map([[masterVideo.camera, {element:sessionVideo, video:masterVideo, lastHardSync:0}]]);
  $("segment-status").textContent = `第 ${activeMomentIndex+1} / ${activeSession.moments.length} 分钟 · ${layout.label} · ${qualityLabel(effectivePlaybackQuality)}`;
  $("download-link").href = `/api/v1/videos/${encodeURIComponent(masterVideo.id)}/download`;
  applyDisplayState();
  applyServerCompositeFrameGeometry();
  renderCameraSwitcher();
  if (autoplay) sessionVideo.play().catch(() => showPlayerMessage("浏览器阻止了自动续播，请点播放继续"));
  return true;
}

async function loadServerSingle(selection, autoplay, token) {
  releaseCompositeAuxiliaryTracks();
  const source = `/api/v1/video-groups/${encodeURIComponent(selection.moment.id)}/composite?layout=single&camera=${encodeURIComponent(activeCamera)}&quality=${encodeURIComponent(effectivePlaybackQuality)}`;
  const sourceKey = `${selection.moment.id}:single:${activeCamera}:${effectivePlaybackQuality}`;
  delete sessionVideo.dataset.videoId;
  if (sessionVideo.dataset.compositeKey !== sourceKey || sessionVideo.readyState < 1 || sessionVideo.error) {
    sessionVideo.pause();
    sessionVideo.dataset.loadingToken = String(token);
    sessionVideo.dataset.compositeKey = sourceKey;
    sessionVideo.src = source;
    sessionVideo.load();
    const loaded = await waitForMetadata(sessionVideo, 15000);
    if (token !== loadToken) return false;
    finishSessionVideoLoad(token);
    if (!loaded) {
      reportPlaybackDiagnostic("load_failed", "单路转码视频元数据加载失败");
      releaseVideoElement(sessionVideo);
      showPlayerMessage("单路转码视频加载失败，请稍后重试");
      return false;
    }
  }
  if (token !== loadToken) return false;
  activeVideo = selection.video;
  setPlaybackTelemetryVideo(activeVideo, token);
  const localTime = Math.max(0, selection.localTime);
  sessionVideo.currentTime = Number.isFinite(sessionVideo.duration)
    ? Math.min(localTime, Math.max(0, sessionVideo.duration-.05))
    : localTime;
  compositeTracks = new Map([[activeCamera, {element:sessionVideo, video:activeVideo, lastHardSync:0}]]);
  $("segment-status").textContent = `第 ${activeMomentIndex+1} / ${activeSession.moments.length} 分钟 · ${cameraLabel(activeCamera)} · ${qualityLabel(effectivePlaybackQuality)}`;
  $("download-link").href = `/api/v1/videos/${encodeURIComponent(activeVideo.id)}/download`;
  applyDisplayState();
  if (autoplay) sessionVideo.play().catch(() => showPlayerMessage("浏览器阻止了自动续播，请点播放继续"));
  return true;
}

function selectMomentAtPosition(session, position) {
  if (!session || session.moments.length === 0) return null;
  const boundedPosition = Math.max(0, Math.min(Number(position) || 0, Number(session.duration_seconds) || 0));
  let momentIndex = 0;
  for (let index=0; index<session.moments.length; index++) {
    const moment = session.moments[index];
    const start = timelineMomentStart(moment);
    const end = start+timelineMomentDuration(moment);
    if (boundedPosition >= start && (boundedPosition < end || index === session.moments.length-1)) {
      momentIndex = index;
      break;
    }
    if (boundedPosition >= start) momentIndex = index;
  }
  const moment = session.moments[momentIndex];
  const localTime = Math.max(0, Math.min(timelineMomentDuration(moment), boundedPosition-timelineMomentStart(moment)));
  const absolute = new Date(moment.recorded_at).getTime()+localTime*1000;
  return {absolute, localTime, momentIndex, moment};
}

function timelineMomentStart(moment) {
  const start = Number(moment?.timeline_start_seconds);
  return Number.isFinite(start) && start >= 0 ? start : 0;
}

function timelineMomentDuration(moment) {
  const duration = Number(moment?.duration_seconds);
  return Number.isFinite(duration) && duration > 0 ? duration : 60;
}

function compositeVideoElement(camera) {
  const existing = compositeTracks.get(camera);
  if (existing && existing.element !== sessionVideo) return existing.element;
  const element = document.createElement("video");
  element.preload = "auto";
  element.playsInline = true;
  element.muted = true;
  return element;
}

function ensureCompositeTiles() {
  const layout = compositeLayouts[activeDisplay] || compositeLayouts.composite6;
  if (compositeGrid.dataset.layout === activeDisplay && compositeGrid.childElementCount === layout.cameras.length) return;
  compositeGrid.replaceChildren();
  compositeGrid.dataset.layout = activeDisplay;
  layout.cameras.forEach((camera) => {
    const tile = document.createElement("div");
    tile.className = "composite-tile";
    tile.dataset.camera = camera;
    const label = document.createElement("span");
    label.className = "composite-tile-label";
    label.textContent = cameraLabel(camera);
    const placeholder = document.createElement("span");
    placeholder.className = "composite-tile-placeholder";
    placeholder.textContent = "该分钟缺少画面";
    tile.append(label, placeholder);
    compositeGrid.append(tile);
  });
}

function resetCompositePlaceholders() {
  ensureCompositeTiles();
  compositeGrid.querySelectorAll(".composite-tile-placeholder").forEach((placeholder) => {
    placeholder.textContent = "该分钟缺少画面";
  });
}

function attachCompositeElement(camera, element) {
  ensureCompositeTiles();
  const tile = compositeGrid.querySelector(`[data-camera="${camera}"]`);
  tile.querySelector(".composite-tile-placeholder").hidden = true;
  tile.append(element);
  element.hidden = false;
}

function showCompositeTrackLoadFailure(camera) {
  const placeholder = compositeGrid.querySelector(`[data-camera="${camera}"] .composite-tile-placeholder`);
  if (!placeholder) return;
  placeholder.textContent = "画面加载失败，可切换布局重试";
  placeholder.hidden = false;
}

function reportCompositeLoadFailures(failedTracks) {
  const details = failedTracks.map(([camera, track]) =>
    `${cameraLabel(camera)}(错误 ${track.element.error?.code || 0}/状态 ${track.element.readyState || 0})`);
  reportPlaybackDiagnostic("load_failed", `原画多路视频元数据加载失败${details.length ? `：${details.join("、")}` : ""}`);
}

async function prepareVideoElement(element, video, absolute, token) {
  const source = `/api/v1/videos/${encodeURIComponent(video.id)}/content`;
  if (element.dataset.videoId !== video.id || element.getAttribute("src") !== source ||
      element.readyState < 1 || element.error) {
    element.pause();
    if (element === sessionVideo) element.dataset.loadingToken = String(token);
    element.dataset.videoId = video.id;
    element.src = source;
    element.load();
    const loaded = await waitForMetadata(element, 12000);
    if (token !== loadToken || element.dataset.videoId !== video.id ||
        element.getAttribute("src") !== source) return false;
    if (element === sessionVideo) finishSessionVideoLoad(token);
    if (!loaded) return false;
  }
  if (token !== loadToken) return false;
  const localTime = Math.max(0, (absolute-new Date(video.recorded_at).getTime())/1000);
  element.currentTime = Number.isFinite(element.duration)
    ? Math.min(localTime, Math.max(0, element.duration-.05))
    : localTime;
  return !element.error;
}

function clearUnusedCompositeTracks(nextTracks) {
  compositeTracks.forEach((track, camera) => {
    if (nextTracks.has(camera) || track.element === sessionVideo) return;
    releaseVideoElement(track.element);
  });
  const layout = compositeLayouts[activeDisplay] || compositeLayouts.composite6;
  layout.cameras.forEach((camera) => {
    if (nextTracks.has(camera)) return;
    const placeholder = compositeGrid.querySelector(`[data-camera="${camera}"] .composite-tile-placeholder`);
    if (placeholder) placeholder.hidden = false;
  });
}

function selectVideoAtPosition(session, camera, position) {
  const momentSelection = selectMomentAtPosition(session, position);
  if (!momentSelection) return null;
  const {absolute, momentIndex, moment} = momentSelection;
  const video = moment.videos.find((item) => item.camera === camera);
  if (!video) return null;
  return {
    ...momentSelection, video, momentIndex,
    localTime: (absolute-new Date(video.recorded_at).getTime())/1000,
  };
}

async function switchCamera(camera) {
  if (!activeSession || (activeDisplay === "single" && camera === activeCamera)) return;
  const position = currentSessionPosition();
  const autoplay = !sessionVideo.paused;
  activeCamera = camera;
  if (isCompositeDisplay(activeDisplay)) {
    sessionVideo.pause();
    activeDisplay = "single";
    resetPlaybackFallbackState();
    releaseCompositeAuxiliaryTracks();
  }
  pauseCompositeTracks();
  applyDisplayState();
  renderCameraSwitcher();
  renderEventMarker();
  await loadSessionPosition(position, autoplay);
}

function togglePlayback() {
  if (!activeSession) return;
  if (!sessionVideo.src) {
    loadSessionPosition(Number($("session-timeline").value), true);
    return;
  }
  if (isCompositeDisplay(activeDisplay) && !usesServerComposite()) {
    if (sessionVideo.paused) {
      playbackWasPlaying = true;
      playCompositeTracks();
    } else {
      playbackWasPlaying = false;
      pauseCompositeTracks();
    }
    return;
  }
  if (sessionVideo.paused) {
    playbackWasPlaying = true;
    sessionVideo.play().catch(() => showPlayerMessage("请再次点击播放"));
  } else {
    playbackWasPlaying = false;
    sessionVideo.pause();
  }
}

function previewTimelinePosition() {
  $("current-time").textContent = formatDuration(Number($("session-timeline").value));
}

function seekTimelinePosition() {
  const autoplay = !sessionVideo.paused;
  if (isCompositeDisplay(activeDisplay) && !usesServerComposite()) pauseCompositeTracks();
  else sessionVideo.pause();
  loadSessionPosition(Number($("session-timeline").value), autoplay);
}

function updateUnifiedTimeline() {
  if (!activeSession || !activeVideo || sessionVideo.seeking || timelinePositionPending) return;
  const moment = activeSession.moments[activeMomentIndex];
  if (!moment) return;
  const position = Math.max(0, timelineMomentStart(moment) +
    (new Date(activeVideo.recorded_at)-new Date(moment.recorded_at))/1000 + sessionVideo.currentTime);
  requestedPosition = Math.min(position, activeSession.duration_seconds);
  $("session-timeline").value = String(requestedPosition);
  $("current-time").textContent = formatDuration(requestedPosition);
  renderPlaybackTelemetry();
  if (isCompositeDisplay(activeDisplay) && !usesServerComposite()) synchronizeCompositeTracks();
}

function setPlaybackTelemetryVideo(video, token) {
  telemetryAbortController?.abort();
  telemetryAbortController = null;
  activeTelemetry = null;
  if (!video?.id) {
    renderPlaybackTelemetry();
    return;
  }
  const cached = telemetryByVideo.get(video.id);
  if (cached) {
    activeTelemetry = cached;
    renderPlaybackTelemetry();
    return;
  }
  const controller = new AbortController();
  telemetryAbortController = controller;
  renderPlaybackTelemetry();
  request(`/api/v1/videos/${encodeURIComponent(video.id)}/sei`, {signal:controller.signal, loading:false}).then((timeline) => {
    if (controller.signal.aborted || token !== loadToken || activeVideo?.id !== video.id) return;
    if (telemetryByVideo.size >= 24) telemetryByVideo.clear();
    telemetryByVideo.set(video.id, timeline);
    activeTelemetry = timeline;
    renderPlaybackTelemetry();
  }).catch((error) => {
    if (controller.signal.aborted || token !== loadToken || activeVideo?.id !== video.id) return;
    activeTelemetry = {available:false, message:`Tesla SEI 信息暂不可用：${error.message}`, samples:[]};
    renderPlaybackTelemetry();
  });
}

function renderPlaybackTelemetry() {
  const container = $("playback-telemetry");
  if (!activeVideo || playerModal.hidden) {
    container.hidden = true;
    return;
  }
  container.hidden = false;
  $("playback-timestamp").textContent = formatRecordingTimestamp(activeVideo.recorded_at, Math.max(0, sessionVideo.currentTime || 0));
  if (!activeTelemetry) {
    $("playback-sei").textContent = "正在读取 Tesla SEI 信息…";
    return;
  }
  if (!activeTelemetry.available) {
    $("playback-sei").textContent = activeTelemetry.message || "该录像没有 Tesla SEI 信息";
    return;
  }
  const samples = Array.isArray(activeTelemetry.samples) ? activeTelemetry.samples : [];
  const currentMS = Math.max(0, Math.round((sessionVideo.currentTime || 0)*1000));
  let selected = samples[0];
  for (const sample of samples) {
    if (Number(sample.offset_ms || 0) > currentMS) break;
    selected = sample;
  }
  $("playback-sei").textContent = selected?.text || "该录像没有可显示的 Tesla SEI 字段";
}

function formatRecordingTimestamp(recordedAt, seconds) {
  const base = new Date(recordedAt);
  if (!Number.isFinite(base.getTime())) return "录像时间未知";
  const source = String(recordedAt || "");
  const match = source.match(/([+-])(\d{2}):(\d{2})$/);
  let offsetMS = 0;
  if (match) {
    const direction = match[1] === "+" ? 1 : -1;
    offsetMS = direction*(Number(match[2])*60+Number(match[3]))*60*1000;
  }
  const value = new Date(base.getTime()+Math.max(0, seconds)*1000+offsetMS);
  const year = value.getUTCFullYear();
  const month = String(value.getUTCMonth()+1).padStart(2, "0");
  const day = String(value.getUTCDate()).padStart(2, "0");
  const hour = String(value.getUTCHours()).padStart(2, "0");
  const minute = String(value.getUTCMinutes()).padStart(2, "0");
  const second = String(value.getUTCSeconds()).padStart(2, "0");
  return `${year}-${month}-${day} ${hour}:${minute}:${second}`;
}

function advanceToNextSegment() {
  if (!activeSession) return;
  const next = activeMomentIndex+1;
  if (next >= activeSession.moments.length) {
    $("session-timeline").value = String(activeSession.duration_seconds);
    $("current-time").textContent = formatDuration(activeSession.duration_seconds);
    $("segment-status").textContent = "本段录像播放完毕";
    return;
  }
  const nextPosition = timelineMomentStart(activeSession.moments[next]);
  loadSessionPosition(nextPosition, true);
}

function updatePlayButton() {
  $("play-toggle").textContent = sessionVideo.paused ? "▶" : "Ⅱ";
  $("play-toggle").setAttribute("aria-label", sessionVideo.paused ? "播放" : "暂停");
}

function currentSessionPosition() {
  if (timelinePositionPending) return requestedPosition;
  if (!activeSession || !activeVideo) return requestedPosition;
  const moment = activeSession.moments[activeMomentIndex];
  if (!moment) return requestedPosition;
  return Math.max(0, Math.min(activeSession.duration_seconds,
    timelineMomentStart(moment) + (new Date(activeVideo.recorded_at)-new Date(moment.recorded_at))/1000 + sessionVideo.currentTime));
}

async function setDisplayMode(display) {
  if (!(display === "single" || compositeLayouts[display]) || display === activeDisplay || !activeSession) return;
  const position = currentSessionPosition();
  const autoplay = !sessionVideo.paused;
  pauseCompositeTracks();
  sessionVideo.pause();
  activeDisplay = display;
  resetPlaybackFallbackState();
  if (isCompositeDisplay(display)) writePreference("teslabox-player-layout", display);
  if (display === "single") releaseCompositeAuxiliaryTracks();
  applyDisplayState();
  renderCameraSwitcher();
  renderEventMarker();
  await loadSessionPosition(position, autoplay);
}

function applyDisplayState() {
  const composite = isCompositeDisplay(activeDisplay);
  const serverComposite = composite && usesServerComposite();
  mainVideoFrame.dataset.layout = composite ? activeDisplay : "single";
  if (!composite || serverComposite) resetCompositeFrameGeometry();
  compositeGrid.hidden = !composite || serverComposite;
  if (composite && !serverComposite) {
    ensureCompositeTiles();
    sessionVideo.hidden = true;
  } else {
    mainVideoFrame.insertBefore(sessionVideo, $("active-camera-label"));
    sessionVideo.hidden = false;
    sessionVideo.preload = serverComposite ? "auto" : "metadata";
    sessionVideo.muted = false;
  }
  $("active-camera-label").hidden = composite;
}

function openExportModal() {
  if (!activeSession) return;
  const modal = $("export-modal");
  modal.hidden = false;
  $("export-error").textContent = "";
  if (exportTask && exportTask.request?.session_id === activeSession.id) {
    restoreExportSelection(exportTask.request);
    setExportMode(exportTask.request?.mode === "source" ? "source" : "clip");
    renderExportTask(exportTask);
    if (["queued", "running"].includes(exportTask.state)) scheduleExportPoll(0);
    return;
  }
  exportTask = null;
  const position = Math.min(currentSessionPosition(), Math.max(0, activeSession.duration_seconds-1));
  $("export-start").max = String(Math.max(0, activeSession.duration_seconds-1));
  $("export-start").value = position.toFixed(1);
  $("export-duration").max = String(Math.min(maximumExportDurationSeconds, activeSession.duration_seconds-position));
  $("export-duration").value = Math.min(60, Math.max(1, activeSession.duration_seconds-position)).toFixed(1);
  $("export-layout").value = isCompositeDisplay(activeDisplay) ? activeDisplay : "single";
  $("export-camera").value = activeCamera;
  $("export-quality").value = effectivePlaybackQuality === "original" && !isCompositeDisplay(activeDisplay) ? "original" : "high";
  $("export-burn-timestamp").checked = true;
  $("export-burn-sei").checked = false;
  resetSourceExportSelection();
  setExportMode("clip");
  renderExportOptions();
  resetExportTaskUI();
}

function restoreExportSelection(request) {
  if (!activeSession || request?.mode !== "source") return;
  sourceExportSessionID = activeSession.id;
  sourceExportMomentIDs.clear();
  sourceExportCameras.clear();
  (request.moment_ids || []).forEach((id) => sourceExportMomentIDs.add(id));
  (request.cameras || []).forEach((camera) => sourceExportCameras.add(camera));
}

function resetSourceExportSelection() {
  if (!activeSession) return;
  sourceExportSessionID = activeSession.id;
  sourceExportMomentIDs.clear();
  sourceExportCameras.clear();
  const current = activeSession.moments?.[activeMomentIndex] || selectMomentAtPosition(activeSession, currentSessionPosition())?.moment;
  if (current?.id) sourceExportMomentIDs.add(current.id);
  const preferred = isCompositeDisplay(activeDisplay) ? compositeLayouts[activeDisplay]?.cameras : [activeCamera];
  (preferred || []).forEach((camera) => sourceExportCameras.add(camera));
}

function setExportMode(mode) {
  exportMode = mode === "source" ? "source" : "clip";
  const source = exportMode === "source";
  $("export-mode-clip").classList.toggle("active", !source);
  $("export-mode-source").classList.toggle("active", source);
  $("export-mode-clip").setAttribute("aria-selected", String(!source));
  $("export-mode-source").setAttribute("aria-selected", String(source));
  $("export-clip-options").hidden = source;
  $("export-source-options").hidden = !source;
  $("export-source-direct").hidden = !source;
  $("export-title").textContent = source ? "导出多路原画" : "导出剪辑视频";
  if (source) {
    if (sourceExportSessionID !== activeSession?.id) resetSourceExportSelection();
    renderSourceExportOptions();
  } else {
    renderExportOptions();
  }
  updateExportSubmitLabel();
}

function renderExportOptions() {
  if (!activeSession) return;
  Object.entries(compositeLayouts).forEach(([layout, definition]) => {
    const option = $("export-layout").querySelector(`option[value="${layout}"]`);
    option.disabled = definition.cameras.some((camera) => !activeSession.cameras.includes(camera));
  });
  if ($("export-layout").selectedOptions[0]?.disabled) $("export-layout").value = "single";
  const cameraSelect = $("export-camera");
  const selectedCamera = cameraSelect.value || activeCamera;
  cameraSelect.replaceChildren();
  cameraOrder.forEach((camera) => {
    if (!activeSession.cameras.includes(camera)) return;
    const option = document.createElement("option");
    option.value = camera;
    option.textContent = cameraLabel(camera);
    cameraSelect.append(option);
  });
  if ([...cameraSelect.options].some((option) => option.value === selectedCamera)) cameraSelect.value = selectedCamera;
  $("export-camera-field").hidden = $("export-layout").value !== "single";
  const originalOption = $("export-quality").querySelector('option[value="original"]');
  originalOption.disabled = $("export-layout").value !== "single";
  if (originalOption.disabled && $("export-quality").value === "original") $("export-quality").value = "high";
  $("export-quality-help").textContent = $("export-layout").value === "single"
    ? "高保真合成使用恒定质量硬编；单路原画保留源分辨率，但跨分钟或精确裁剪仍需重新编码。"
    : "高保真多路合成会导出 H.265（HEVC）MP4，以避开 RKMPP H.264 多路残块；请使用支持 HEVC 的本地播放器。需要最广兼容或与网页原画完全一致时，请切换到“多路原画”下载原始文件和离线浏览页。";
}

function renderSourceExportOptions() {
  if (!activeSession) return;
  const moments = activeSession.moments || [];
  const momentRoot = $("export-source-moments");
  momentRoot.replaceChildren();
  moments.forEach((moment, index) => {
    const button = document.createElement("button");
    const selected = sourceExportMomentIDs.has(moment.id);
    button.type = "button";
    button.className = "export-source-moment";
    button.classList.toggle("selected", selected);
    button.setAttribute("aria-pressed", String(selected));
    const available = (moment.videos || []).length;
    const start = formatDate(moment.recorded_at);
    const duration = Math.max(1, Math.round(timelineMomentDuration(moment)));
    const title = document.createElement("strong");
    title.textContent = `第 ${index+1} 分钟 · ${start}`;
    const detail = document.createElement("span");
    detail.textContent = `${available} 路可用 · ${formatDuration(duration)}`;
    button.append(title, detail);
    button.addEventListener("click", () => {
      if (sourceExportMomentIDs.has(moment.id)) sourceExportMomentIDs.delete(moment.id);
      else sourceExportMomentIDs.add(moment.id);
      renderSourceExportOptions();
    });
    momentRoot.append(button);
  });

  const cameraRoot = $("export-source-cameras");
  cameraRoot.replaceChildren();
  cameraOrder.forEach((camera) => {
    const count = sourceExportFileSummary().byCamera.get(camera) || 0;
    const button = document.createElement("button");
    const selected = sourceExportCameras.has(camera);
    button.type = "button";
    button.className = "export-source-camera";
    button.classList.toggle("selected", selected);
    button.classList.toggle("unavailable", count === 0);
    button.setAttribute("aria-pressed", String(selected));
    button.textContent = `${cameraLabel(camera)} · ${count} 个`;
    button.addEventListener("click", () => {
      if (sourceExportCameras.has(camera)) sourceExportCameras.delete(camera);
      else sourceExportCameras.add(camera);
      renderSourceExportOptions();
    });
    cameraRoot.append(button);
  });

  const summary = sourceExportFileSummary();
  const missing = sourceExportMomentIDs.size*sourceExportCameras.size-summary.count;
  let text = `已选 ${sourceExportMomentIDs.size} 分钟 · ${sourceExportCameras.size} 路视角 · ${summary.count} 个源文件 · ${formatBytes(summary.bytes)}`;
  if (missing > 0) text += ` · ${missing} 个视角在所选分钟中缺失，会自动跳过`;
  $("export-source-summary").textContent = text;
}

function sourceExportFileSummary() {
  const byCamera = new Map();
  let count = 0;
  let bytes = 0;
  selectedSourceVideos().forEach((video) => {
    count++;
    bytes += Math.max(0, Number(video.size || 0));
    byCamera.set(video.camera, (byCamera.get(video.camera) || 0)+1);
  });
  return {count, bytes, byCamera};
}

function selectedSourceVideos() {
  if (!activeSession) return [];
  const selected = [];
  const seen = new Set();
  activeSession.moments?.forEach((moment) => {
    if (!sourceExportMomentIDs.has(moment.id)) return;
    (moment.videos || []).forEach((video) => {
      if (!sourceExportCameras.has(video.camera) || !video.id || seen.has(video.id)) return;
      seen.add(video.id);
      selected.push(video);
    });
  });
  return selected;
}

function selectCurrentSourceMoment() {
  if (!activeSession) return;
  const moment = activeSession.moments?.[activeMomentIndex] || selectMomentAtPosition(activeSession, currentSessionPosition())?.moment;
  sourceExportMomentIDs.clear();
  if (moment?.id) sourceExportMomentIDs.add(moment.id);
  renderSourceExportOptions();
}

function selectAllSourceMoments() {
  activeSession?.moments?.forEach((moment) => sourceExportMomentIDs.add(moment.id));
  renderSourceExportOptions();
}

function clearSourceMomentSelection() {
  sourceExportMomentIDs.clear();
  renderSourceExportOptions();
}

function selectAllSourceCameras() {
  sourceExportCameras.clear();
  cameraOrder.forEach((camera) => sourceExportCameras.add(camera));
  renderSourceExportOptions();
}

function selectSourceCamerasForCurrentLayout() {
  sourceExportCameras.clear();
  const cameras = isCompositeDisplay(activeDisplay) ? compositeLayouts[activeDisplay]?.cameras : [activeCamera];
  (cameras || []).forEach((camera) => sourceExportCameras.add(camera));
  renderSourceExportOptions();
}

function downloadSourceFiles() {
  const videos = selectedSourceVideos();
  $("export-error").textContent = "";
  if (videos.length === 0) {
    $("export-error").textContent = "请至少选择一个包含录像的分钟和摄像头视角";
    return;
  }
  const button = $("export-source-direct");
  button.disabled = true;
  button.classList.add("is-loading");
  button.setAttribute("aria-busy", "true");
  const anchors = [];
  videos.forEach((video) => {
    const anchor = document.createElement("a");
    anchor.href = `/api/v1/videos/${encodeURIComponent(video.id)}/download`;
    anchor.download = "";
    anchor.hidden = true;
    document.body.append(anchor);
    anchors.push(anchor);
    anchor.click();
  });
  window.setTimeout(() => anchors.forEach((anchor) => anchor.remove()), 0);
  window.setTimeout(() => {
    button.disabled = false;
    button.classList.remove("is-loading");
    button.removeAttribute("aria-busy");
  }, 1200);
  $("export-error").textContent = `已立即向浏览器发起 ${videos.length} 个原始 MP4 下载请求。浏览器如询问，请允许多个文件下载；若选中文件较多，建议使用“打包多路原画”后下载一个压缩包。`;
}

async function createExportTask(event) {
  event.preventDefault();
  if (!activeSession || ["queued", "running"].includes(exportTask?.state)) return;
  if (exportMode === "source") {
    const summary = sourceExportFileSummary();
    $("export-error").textContent = "";
    if (sourceExportMomentIDs.size === 0 || sourceExportCameras.size === 0 || summary.count === 0) {
      $("export-error").textContent = "请至少选择一个包含录像的分钟和摄像头视角";
      return;
    }
    $("export-submit").disabled = true;
    try {
      exportTask = await request("/api/v1/exports", {
        method: "POST", headers: {"Content-Type":"application/json"},
        body: JSON.stringify({mode:"source", session_id:activeSession.id, moment_ids:[...sourceExportMomentIDs], cameras:[...sourceExportCameras]}),
      });
      renderExportTask(exportTask);
      scheduleExportPoll(500);
    } catch (error) {
      $("export-error").textContent = error.message;
      $("export-submit").disabled = false;
    }
    return;
  }
  const layout = $("export-layout").value;
  const cameras = layout === "single" ? [$("export-camera").value] : (compositeLayouts[layout]?.cameras || []);
  const start = Number($("export-start").value);
  const duration = Number($("export-duration").value);
  $("export-error").textContent = "";
  if (cameras.length === 0) {
    $("export-error").textContent = "请至少选择一个摄像头视角";
    return;
  }
  if (!Number.isFinite(start) || !Number.isFinite(duration) || start < 0 || duration < 1 || duration > maximumExportDurationSeconds || start+duration > activeSession.duration_seconds+.01) {
    $("export-error").textContent = "导出范围必须位于当前完整录像内，且最长 2 分钟";
    return;
  }
  $("export-submit").disabled = true;
  try {
    playbackWasPlaying = false;
    pauseCompositeTracks();
    releaseCompositeAuxiliaryTracks();
    releaseVideoElement(sessionVideo);
    showPlayerMessage("导出任务已接管硬件媒体资源，当前播放已停止");
    exportTask = await request("/api/v1/exports", {
      method: "POST", headers: {"Content-Type":"application/json"},
      body: JSON.stringify({
        mode:"clip", session_id:activeSession.id, start_seconds:start, duration_seconds:duration, layout, cameras, quality:$("export-quality").value,
        burn_timestamp:$("export-burn-timestamp").checked, burn_sei:$("export-burn-sei").checked,
      }),
    });
    renderExportTask(exportTask);
    scheduleExportPoll(1000);
  } catch (error) {
    $("export-error").textContent = error.message;
    $("export-submit").disabled = false;
  }
}

function scheduleExportPoll(delay = 1500) {
  window.clearTimeout(exportPollTimer);
  if (!exportTask || !["queued", "running"].includes(exportTask.state)) return;
  exportPollTimer = window.setTimeout(pollExportTask, delay);
}

async function pollExportTask() {
  if (!exportTask) return;
  try {
    exportTask = await request(`/api/v1/exports/${encodeURIComponent(exportTask.id)}`);
    renderExportTask(exportTask);
  } catch (error) {
    $("export-error").textContent = error.message;
  }
  scheduleExportPoll();
}

async function cancelExportTask() {
  if (!exportTask || !["queued", "running"].includes(exportTask.state)) return;
  $("export-cancel").disabled = true;
  try {
    exportTask = await request(`/api/v1/exports/${encodeURIComponent(exportTask.id)}`, {method:"DELETE"});
    renderExportTask(exportTask);
  } catch (error) {
    $("export-error").textContent = error.message;
  } finally {
    $("export-cancel").disabled = false;
  }
}

function renderExportTask(task) {
  const source = task.request?.mode === "source";
  const labels = {queued:"正在排队", running:source ? "正在打包" : "正在导出", completed:source ? "打包完成" : "导出完成", failed:source ? "打包失败" : "导出失败", canceled:"已取消"};
  const running = ["queued", "running"].includes(task.state);
  $("export-task-status").hidden = false;
  $("export-state").textContent = labels[task.state] || task.state;
  $("export-progress").value = task.progress_percent || 0;
  const estimate = task.estimated_bytes ? ` · 预计 ${formatBytes(task.estimated_bytes)}` : "";
  const files = source && task.file_count ? ` · ${task.file_count} 个源文件` : "";
  $("export-detail").textContent = task.error || (running ? `已完成 ${task.progress_percent || 0}%${files}${estimate}` : `可以修改参数后再次导出${files}${estimate}`);
  $("export-cancel").hidden = !running;
  $("export-download").hidden = task.state !== "completed" || !task.download_ready;
  $("export-download").href = task.download_ready ? `/api/v1/exports/${encodeURIComponent(task.id)}/download` : "#";
  $("export-download").textContent = source ? "下载压缩包" : "下载视频";
  $("export-source-direct").hidden = !source;
  $("export-submit").disabled = running;
  updateExportSubmitLabel(task);
}

function updateExportSubmitLabel(task = exportTask) {
  const source = (task?.request?.mode || exportMode) === "source";
  $("export-submit").textContent = task?.state === "completed" ? (source ? "再次打包" : "再次导出") : (source ? "打包多路原画" : "开始导出");
}

function resetExportTaskUI() {
  window.clearTimeout(exportPollTimer);
  $("export-task-status").hidden = true;
  $("export-cancel").hidden = true;
  $("export-download").hidden = true;
  $("export-download").href = "#";
  $("export-download").textContent = "下载视频";
  $("export-source-direct").hidden = exportMode !== "source";
  $("export-submit").disabled = false;
  updateExportSubmitLabel(null);
}

function closeExportModal() {
  window.clearTimeout(exportPollTimer);
  $("export-modal").hidden = true;
}

function applyCompositeFrameGeometry(masterElement) {
  if (!isCompositeDisplay(activeDisplay)) {
    resetCompositeFrameGeometry();
    return;
  }
  const sourceAspect = masterElement?.videoWidth > 0 && masterElement?.videoHeight > 0
    ? masterElement.videoWidth/masterElement.videoHeight
    : 724/469;
  applyCompositeFrameAspect(sourceAspect*compositeLayoutFactor(activeDisplay));
}

function applyServerCompositeFrameGeometry() {
  if (!isCompositeDisplay(activeDisplay)) return;
  const outputAspect = sessionVideo.videoWidth > 0 && sessionVideo.videoHeight > 0
    ? sessionVideo.videoWidth/sessionVideo.videoHeight
    : (724/469)*compositeLayoutFactor(activeDisplay);
  applyCompositeFrameAspect(outputAspect);
}

function compositeLayoutFactor(layout) {
  return ({composite6:3/2, composite4a:1, composite4b:3/4, composite3:2/3})[layout] || 1;
}

function applyCompositeFrameAspect(aspect) {
  activeCompositeAspect = aspect;
  mainVideoFrame.style.aspectRatio = String(activeCompositeAspect);
  mainVideoFrame.style.maxHeight = "none";
  mainVideoFrame.style.marginInline = "auto";
  fitCompositeFrame();
}

function fitCompositeFrame() {
  if (!activeCompositeAspect || !isCompositeDisplay(activeDisplay) || playerModal.hidden) return;
  if (isPlayerFullscreen()) {
    fitFullscreenCompositeGrid();
    return;
  }
  resetFullscreenCompositeGrid();
  const panel = document.querySelector(".player-panel");
  const frameHeight = mainVideoFrame.getBoundingClientRect().height;
  const fixedHeight = Math.max(0, panel.scrollHeight-frameHeight);
  const availableHeight = Math.max(180, window.innerHeight-40-fixedHeight);
  const availableWidth = Math.max(240, panel.clientWidth-2);
  mainVideoFrame.style.width = `${Math.floor(Math.min(availableWidth, availableHeight*activeCompositeAspect))}px`;
}

function handlePlayerViewportChange() {
  if (playerModal.hidden) return;
  window.requestAnimationFrame(() => {
    if (isPlayerFullscreen()) fitFullscreenCompositeGrid();
    else fitCompositeFrame();
  });
}

function fitFullscreenCompositeGrid() {
  if (!activeCompositeAspect || !isCompositeDisplay(activeDisplay)) return;
  const availableWidth = mainVideoFrame.clientWidth || window.innerWidth;
  const availableHeight = mainVideoFrame.clientHeight || window.innerHeight;
  const width = Math.min(availableWidth, availableHeight*activeCompositeAspect);
  const height = width/activeCompositeAspect;
  const content = usesServerComposite() ? sessionVideo : compositeGrid;
  content.style.width = `${Math.floor(width)}px`;
  content.style.height = `${Math.floor(height)}px`;
}

function resetFullscreenCompositeGrid() {
  compositeGrid.style.removeProperty("width");
  compositeGrid.style.removeProperty("height");
  sessionVideo.style.removeProperty("width");
  sessionVideo.style.removeProperty("height");
}

function isPlayerFullscreen() {
  if (document.fullscreenElement === mainVideoFrame || document.webkitFullscreenElement === mainVideoFrame) return true;
  try {
    if (mainVideoFrame.matches(":fullscreen") || mainVideoFrame.matches(":-webkit-full-screen")) return true;
  } catch (_) {
    // 某些浏览器不识别带前缀的全屏伪类，继续使用尺寸兜底判断。
  }
  const rect = mainVideoFrame.getBoundingClientRect();
  return rect.width >= window.innerWidth-2 && rect.height >= window.innerHeight-2;
}

function resetCompositeFrameGeometry() {
  activeCompositeAspect = 0;
  mainVideoFrame.style.removeProperty("width");
  mainVideoFrame.style.removeProperty("max-height");
  mainVideoFrame.style.removeProperty("margin-inline");
  mainVideoFrame.style.removeProperty("aspect-ratio");
  resetFullscreenCompositeGrid();
}

async function playCompositeTracks() {
  playbackWasPlaying = true;
  const results = await Promise.allSettled([...compositeTracks.values()].map((track) => track.element.play()));
  if (results.some((result) => result.status === "rejected")) showPlayerMessage("浏览器阻止了自动播放，请再次点击播放");
}

function pauseCompositeTracks() {
  compositeTracks.forEach((track) => track.element.pause());
}

function releaseCompositeAuxiliaryTracks() {
  releaseCompositeTrackMap(compositeTracks);
  if (pendingCompositeTracks !== compositeTracks) releaseCompositeTrackMap(pendingCompositeTracks);
  compositeTracks.clear();
  pendingCompositeTracks.clear();
}

function releaseCompositeTrackMap(tracks) {
  tracks.forEach((track) => {
    if (track.element !== sessionVideo) releaseVideoElement(track.element);
  });
}

function synchronizeCompositeTracks() {
  const now = performance.now();
  if (now-lastCompositeSync < 750 || sessionVideo.paused || !activeVideo) return;
  lastCompositeSync = now;
  const masterAbsolute = new Date(activeVideo.recorded_at).getTime()/1000+sessionVideo.currentTime;
  compositeTracks.forEach((track) => {
    if (track.element === sessionVideo) {
      track.element.playbackRate = 1;
      return;
    }
    const desired = Math.max(0, masterAbsolute-new Date(track.video.recorded_at).getTime()/1000);
    const drift = desired-track.element.currentTime;
    if (track.element.readyState < 3) {
      track.element.playbackRate = 1;
      return;
    }
    if (Math.abs(drift) > 1.25 && now-track.lastHardSync > 5000 && hasBufferedTime(track.element, desired)) {
      track.element.currentTime = desired;
      track.element.playbackRate = 1;
      track.lastHardSync = now;
      return;
    }
    track.element.playbackRate = drift > .15 ? 1.03 : drift < -.15 ? .97 : 1;
  });
}

function hasBufferedTime(video, time) {
  for (let index=0; index<video.buffered.length; index++) {
    if (video.buffered.start(index) <= time && video.buffered.end(index) >= time+.5) return true;
  }
  return false;
}

function enterFullscreen() {
  const frame = document.querySelector(".main-video-frame");
  if (frame.requestFullscreen) frame.requestFullscreen().then(handlePlayerViewportChange).catch(() => {});
  else if (sessionVideo.webkitEnterFullscreen) sessionVideo.webkitEnterFullscreen();
}

function waitForMetadata(video, timeout = 4000) {
  if (video.readyState >= 1 && !video.error) return Promise.resolve(true);
  return new Promise((resolve) => {
    let settled = false;
    let timer = 0;
    const done = () => {
      if (settled) return;
      settled = true;
      window.clearTimeout(timer);
      video.removeEventListener("loadedmetadata", done);
      video.removeEventListener("error", done);
      resolve(video.readyState >= 1 && !video.error);
    };
    video.addEventListener("loadedmetadata", done);
    video.addEventListener("error", done);
    timer = window.setTimeout(done, timeout);
  });
}

function showPlayerMessage(message) {
  $("player-message").textContent = message;
  $("player-message").hidden = false;
}

function hidePlayerMessage() { $("player-message").hidden = true; }

function closePlayerMedia() {
  loadToken++;
  telemetryAbortController?.abort();
  telemetryAbortController = null;
  activeTelemetry = null;
  timelinePositionRequestID++;
  timelinePositionPending = false;
  pauseCompositeTracks();
  releaseCompositeAuxiliaryTracks();
  releaseVideoElement(sessionVideo);
  compositeGrid.replaceChildren();
  resetCompositeFrameGeometry();
  activeSession = null;
  activeVideo = null;
  activeMomentIndex = -1;
  requestedPosition = 0;
  playbackWasPlaying = false;
  resetPlaybackFallbackState();
  $("camera-switcher").replaceChildren();
  $("segment-markers").replaceChildren();
  $("event-marker-layer").replaceChildren();
  hidePlayerMessage();
  $("playback-telemetry").hidden = true;
  stopPlaybackDiagnostics();
}

function setPlaybackQuality(quality) {
  playbackQuality = normalizePlaybackQuality(quality);
  resetPlaybackFallbackState();
  writePreference("teslabox-playback-quality", playbackQuality);
  if (isAutomaticPlaybackQuality(playbackQuality)) showPlayerMessage(`自动画质：当前${qualityLabel(effectivePlaybackQuality)}；卡顿时会自动降低画质`);
  else hidePlayerMessage();
  if (activeSession) {
    const position = currentSessionPosition();
    const autoplay = playbackWasPlaying;
    sessionVideo.pause();
    pauseCompositeTracks();
    applyDisplayState();
    loadSessionPosition(position, autoplay);
  }
}

function normalizePlaybackQuality(quality) {
  // 兼容上一版把自动策略拆成三个选项时留下的浏览器本地偏好，统一回真正的自动模式。
  if (["auto-smooth", "auto-standard", "auto-high"].includes(quality)) return "auto";
  return ["auto", "smooth", "standard", "high", "original"].includes(quality) ? quality : "auto";
}

function usesServerComposite() {
  return isCompositeDisplay(activeDisplay) && usesServerTranscode();
}

function usesServerTranscode() {
  return ["smooth", "standard", "high"].includes(effectivePlaybackQuality);
}

function qualityLabel(quality) {
  return ({smooth:"流畅", standard:"标清", high:"高清", original:"原画", auto:"自动"})[quality] || "自动";
}

const playbackFallbackLadder = {original:"high", high:"standard", standard:"smooth", smooth:"single"};

function resetPlaybackFallbackState() {
  effectivePlaybackQuality = resolvePlaybackQuality(playbackQuality, activeDisplay);
  playbackFallbackInProgress = false;
  refreshPlaybackQualityControl();
}

function resolvePlaybackQuality(quality, display) {
  if (!isAutomaticPlaybackQuality(quality)) return quality;
  if (!isCompositeDisplay(display)) return "original";
  // 局域网应先直连源文件，只有原画无法稳定播放时才启动板端转码，避免无必要的编码功耗和画质损失。
  return isLocalAccessPath() ? "original" : "standard";
}

function isAutomaticPlaybackQuality(quality) {
  return quality === "auto";
}

function refreshPlaybackQualityControl() {
  const qualitySelect = $("player-quality");
  const automaticOption = qualitySelect?.querySelector('option[value="auto"]');
  if (automaticOption) automaticOption.textContent = `自动（当前：${qualityLabel(effectivePlaybackQuality)}）`;
  if (qualitySelect) qualitySelect.value = playbackQuality;
}

function shouldAutomaticallyDowngrade() {
  return isAutomaticPlaybackQuality(playbackQuality);
}

function canAutomaticallyFallback() {
  return shouldAutomaticallyDowngrade() && isCompositeDisplay(activeDisplay) &&
    ["original", "high", "standard", "smooth"].includes(effectivePlaybackQuality);
}

function handlePlaybackError() {
  // 设置新媒体源期间的错误由对应加载任务判定，避免旧源的迟到事件触发错误降级。
  if (!sessionVideo.getAttribute("src") || sessionVideo.dataset.loadingToken) return;
  reportPlaybackDiagnostic("media_error", "浏览器触发视频元素错误");
  requestPlaybackFallback("视频流播放失败");
}

function requestPlaybackFallback(reason) {
  if (!activeSession || playbackFallbackInProgress || !canAutomaticallyFallback()) return;
  reportPlaybackDiagnostic("fallback_requested", reason);
  playbackFallbackInProgress = true;
  recoverPlayback(reason).catch(() => {
    showPlayerMessage("自动降级失败，请手动选择单路原画");
  }).finally(() => {
    playbackFallbackInProgress = false;
  });
}

async function recoverPlayback(reason) {
  const session = activeSession;
  const position = currentSessionPosition();
  const autoplay = playbackWasPlaying;
  while (activeSession === session && canAutomaticallyFallback()) {
    const previous = effectivePlaybackQuality;
    const next = playbackFallbackLadder[effectivePlaybackQuality] || "single";
    let notice = "";
    sessionVideo.pause();
    pauseCompositeTracks();
    releaseCompositeAuxiliaryTracks();

    if (next === "single") {
      effectivePlaybackQuality = "original";
      activeDisplay = "single";
      activeCamera = preferredCamera(activeSession);
      notice = `${reason}，已自动切换到单路原画`;
    } else {
      effectivePlaybackQuality = next;
      notice = previous === "original"
        ? `${reason}，原画不可用，已自动切换到高清合成`
        : `${reason}，已自动降至${qualityLabel(next)}画质`;
    }

    refreshPlaybackQualityControl();
    applyDisplayState();
    renderCameraSwitcher();
    renderEventMarker();
    const loaded = await loadSessionPosition(position, autoplay);
    if (activeSession !== session) return;
    if (loaded && !sessionVideo.error) {
      showPlayerMessage(notice);
      return;
    }
    if (next === "single") {
      showPlayerMessage("板端合成和单路原画均无法播放，请稍后重试");
      return;
    }
  }
}

function startPlaybackDiagnostics() {
  stopPlaybackDiagnostics();
  updatePlaybackDiagnostics();
  playbackDiagnosticsTimer = window.setInterval(updatePlaybackDiagnostics, 2000);
}

function stopPlaybackDiagnostics() {
  if (playbackDiagnosticsTimer) window.clearInterval(playbackDiagnosticsTimer);
  playbackDiagnosticsTimer = 0;
}

function resetPlaybackDiagnostics() {
  playbackStalls = 0;
  isPlaybackBuffering = false;
  lastPlaybackProgressAt = Date.now();
  lastPlaybackTime = 0;
  mediaThroughputSamples = [];
  observedMediaEntries.clear();
  $("playback-throughput").textContent = "媒体吞吐：等待数据";
  $("playback-stalls").textContent = "卡顿：0 次";
}

function markPlaybackStall() {
  if (!activeSession || sessionVideo.paused || isPlaybackBuffering) return;
  isPlaybackBuffering = true;
  playbackStalls++;
  $("playback-stalls").textContent = `卡顿：${playbackStalls} 次`;
  if (playbackStalls === 1 || playbackStalls%5 === 0) {
    reportPlaybackDiagnostic("playback_stall", `播放累计卡顿 ${playbackStalls} 次`);
  }
}

function reportPlaybackDiagnostic(event, reason) {
  if (!activeSession) return;
  const moment = activeSession.moments?.[activeMomentIndex] || null;
  const key = `${event}|${reason}|${activeSession.id}|${moment?.id || ""}|${activeDisplay}|${effectivePlaybackQuality}`;
  const now = Date.now();
  if (now-(playbackDiagnosticLastSent.get(key) || 0) < 15000) return;
  playbackDiagnosticLastSent.set(key, now);
  if (playbackDiagnosticLastSent.size > 80) {
    const oldest = [...playbackDiagnosticLastSent.entries()].sort((left, right) => left[1]-right[1]).slice(0, 20);
    oldest.forEach(([entry]) => playbackDiagnosticLastSent.delete(entry));
  }
  const connection = navigator.connection;
  const average = mediaThroughputSamples.length
    ? mediaThroughputSamples.reduce((sum, value) => sum+value, 0)/mediaThroughputSamples.length
    : 0;
  const payload = {
    event,
    reason,
    session_id: activeSession.id || "",
    group_id: moment?.id || "",
    display: activeDisplay,
    quality: playbackQuality,
    effective_quality: effectivePlaybackQuality,
    access_path: classifyAccessPath(),
    network_type: connection?.effectiveType || "",
    downlink_mbps: Number(connection?.downlink) || 0,
    rtt_ms: Number(connection?.rtt) || 0,
    media_mbps: Number(average.toFixed(2)) || 0,
    buffer_seconds: Number(bufferedSeconds(sessionVideo).toFixed(2)) || 0,
    stalls: playbackStalls,
    media_error_code: sessionVideo.error?.code || 0,
    ready_state: sessionVideo.readyState || 0,
  };
  fetch("/api/v1/diagnostics/client-event", {
    method:"POST",
    credentials:"same-origin",
    keepalive:true,
    headers:{"Content-Type":"application/json"},
    body:JSON.stringify(payload),
  }).catch(() => {});
}

function updatePlaybackDiagnostics() {
  if (!$("playback-path")) return;
  collectMediaThroughputSamples();
  monitorPlaybackProgress();
  const connection = navigator.connection;
  const hints = [];
  if (connection?.effectiveType) hints.push(connection.effectiveType.toUpperCase());
  if (Number.isFinite(connection?.downlink)) hints.push(`浏览器估算 ${connection.downlink.toFixed(1)} Mbps`);
  if (Number.isFinite(connection?.rtt)) hints.push(`RTT ${connection.rtt} ms`);
  $("playback-path").textContent = `链路：${classifyAccessPath()}${hints.length ? ` · ${hints.join(" · ")}` : ""}`;
  const average = mediaThroughputSamples.length
    ? mediaThroughputSamples.reduce((sum, value) => sum+value, 0)/mediaThroughputSamples.length
    : 0;
  $("playback-throughput").textContent = average > 0 ? `媒体实测：${average.toFixed(1)} Mbps` : "媒体吞吐：等待数据";
  $("playback-buffer").textContent = `缓冲：${bufferedSeconds(sessionVideo).toFixed(1)} 秒`;
}

function monitorPlaybackProgress() {
  if (!activeSession || sessionVideo.paused || sessionVideo.seeking) {
    lastPlaybackProgressAt = Date.now();
    lastPlaybackTime = sessionVideo.currentTime || 0;
    return;
  }
  if (sessionVideo.currentTime > lastPlaybackTime+.05) {
    lastPlaybackTime = sessionVideo.currentTime;
    lastPlaybackProgressAt = Date.now();
    return;
  }
  if (canAutomaticallyFallback() && Date.now()-lastPlaybackProgressAt >= 8000) {
    lastPlaybackProgressAt = Date.now();
    requestPlaybackFallback("播放连续卡顿超过 8 秒");
  }
}

function collectMediaThroughputSamples() {
  performance.getEntriesByType("resource").forEach((entry) => {
    const singleVideo = entry.name.includes("/api/v1/videos/") && entry.name.includes("/content");
    const compositeVideo = entry.name.includes("/api/v1/video-groups/") && entry.name.includes("/composite");
    if ((!singleVideo && !compositeVideo) || entry.duration <= 0) return;
    const key = `${entry.name}|${entry.startTime}|${entry.duration}`;
    if (observedMediaEntries.has(key)) return;
    observedMediaEntries.add(key);
    const bytes = entry.encodedBodySize || entry.transferSize;
    if (!bytes) return;
    const mbps = bytes*8/entry.duration/1000;
    if (Number.isFinite(mbps) && mbps > 0) mediaThroughputSamples.push(mbps);
  });
  if (mediaThroughputSamples.length > 8) mediaThroughputSamples = mediaThroughputSamples.slice(-8);
}

function bufferedSeconds(video) {
  if (!video || !Number.isFinite(video.currentTime)) return 0;
  for (let index=0; index<video.buffered.length; index++) {
    if (video.buffered.start(index) <= video.currentTime && video.buffered.end(index) >= video.currentTime) {
      return Math.max(0, video.buffered.end(index)-video.currentTime);
    }
  }
  return 0;
}

function classifyAccessPath() {
  if (isLocalAccessPath()) return "AP/局域网";
  const hostname = window.location.hostname.toLowerCase();
  const tailscale = hostname.match(/^100\.(\d+)\./);
  if (tailscale && Number(tailscale[1]) >= 64 && Number(tailscale[1]) <= 127) return "Tailscale";
  return navigator.onLine ? "公网或中继（待服务端确认）" : "浏览器离线";
}

function isLocalAccessPath() {
  const hostname = window.location.hostname.toLowerCase();
  if (hostname.endsWith(".local") || hostname === "localhost" || hostname === "127.0.0.1") return true;
  return /^(10\.|192\.168\.|172\.(1[6-9]|2\d|3[01])\.)/.test(hostname);
}

function releaseVideoElement(element) {
  const placeholder = element.parentElement?.querySelector(".composite-tile-placeholder");
  element.pause();
  element.removeAttribute("src");
  element.removeAttribute("data-video-id");
  element.removeAttribute("data-composite-key");
  element.removeAttribute("data-loading-token");
  element.load();
  if (element !== sessionVideo) element.remove();
  if (placeholder) placeholder.hidden = false;
}

function finishSessionVideoLoad(token) {
  if (sessionVideo.dataset.loadingToken === String(token)) delete sessionVideo.dataset.loadingToken;
}

function closePlayer() {
  closeExportModal();
  closePlayerMedia();
  playerModal.hidden = true;
  document.body.style.overflow = "";
}

function setView(view) {
  if (view !== "multi" && view !== "single") return;
  state.view = view;
  writePreference("teslabox-view", view);
  applyViewState();
  renderArchiveTimeline();
}

function applyViewState() {
  document.querySelectorAll("[data-view]").forEach((button) => button.classList.toggle("active", button.dataset.view === state.view));
  document.querySelectorAll("[data-period]").forEach((button) => button.classList.toggle("active", button.dataset.period === state.period));
}

function setPeriod(period) {
  if (!["month", "week", "day"].includes(period)) return;
  state.period = period;
  writePreference("teslabox-period", period);
  applyViewState();
  renderArchiveTimeline();
}

function archiveSessions(sessions, period) {
  if (sessions.length === 0) return [];
  const buckets = new Map();
  const anchor = localMidnight(new Date(sessions[0].start_at));
  sessions.forEach((session) => {
    const date = new Date(session.start_at);
    let bucketDate = localMidnight(date);
    if (period === "month") bucketDate = new Date(date.getFullYear(), date.getMonth(), 1);
    if (period === "week") {
      const elapsedDays = Math.max(0, Math.floor((anchor-localMidnight(date))/86400000));
      bucketDate = new Date(anchor);
      bucketDate.setDate(anchor.getDate()-Math.floor(elapsedDays/7)*7);
    }
    const key = period === "month"
      ? `${bucketDate.getFullYear()}-${bucketDate.getMonth()}`
      : `${bucketDate.getFullYear()}-${bucketDate.getMonth()}-${bucketDate.getDate()}`;
    if (!buckets.has(key)) buckets.set(key, {key, label: archiveLabel(bucketDate, period), sessions: []});
    buckets.get(key).sessions.push(session);
  });
  return [...buckets.values()];
}

function localMidnight(date) { return new Date(date.getFullYear(), date.getMonth(), date.getDate()); }

function archiveLabel(date, period) {
  if (period === "month") return `${date.getFullYear()}年${date.getMonth()+1}月`;
  return `${date.getFullYear()}年${date.getMonth()+1}月${date.getDate()}日`;
}

function normalizeTheme(theme) {
  if (theme === "forest") return "green";
  if (theme === "ocean") return "blue";
  return ["blue", "green", "red"].includes(theme) ? theme : "blue";
}

function applyTheme(theme) {
  const selected = normalizeTheme(theme);
  document.documentElement.dataset.theme = selected;
  $("theme-select").value = selected;
  $("theme-color").content = "#000000";
  writePreference("teslabox-theme", selected);
}

function isCompositeDisplay(display) {
  return Boolean(compositeLayouts[display]);
}

function normalizeDisplayMode(display) {
  return compositeLayouts[display] ? display : "composite6";
}

function normalizeUIFont(value) {
  return ["compact", "small", "medium", "large", "xlarge"].includes(value) ? value : "medium";
}

function applyUIFont(value) {
  const selected = normalizeUIFont(value);
  state.uiFont = selected;
  document.documentElement.dataset.uiFont = selected;
  $("settings-font-size").value = selected;
  writePreference("teslabox-settings-font", selected);
}

function preferredCamera(session) {
  return session.cameras.includes("front") ? "front" : session.cameras[0];
}

function readPreference(key, fallback) {
  try { return window.localStorage.getItem(key) || fallback; } catch (_) { return fallback; }
}

function writePreference(key, value) {
  try { window.localStorage.setItem(key, value); } catch (_) { /* 无持久存储时仍保持当前页面状态 */ }
}

function setNotice(message, error, toast = true) {
  presentNotice(notice, message, error, toast);
}

function presentNotice(element, message, error, toast = true) {
  const content = String(message || "").trim();
  element.textContent = content;
  element.classList.toggle("error", Boolean(error));
  if (toast && content) showToast(content, Boolean(error));
}

function showToast(message, error) {
  if (!toastRegion || !message) return;
  const content = String(message).trim();
  if (!content) return;
  const fingerprint = `${error ? "error" : "info"}:${content}`;
  const existing = Array.from(toastRegion.children).find((item) => item.dataset.fingerprint === fingerprint);
  if (existing) {
    existing.classList.remove("closing");
    scheduleToastDismiss(existing, error);
    return;
  }
  while (toastRegion.children.length >= 4) toastRegion.firstElementChild?.remove();
  const toast = document.createElement("article");
  toast.className = `toast${error ? " error" : ""}`;
  toast.dataset.fingerprint = fingerprint;
  const icon = document.createElement("span");
  icon.className = "toast-icon";
  icon.setAttribute("aria-hidden", "true");
  icon.textContent = error ? "!" : "✓";
  const text = document.createElement("p");
  text.className = "toast-message";
  text.textContent = content.length > 480 ? `${content.slice(0, 479)}…` : content;
  const dismiss = document.createElement("button");
  dismiss.type = "button";
  dismiss.className = "toast-dismiss";
  dismiss.setAttribute("aria-label", "关闭提示");
  dismiss.textContent = "×";
  dismiss.addEventListener("click", () => dismissToast(toast));
  toast.append(icon, text, dismiss);
  toastRegion.append(toast);
  scheduleToastDismiss(toast, error);
}

function scheduleToastDismiss(toast, error) {
  window.clearTimeout(toast.dismissTimer);
  toast.dismissTimer = window.setTimeout(() => dismissToast(toast), error ? 10000 : 5200);
}

function dismissToast(toast) {
  if (!toast?.parentElement || toast.classList.contains("closing")) return;
  window.clearTimeout(toast.dismissTimer);
  toast.classList.add("closing");
  window.setTimeout(() => toast.remove(), 220);
}

function cameraLabel(value) {
  return ({front:"前视", back:"后视", left_repeater:"左翼子板", right_repeater:"右翼子板", left_pillar:"左 B 柱", right_pillar:"右 B 柱"})[value] || value || "未知摄像头";
}

function clipLabel(value) {
  return ({recent:"最近录像", saved:"已保存", sentry:"哨兵事件", track:"赛道录像"})[value] || value || "其他";
}

function clipTypeClass(value) { return ["recent", "saved", "sentry", "track"].includes(value) ? value : "unknown"; }

function createEventSummary(metadata) {
  if (!metadata) return null;
  const summary = document.createElement("p");
  summary.className = "session-event";
  const dot = document.createElement("i");
  dot.className = "event-dot";
  summary.append(dot, document.createTextNode(eventMetadataText(metadata, false)));
  return summary;
}

function sessionEventPosition(session) {
  if (!session?.event_metadata?.occurred_at || !session.duration_seconds) return null;
  const occurredAt = new Date(session.event_metadata.occurred_at).getTime();
  for (const moment of session.moments || []) {
    const startAt = new Date(moment.recorded_at).getTime();
    const duration = timelineMomentDuration(moment);
    if (occurredAt >= startAt && occurredAt <= startAt+duration*1000) {
      return timelineMomentStart(moment)+(occurredAt-startAt)/1000;
    }
  }
  const position = (occurredAt-new Date(session.start_at))/1000;
  if (!Number.isFinite(position) || position < 0 || position > session.duration_seconds) return null;
  return position;
}

function eventMetadataText(metadata, includeLocation = true) {
  const parts = [];
  if (metadata.occurred_at) parts.push(`触发于 ${formatTime(metadata.occurred_at)}`);
  if (includeLocation) {
    const location = eventLocation(metadata);
    if (location) parts.push(location);
  }
  if (metadata.reason || metadata.trigger_type) parts.push(eventReasonLabel(metadata));
  const camera = eventCameraView(metadata);
  if (camera) parts.push(cameraLabel(camera));
  return parts.join(" · ") || "事件详情不可用";
}

function eventLocation(metadata) {
  const place = eventPlace(metadata);
  const coordinates = eventCoordinates(metadata);
  return [place, coordinates].filter(Boolean).join(" · ");
}

function eventCoordinates(metadata) {
  if (!metadata) return "";
  const latitude = Number(metadata.latitude);
  const longitude = Number(metadata.longitude);
  return Number.isFinite(latitude) && Number.isFinite(longitude)
    ? `${latitude.toFixed(4)}, ${longitude.toFixed(4)}` : "";
}

function eventPlace(metadata) {
  if (!metadata) return "";
  return [metadata.city, metadata.street].filter(Boolean).join(" ");
}

function eventReasonLabel(metadata) {
  const reason = typeof metadata === "string" ? metadata : String(metadata?.reason || "");
  const triggerType = typeof metadata === "string" ? "" : String(metadata?.trigger_type || "");
  if (triggerType === "manual_save") return "手动保存录像";
  if (triggerType === "manual_honk") return "鸣笛保存录像";
  if (triggerType === "manual_other") return "用户手动保存录像";
  if (triggerType === "sentry_object") return "哨兵检测到周边对象";
  if (triggerType === "sentry_motion") return "哨兵检测到车辆震动";
  if (triggerType === "sentry_alarm") return "哨兵警报触发";
  if (triggerType === "sentry_other") return "哨兵模式事件";
  if (reason === "user_interaction_dashcam_icon_tapped") return "手动点击保存";
  if (reason === "user_interaction_dashcam_panel_save" || reason === "user_interaction_dashcam_launcher_action_tapped") return "手动保存录像";
  if (reason === "user_interaction_honk") return "鸣笛保存";
  if (reason.startsWith("sentry_panic")) return "哨兵警报触发";
  if (reason.startsWith("sentry_aware_accel")) return "哨兵检测到车辆震动";
  if (reason.startsWith("sentry_aware_object")) return "哨兵检测到周边对象";
  if (reason.startsWith("sentry_aware")) return "哨兵警戒触发";
  return reason ? `车辆事件触发（${reason}）` : "车辆事件触发";
}

function eventCameraView(metadata) {
  const cameraView = String(metadata?.camera_view || "").trim();
  if (cameraOrder.includes(cameraView)) return cameraView;
  const camera = String(metadata?.camera || "").trim();
  if (cameraOrder.includes(camera)) return camera;
  // 事件文件使用的是车辆检测摄像头编号，不能把未知编号硬映射到所有视角，避免误导用户。
  return ({"0":"front", "1":"front", "2":"front", "3":"left_repeater", "4":"right_repeater", "5":"left_pillar", "6":"right_pillar", "7":"back"})[camera] || "";
}

function eventMarkerTitle(metadata) { return `事件时间点：${eventMetadataText(metadata)}`; }

function formatDate(value) {
  if (!value) return "时间未知";
  return new Intl.DateTimeFormat("zh-CN", {month:"2-digit", day:"2-digit", hour:"2-digit", minute:"2-digit", second:"2-digit"}).format(new Date(value));
}

function formatTime(value) {
  return new Intl.DateTimeFormat("zh-CN", {hour:"2-digit", minute:"2-digit", second:"2-digit"}).format(new Date(value));
}

function formatSessionRange(start, end) {
  const date = new Intl.DateTimeFormat("zh-CN", {month:"2-digit", day:"2-digit"}).format(new Date(start));
  return `${date} ${formatTime(start)} — ${formatTime(end)}`;
}

function formatDuration(value) {
  const total = Math.max(0, Math.round(Number(value || 0)));
  const hours = Math.floor(total/3600);
  const minutes = Math.floor((total%3600)/60);
  const seconds = total%60;
  if (hours > 0) return `${String(hours).padStart(2,"0")}:${String(minutes).padStart(2,"0")}:${String(seconds).padStart(2,"0")}`;
  return `${String(minutes).padStart(2,"0")}:${String(seconds).padStart(2,"0")}`;
}

function formatBytes(value) {
  const bytes = Number(value || 0);
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let size = bytes/1024;
  let index = 0;
  while (size >= 1024 && index < units.length-1) { size /= 1024; index++; }
  return `${size.toFixed(size >= 10 ? 1 : 2)} ${units[index]}`;
}

function formatNumber(value) { return new Intl.NumberFormat("zh-CN").format(value); }

function formatUptime(value) {
  const seconds = Math.max(0, Number(value || 0));
  const days = Math.floor(seconds/86400);
  const hours = Math.floor((seconds%86400)/3600);
  const minutes = Math.floor((seconds%3600)/60);
  if (days > 0) return `${days} 天 ${hours} 小时`;
  if (hours > 0) return `${hours} 小时 ${minutes} 分钟`;
  return `${minutes} 分钟`;
}

window.setInterval(() => { if (!appView.hidden) loadStatus(); }, 10000);
window.setInterval(() => {
  if (!appView.hidden && state.page === "dashboard" && !document.hidden && playerModal.hidden) loadSessions(true);
}, 30000);
initialize();
