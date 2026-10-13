"use strict";

const cameraOrder = ["left_pillar", "front", "right_pillar", "left_repeater", "back", "right_repeater"];
const compositeLayouts = {
  composite6: {label:"六路合成", cameras:cameraOrder},
  composite4a: {label:"四路合成 A", cameras:["front", "back", "left_repeater", "right_repeater"]},
  composite4b: {label:"四路合成 B", cameras:["front", "right_repeater", "back", "left_repeater"]},
  composite3: {label:"三路合成", cameras:["front", "right_repeater", "left_repeater"]},
};
const musicPageSize = 24;
const savedView = readPreference("teslabox-view", "multi");
const savedPeriod = readPreference("teslabox-period", "month");
const savedUIFont = normalizeUIFont(readPreference("teslabox-settings-font", "medium"));
const savedPlaybackQuality = normalizePlaybackQuality(readPreference("teslabox-playback-quality", "auto"));
const recordingRefreshPreferenceKey = "teslabox-recording-refresh-seconds";
const savedRecordingRefreshSeconds = normalizeRecordingRefreshSeconds(readPreference(recordingRefreshPreferenceKey, "60"));
const initialPage = pageFromPath(window.location.pathname);
function pageFromPath(path) { return ({"/media":"media", "/settings":"settings", "/logs":"logs", "/backup":"backup", "/exports":"exports"})[path] || "dashboard"; }
const state = {
  offset: 0, total: 0, loading: false, refreshing: false, authEnabled: true,
  view: savedView === "single" ? "single" : "multi",
  period: ["month", "week", "day"].includes(savedPeriod) ? savedPeriod : "month",
  page: initialPage, mustChangePassword: false,
  wifi: null, ap: null, storage: null, update: null, updateHistory: null, maintenance: null, fan: null, mediaAssets: null, uiFont: savedUIFont,
  musicPath: "", musicPage: 0,
};
const $ = (id) => document.getElementById(id);

function appendReleaseNoteInline(parent, source) {
  const pattern = /\*\*([^*]+)\*\*|`([^`]+)`|\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g;
  let offset = 0;
  let match;
  while ((match = pattern.exec(source)) !== null) {
    if (match.index > offset) parent.append(document.createTextNode(source.slice(offset, match.index)));
    if (match[1] !== undefined) {
      const strong = document.createElement("strong");
      strong.textContent = match[1];
      parent.append(strong);
    } else if (match[2] !== undefined) {
      const code = document.createElement("code");
      code.textContent = match[2];
      parent.append(code);
    } else {
      const link = document.createElement("a");
      link.textContent = match[3];
      link.href = match[4];
      link.target = "_blank";
      link.rel = "noopener noreferrer";
      parent.append(link);
    }
    offset = match.index + match[0].length;
  }
  if (offset < source.length) parent.append(document.createTextNode(source.slice(offset)));
}

function isReleaseNoteBlockStart(line) {
  return /^(#{1,6})\s+/.test(line) || /^\s*[-*+]\s+/.test(line) || /^\s*\d+[.)]\s+/.test(line)
    || /^\s*>\s?/.test(line) || /^\s*```/.test(line) || /^\s*(?:---+|___+|\*\*\*+)\s*$/.test(line);
}

function renderReleaseNotes(container, markdown) {
  container.replaceChildren();
  const lines = String(markdown || "").replace(/\r\n?/g, "\n").split("\n");
  let index = 0;
  while (index < lines.length) {
    const line = lines[index];
    if (!line.trim()) {
      index++;
      continue;
    }
    const fence = line.match(/^\s*```\s*([^\s`]*)\s*$/);
    if (fence) {
      const pre = document.createElement("pre");
      const code = document.createElement("code");
      const content = [];
      index++;
      while (index < lines.length && !/^\s*```\s*$/.test(lines[index])) content.push(lines[index++]);
      if (index < lines.length) index++;
      code.textContent = content.join("\n");
      if (fence[1]) code.dataset.language = fence[1];
      pre.append(code);
      container.append(pre);
      continue;
    }
    const heading = line.match(/^(#{1,6})\s+(.+?)\s*#*\s*$/);
    if (heading) {
      const title = document.createElement(`h${heading[1].length}`);
      appendReleaseNoteInline(title, heading[2]);
      container.append(title);
      index++;
      continue;
    }
    if (/^\s*(?:---+|___+|\*\*\*+)\s*$/.test(line)) {
      container.append(document.createElement("hr"));
      index++;
      continue;
    }
    const unordered = line.match(/^\s*[-*+]\s+(.+)$/);
    const ordered = line.match(/^\s*\d+[.)]\s+(.+)$/);
    if (unordered || ordered) {
      const list = document.createElement(unordered ? "ul" : "ol");
      const itemPattern = unordered ? /^\s*[-*+]\s+(.+)$/ : /^\s*\d+[.)]\s+(.+)$/;
      while (index < lines.length) {
        const item = lines[index].match(itemPattern);
        if (!item) break;
        const listItem = document.createElement("li");
        appendReleaseNoteInline(listItem, item[1]);
        list.append(listItem);
        index++;
      }
      container.append(list);
      continue;
    }
    if (/^\s*>\s?/.test(line)) {
      const quote = document.createElement("blockquote");
      const content = [];
      while (index < lines.length) {
        const part = lines[index].match(/^\s*>\s?(.*)$/);
        if (!part) break;
        content.push(part[1]);
        index++;
      }
      appendReleaseNoteInline(quote, content.join(" "));
      container.append(quote);
      continue;
    }
    const paragraph = document.createElement("p");
    const content = [line.trim()];
    index++;
    while (index < lines.length && lines[index].trim() && !isReleaseNoteBlockStart(lines[index])) {
      content.push(lines[index].trim());
      index++;
    }
    appendReleaseNoteInline(paragraph, content.join(" "));
    container.append(paragraph);
  }
}

const loginView = $("login-view");
const appView = $("app-view");
const loginForm = $("login-form");
const loginError = $("login-error");
const timelineList = $("video-grid");
const notice = $("notice");
const toastRegion = $("toast-region");
const playerModal = $("player-modal");
const sessionPlayerView = $("session-player-view");
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
let sessionOpenSequence = 0;
let sessionOpening = false;
let sessionOpenController = null;
let archivedSessions = [];
let archivePages = 1;
let archiveTotal = 0;
const selectedRecordingSessions = new Set();
let recordingDeleteInProgress = false;
let recordingDeleteMode = false;
let recordingRefreshSeconds = savedRecordingRefreshSeconds;
let recordingRefreshTimer = 0;
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
let timelineScrubbing = false;
let activeStreamTimelineStart = null;
let activeTelemetryMediaOffset = 0;
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
const telemetryRequestsByVideo = new Map();
let activeTelemetry = null;
let telemetryAbortController = null;
let telemetryPrefetchTimer = 0;
let passwordChangeForced = false;
let updatePollTimer = 0;
let logsRefreshTimer = 0;
let playbackProbeRun = 0;
let playbackProbeFinish = null;
let playbackProbeActive = false;
let playbackProbeLastID = "";
let activeLogSource = "usb";
const observedMediaEntries = new Set();
const playbackDiagnosticLastSent = new Map();
let pendingRequestControl = null;
let pendingRequestControlTimer = 0;
let loadingSequence = 0;
const networkLoadingTasks = new Map();
const requestLoadingControls = new Map();
let networkLoadingTimer = 0;
let settingsLoadSequence = 0;
const compatibilitySelects = new Map();
let compatibilitySelectSequence = 0;
let compatibilitySelectDocumentObserver = null;
let onboardingStorage = null;
let onboardingState = null;
let onboardingChecking = false;
let pageGuideIndex = -1;
const pageGuideSteps = [
  {target:"nav-toggle", title:"页面从这里切换", detail:"点击左上角 TeslaBox 标志可以打开主导航，切换概览、录像备份、导出管理、媒体中心、日志中心和设置中心。"},
  {target:"live-indicator", title:"设备连接状态", detail:"这里表示管理页面能否正常连接 TeslaBox 服务。显示“设备在线”时，页面可以继续读取设备状态。"},
  {target:"internet-indicator", title:"互联网状态", detail:"这里表示 TeslaBox 当前是否能够访问互联网。无互联网不影响车机写入录像和局域网查看。"},
  {target:"appearance-settings-title", page:"settings", section:"appearance-settings-section", title:"页面设置", detail:"在设置中心 → 系统 → 页面设置中切换主题、显示大小、全局字体和车机浏览器兼容模式，也可以退出管理页面。修改立即生效，仅保存在当前浏览器。"},
];

applyTheme(normalizeTheme(readPreference("teslabox-theme", "blue")));
applyUIFont(state.uiFont);
$("settings-display-scale").value = String(TeslaBoxPageDisplay.getPercent());
$("settings-display-slider").value = String(TeslaBoxPageDisplay.getPercent());
applyTeslaBrowserCompatibility(readPreference("teslabox-tesla-browser-compatibility", "false") === "true");
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
    let reasonCode = "";
    try {
      const payload = await response.json();
      message = payload.error || message;
      reasonCode = payload.reason_code || "";
      if (payload.suggestion) message += `；${payload.suggestion}`;
    } catch (_) { /* 保留通用提示 */ }
    const error = new Error(message);
    error.status = response.status;
    error.reasonCode = reasonCode;
    throw error;
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
  void globalThis.TeslaBoxCarPlaySettings?.load();
  $("logout-button").hidden = !state.authEnabled;
}

function initializeSettingsTabs() {
  globalThis.TeslaBoxSettingsLayout?.initialize();
}

function activateSettingsTab(id, remember = true) {
  globalThis.TeslaBoxSettingsLayout?.selectSection(id, remember);
}

async function initialize() {
	initializeSettingsTabs();
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
    if (state.page === "exports") await loadExportSettings();
    await maybeShowOnboarding();
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
    if (state.page === "settings") await loadSettings();
    if (state.page === "exports") await loadExportSettings();
    if (state.page === "media") await loadMediaAssets();
    if (state.page === "logs") await loadLogs();
    await maybeShowOnboarding();
  } catch (error) {
    loginError.textContent = error.message;
  } finally {
    button.disabled = false;
  }
});

$("logout-button").addEventListener("click", async () => {
  await requestResponse("/api/v1/auth/logout", { method: "POST", loadingMessage:"正在退出管理页面…" });
  closePasswordModal(true);
  closePlayer();
  showLogin();
});
$("theme-select").addEventListener("change", (event) => applyTheme(event.target.value));
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
$("playback-probe-start").addEventListener("click", () => startPlaybackProbeMatrix());
$("playback-probe-stop").addEventListener("click", () => stopPlaybackProbeMatrix("用户停止"));
document.addEventListener("visibilitychange", () => {
  if (document.hidden && playbackProbeActive) stopPlaybackProbeMatrix("页面进入后台");
});
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
function updateDisplayScale(value) {
  const selected = String(TeslaBoxPageDisplay.apply(value));
  $("settings-display-scale").value = selected;
  $("settings-display-slider").value = selected;
  window.dispatchEvent(new Event("resize"));
}
$("settings-display-scale").addEventListener("change", event => updateDisplayScale(event.target.value));
// 拖动中只显示预览值，松开再缩放，避免滑块本身移动导致跳值。
$("settings-display-slider").addEventListener("input", event => { $("settings-display-scale").value = event.target.value; });
$("settings-display-slider").addEventListener("change", event => updateDisplayScale(event.target.value));
$("settings-display-reset").addEventListener("click", () => updateDisplayScale(100));
document.querySelectorAll("[data-display-scale]").forEach(button => {
  button.addEventListener("click", () => updateDisplayScale(button.dataset.displayScale));
});
$("settings-font-size").addEventListener("change", (event) => applyUIFont(event.target.value));
$("tesla-browser-compatibility").addEventListener("change", (event) => applyTeslaBrowserCompatibility(event.target.checked));
$("wifi-scan").addEventListener("click", scanWiFi);
$("saved-network-toggle").addEventListener("click", () => toggleNetworkPanel("saved-network-panel", "saved-network-toggle"));
$("available-network-toggle").addEventListener("click", () => toggleNetworkPanel("available-network-panel", "available-network-toggle"));
$("ap-band").addEventListener("change", () => populateAPChannels($("ap-band").value, 0, state.ap?.allowed_channels));
$("ap-form").addEventListener("submit", saveAPSettings);
$("ap-repair-interface").addEventListener("click", repairAPInterface);
$("export-settings-save").addEventListener("click", saveExportSettings);
$("export-history-refresh").addEventListener("click", loadExportSettings);
$("export-delete-after-download").addEventListener("change", renderExportRetentionControls);
$("fan-maximum").addEventListener("input", renderFanCalculation);
$("fan-mode-automatic").addEventListener("click", () => setFanTemperatureMode("automatic"));
$("fan-mode-manual").addEventListener("click", () => setFanTemperatureMode("manual"));
$("fan-save").addEventListener("click", saveFanSettings);
$("wifi-connect-form").addEventListener("submit", connectNewWiFi);
$("wifi-ipv4-form").addEventListener("submit", saveWiFiIPv4Settings);
$("wifi-ipv4-mode").addEventListener("change", renderWiFiIPv4Fields);
$("storage-refresh").addEventListener("click", loadStorageDevices);
$("storage-fragmentation-check").addEventListener("click", inspectStorageFragmentation);
$("storage-capacity-migrate").addEventListener("click", migrateStorageCapacity);
$("storage-init-form").addEventListener("submit", initializeStorageDevice);
$("onboarding-skip").addEventListener("click", dismissOnboarding);
$("onboarding-primary").addEventListener("click", followOnboardingPrimaryAction);
$("page-guide-skip").addEventListener("click", completePageGuide);
$("page-guide-previous").addEventListener("click", () => renderPageGuideStep(pageGuideIndex - 1));
$("page-guide-next").addEventListener("click", () => {
  if (pageGuideIndex >= pageGuideSteps.length - 1) completePageGuide();
  else renderPageGuideStep(pageGuideIndex + 1);
});
window.addEventListener("resize", () => { if (pageGuideIndex >= 0) positionPageGuide(); });
$("storage-music-enabled").addEventListener("change", renderStoragePartitionPreview);
$("storage-teslacam-percent").addEventListener("input", renderStoragePartitionPreview);
$("maintenance-auto-recovery").addEventListener("click", toggleMaintenanceAutoRecovery);
$("storage-reclaim").addEventListener("click", reclaimStorageSpace);
$("snapshot-reserve-repair").addEventListener("click", repairSnapshotReserve);
$("snapshot-reserve-cancel").addEventListener("click", cancelSnapshotReserveRepair);
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
$("update-channel-stable").addEventListener("click", () => setUpdateChannel("stable"));
$("update-channel-beta").addEventListener("click", () => setUpdateChannel("beta"));
$("update-check").addEventListener("click", checkUpdate);
$("update-history-load").addEventListener("click", loadUpdateHistory);
$("update-download").addEventListener("click", downloadUpdate);
$("update-install").addEventListener("click", installUpdate);
$("filter-form").addEventListener("submit", (event) => { event.preventDefault(); archivePages = 1; loadSessions(false); });
$("load-more").addEventListener("click", () => { if (!state.loading) { archivePages++; loadSessions(true); } });
$("recording-refresh-interval").value = String(recordingRefreshSeconds);
renderCompatibilitySelect($("recording-refresh-interval"));
$("recording-refresh-interval").addEventListener("change", (event) => {
  recordingRefreshSeconds = normalizeRecordingRefreshSeconds(event.target.value);
  event.target.value = String(recordingRefreshSeconds);
  writePreference(recordingRefreshPreferenceKey, String(recordingRefreshSeconds));
  scheduleRecordingRefresh();
});
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
$("export-burn-timestamp").addEventListener("change", renderExportOverlayOptions);
$("export-burn-sei").addEventListener("change", renderExportOverlayOptions);
document.querySelectorAll('input[name="export-overlay-mode"]').forEach((input) => input.addEventListener("change", renderExportOverlayOptions));
$("export-range-start").addEventListener("input", () => updateExportRangeFromSlider("start"));
$("export-range-end").addEventListener("input", () => updateExportRangeFromSlider("end"));
$("export-start-time").addEventListener("change", () => updateExportRangeFromTime("start"));
$("export-end-time").addEventListener("change", () => updateExportRangeFromTime("end"));
document.querySelectorAll("[data-export-adjust]").forEach((button) => button.addEventListener("click", () => adjustExportRange(button.dataset.exportAdjust)));
document.querySelectorAll("[data-export-step]").forEach((button) => button.addEventListener("click", () => {
  $("export-adjust-step").value = button.dataset.exportStep;
  document.querySelectorAll("[data-export-step]").forEach((item) => item.setAttribute("aria-pressed", String(item === button)));
  document.querySelectorAll("[data-export-adjust]").forEach((item) => {
    const [edge, direction] = item.dataset.exportAdjust.split(":");
    item.textContent = `${edge === "start" ? "开始" : "结束"} ${Number(direction) < 0 ? "−" : "+"}${button.dataset.exportStep} 秒`;
  });
}));
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
  if (isPlayerFullscreen()) {
    exitPlayerFullscreen();
    return;
  }
  if (closeOpenCompatibilitySelect()) return;
  if (!$("confirm-modal").hidden) resolveConfirmation(false);
  else if (!$("password-modal").hidden) closePasswordModal(false);
  else if (!$("export-modal").hidden) closeExportModal();
  else if (!$("wifi-modal").hidden) closeWiFiModal();
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
sessionVideo.addEventListener("loadeddata", finishPlayerLoadingFromMedia);
sessionVideo.addEventListener("canplay", finishPlayerLoadingFromMedia);
sessionVideo.addEventListener("playing", () => {
  isPlaybackBuffering = false;
  finishPlayerLoadingFromMedia();
});
sessionVideo.addEventListener("error", handlePlaybackError);
navigator.connection?.addEventListener?.("change", updatePlaybackDiagnostics);
window.addEventListener("resize", handlePlayerViewportChange);
document.addEventListener("fullscreenchange", handlePlayerViewportChange);
document.addEventListener("webkitfullscreenchange", handlePlayerViewportChange);

async function refreshAll() {
  await Promise.allSettled([loadStatus(), loadSessions(false)]);
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
    if (state.page === "exports") await loadExportSettings();
    await maybeShowOnboarding();
  } catch (error) {
    errorElement.textContent = error.message;
  } finally {
    button.disabled = false;
  }
}

function showPage(page, updateHistory) {
  if (page !== "logs" && playbackProbeActive) stopPlaybackProbeMatrix("离开日志中心");
  state.page = ["media", "settings", "logs", "backup", "exports"].includes(page) ? page : "dashboard";
  $("dashboard-page").hidden = state.page !== "dashboard";
  $("media-page").hidden = state.page !== "media";
  $("settings-page").hidden = state.page !== "settings";
  $("logs-page").hidden = state.page !== "logs";
  $("backup-page").hidden = state.page !== "backup";
  $("exports-page").hidden = state.page !== "exports";
  document.querySelectorAll("[data-page-link]").forEach((element) => element.classList.toggle("active", element.dataset.pageLink === state.page));
  closeNavigation();
  const pagePath = state.page === "dashboard" ? "/" : `/${state.page}`;
  if (updateHistory) window.history.pushState({page:state.page}, "", pagePath);
  if (state.page === "media" && !appView.hidden) loadMediaAssets();
  globalThis.TeslaBoxSettingsLayout?.enterPage();
  if (state.page === "settings" && !appView.hidden && !state.settingsLoaded) loadSettings();
  if (state.page === "exports" && !appView.hidden) loadExportSettings();
  if (state.page === "logs" && !appView.hidden) loadLogs();
  scheduleLogsRefresh();
  window.scrollTo({top:0, behavior:"smooth"});
}

window.addEventListener("popstate", () => {
  const page = pageFromPath(window.location.pathname);
  showPage(page, false);
});

async function loadLogs({background = false} = {}) {
  if (state.page !== "logs" || appView.hidden) return;
  const load = async () => {
    $("logs-refresh").disabled = true;
    setLogsNotice("", false);
    try {
      const lineCount = Number($("logs-lines").value) || 50;
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
  return load();
}

async function downloadDiagnosticReport() {
  const button = $("logs-download-report");
  const originalLabel = button.textContent;
  button.disabled = true;
  button.textContent = "正在收集…";
  try {
    await globalThis.TeslaBoxPlaybackTrace?.drain();
    const source = $("logs-download-source").value;
    const parameters = new URLSearchParams();
    if (source) parameters.set("source", source);
    else if (playbackProbeLastID) parameters.set("probe_id", playbackProbeLastID);
    const reportPath = "/api/v1/diagnostics/report" + (parameters.size ? "?" + parameters : "");
    const response = await requestResponse(reportPath, {cache:"no-store", loadingMessage:"正在生成完整诊断报告…"});
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
    showToast("诊断日志已下载，可以直接发给维护者。", "success");
  } catch (error) {
    showToast(error.message, "error");
  } finally {
    button.disabled = false;
    button.textContent = originalLabel;
  }
}

function playbackProbeID() {
  const bytes = new Uint8Array(16);
  window.crypto.getRandomValues(bytes);
  return Array.from(bytes, byte => byte.toString(16).padStart(2, "0")).join("");
}

async function loadMediaAssets() {
  if (state.page !== "media" || appView.hidden) return;
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
}

function renderMediaAssets(payload) {
  const assets = Array.isArray(payload.assets) ? payload.assets : [];
  const categories = ["boombox", "wrap", "lightshow"];
  categories.forEach((category) => renderMediaCategory(category, assets.filter((asset) => asset.category === category)));
  renderMusicBrowser(assets.filter((asset) => asset.category === "music"));
  renderMusicPartitionAvailability(payload);
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

function renderMusicPartitionAvailability(payload) {
  const known = Boolean(payload.music_partition_known);
  const available = Boolean(payload.music_partition_available);
  const disabled = known && !available;
  $("media-music-card").classList.toggle("disabled", disabled);
  $("media-music-files").disabled = disabled;
  $("media-music-upload").disabled = disabled;
  if (disabled) {
    $("media-music-count").textContent = "未创建分区";
    $("media-music-list").replaceChildren(Object.assign(document.createElement("p"), {className:"empty-state", textContent:"当前存储没有 FAT32 音乐分区。请在设置中心的“外接存储”中重新初始化并启用音乐分区。"}));
    $("media-music-page-summary").textContent = "音乐导入已禁用";
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
	if (category === "music" && state.mediaAssets?.music_partition_known && !state.mediaAssets?.music_partition_available) {
		setMediaNotice("当前存储没有 FAT32 音乐分区，请先在设置中心重新初始化并启用音乐分区。", true);
		return;
	}
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
  const labels = {usb:"USB 枚举", media:"视频与转码", application:"TeslaBox 服务", network:"网络与热点", carplay:"CarPlay 服务"};
  const lines = Array.isArray(report.lines) ? report.lines : [];
  const summaries = Array.isArray(report.summaries) ? report.summaries : [];
  const summary = $("logs-summary");
  const summaryList = $("logs-summary-list");
  summary.hidden = summaries.length === 0;
  summaryList.replaceChildren(...summaries.map((message) => {
    const item = document.createElement("li");
    item.textContent = message;
    return item;
  }));
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
  const sequence = ++settingsLoadSequence;
  const releaseRefresh = setRequestControlLoading($("settings-refresh"));
  const current = () => sequence === settingsLoadSequence;
  const apply = (operation, success, failure) => operation.then(
    (value) => {
      if (!current()) return false;
      success(value);
      return true;
    },
    (error) => {
      if (!current()) return false;
      failure(error);
      return false;
    },
  );

  try {
    const results = await Promise.all([
      apply(request("/api/v1/network/wifi"), (value) => { state.wifi = value; renderWiFiSettings(value); }, (error) => renderWiFiUnavailable(error.message)),
      apply(request("/api/v1/network/ap"), (value) => { state.ap = value; if (!globalThis.TeslaBoxSettingsLayout?.isDirty("ap-settings-title")) renderAPSettings(value); }, (error) => renderAPUnavailable(error.message)),
      apply(request("/api/v1/storage/devices"), (value) => { state.storage = value; renderStorageDevices(value); }, (error) => renderStorageUnavailable(error.message)),
      apply(request("/api/v1/update"), (value) => { state.update = value; renderUpdate(value); }, (error) => renderUpdateUnavailable(error.message)),
      apply(request("/api/v1/maintenance"), (value) => { state.maintenance = value; renderMaintenanceSettings(value); }, (error) => renderMaintenanceUnavailable(error.message)),
      apply(request("/api/v1/storage/snapshot-reserve"), renderSnapshotReserveStatus, () => {}),
      apply(request("/api/v1/fan"), (value) => { if (!globalThis.TeslaBoxSettingsLayout?.isDirty("fan-settings-title")) { state.fan = value; renderFanSettings(); } }, (error) => renderFanUnavailable(error.message)),
    ]);
    if (!current()) return;
    state.settingsLoaded = true;
    if (results.slice(0, 4).every(Boolean)) setSettingsNotice("设置已更新", false, false);
    else setSettingsNotice("部分设置暂不可用，请检查设备状态", true);
  } finally {
    releaseRefresh();
  }
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

const fanSceneDefinitions = [
  ["idle", "待机 / 持续录像"], ["single", "单路在线播放"], ["multi", "多路合成播放"],
  ["export", "视频导出"], ["maintenance", "存储维护"],
];

function renderFanSettings() {
  const fan = state.fan;
  if (!fan) return;
  $("fan-status").textContent = fan.available ? "控制在线" : "硬件不可用";
  $("fan-status").className = `badge ${fan.available ? "ok" : "error"}`;
  $("fan-temperature").textContent = fan.temperature_c ? `${fan.temperature_c}℃` : "不可用";
  $("fan-scene").textContent = ({idle:"待机 / 持续录像",single:"单路在线播放",multi:"多路合成播放",export:"视频导出",maintenance:"存储维护"})[fan.scenario] || fan.scenario || "待机";
  $("fan-temperature-request").textContent = `${Number(fan.temperature_request || 0)}%`;
  $("fan-output").textContent = `${Number(fan.output_percent || 0)}%`;
  $("fan-maximum").value = String(fan.maximum_percent ?? 50);
  $("fan-mode-automatic").classList.toggle("active", fan.temperature_mode === "automatic");
  $("fan-mode-manual").classList.toggle("active", fan.temperature_mode === "manual");
  $("fan-manual-levels").hidden = fan.temperature_mode !== "manual";
  renderFanLevels();
  renderFanScenes();
  renderFanCalculation();
  $("fan-save").disabled = false;
  $("fan-detail").textContent = fan.available ? "场景请求与温度请求取较高值后，再乘以最大转速限制。" : (fan.reason || "当前板卡未提供可用的PWM风扇接口，不影响其他功能运行。");
}

function renderFanLevels() {
  const container = $("fan-manual-levels");
  container.replaceChildren();
  (state.fan?.temperature_levels || []).forEach((level, index) => {
    const row = document.createElement("label");
    row.className = "fan-level";
    row.innerHTML = `<strong>档位 ${index+1}</strong><input data-fan-temperature type="number" min="20" max="100" value="${Number(level.temperature_c)}"><span>℃</span><input data-fan-percent type="number" min="0" max="100" value="${Number(level.percent)}"><span>%</span>`;
    container.append(row);
  });
}

function renderFanScenes() {
  const container = $("fan-scene-controls");
  container.replaceChildren();
  for (const [key,label] of fanSceneDefinitions) {
    const value = Number(state.fan?.scenarios?.[key] ?? 0);
    const control = document.createElement("label");
    control.className = "fan-scene-control";
    control.innerHTML = `<header><strong>${label}</strong><output>${value}%</output></header><input data-fan-scene="${key}" type="range" min="0" max="100" step="5" value="${value}">`;
    control.querySelector("input").addEventListener("input", (event) => { control.querySelector("output").value = `${event.target.value}%`; renderFanCalculation(); });
    container.append(control);
  }
}

function renderFanCalculation() {
  const maximum = Number($("fan-maximum").value || 0);
  $("fan-maximum-output").value = `${maximum}%`;
  if (!state.fan) return;
  const requestPercent = Math.max(Number(state.fan.scenario_request || 0), Number(state.fan.temperature_request || 0));
  $("fan-output").textContent = `${Math.round(requestPercent*maximum/100)}%`;
}

function setFanTemperatureMode(mode) {
  if (!state.fan || !["automatic","manual"].includes(mode)) return;
  state.fan.temperature_mode = mode;
  $("fan-mode-automatic").classList.toggle("active", mode === "automatic");
  $("fan-mode-manual").classList.toggle("active", mode === "manual");
  $("fan-manual-levels").hidden = mode !== "manual";
}

function fanSettingsPayload() {
  const levels = [...$("fan-manual-levels").querySelectorAll(".fan-level")].map((row) => ({
    temperature_c:Number(row.querySelector("[data-fan-temperature]").value), percent:Number(row.querySelector("[data-fan-percent]").value),
  }));
  const scenarios = {};
  document.querySelectorAll("[data-fan-scene]").forEach((input) => { scenarios[input.dataset.fanScene] = Number(input.value); });
  return {enabled:Boolean(state.fan.enabled), maximum_percent:Number($("fan-maximum").value), temperature_mode:state.fan.temperature_mode, temperature_levels:levels, scenarios};
}

async function saveFanSettings() {
  const button = $("fan-save"); button.disabled = true;
  try {
    state.fan = await postJSON("/api/v1/fan", fanSettingsPayload());
    globalThis.TeslaBoxSettingsLayout?.markClean("fan-settings-title");
    renderFanSettings();
    setSettingsNotice("风扇设置已保存", false);
  } catch (error) { setSettingsNotice(`保存风扇设置失败：${error.message}`, true); }
  finally { button.disabled = false; }
}

function renderFanUnavailable(message) {
  $("fan-status").textContent = "不可用"; $("fan-status").className = "badge error";
  $("fan-detail").textContent = `风扇控制暂不可用：${message}`; $("fan-save").disabled = true;
}

function renderExportSettings() {
  const settings = state.exportSettings || {delete_after_download:false, retain_count:8};
  const deleteAfterDownload = Boolean(settings.delete_after_download);
  const retainSelect = $("export-retain-count");
  if (!globalThis.TeslaBoxSettingsLayout?.isDirty("export-settings-title")) {
    $("export-delete-after-download").checked = deleteAfterDownload;
    $("export-cache-idle-hours").value = String(settings.cache_idle_hours ?? 24);
    if ([...retainSelect.options].some((option) => Number(option.value) === Number(settings.retain_count))) retainSelect.value = String(settings.retain_count);
    else retainSelect.value = "8";
  }
  $("export-settings-save").disabled = false;
  $("export-history-refresh").disabled = false;
  $("export-retention-status").textContent = deleteAfterDownload ? "下载即清空" : `保留 ${settings.retain_count || 8} 个`;
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
    const size = task.output_bytes ? ` · 实际 ${formatBytes(task.output_bytes)}` : (task.estimated_bytes ? ` · 预计 ${formatBytes(task.estimated_bytes)}` : "");
    detail.textContent = `${formatDate(completedAt)}${size}`;
    copy.append(title, detail);
    const download = document.createElement("a");
    download.className = "secondary compact";
    download.href = `/api/v1/exports/${encodeURIComponent(task.id)}/download`;
    download.download = "";
    download.textContent = "再次下载";
    const remove = document.createElement("button");
    remove.className = "danger-button compact";
    remove.type = "button";
    remove.textContent = "删除";
    remove.addEventListener("click", () => deleteExportHistoryTask(task, remove));
    const actions = document.createElement("div");
    actions.className = "export-history-actions";
    actions.append(download, remove);
    row.append(copy, actions);
    root.append(row);
  });
}

async function deleteExportHistoryTask(task, button) {
  const label = task.request?.mode === "source" ? "多路原画压缩包" : exportTaskLabel(task.request);
  if (!window.confirm(`确认删除“${label}”？删除后无法再次下载。`)) return;
  button.disabled = true;
  button.textContent = "删除中…";
  try {
    await request(`/api/v1/exports/history/${encodeURIComponent(task.id)}`, {method:"DELETE"});
    state.exportHistory = (state.exportHistory || []).filter((item) => item.id !== task.id);
    renderExportSettings();
    setSettingsNotice("历史导出已删除，数据盘空间已释放", false);
  } catch (error) {
    button.disabled = false;
    button.textContent = "删除";
    setSettingsNotice(`删除历史导出失败：${error.message}`, true);
  }
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
		cache_idle_hours:Number($("export-cache-idle-hours").value),
      }),
    });
    globalThis.TeslaBoxSettingsLayout?.markClean("export-settings-title");
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
  $("snapshot-reserve-repair").disabled = false;
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
  $("snapshot-reserve-repair").disabled = true;
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

async function repairSnapshotReserve() {
  const automatic = Boolean(state.maintenance?.auto_recovery);
  const confirmed = await askConfirmation(
    "修复快照预留空间",
    "系统会进入维护窗口，重新测量实际缺口，并且只打洞回收达到安全目标所需的 exFAT 空闲簇。操作可在中断后安全重试，不会删除录像。",
    "确认修复",
    true,
  );
  if (!confirmed) return;
  const button = $("snapshot-reserve-repair");
  button.disabled = true;
  button.textContent = "正在修复…";
  setSettingsNotice("正在校验文件系统并定量回收快照空间，请勿断电或插拔数据盘…", false, false);
  try {
    const result = await postJSON("/api/v1/storage/snapshot-reserve/repair", {confirm_otg_disconnected:!automatic});
    const states = {
      completed:"修复完成",
      cancelled_safe:"已满足运行门槛",
      cancelled_unsafe:"空间仍不足",
      interrupted:"修复已中断",
      failed:"修复失败",
    };
    const summary = `${states[result.state] || "修复结束"}：目标 ${formatBytes(result.target_bytes || 0)}，实际释放 ${formatBytes(result.reclaimed_bytes || 0)}，外层可用 ${formatBytes(result.outer_free_bytes || 0)}。${result.message || ""}`;
    setSettingsNotice(summary, !result.runtime_ready);
    if (!automatic || !result.runtime_ready) await loadStatus();
  } catch (error) {
    setSettingsNotice(`修复中断：${error.message}。重新执行时会按磁盘实际状态继续。`, true);
  } finally {
    button.disabled = false;
    button.textContent = "修复快照预留空间";
  }
}

function renderSnapshotReserveStatus(result) {
  const interrupted = ["running", "interrupted", "failed"].includes(result.state);
  $("snapshot-reserve-cancel").hidden = !interrupted;
  const labels = {pending:"待修复", repairing:"修复中", interrupted:"修复中断", completed:"修复完成", no_reclaimable_space:"无可回收空间"};
  $("snapshot-reserve-detail").textContent = `${labels[result.phase] || "状态正常"} · 外层可用 ${formatBytes(result.outer_free_bytes || 0)} · 运行门槛 ${formatBytes(result.required_bytes || 0)}`;
  if (!result.runtime_ready && result.reason_code === "needs_maintenance_diagnosis") {
    setSettingsNotice(`快照空间需要维护：外层可用 ${formatBytes(result.outer_free_bytes || 0)}，运行期至少需要 ${formatBytes(result.required_bytes || 0)}。`, true);
  } else if (interrupted) {
    setSettingsNotice(`上次快照空间修复未正常结束：${result.message || "可以继续修复或执行安全放弃校验。"}`, true);
  }
}

async function cancelSnapshotReserveRepair() {
  const confirmed = await askConfirmation(
    "安全放弃修复",
    "系统不会回滚已经释放的空闲簇。放弃前会重新检查 exFAT 和运行期空间；只有校验通过后才允许冷启动恢复 Gadget。",
    "确认放弃",
    true,
  );
  if (!confirmed) return;
  const result = await postJSON("/api/v1/storage/snapshot-reserve/cancel", {confirm_otg_disconnected:true});
  renderSnapshotReserveStatus(result);
  setSettingsNotice(result.message || "放弃校验完成", !result.runtime_ready);
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
  const channels = Array.isArray(update.channels) ? update.channels : [];
  const selectedChannel = update.channel || "stable";
  const selectedOption = channels.find((option) => option.id === selectedChannel);
  const detail = update.configured
    ? `更新通道：${selectedOption?.label || selectedChannel}${update.current_commit && update.current_commit !== "unknown" ? ` · 提交 ${update.current_commit.slice(0, 12)}` : ""}`
    : "尚未配置 Gitee 更新清单地址和设备公钥。";
  const installDetail = update.install_result === "success" ? "上次更新已成功安装。" : "";
  $("update-version-detail").textContent = update.error || update.message || installDetail || detail;
  $("update-release-notes").hidden = !update.release_notes;
  renderReleaseNotes($("update-release-notes"), update.release_notes);
  const downloading = update.state === "downloading";
  $("update-progress-wrap").hidden = !downloading;
  $("update-progress").value = Number(update.progress_percent || 0);
  $("update-progress-detail").textContent = `${Number(update.progress_percent || 0)}% · ${formatBytes(update.downloaded_bytes || 0)} / ${formatBytes(update.total_bytes || 0)}`;
  const channelBusy = ["checking", "downloading", "downloaded", "installing"].includes(update.state);
  const fallbackChannels = [
    {id:"stable", label:"稳定版", description:"只接收维护者正式发布并签名的版本。", available:true},
    {id:"beta", label:"内测版", description:"可接收维护者发布的预览版本，可能包含尚未覆盖全部场景的改动。", available:false},
  ];
  const usableChannels = channels.length ? channels : fallbackChannels;
  usableChannels.forEach((option) => {
    const button = $(option.id === "beta" ? "update-channel-beta" : "update-channel-stable");
    button.classList.toggle("active", option.id === selectedChannel);
    button.disabled = !update.configured || !option.available || channelBusy;
    button.title = option.available ? option.description : "该更新源尚未配置";
  });
  $("update-channel-detail").textContent = selectedOption?.description || "稳定版只接收正式发布；内测版可接收预览更新。";
  $("update-check").disabled = !update.configured || ["checking", "downloading", "installing"].includes(update.state);
  $("update-download").hidden = update.state !== "available";
  $("update-install").hidden = update.state !== "downloaded";
  $("update-download").textContent = update.selected_action === "rollback" ? "下载回退版本" : "下载更新";
  $("update-install").textContent = update.selected_action === "rollback" ? "回退并重启" : "安装并重启";
  $("update-history-section").hidden = !update.history_configured;
  if (["downloading", "installing"].includes(update.state)) scheduleUpdatePoll();
  else clearUpdatePoll();
}

async function setUpdateChannel(channel) {
  if (state.update?.channel === channel) return;
  if (channel === "beta") {
    const confirmed = await askConfirmation(
      "切换到内测版",
      "内测版会显示维护者签名的预览更新，可能包含尚未覆盖全部车辆和存储场景的改动。稳定版只接收正式发布。切换后需要重新检查更新。",
      "确认切换到内测版",
      true,
    );
    if (!confirmed) return;
  }
  try {
    state.update = await postJSON("/api/v1/update/channel", {channel});
    state.updateHistory = null;
    $("update-history-list").hidden = true;
    $("update-history-detail").hidden = true;
    renderUpdate(state.update);
    setUpdateNotice(`已切换到${channel === "beta" ? "内测版" : "稳定版"}，请重新检查更新。`, false);
  } catch (error) {
    setUpdateNotice(error.message, true);
  }
}

function renderUpdateUnavailable(message) {
  clearUpdatePoll();
  $("update-status").textContent = "不可用";
  $("update-status").className = "badge error";
  $("update-version-detail").textContent = message;
  $("update-channel-stable").disabled = true;
  $("update-channel-beta").disabled = true;
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
  detail.textContent = history.message || (history.generated_at ? `版本目录更新于 ${formatDate(history.generated_at)}` : "历史版本目录已校验");
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
      const notes = document.createElement("div");
      notes.className = "update-history-notes update-release-markdown";
      renderReleaseNotes(notes, release.release_notes);
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
    rollback ? "选择 TeslaBox 回退版本" : "选择指定 TeslaBox 版本",
    `${rollback ? "将回退" : "将安装"} TeslaBox 至 ${release.version}。内核、无线驱动和 systemd 不会变更；带媒体运行时的新版可以升级 FFmpeg，但回退不会主动降级已安装的媒体运行时。确认后仍需下载并安装。`,
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
    rollback ? "回退 TeslaBox" : "安装系统更新",
		automatic ? `将${rollback ? "回退" : "安装"} ${state.update?.latest_version || "已下载版本"}。TeslaBox 会自动断开 USB Gadget，安装健康检查通过后自动重启恢复；车机或电脑会短暂失去 USB 存储。内核、无线驱动和 systemd 不会变更；带媒体运行时的新版会一并更新 FFmpeg。` : `将${rollback ? "回退" : "安装"} ${state.update?.latest_version || "已下载版本"} 并重启 TeslaBox 服务。请先物理拔掉车机 OTG。即使页面仍显示 configured，也可以确认继续：TeslaBox 会先真正解绑 UDC，再提交更新。更新后请插好 OTG，在日志中心准备下次冷启动，再关机断电重启。`,
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
    if (error.reasonCode === "carplay_network_reserved" && !payload.confirm_carplay_reconnect) {
      const confirmed = await askConfirmation(
        "强制连接 Wi-Fi",
        "连接此 Wi-Fi 会暂时中断 CarPlay，并可能让热点和当前页面短暂断开。连接结束后会自动重新启动 CarPlay，录像继续运行。是否继续？",
        "中断 CarPlay 并连接", true,
      );
      if (confirmed) {
        await runWiFiConnect({...payload, confirm_carplay_reconnect:true});
      } else {
        setSettingsNotice("已取消连接，保持当前 CarPlay 会话", false);
      }
      return;
    }
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
  const labels = {disabled:"已停用", starting:"正在启动", running:"运行中", degraded:"启动异常", blocked:"受限", failed:"启动失败", recovering:"正在恢复"};
  const operationalState = !available ? "unavailable" : (ap.state || (ap.enabled ? "running" : "disabled"));
  $("ap-status").textContent = !available ? "不可用" : (labels[operationalState] || "状态未知");
  $("ap-status").className = `hardware-value${operationalState === "running" ? " ok" : operationalState === "starting" || operationalState === "recovering" ? "" : " error"}`;
  $("ap-interface").textContent = ap.interface || "—";
  $("ap-interface-role").textContent = ap.interface_role || "—";
  $("ap-mac-address").textContent = ap.mac_address || "—";
  $("ap-active-band").textContent = ap.active_band === "a" ? "5 GHz" : ap.active_band === "bg" ? "2.4 GHz" : operationalState === "running" ? "读取中" : "未运行";
  $("ap-active-channel").textContent = ap.active_channel ? String(ap.active_channel) : operationalState === "running" ? "读取中" : "未运行";
  $("ap-ssid").value = ap.ssid || "";
  $("ap-band").value = ap.band || "a";
  populateAPChannels($("ap-band").value, Number(ap.channel || 0), ap.allowed_channels);
  $("ap-enabled").checked = Boolean(ap.desired_enabled ?? ap.enabled);
  $("ap-autoconnect").checked = Boolean(ap.auto_connect);
  $("ap-address").textContent = ap.address || "—";
  $("ap-sharing").textContent = ap.sharing ? "已开启" : "未开启";
  const notices = [];
  if (operationalState !== "running" && ap.status_message) notices.push(ap.status_message);
  if (ap.suggested_action) notices.push(ap.suggested_action);
  if (ap.capability_notice) notices.push(ap.capability_notice);
  $("ap-capability-notice").hidden = notices.length === 0;
  $("ap-capability-notice").textContent = notices.join(" ");
  $("ap-repair-interface").hidden = !Array.isArray(ap.conflicting_profiles) || ap.conflicting_profiles.length === 0;
  const preferredBand = ap.band === "bg" ? "2.4 GHz" : "5 GHz";
  const preferredChannel = ap.channel ? `信道 ${ap.channel}` : "自动信道";
  $("ap-channel-detail").textContent = ["same_channel", "unknown"].includes(ap.concurrency_mode)
    ? `首选配置为 ${preferredBand} / ${preferredChannel}；连接上游 WiFi 时，热点会切换到上游的实际频段和信道。`
    : `首选配置为 ${preferredBand} / ${preferredChannel}；当前无线能力允许时，热点会保持该配置。`;
  $("ap-form").querySelectorAll("input,select,button").forEach((element) => { element.disabled = !available; });
}

function renderAPUnavailable(message) {
  $("ap-status").textContent = "不可用";
  $("ap-status").className = "hardware-value error";
  $("ap-interface").textContent = "—";
  $("ap-interface-role").textContent = "—";
  $("ap-mac-address").textContent = "—";
  $("ap-active-band").textContent = "—";
  $("ap-active-channel").textContent = "—";
  $("ap-address").textContent = message;
  $("ap-capability-notice").hidden = false;
  $("ap-capability-notice").textContent = message;
  $("ap-repair-interface").hidden = true;
  $("ap-channel-detail").textContent = "无法读取热点能力和当前工作信道。";
  $("ap-form").querySelectorAll("input,select,button").forEach((element) => { element.disabled = true; });
}

async function repairAPInterface() {
  const confirmed = await askConfirmation(
    "修复 AP 接口",
    "系统将断开占用 AP 接口的普通 WiFi 配置，关闭它们的自动连接并解除接口绑定；不会删除已保存的 WiFi。请优先通过网线访问此页面。",
    "确认修复",
    true,
  );
  if (!confirmed) return;
  setSettingsNotice("正在提交 AP 接口修复…", false);
  try {
    await postJSON("/api/v1/network/ap/repair", {});
    setSettingsNotice("修复已提交，AP 接口将在 1 秒后释放，请稍后重新开启热点", false);
    window.setTimeout(() => loadSettings(), 2500);
  } catch (error) {
    setSettingsNotice(error.message, true);
  }
}

function populateAPChannels(band, selected, allowedChannels = []) {
  const select = $("ap-channel");
  const supported = new Set((allowedChannels || []).map(Number).filter((channel) => channel > 0));
  const candidates = band === "bg" ? [0,1,2,3,4,5,6,7,8,9,10,11,12,13] : [0,36,40,44,48,149,153,157,161,165];
  const channels = candidates.filter((channel) => channel === 0 || supported.size === 0 || supported.has(channel) || channel === selected);
  select.replaceChildren();
  channels.forEach((channel) => {
    const option = document.createElement("option");
    option.value = String(channel);
    const restricted = channel > 0 && supported.size > 0 && !supported.has(channel);
    option.textContent = channel === 0 ? "自动（按网卡能力选择）" : `${channel}${restricted ? "（当前受限）" : ""}`;
    option.disabled = restricted;
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
    globalThis.TeslaBoxSettingsLayout?.markClean("ap-settings-title");
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
    onboardingStorage = state.storage;
    if (onboardingState?.page_guide_complete && $("onboarding-modal").hidden) maybeShowStorageOnboarding();
    setSettingsNotice("存储设备列表已更新", false);
  } catch (error) {
    renderStorageUnavailable(error.message);
    setSettingsNotice(error.message, true);
  } finally {
    button.disabled = false;
  }
}

async function inspectStorageFragmentation() {
  const button = $("storage-fragmentation-check");
  button.disabled = true;
  button.textContent = "检查中…";
  try {
    const report = await request("/api/v1/storage/fragmentation");
    renderStorageFragmentation(report);
    setSettingsNotice("镜像碎片检查完成", false);
  } catch (error) {
    $("storage-fragmentation-detail").textContent = error.message;
    setSettingsNotice(error.message, true);
  } finally {
    button.disabled = false;
    button.textContent = "检查碎片";
  }
}

function renderStorageFragmentation(report) {
  const levels = {healthy:"正常", warning:"偏多", critical:"严重"};
  $("storage-fragmentation-detail").textContent = `${levels[report.severity] || "未知"} · ${formatNumber(report.extent_count || 0)} 个 extent · ${report.reason || "无诊断说明"}`;
}

async function migrateStorageCapacity() {
  const confirmed = window.confirm("此操作会先预分配新增可写空间，再把固定 20GB 保留区优化为 10GB 或物理容量的 5%（取较大值），不会删除录像。操作期间必须保持供电稳定；旧式 95.9 GiB 分区仍需备份后重新初始化。请先物理拔掉 OTG，确认继续吗？");
  if (!confirmed) return;
  const button = $("storage-capacity-migrate");
  button.disabled = true;
  button.textContent = "优化中，请勿断电…";
  try {
    const result = await postJSON("/api/v1/storage/migrate-capacity", {confirm_otg_disconnected:true});
    const action = result.changed ? "保留空间已安全优化" : "容量布局无需调整";
    const restart = result.requires_cold_boot ? "；请关机后重新上电恢复 Gadget" : "";
    setSettingsNotice(`${action}：卷总容量 ${formatBytes(result.advertised_bytes)}，安全可写约 ${formatBytes(result.writable_bytes)}${restart}`, false);
  } catch (error) {
    setSettingsNotice(error.message, true);
  } finally {
    button.disabled = false;
    button.textContent = "优化保留空间";
  }
}

function renderStorageDevices(storage) {
  const devices = storage.devices || [];
  const list = $("storage-device-list");
  list.replaceChildren();
  const ready = devices.filter((device) => device.initialized).length;
  $("storage-status").textContent = ready ? `${ready} 个已初始化` : devices.length ? "需要初始化" : "未检测到设备";
  $("storage-status").className = `badge${ready ? " ok" : devices.length ? " error" : ""}`;
  renderStorageBenchmark(storage.benchmark, storage.benchmark_error);
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

function renderStorageBenchmark(report, error) {
  if (error) {
    $("storage-benchmark-detail").textContent = `性能体检记录不可用：${error}`;
    return;
  }
  if (!report) {
    $("storage-benchmark-detail").textContent = "尚无性能体检结果；新初始化数据盘时会自动执行一次。";
    return;
  }
  const profiles = {stable:"稳定", limited:"受限", unsafe:"不建议"};
  $("storage-benchmark-detail").textContent = `${profiles[report.profile] || "未知"} · 最低突发 ${Number(report.minimum_burst_mbps || 0).toFixed(1)} MB/s · 最长写停顿 ${formatDurationMS(report.maximum_write_stall_ms || 0)} · fsync P95 ${formatDurationMS(report.fsync_p95_ms || 0)}。${report.reason || ""}`;
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
  const advertisedSize = device.planned_image_size_bytes || device.size_bytes;
  const writableSize = device.planned_writable_bytes || advertisedSize;
  $("storage-modal-detail").textContent = `${device.model || "USB 存储设备"}，物理容量 ${formatBytes(device.size_bytes)}；TeslaCam 单分区卷显示完整 ${formatBytes(advertisedSize)}，安全可写约 ${formatBytes(writableSize)}，其余空间由受保护保留区留给快照和持续写入。目标序列号：${device.serial}。该操作会擦除整块设备。`;
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
  const total = Number(pendingStorage.planned_writable_bytes || pendingStorage.planned_image_size_bytes || pendingStorage.size_bytes || 0);
  const teslaCam = Math.floor(total * percent / 100);
  $("storage-partition-preview").textContent = `TeslaCam ${percent}%（约 ${formatBytes(teslaCam)}） · MUSIC ${100 - percent}%（约 ${formatBytes(total - teslaCam)}）`;
}

function closeStorageModal() {
  $("storage-modal").hidden = true;
  pendingStorage = null;
}

async function maybeShowOnboarding() {
  if (onboardingChecking || state.mustChangePassword || appView.hidden) return;
  onboardingChecking = true;
  try {
    const [guide, storage] = await Promise.all([
      request("/api/v1/onboarding", {loading:false}),
      state.storage ? Promise.resolve(state.storage) : request("/api/v1/storage/devices", {loading:false}),
    ]);
    onboardingState = guide;
    onboardingStorage = storage;
    if (!guide.page_guide_complete) {
      startPageGuide();
      return;
    }
    maybeShowStorageOnboarding();
  } catch (error) {
    console.warn("读取首次引导状态失败", error);
  } finally {
    onboardingChecking = false;
  }
}

function maybeShowStorageOnboarding() {
  const devices = Array.isArray(onboardingStorage?.devices) ? onboardingStorage.devices : [];
  const dismissed = new Set(onboardingState?.dismissed_storage_stable_paths || []);
  const candidate = devices.find((device) => (device.can_resume || device.can_initialize) && !device.initialized && !dismissed.has(device.stable_path));
  if (!candidate) return;
  onboardingStorage = {...onboardingStorage, onboardingCandidate:candidate};
  const status = $("onboarding-storage-status");
  const detail = $("onboarding-storage-detail");
  $("onboarding-storage-name").textContent = candidate.model || "初始化外接存储";
  status.textContent = candidate.can_resume ? "可继续初始化" : "需要初始化";
  status.className = "badge error";
  detail.textContent = `${formatBytes(candidate.size_bytes)} · ${candidate.serial ? `序列号 ${candidate.serial}` : "已取得稳定设备标识"}`;
  $("onboarding-modal").hidden = false;
  window.setTimeout(() => $("onboarding-primary").focus(), 0);
}

async function dismissOnboarding() {
  const candidate = onboardingStorage?.onboardingCandidate;
  $("onboarding-modal").hidden = true;
  if (!candidate?.stable_path) return;
  try {
    onboardingState = await postJSON("/api/v1/onboarding/storage-dismiss", {stable_path:candidate.stable_path}, {loading:false});
  } catch (error) {
    showToast(`无法保存暂不处理状态：${error.message}`, true);
  }
}

function followOnboardingPrimaryAction() {
  const candidate = onboardingStorage?.onboardingCandidate;
  $("onboarding-modal").hidden = true;
  showPage("settings", true);
  activateSettingsTab("storage-settings-section", true);
  window.requestAnimationFrame(() => $("storage-settings-section").scrollIntoView({behavior:"smooth", block:"start"}));
  if (candidate) openStorageModal(candidate);
}

function startPageGuide() {
  $("page-guide").hidden = false;
  renderPageGuideStep(0);
}

function clearPageGuideTarget() {
  document.querySelectorAll(".page-guide-target").forEach((element) => element.classList.remove("page-guide-target"));
  document.querySelectorAll(".page-guide-host").forEach((element) => element.classList.remove("page-guide-host"));
}

function renderPageGuideStep(index) {
  pageGuideIndex = Math.max(0, Math.min(pageGuideSteps.length - 1, index));
  clearPageGuideTarget();
  const step = pageGuideSteps[pageGuideIndex];
  showPage(step.page || "dashboard", false);
  if (step.section) activateSettingsTab(step.section, false);
  const target = $(step.target);
  target.classList.add("page-guide-target");
  target.closest(".topbar")?.classList.add("page-guide-host");
  $("page-guide-title").textContent = step.title;
  $("page-guide-detail").textContent = step.detail;
  $("page-guide-progress").textContent = `${pageGuideIndex + 1} / ${pageGuideSteps.length}`;
  $("page-guide-previous").hidden = pageGuideIndex === 0;
  $("page-guide-next").textContent = pageGuideIndex === pageGuideSteps.length - 1 ? "完成" : "下一步";
  window.requestAnimationFrame(positionPageGuide);
}

function positionPageGuide() {
  const step = pageGuideSteps[pageGuideIndex];
  const target = step ? $(step.target) : null;
  const card = $("page-guide-card");
  if (!target || !card) return;
  const targetRect = TeslaBoxPageDisplay.rect(target);
  const cardRect = TeslaBoxPageDisplay.rect(card);
  const viewport = TeslaBoxPageDisplay.viewport();
  const margin = 14;
  const top = targetRect.bottom + margin + cardRect.height <= viewport.height
    ? targetRect.bottom + margin
    : Math.max(margin, targetRect.top - cardRect.height - margin);
  const left = Math.max(margin, Math.min(viewport.width - cardRect.width - margin, targetRect.left + targetRect.width / 2 - cardRect.width / 2));
  card.style.top = `${top}px`;
  card.style.left = `${left}px`;
}

async function completePageGuide() {
  clearPageGuideTarget();
  pageGuideIndex = -1;
  $("page-guide").hidden = true;
  try {
    onboardingState = await request("/api/v1/onboarding/page-complete", {method:"POST", loading:false});
    maybeShowStorageOnboarding();
  } catch (error) {
    showToast(`无法保存页面引导状态：${error.message}`, true);
  }
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
  presentNotice($(state.page === "exports" ? "exports-notice" : "settings-notice"), message, error, toast);
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
  const scan = storage.scan_progress;
  storageScanActive = Boolean(scan);
  $("video-count").textContent = storage.media_snapshot ? `${formatNumber(storage.video_count || 0)} 个`
    : storage.cached_index_at ? `${formatNumber(storage.cached_video_count || 0)} 个`
    : scan?.stage === "scanning" && scan.discovered > 0 ? `${formatNumber(scan.discovered)} 个` : "—";

  const outerSpace = storage.snapshot_space || {};
  const liveVolumeSpace = storage.media_snapshot?.volume_space || {};
  const cachedVolumeSpace = storage.media_volume_space || {};
  const hasLiveVolumeSpace = Number(liveVolumeSpace.total_bytes || 0) > 0;
  const volumeSpace = hasLiveVolumeSpace ? liveVolumeSpace : cachedVolumeSpace;
  const hasVolumeSpace = Number(volumeSpace.total_bytes || 0) > 0;
  const freePercent = Number(volumeSpace.free_percent || 0);
  $("space-value").textContent = hasVolumeSpace ? formatBytes(volumeSpace.free_bytes || 0) : "—";
  $("space-progress").classList.toggle("is-loading", !hasVolumeSpace);
  $("space-progress").style.width = hasVolumeSpace ? `${Math.max(0, Math.min(100, 100-freePercent))}%` : "30%";
  $("space-progress").style.background = freePercent < 10 ? "var(--warning)" : "var(--ok)";
  const outerPercent = Number(outerSpace.free_percent || 0);
  const sampledAt = storage.media_volume_space_updated_at ? formatDate(storage.media_volume_space_updated_at) : "";
  const volumeLabel = hasLiveVolumeSpace ? "exFAT" : `exFAT 上次采样${sampledAt ? `于 ${sampledAt}` : ""}`;
  const outerLabel = Number(outerSpace.total_bytes) > 0 ? ` · 外层 XFS 可用 ${formatBytes(outerSpace.free_bytes || 0)}（${outerPercent.toFixed(1)}%）` : "";
  $("space-detail").textContent = hasVolumeSpace
    ? `${volumeLabel} · 可用 ${freePercent.toFixed(1)}%${outerLabel}`
    : `正在读取车机虚拟盘空间，完成后自动显示${outerLabel}`;

  const snapshot = storage.media_snapshot;
  const snapshotSpaceProtected = !snapshot && Boolean(storage.cached_index_at) &&
    String(storage.snapshot_error || "").includes("快照空间不足");
  recordingSnapshotSpaceBlocked = snapshotSpaceProtected;
  $("snapshot-state").className = "badge";
  if (scan) {
    $("snapshot-state").textContent = snapshot ? "更新中 · 旧索引可用" : "正在建立";
    $("snapshot-detail").textContent = scan.stage === "scanning"
      ? `已发现 ${formatNumber(scan.discovered)} 个单路 MP4 · 扫描完成后发布索引`
      : scan.stage === "repairing" ? "正在修复快照副本，随后重新统计" : "正在准备只读快照，随后开始统计";
  } else if (storage.snapshot_error) {
    $("snapshot-state").textContent = snapshot ? "旧索引可播放" : snapshotSpaceProtected ? "历史索引 · 空间保护" : "等待重试";
    const updatedAt = snapshot?.created_at ? `上次成功快照于 ${formatDate(snapshot.created_at)}`
      : storage.cached_index_at ? `历史索引保存于 ${formatDate(storage.cached_index_at)}` : "索引尚未完成";
    const retryAt = storage.snapshot_retry_at ? ` · 下次重试 ${formatDate(storage.snapshot_retry_at)}` : "";
    $("snapshot-detail").textContent = snapshotSpaceProtected
      ? `${updatedAt} · 当前只读快照因外层空间不足已回收；释放空间后自动核对并恢复时长与播放${retryAt}`
      : `${updatedAt} · ${storage.snapshot_error}${retryAt}`;
  } else if (!snapshot && storage.cached_index_at) {
    $("snapshot-state").textContent = "历史索引 · 待核对";
    $("snapshot-detail").textContent = `上次保存于 ${formatDate(storage.cached_index_at)} · 当前录像核对完成后可播放`;
  } else {
    $("snapshot-state").textContent = snapshot ? "可播放" : "尚未建立";
    $("snapshot-state").classList.add(snapshot ? "ok" : "error");
    $("snapshot-detail").textContent = snapshot ? `当前快照内的单路 MP4 · 更新于 ${formatDate(snapshot.created_at)}` : "等待首次快照";
  }
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

let recordingIndexCached = false;
let recordingSnapshotSpaceBlocked = false;

async function loadSessions(silent = false, automatic = false) {
  if (state.loading) return;
  const operation = async () => {
    state.loading = true;
    if (!silent) {
      setNotice("正在整理完整录像时间轴…", false, false);
    }
    try {
      const archiveParams = new URLSearchParams({ limit: "50", clip_type: "archive" });
      // 页面定时刷新只读取当前内存结果，不能因为浏览器轮询反复触发快照建立或索引同步。
      if (automatic) archiveParams.set("metadata_only", "true");
      for (const id of ["clip-type", "date", "event"]) {
        const value = $(id).value.trim();
        if (value) archiveParams.set(id.replace("-", "_"), value);
      }
      const recentParams = new URLSearchParams({clip_type: "recent", limit: "1000"});
      if (automatic) recentParams.set("metadata_only", "true");
      const [recentPayload, archivePayloads] = await Promise.all([
        request(`/api/v1/video-sessions?${recentParams}`, {loading: !silent}),
        (async () => {
          const pages = [];
          for (let page = 0; page < archivePages; page++) {
            archiveParams.set("offset", String(page * 50));
            const payload = await request(`/api/v1/video-sessions?${archiveParams}`, {loading: !silent});
            if (pages.length && payload.snapshot_id !== pages[0].snapshot_id) throw new Error("录像正在更新，请稍后重试");
            pages.push(payload);
            if ((page + 1) * 50 >= payload.total) break;
          }
          return pages;
        })(),
      ]);
      // 自动请求开始后用户可能进入删除选择；此时保留原列表，避免已选卡片被重建。
      if (automatic && recordingInteractionActive()) return;
      if (archivePayloads.length && recentPayload.snapshot_id !== archivePayloads[0].snapshot_id) throw new Error("录像正在更新，请稍后重试");
      const nextRecent = (recentPayload.items || []).filter((session) => session.clip_type === "recent");
      const nextArchive = archivePayloads.flatMap((payload) => payload.items || []);
      const unchanged = JSON.stringify(recentSessions) === JSON.stringify(nextRecent) && JSON.stringify(archivedSessions) === JSON.stringify(nextArchive);
      recentSessions = nextRecent;
      archivedSessions = nextArchive;
      const wasCachedIndex = recordingIndexCached;
      recordingIndexCached = Boolean(recentPayload.cached || archivePayloads.some(payload => payload.cached));
      if (wasCachedIndex && !recordingIndexCached) void loadStatus();
      archiveTotal = archivePayloads[0]?.total || 0;
      reconcileRecordingSelection();
      state.total = archivedSessions.length;
      if (!automatic || !unchanged) renderArchiveTimeline();
      $("result-count").textContent = `${formatNumber(archiveTotal)} 段已保存/哨兵录像`;
      $("load-more").hidden = archivedSessions.length >= archiveTotal;
      if (!silent) setNotice(archivedSessions.length === 0 ? "当前筛选条件下没有已保存或哨兵录像" : "", false, false);
    } catch (error) {
      if (automatic) console.warn("后台刷新录像索引失败", error);
      else setNotice(error.message, true);
    } finally {
      state.loading = false;
    }
  };
  return operation();
}

function normalizeRecordingRefreshSeconds(value) {
  const seconds = Number.parseInt(String(value), 10);
  return [0, 15, 30, 60].includes(seconds) ? seconds : 60;
}

let durationRefreshInFlight = false;

// 仅合并同一快照的时长字段，不重建卡片或修改正在播放的详情。
async function refreshRecordingDurations() {
  if (durationRefreshInFlight || appView.hidden || document.hidden || state.page !== "dashboard") return;
  const pending = [...recentSessions, ...archivedSessions].filter(session => !session.cached && (session.duration_pending || session.duration_state === "estimated"));
  if (!pending.length) return;
  durationRefreshInFlight = true;
  try {
    // 分批限制 URL 长度；一批失败不阻断其他批次，并由下一轮重试。
    for (let offset = 0; offset < pending.length; offset += 50) {
      const batch = pending.slice(offset, offset + 50);
      const params = new URLSearchParams({metadata_only: "true", limit: "50"});
      batch.forEach(session => params.append("session_id", session.id));
      try {
        const payload = await request(`/api/v1/video-sessions?${params}`, {loading: false});
        for (const updated of payload.items || []) {
          const session = batch.find(item => item.id === updated.id);
          if (!session || !session.snapshot_id || session.snapshot_id !== payload.snapshot_id ||
              ![...recentSessions, ...archivedSessions].includes(session)) continue;
          session.duration_state = updated.duration_state;
          session.duration_pending = updated.duration_pending;
          session.duration_display_known = updated.duration_display_known;
          session.duration_seconds = updated.duration_seconds;
          session.end_at = updated.end_at;
          for (const card of document.querySelectorAll(".session-card")) {
            if (card.dataset.sessionId !== session.id) continue;
            card.querySelector(".duration-pill").textContent = sessionDurationLabel(session);
            card.querySelector(".session-duration-text").textContent = sessionDurationLabel(session);
            card.querySelector(".session-title").textContent = formatSessionRange(session.start_at, session.end_at);
          }
        }
      } catch (error) { console.warn("后台更新录像时长失败", error); }
    }
  } finally { durationRefreshInFlight = false; }
}

function recordingInteractionActive() {
  return recordingDeleteMode || recordingDeleteInProgress || selectedRecordingSessions.size > 0 || timelineScrubbing || !playerModal.hidden;
}

function canAutomaticallyRefreshRecordings() {
  return recordingRefreshSeconds > 0 && !appView.hidden && state.page === "dashboard" && !document.hidden
    && !state.loading && !state.refreshing && !recordingInteractionActive();
}

function scheduleRecordingRefresh() {
  window.clearTimeout(recordingRefreshTimer);
  recordingRefreshTimer = 0;
  if (recordingRefreshSeconds <= 0) return;
  recordingRefreshTimer = window.setTimeout(async () => {
    try {
      if (canAutomaticallyRefreshRecordings()) await loadSessions(true, true);
    } finally {
      scheduleRecordingRefresh();
    }
  }, recordingRefreshSeconds * 1000);
}

function renderArchiveTimeline() {
  const recentSection = $("recent-recordings-section");
  const recentList = $("recent-video-grid");
  recentList.replaceChildren();
  if (recentSessions.length) {
    recentSessions.forEach((session) => recentList.append(createSessionTimeline(session)));
  }
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

let activeRecordingMenu = null;

function closeRecordingMenu(restoreFocus = false) {
  if (!activeRecordingMenu) return;
  const {button, panel} = activeRecordingMenu;
  panel.hidden = true;
  button.setAttribute("aria-expanded", "false");
  activeRecordingMenu = null;
  if (restoreFocus && button.isConnected) button.focus();
}

document.addEventListener("click", (event) => {
  if (activeRecordingMenu && !activeRecordingMenu.container.contains(event.target)) closeRecordingMenu();
});
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape" && activeRecordingMenu) {
    event.preventDefault();
    closeRecordingMenu(true);
  }
});
window.addEventListener("resize", () => closeRecordingMenu());
document.addEventListener("scroll", () => closeRecordingMenu(), true);

function createRecordingMenu(session) {
  const container = document.createElement("div");
  container.className = "session-more";
  const button = document.createElement("button");
  button.type = "button";
  button.className = "session-more-toggle";
  button.textContent = "⋯";
  button.title = "更多";
  button.setAttribute("aria-label", `更多操作：${formatSessionRange(session.start_at, session.end_at)}`);
  button.setAttribute("aria-haspopup", "menu");
  button.setAttribute("aria-expanded", "false");
  const panel = document.createElement("div");
  panel.className = "session-more-menu";
  panel.id = `recording-menu-${session.id}`;
  panel.hidden = true;
  panel.setAttribute("role", "menu");
  panel.setAttribute("aria-label", "录像操作");
  button.setAttribute("aria-controls", panel.id);
  const remove = document.createElement("button");
  remove.type = "button";
  remove.className = "session-menu-delete";
  remove.textContent = "删除";
  remove.setAttribute("role", "menuitem");
  remove.disabled = recordingDeleteInProgress || session.cached || !["saved", "sentry", "recent"].includes(session.clip_type);
  remove.addEventListener("click", () => { void deleteRecordingSession(session, remove); });
  panel.append(remove);
  panel.addEventListener("click", (event) => {
    if (event.target.closest("[role='menuitem']")) closeRecordingMenu();
  });
  panel.addEventListener("keydown", (event) => {
    const items = [...panel.querySelectorAll("[role='menuitem']:not(:disabled)")];
    const index = items.indexOf(document.activeElement);
    if (["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key) && items.length) {
      event.preventDefault();
      const next = event.key === "Home" ? 0 : event.key === "End" ? items.length-1 : (index + (event.key === "ArrowDown" ? 1 : -1) + items.length) % items.length;
      items[next].focus();
    } else if (event.key === "Tab") closeRecordingMenu();
  });
  button.addEventListener("click", (event) => {
    event.stopPropagation();
    const wasOpen = activeRecordingMenu?.button === button;
    closeRecordingMenu();
    if (wasOpen) return;
    panel.hidden = false;
    button.setAttribute("aria-expanded", "true");
    // 固定定位使下拉菜单不被卡片的圆角裁剪，窄屏和靠近底部时仍留在可视区域。
    const anchor = TeslaBoxPageDisplay.rect(button);
    const box = TeslaBoxPageDisplay.rect(panel);
    const viewport = TeslaBoxPageDisplay.viewport();
    panel.style.left = `${Math.max(8, Math.min(anchor.right-box.width, viewport.width-box.width-8))}px`;
    panel.style.top = `${Math.max(8, Math.min(anchor.bottom+6, viewport.height-box.height-8))}px`;
    activeRecordingMenu = {container, button, panel};
    panel.querySelector("[role='menuitem']:not(:disabled)")?.focus({preventScroll:true});
  });
  container.append(button, panel);
  return container;
}

function createSessionTimeline(session) {
  const card = document.createElement("article");
  card.className = `session-card clip-${clipTypeClass(session.clip_type)}`;
  card.dataset.sessionId = session.id;
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
  heading.className = "session-heading";
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
  const durationText = document.createElement("span");
  durationText.className = "session-duration-text";
  durationText.textContent = sessionDurationLabel(session);
  meta.append(clipBadge, durationText, document.createTextNode(session.event && !session.event_metadata ? ` · ${session.event}` : ""));
  const duration = document.createElement("span");
  duration.className = "duration-pill";
  duration.textContent = sessionDurationLabel(session);
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
  asideMeta.append(duration, createRecordingMenu(session));
  aside.append(asideMeta);
  heading.append(title, meta);
  const eventSummary = createEventSummary(session.event_metadata);
  if (eventSummary) heading.append(eventSummary);

  const open = document.createElement("button");
  open.className = "primary session-open";
  open.type = "button";
  open.textContent = "打开完整录像";
  if (session.cached) {
    open.disabled = true;
    open.textContent = recordingSnapshotSpaceBlocked ? "空间不足 · 暂不可播放" : "历史索引 · 等待核对";
  }
  open.addEventListener("click", () => {
    const display = state.view === "multi" ? "composite" : "single";
    openSessionPlayer(session, preferredCamera(session), 0, display);
  });
  const actions = document.createElement("div");
  actions.className = "session-actions";
  actions.append(open);
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
  globalThis.TeslaBoxNASUploads?.attachSession(card, session);
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
  return !session.cached && (session.clip_type === "saved" || session.clip_type === "sentry");
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
  if (session.cached) { segment.disabled = true; segment.title = "历史索引，正在核对当前录像"; }
  segment.setAttribute("aria-label", segment.title);
  segment.addEventListener("click", (event) => {
    const bounds = track.getBoundingClientRect();
    const ratio = bounds.width > 0 ? Math.max(0, Math.min(1, (event.clientX-bounds.left)/bounds.width)) : 0;
    const position = Number(session.duration_seconds || 0)*ratio;
    openSessionPlayer(session, camera || preferredCamera(session), position, camera ? "single" : "composite", (detail) => Number(detail.duration_seconds || 0)*ratio);
  });
  track.append(segment);
  const eventPosition = sessionEventPosition(session);
  const eventCamera = eventCameraView(session.event_metadata);
  if (eventPosition !== null && (!camera || eventCamera === camera)) {
    const marker = document.createElement("button");
    marker.type = "button";
    if (session.cached) marker.disabled = true;
    marker.className = "event-track-marker";
    marker.style.left = `${Math.max(.5, Math.min(99.5, eventPosition/session.duration_seconds*100))}%`;
    marker.title = eventMarkerTitle(session.event_metadata);
    marker.setAttribute("aria-label", marker.title);
    marker.addEventListener("click", () => {
    openSessionPlayer(session, camera || preferredCamera(session), eventPosition, camera ? "single" : "composite", sessionEventPosition);
    });
    track.append(marker);
  }
  row.append(label, track);
  return row;
}

function sessionMediaURL(url) {
  if (!activeSession?.snapshot_id) return url;
  return `${url}${url.includes("?") ? "&" : "?"}snapshot_id=${encodeURIComponent(activeSession.snapshot_id || "")}`;
}

function observePlaybackTrack(element, video, source, server = false) {
  const reused = !server && element.dataset.videoId === video.id && element.getAttribute("src") === source && element.readyState >= 1 && !element.error;
  globalThis.TeslaBoxPlaybackTrace?.watch(element, {
    session_id:activeSession?.id || "", snapshot_id:activeSession?.snapshot_id || "",
    video_id:video?.id || "", camera:video?.camera || activeCamera,
    display:activeDisplay, quality:effectivePlaybackQuality,
    transport:server ? (TeslaBoxHLS.supported(element) ? "hls" : "fmp4") : "range_mp4",
    media_path:source.split("?")[0],
  }, reused, source);
  if (reused) globalThis.TeslaBoxPlaybackTrace?.arm(element);
}

async function openSessionPlayer(summary, camera, position, display, resolvePosition, autoplay = false) {
	if (summary.cached) { setNotice("正在核对当前录像，完成后即可播放", false, false); return; }
	const sequence = ++sessionOpenSequence;
	sessionOpening = true;
	sessionOpenController?.abort();
	sessionOpenController = new AbortController();
	try {
		const snapshotQuery = summary.snapshot_id ? `?snapshot_id=${encodeURIComponent(summary.snapshot_id)}` : "";
		const session = await request(`/api/v1/video-sessions/${encodeURIComponent(summary.id)}${snapshotQuery}`, {loading:false, signal:sessionOpenController.signal});
		if (sequence !== sessionOpenSequence) return;
		if (typeof resolvePosition === "function") position = resolvePosition(session);
		position = Math.max(0, Math.min(Number(position) || 0, Number(session.duration_seconds) || 0));
		closePlayerMedia();
    activeSession = session;
    globalThis.TeslaBoxPlaybackTrace?.beginOperation();
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
    await loadSessionPosition(position, autoplay);
	} catch (error) {
    if (sequence !== sessionOpenSequence || error.name === "AbortError") return;
    if (error.status === 409) {
      await loadSessions(true, true);
      if (sequence !== sessionOpenSequence) return;
      const refreshed = [...recentSessions, ...archivedSessions].find((session) =>
        session.id === summary.id ||
        (summary.clip_type !== "recent" && session.clip_type === summary.clip_type && session.event && session.event === summary.event)
      );
      if (refreshed && refreshed.snapshot_id !== summary.snapshot_id) {
        return openSessionPlayer(refreshed, camera, position, display, resolvePosition, autoplay);
      }
      if (refreshed) {
        setNotice("录像索引正在更新，请稍后再试", false, false);
        return;
      }
      setNotice("该录像已被删除", false);
      return;
    }
    if (!playerModal.hidden) showPlayerMessage(`无法打开录像：${error.message}`);
    else setNotice(`无法打开录像：${error.message}`, true);
  } finally {
    if(sequence===sessionOpenSequence)sessionOpening=false;
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
	meta.replaceChildren(badge, document.createTextNode(`${session.segment_count} 个片段 · ${sessionDurationLabel(session)}`));
  const unreadable = sessionUnreadableVideoCount(session);
  if (unreadable > 0) {
    const warning = document.createElement("span");
    warning.className = "session-warning";
    warning.textContent = `${unreadable} 个文件暂不可读取，可能尚未写完、已损坏或已由车机移除`;
    meta.append(warning);
  }
  if (session.event_metadata) {
    const context = document.createElement("span");
    context.className = "player-event-context";
    const dot = document.createElement("i");
    dot.className = "event-dot";
    context.append(dot, document.createTextNode(eventMetadataText(session.event_metadata)));
    meta.append(context);
  }
}

function sessionDurationLabel(session) {
  // 历史核对结果对用户就是录像时长，不暴露“缓存”等内部实现状态。
  if (session.duration_state === "cached") return formatDuration(session.duration_seconds);
  if (session.duration_state === "verified") return formatDuration(session.duration_seconds);
  if (session.duration_state === "unavailable") return session.duration_display_known ? formatDuration(session.duration_seconds) : "部分文件不可读取";
  if (session.cached && recordingSnapshotSpaceBlocked) return "待释放空间后核对";
  if (session.cached) return "待当前录像核对";
  if (Number(session.duration_seconds) > 0) return `约 ${formatDuration(session.duration_seconds)}`;
  return "正在读取时长";
}

function sessionUnreadableVideoCount(session) {
  let count = 0;
  session?.moments?.forEach((moment) => {
    moment.videos?.forEach((video) => {
      if (video.duration_state === "unavailable") count++;
    });
  });
  return count;
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

async function loadSessionPosition(position, autoplay, preferredMomentIndex = -1) {
  const session = activeSession;
  const pending = loadSessionPositionAttempt(position, autoplay, preferredMomentIndex);
  const token = loadToken;
  const loaded = await pending;
  if (loaded || !session || token !== loadToken || activeSession !== session) return loaded;
  // 原生 video 把 HTTP 409 显示成媒体错误 4；先核实快照，再决定是否重试。
  try {
    const refreshed = await refreshPlaybackSessionSnapshot(session, position, preferredMomentIndex,
      () => token === loadToken && activeSession === session);
    if (!refreshed || token !== loadToken || activeSession !== session) return false;
    reportPlaybackDiagnostic("load_failed", "snapshot_refreshed:录像快照已更新，按原录像时刻重试一次");
    activeSession = refreshed.session;
    $("session-timeline").max = String(activeSession.duration_seconds);
    $("total-time").textContent = formatDuration(activeSession.duration_seconds);
    renderPlayerMeta(activeSession);
    renderCameraSwitcher();
    renderSegmentMarkers();
    renderEventMarker();
    // 只重试一次；快照再次轮换或视频确实损坏时不能形成无限请求。
    return await loadSessionPositionAttempt(refreshed.position, autoplay, refreshed.momentIndex);
  } catch (error) {
    if (token !== loadToken || activeSession !== session) return false;
    reportPlaybackDiagnostic("load_failed", `snapshot_refresh_failed:${error.status || ""}:${error.message}`);
    showPlayerMessage(error.message || "录像状态发生变化，请刷新列表后重新打开");
    return false;
  }
}

async function refreshPlaybackSessionSnapshot(session, position, preferredMomentIndex, current) {
  const endTrace = globalThis.TeslaBoxPlaybackTrace?.span(sessionVideo,"snapshot_check") || (()=>{});
  const endpoint = `/api/v1/video-sessions/${encodeURIComponent(session.id)}`;
  try {
    await request(`${endpoint}?snapshot_id=${encodeURIComponent(session.snapshot_id || "")}`, {loading:false});
    endTrace("unchanged");
    return null;
  } catch (error) {
    endTrace(error.status === 409 ? "snapshot_expired" : "request_failed");
    if (!current()) return null;
    if (error.status !== 409) throw error;
  }
  // 循环窗口会覆盖，不能把旧窗口的相对秒数套在新的窗口上。
  if (session.id.startsWith("rw-")) throw new Error("最近录像已更新或循环覆盖，请刷新列表后重新打开对应时间段");
  const selected = selectMomentAtPosition(session, position, preferredMomentIndex);
  if (!selected) throw new Error("原录像时刻已不可用，请刷新列表后重新选择");
  const latest = await request(endpoint, {loading:false});
  globalThis.TeslaBoxPlaybackTrace?.mark(sessionVideo,"snapshot_refresh","detail_received");
  if (!current()) return null;
  if (!latest.snapshot_id || latest.snapshot_id === session.snapshot_id) return null;
  const momentIndex = latest.moments?.findIndex(moment => moment.id === selected.moment.id &&
    moment.recorded_at === selected.moment.recorded_at) ?? -1;
  if (latest.id !== session.id || momentIndex < 0) throw new Error("所选录像已被删除或更新，请刷新列表后重新选择");
  const moment = latest.moments[momentIndex];
  if (selected.localTime > timelineMomentDuration(moment)) throw new Error("所选录像时刻已不可用，请重新选择播放位置");
  return {session:latest, momentIndex, position:timelineMomentStart(moment)+selected.localTime};
}

async function loadSessionPositionAttempt(position, autoplay, preferredMomentIndex = -1) {
  if (!activeSession) return false;
  globalThis.TeslaBoxPlaybackTrace?.beginBatch();
  const requestID = ++timelinePositionRequestID;
  const token = ++loadToken;
  showPlayerLoading(token);
  timelinePositionPending = true;
  requestedPosition = Math.max(0, Math.min(Number(position), Number(activeSession.duration_seconds)));
  activeStreamTimelineStart = null;
  activeTelemetryMediaOffset = 0;
  try {
    if (isCompositeDisplay(activeDisplay)) return await loadCompositePosition(requestedPosition, autoplay, token, preferredMomentIndex);
    const selection = selectVideoAtPosition(activeSession, activeCamera, requestedPosition, preferredMomentIndex);
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
    resetPlaybackTelemetryLoad();
    if (usesServerTranscode()) return await loadServerSingle(selection, autoplay, token);
    $("segment-status").textContent = `第 ${activeMomentIndex+1} / ${activeSession.moments.length} 分钟 · ${cameraLabel(activeCamera)}`;
    $("download-link").href = sessionMediaURL(`/api/v1/videos/${encodeURIComponent(activeVideo.id)}/download`);
    const source = sessionMediaURL(`/api/v1/videos/${encodeURIComponent(activeVideo.id)}/content`);
    observePlaybackTrack(sessionVideo, activeVideo, source);
    delete sessionVideo.dataset.compositeKey;
    if (sessionVideo.dataset.videoId !== activeVideo.id || sessionVideo.getAttribute("src") !== source ||
        sessionVideo.readyState < 1 || sessionVideo.error) {
      sessionVideo.pause();
      sessionVideo.dataset.loadingToken = String(token);
      sessionVideo.dataset.videoId = activeVideo.id;
      TeslaBoxHLS.release(sessionVideo);
      sessionVideo.src = source;
      sessionVideo.load();
      globalThis.TeslaBoxPlaybackTrace?.arm(sessionVideo);
      const loaded = await waitForMetadata(sessionVideo, 8000, token);
      if (token !== loadToken) return false;
      finishSessionVideoLoad(token);
      if (!loaded) {
        reportPlaybackDiagnostic("load_failed", "单路原画视频元数据加载失败");
        releaseVideoElement(sessionVideo);
        showPlayerMessage("单路视频加载失败，具体媒体错误或等待超时已记录，请重试或切换压缩画质");
        return false;
      }
    }
    if (token !== loadToken) return false;
    const localTime = Math.max(0, selection.localTime);
    if (!await seekVideoElement(sessionVideo, localTime, token)) return false;
    startPlaybackTelemetryLoad(activeVideo);
    if (autoplay) void playVideoSafely(sessionVideo, token);
    return true;
  } finally {
    if (requestID === timelinePositionRequestID) timelinePositionPending = false;
  }
}

async function loadCompositePosition(position, autoplay, token, preferredMomentIndex = -1) {
  const selection = selectMomentAtPosition(activeSession, position, preferredMomentIndex);
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
    .filter((video) => video && videoCoversAbsolutePosition(video, selection.absolute));
  if (layoutVideos.length === 0) {
    showPlayerMessage("这个时刻缺少当前布局需要的摄像头画面");
    releaseCompositeAuxiliaryTracks();
    releaseVideoElement(sessionVideo);
    return false;
  }
  const masterVideo = layoutVideos.find((video) => video.camera === "front") || layoutVideos[0];
  activeCamera = masterVideo.camera;
  activeVideo = masterVideo;
  resetPlaybackTelemetryLoad();
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
  startPlaybackTelemetryLoad(activeVideo);
  $("segment-status").textContent = `第 ${activeMomentIndex+1} / ${activeSession.moments.length} 分钟 · ${layout.label} · ${nextTracks.size} 路可用 · ${qualityLabel(effectivePlaybackQuality)}`;
  $("download-link").href = sessionMediaURL(`/api/v1/videos/${encodeURIComponent(masterVideo.id)}/download`);
  renderCameraSwitcher();
  // 定位已完成；辅助视角的 play() 可能长期等待缓冲，不能继续锁住时间轴，
  // 否则布局切换会取到旧 requestedPosition，把已推进的主视角倒退回去。
  if (autoplay) void playCompositeTracks();
  return true;
}

async function loadServerComposite(selection, layout, masterVideo, autoplay, token) {
  releaseCompositeAuxiliaryTracks();
  const streamStart = Math.max(0, selection.localTime);
  const source = sessionMediaURL(`/api/v1/video-groups/${encodeURIComponent(selection.moment.id)}/composite?layout=${encodeURIComponent(activeDisplay)}&quality=${encodeURIComponent(effectivePlaybackQuality)}&start_seconds=${encodeURIComponent(streamStart.toFixed(3))}`);
  const sourceKey = `${selection.moment.id}:${activeDisplay}:${effectivePlaybackQuality}:${streamStart.toFixed(3)}`;
  if (sessionVideo.dataset.compositeKey !== sourceKey || sessionVideo.readyState < 1 || sessionVideo.error) {
    sessionVideo.pause();
    sessionVideo.dataset.loadingToken = String(token);
    sessionVideo.dataset.compositeKey = sourceKey;
    sessionVideo.dataset.videoId = masterVideo.id;
    if (!await attachServerPlayback(source, token)) return false;
    const loaded = await waitForMetadata(sessionVideo, 15000, token);
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
  if (!await seekVideoElement(sessionVideo, 0, token)) return false;
  activeStreamTimelineStart = timelineMomentStart(selection.moment)+selection.localTime;
  activeTelemetryMediaOffset = Math.max(0, (selection.absolute-new Date(activeVideo.recorded_at).getTime())/1000);
  compositeTracks = new Map([[masterVideo.camera, {element:sessionVideo, video:masterVideo, lastHardSync:0}]]);
  startPlaybackTelemetryLoad(activeVideo);
  $("segment-status").textContent = `第 ${activeMomentIndex+1} / ${activeSession.moments.length} 分钟 · ${layout.label} · ${qualityLabel(effectivePlaybackQuality)}`;
  $("download-link").href = sessionMediaURL(`/api/v1/videos/${encodeURIComponent(masterVideo.id)}/download`);
  applyDisplayState();
  applyServerCompositeFrameGeometry();
  renderCameraSwitcher();
  if (autoplay) void playVideoSafely(sessionVideo, token);
  return true;
}

async function loadServerSingle(selection, autoplay, token) {
  releaseCompositeAuxiliaryTracks();
  const streamStart = Math.max(0, selection.localTime);
  const source = sessionMediaURL(`/api/v1/video-groups/${encodeURIComponent(selection.moment.id)}/composite?layout=single&camera=${encodeURIComponent(activeCamera)}&quality=${encodeURIComponent(effectivePlaybackQuality)}&start_seconds=${encodeURIComponent(streamStart.toFixed(3))}`);
  const sourceKey = `${selection.moment.id}:single:${activeCamera}:${effectivePlaybackQuality}:${streamStart.toFixed(3)}`;
  delete sessionVideo.dataset.videoId;
  if (sessionVideo.dataset.compositeKey !== sourceKey || sessionVideo.readyState < 1 || sessionVideo.error) {
    sessionVideo.pause();
    sessionVideo.dataset.loadingToken = String(token);
    sessionVideo.dataset.compositeKey = sourceKey;
    if (!await attachServerPlayback(source, token)) return false;
    const loaded = await waitForMetadata(sessionVideo, 15000, token);
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
  resetPlaybackTelemetryLoad();
  if (!await seekVideoElement(sessionVideo, 0, token)) return false;
  activeStreamTimelineStart = timelineMomentStart(selection.moment)+selection.localTime;
  activeTelemetryMediaOffset = Math.max(0, selection.localTime);
  compositeTracks = new Map([[activeCamera, {element:sessionVideo, video:activeVideo, lastHardSync:0}]]);
  startPlaybackTelemetryLoad(activeVideo);
  $("segment-status").textContent = `第 ${activeMomentIndex+1} / ${activeSession.moments.length} 分钟 · ${cameraLabel(activeCamera)} · ${qualityLabel(effectivePlaybackQuality)}`;
  $("download-link").href = sessionMediaURL(`/api/v1/videos/${encodeURIComponent(activeVideo.id)}/download`);
  applyDisplayState();
  if (autoplay) void playVideoSafely(sessionVideo, token);
  return true;
}

function selectMomentAtPosition(session, position, preferredMomentIndex = -1) {
  if (!session || session.moments.length === 0) return null;
  const boundedPosition = Math.max(0, Math.min(Number(position) || 0, Number(session.duration_seconds) || 0));
  let momentIndex = Number.isInteger(preferredMomentIndex) && preferredMomentIndex >= 0 && preferredMomentIndex < session.moments.length
    ? preferredMomentIndex
    : 0;
  if (momentIndex !== preferredMomentIndex) {
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
  failedTracks.forEach(([camera, track]) => {
    reportPlaybackDiagnostic("load_failed", `原画视角加载失败:${camera}`, track.element);
  });
  const details = failedTracks.map(([camera, track]) =>
    `${cameraLabel(camera)}(错误 ${track.element.error?.code || 0}/状态 ${track.element.readyState || 0})`);
  reportPlaybackDiagnostic("load_failed", `原画多路视频元数据加载失败${details.length ? `：${details.join("、")}` : ""}`);
}

async function prepareVideoElement(element, video, absolute, token) {
  const source = sessionMediaURL(`/api/v1/videos/${encodeURIComponent(video.id)}/content`);
  observePlaybackTrack(element, video, source);
  if (element.dataset.videoId !== video.id || element.getAttribute("src") !== source ||
      element.readyState < 1 || element.error) {
    element.pause();
    if (element === sessionVideo) element.dataset.loadingToken = String(token);
    element.dataset.videoId = video.id;
    TeslaBoxHLS.release(element);
    element.src = source;
    element.load();
    globalThis.TeslaBoxPlaybackTrace?.arm(element);
    const loaded = await waitForMetadata(element, 12000, token);
    if (token !== loadToken || element.dataset.videoId !== video.id ||
        element.getAttribute("src") !== source) return false;
    if (element === sessionVideo) finishSessionVideoLoad(token);
    if (!loaded) return false;
  }
  if (token !== loadToken) return false;
  const localTime = Math.max(0, (absolute-new Date(video.recorded_at).getTime())/1000);
  return await seekVideoElement(element, localTime, token);
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

function selectVideoAtPosition(session, camera, position, preferredMomentIndex = -1) {
  const momentSelection = selectMomentAtPosition(session, position, preferredMomentIndex);
  if (!momentSelection) return null;
  const {absolute, momentIndex, moment} = momentSelection;
  const video = moment.videos.find((item) => item.camera === camera);
  if (!video || !videoCoversAbsolutePosition(video, absolute)) return null;
  return {
    ...momentSelection, video, momentIndex,
    localTime: (absolute-new Date(video.recorded_at).getTime())/1000,
  };
}

function videoCoversAbsolutePosition(video, absolute) {
  const recordedAt = new Date(video?.recorded_at).getTime();
  const duration = Number(video?.duration_seconds);
  if (video?.duration_state === "unavailable") return false;
  if (!Number.isFinite(recordedAt) || !Number.isFinite(absolute)) return false;
  // 旧索引可能没有时长，继续交给浏览器探测，避免升级后误判已有录像不可播放。
  if (!Number.isFinite(duration) || duration <= 0) return true;
  const localTime = (absolute-recordedAt)/1000;
  // 同一分钟的六路文件起始时间可能有少量偏差，起点前允许沿用该路首帧。
  return localTime >= -3 && localTime < Math.max(.05, duration-.05);
}

async function switchCamera(camera) {
  if (!activeSession || (activeDisplay === "single" && camera === activeCamera)) return;
  globalThis.TeslaBoxPlaybackTrace?.beforeSwitch("camera_switch");
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
  globalThis.TeslaBoxPlaybackTrace?.mark(sessionVideo,"user_action",sessionVideo.paused ? "user_play" : "user_pause");
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
    void playVideoSafely(sessionVideo);
  } else {
    playbackWasPlaying = false;
    sessionVideo.pause();
  }
}

function previewTimelinePosition() {
  timelineScrubbing = true;
  $("current-time").textContent = formatDuration(Number($("session-timeline").value));
}

function seekTimelinePosition() {
  const position = Number($("session-timeline").value);
  timelineScrubbing = false;
  const autoplay = !sessionVideo.paused;
  if (isCompositeDisplay(activeDisplay) && !usesServerComposite()) pauseCompositeTracks();
  else sessionVideo.pause();
  loadSessionPosition(position, autoplay);
}

function updateUnifiedTimeline() {
  if (!activeSession || !activeVideo || timelineScrubbing || sessionVideo.seeking || timelinePositionPending) return;
  const moment = activeSession.moments[activeMomentIndex];
  if (!moment) return;
  const position = activeStreamTimelineStart === null
    ? Math.max(0, timelineMomentStart(moment) +
      (new Date(activeVideo.recorded_at)-new Date(moment.recorded_at))/1000 + sessionVideo.currentTime)
    : activeStreamTimelineStart+sessionVideo.currentTime;
  requestedPosition = Math.min(position, activeSession.duration_seconds);
  $("session-timeline").value = String(requestedPosition);
  $("current-time").textContent = formatDuration(requestedPosition);
  renderPlaybackTelemetry();
  if (isCompositeDisplay(activeDisplay) && !usesServerComposite()) synchronizeCompositeTracks();
}

function resetPlaybackTelemetryLoad() {
  telemetryAbortController?.abort();
  telemetryAbortController = null;
  clearTimeout(telemetryPrefetchTimer);
  telemetryPrefetchTimer = 0;
  activeTelemetry = null;
  renderPlaybackTelemetry();
}

function telemetryCacheKey(video) {
  return `${activeSession?.snapshot_id || ""}:${video.id}`;
}

function cachePlaybackTelemetry(videoID, timeline) {
  if (telemetryByVideo.size >= 24 && !telemetryByVideo.has(videoID)) {
    telemetryByVideo.delete(telemetryByVideo.keys().next().value);
  }
  telemetryByVideo.set(videoID, timeline);
  return timeline;
}

function requestPlaybackTelemetry(video, signal) {
  if (!video?.id) return Promise.reject(new Error("缺少 SEI 视频标识"));
  const key = telemetryCacheKey(video);
  const cached = telemetryByVideo.get(key);
  if (cached) return Promise.resolve(cached);
  const pending = telemetryRequestsByVideo.get(key);
  if (pending) return pending;
  const promise = request(sessionMediaURL(`/api/v1/videos/${encodeURIComponent(video.id)}/sei`), {signal, loading:false})
    .then((timeline) => cachePlaybackTelemetry(key, timeline))
    .finally(() => {
      if (telemetryRequestsByVideo.get(key) === promise) telemetryRequestsByVideo.delete(key);
    });
  telemetryRequestsByVideo.set(key, promise);
  return promise;
}

function scheduleAdjacentTelemetryPrefetch() {
  clearTimeout(telemetryPrefetchTimer);
  const session = activeSession;
  const momentIndex = activeMomentIndex;
  const camera = activeVideo?.camera || activeCamera;
  if (!session || momentIndex < 0) return;
  telemetryPrefetchTimer = window.setTimeout(async () => {
    telemetryPrefetchTimer = 0;
    if (activeSession !== session) return;
    for (const index of [momentIndex+1, momentIndex-1]) {
      const moment = session.moments?.[index];
      const video = moment?.videos?.find((item) => item.camera === camera) ||
        moment?.videos?.find((item) => item.camera === "front") || moment?.videos?.[0];
      if (!video?.id || telemetryByVideo.has(telemetryCacheKey(video))) continue;
      try {
        await requestPlaybackTelemetry(video);
      } catch (_) {
        // 预取失败不能影响当前视频；真正切换到该分钟时仍会按正常路径重试。
      }
      if (activeSession !== session) return;
    }
  }, 700);
}

function startPlaybackTelemetryLoad(video) {
  if (!video?.id) {
    renderPlaybackTelemetry();
    return;
  }
  const cached = telemetryByVideo.get(telemetryCacheKey(video));
  if (cached) {
    activeTelemetry = cached;
    renderPlaybackTelemetry();
    scheduleAdjacentTelemetryPrefetch();
    return;
  }
  const controller = new AbortController();
  const videoID = video.id;
  telemetryAbortController = controller;
  renderPlaybackTelemetry();
  requestPlaybackTelemetry(video, controller.signal).then((timeline) => {
    if (controller.signal.aborted || telemetryAbortController !== controller || activeVideo?.id !== videoID) return;
    activeTelemetry = timeline;
    telemetryAbortController = null;
    renderPlaybackTelemetry();
    scheduleAdjacentTelemetryPrefetch();
  }).catch((error) => {
    if (controller.signal.aborted || telemetryAbortController !== controller || activeVideo?.id !== videoID) return;
    activeTelemetry = {available:false, message:`Tesla SEI 信息暂不可用：${error.message}`, samples:[]};
    telemetryAbortController = null;
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
  const sourceSeconds = Math.max(0, (sessionVideo.currentTime || 0)+activeTelemetryMediaOffset);
  $("playback-timestamp").textContent = formatRecordingTimestamp(activeVideo.recorded_at, sourceSeconds);
  if (!activeTelemetry) {
    renderPlaybackTelemetrySample(null, "loading", "正在读取 Tesla SEI 信息…");
    return;
  }
  if (!activeTelemetry.available) {
    renderPlaybackTelemetrySample(null, "unavailable", activeTelemetry.message || "该录像没有 Tesla SEI 信息");
    return;
  }
  const samples = Array.isArray(activeTelemetry.samples) ? activeTelemetry.samples : [];
  const currentMS = Math.max(0, Math.round(sourceSeconds*1000));
  let selected = null;
  for (const sample of samples) {
    if (Number(sample.offset_ms || 0) > currentMS) break;
    selected = sample;
  }
  renderPlaybackTelemetrySample(selected?.metadata, selected?.metadata ? "ready" : "unavailable", "当前画面尚无 Tesla SEI 信息");
}

function renderPlaybackTelemetrySample(metadata, state, message) {
  const container = $("playback-telemetry");
  container.dataset.state = state;
  $("playback-sei").textContent = message;
  const gear = String(metadata?.gear || "").trim().toUpperCase();
  const speedMPS = Number(metadata?.speed_mps);
  const accelerator = normalizeAcceleratorPercent(metadata?.accelerator_position);
  const steering = Number(metadata?.steering_angle);
  const hasSteering = Number.isFinite(steering);
  const brakeApplied = metadata?.brake_applied === true;
  const leftBlinker = metadata?.left_blinker === true;
  const rightBlinker = metadata?.right_blinker === true;
  const autopilot = playbackAutopilotState(metadata?.autopilot);
  $("playback-gear").textContent = gear || "—";
  $("playback-gear-cell").dataset.gear = gear;
  $("playback-speed").textContent = Number.isFinite(speedMPS) ? String(Math.round(Math.abs(speedMPS)*3.6)) : "—";
  $("playback-accelerator").textContent = accelerator === null ? "—" : String(accelerator);
  $("playback-autopilot").dataset.mode = autopilot.mode;
  $("playback-autopilot").title = autopilot.title;
  $("playback-autopilot-label").textContent = autopilot.label;
  $("playback-accelerator").closest(".telemetry-accelerator").dataset.active = String(accelerator !== null && accelerator > 0);
  $("playback-accelerator").closest(".telemetry-accelerator").style.setProperty("--accelerator-level", `${accelerator || 0}%`);
  $("playback-steering").closest(".telemetry-steering").dataset.assisted = String(autopilot.steeringAssisted);
  const steeringSign = steering > 0 ? "+" : steering < 0 ? "−" : "\u00a0";
  $("playback-steering").textContent = hasSteering ? `${steeringSign}${Math.abs(steering).toFixed(1)}°` : "—";
  $("playback-steering-wheel").style.transform = `rotate(${hasSteering ? Math.max(-540, Math.min(540, steering)) : 0}deg)`;
  $("playback-brake").dataset.active = String(brakeApplied);
  $("playback-brake").title = brakeApplied ? "刹车已踩下" : "刹车松开";
  $("playback-left-blinker").dataset.active = String(leftBlinker);
  $("playback-left-blinker").title = leftBlinker ? "左转向灯已开启" : "左转向灯关闭";
  $("playback-right-blinker").dataset.active = String(rightBlinker);
  $("playback-right-blinker").title = rightBlinker ? "右转向灯已开启" : "右转向灯关闭";
}

function normalizeAcceleratorPercent(value) {
  const raw = Number(value);
  if (!Number.isFinite(raw)) return null;
  const percent = Math.abs(raw) <= 1.01 ? raw*100 : raw;
  return Math.round(Math.max(0, Math.min(100, percent)));
}

function playbackAutopilotState(value) {
  const source = String(value || "").trim();
  const normalized = source.toUpperCase();
  if (normalized.includes("FSD")) return {mode:"fsd", label:"FSD", title:`辅助驾驶：${source}`, steeringAssisted:true};
  if (normalized === "EAP") return {mode:"steering", label:"EAP", title:"辅助驾驶：EAP", steeringAssisted:true};
  if (normalized === "AP" || normalized.includes("AUTOSTEER") || source.includes("自动辅助转向")) {
    return {mode:"steering", label:"AP", title:`辅助驾驶：${source || "自动辅助转向"}`, steeringAssisted:true};
  }
  if (normalized.includes("TACC")) return {mode:"tacc", label:"TACC", title:`辅助驾驶：${source}`, steeringAssisted:false};
  if (!source || source === "无") return {mode:"off", label:"手动", title:"辅助驾驶：关闭", steeringAssisted:false};
  return {mode:"unknown", label:source, title:`辅助驾驶：${source}`, steeringAssisted:false};
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
  // 换源和拖动会产生旧媒体的迟到 ended；此时推进会把用户刚选中的短尾片段覆盖掉。
  if (!activeSession || sessionOpening || timelinePositionPending || sessionVideo.seeking) return;
  const next = activeMomentIndex+1;
  if (next >= activeSession.moments.length) {
    $("session-timeline").value = String(activeSession.duration_seconds);
    $("current-time").textContent = formatDuration(activeSession.duration_seconds);
    $("segment-status").textContent = "本段录像播放完毕";
    return;
  }
  const nextPosition = timelineMomentStart(activeSession.moments[next]);
  // 自然续播已知道目标片段，不能再用可能重叠的索引时长反查，否则会重新选中刚结束的旧片段。
  loadSessionPosition(nextPosition, true, next);
}

function updatePlayButton() {
  const paused = sessionVideo.paused;
  $("play-toggle").dataset.state = paused ? "paused" : "playing";
  $("play-toggle").setAttribute("aria-label", paused ? "播放" : "暂停");
}

function currentSessionPosition() {
  if (timelinePositionPending) return requestedPosition;
  if (!activeSession || !activeVideo) return requestedPosition;
  const moment = activeSession.moments[activeMomentIndex];
  if (!moment) return requestedPosition;
  const position = activeStreamTimelineStart === null
    ? timelineMomentStart(moment) + (new Date(activeVideo.recorded_at)-new Date(moment.recorded_at))/1000 + sessionVideo.currentTime
    : activeStreamTimelineStart+sessionVideo.currentTime;
  return Math.max(0, Math.min(activeSession.duration_seconds, position));
}

async function setDisplayMode(display) {
  if (!(display === "single" || compositeLayouts[display]) || display === activeDisplay || !activeSession) return;
  globalThis.TeslaBoxPlaybackTrace?.mark(sessionVideo,"user_action","layout_switch");
  globalThis.TeslaBoxPlaybackTrace?.beforeSwitch("layout_switch");
  const position = currentSessionPosition();
  const autoplay = !sessionVideo.paused;
  pauseCompositeTracks();
  sessionVideo.pause();
  // 不同合成布局的宽高比不同，切换前必须清除上一布局留下的内联尺寸和全屏舞台变量。
  resetCompositeFrameGeometry();
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
  initializeExportRange(position, Math.min(exportTimeBounds().duration, position+60));
  $("export-layout").value = isCompositeDisplay(activeDisplay) ? activeDisplay : "single";
  $("export-camera").value = activeCamera;
  $("export-quality").value = effectivePlaybackQuality === "original" && !isCompositeDisplay(activeDisplay) ? "original" : "high";
  $("export-burn-timestamp").checked = true;
  $("export-burn-sei").checked = false;
  document.querySelector('input[name="export-overlay-mode"][value="dashcam"]').checked = true;
  resetSourceExportSelection();
  setExportMode("clip");
  renderExportOptions();
  resetExportTaskUI();
}

function restoreExportSelection(request) {
  if (!activeSession) return;
  if (request?.mode !== "source") {
    const start = Math.max(0, Number(request?.start_seconds || 0));
    initializeExportRange(start, start+Math.max(1, Number(request?.duration_seconds || 1)));
	if (request?.layout) $("export-layout").value = request.layout;
	if (request?.camera || request?.cameras?.[0]) $("export-camera").value = request.camera || request.cameras[0];
	if (request?.quality) $("export-quality").value = request.quality;
	$("export-burn-timestamp").checked = Boolean(request?.burn_timestamp);
	$("export-burn-sei").checked = Boolean(request?.burn_sei);
	const mode = request?.overlay_mode || ($("export-burn-sei").checked ? "top_bar" : "dashcam");
	const modeInput = document.querySelector(`input[name="export-overlay-mode"][value="${mode}"]`);
	if (modeInput) modeInput.checked = true;
	renderExportOptions();
    return;
  }
  sourceExportSessionID = activeSession.id;
  sourceExportMomentIDs.clear();
  sourceExportCameras.clear();
  (request.moment_ids || []).forEach((id) => sourceExportMomentIDs.add(id));
  (request.cameras || []).forEach((camera) => sourceExportCameras.add(camera));
}

function exportTimeBounds() {
  const start=Date.parse(activeSession?.start_at);
  const duration=Number(activeSession?.duration_seconds)||0;
  return {start,end:start+duration*1000,duration};
}

function initializeExportRange(start, end) {
  if (!activeSession) return;
  const maximum = Math.max(1, exportTimeBounds().duration);
  for (const input of [$("export-range-start"), $("export-range-end")]) {
    input.min = "0";
    input.max = String(maximum);
    input.step = "0.1";
  }
  setExportRange(start, end);
  const markers = $("export-range-markers");
  markers.replaceChildren();
  (activeSession.moments || []).slice(1).forEach((moment) => {
    const position = Number(moment.timeline_start_seconds || 0);
    if (!(position > 0 && position < maximum)) return;
    const marker = document.createElement("span");
    marker.className = "export-range-marker";
    marker.style.left = `${position/maximum*100}%`;
    markers.append(marker);
  });
}

function setExportRange(start, end) {
  if (!activeSession) return;
  const maximum = Math.max(1, exportTimeBounds().duration);
  start = Math.max(0, Math.min(Number(start) || 0, maximum-1));
  end = Math.max(start+1, Math.min(Number(end) || start+1, maximum));
  $("export-range-start").value = start.toFixed(1);
  $("export-range-end").value = end.toFixed(1);
  renderExportRange(start, end, maximum);
}

function updateExportRangeFromSlider(changed) {
  let start = Number($("export-range-start").value);
  let end = Number($("export-range-end").value);
  if (end-start < 1) {
    if (changed === "start") start = end-1;
    else end = start+1;
  }
  setExportRange(start, end);
}

function updateExportRangeFromTime(changed) {
  if (!activeSession) return;
  const input = changed === "start" ? $("export-start-time") : $("export-end-time");
  const seconds = parseExportPosition(input.value);
  if (!Number.isFinite(seconds)) {
    $("export-error").textContent = "请输入 时:分:秒，例如 00:03:25.5";
    renderExportRange(Number($("export-range-start").value), Number($("export-range-end").value), exportTimeBounds().duration);
    return;
  }
  $("export-error").textContent = "";
  const start = changed === "start" ? seconds : Number($("export-range-start").value);
  const end = changed === "end" ? seconds : Number($("export-range-end").value);
  setExportRange(start, end);
}

function adjustExportRange(value) {
  const [edge, deltaText] = String(value || "").split(":");
  const step = Number($("export-adjust-step").value);
  if (![1,3,5,10,15,30,60].includes(step)) return;
  const delta = Number(deltaText)*step;
  if (!Number.isFinite(delta)) return;
  const currentStart = Number($("export-range-start").value);
  const currentEnd = Number($("export-range-end").value);
  const start = edge === "start" ? Math.min(currentEnd-1, currentStart+delta) : currentStart;
  const end = edge === "end" ? Math.max(currentStart+1, currentEnd+delta) : currentEnd;
  setExportRange(start, end);
}

function renderExportRange(start, end, maximum) {
  const startPercent = Math.max(0, Math.min(100, start/maximum*100));
  const endPercent = Math.max(startPercent, Math.min(100, end/maximum*100));
  $("export-range-track").style.setProperty("--range-start", `${startPercent}%`);
  $("export-range-track").style.setProperty("--range-end", `${endPercent}%`);
  $("export-range-duration").textContent = `已选择 ${formatDuration(end-start)} · 总时长 ${formatDuration(maximum)}`;
  $("export-start-time").value = formatExportPosition(start);
  $("export-end-time").value = formatExportPosition(end);
}

function formatExportPosition(seconds) {
  const ticks = Math.round(Math.max(0, seconds)*10);
  return `${String(Math.floor(ticks/36000)).padStart(2,"0")}:${String(Math.floor(ticks/600)%60).padStart(2,"0")}:${String(Math.floor(ticks/10)%60).padStart(2,"0")}.${ticks%10}`;
}

function parseExportPosition(value) {
  const match = /^(\d+):([0-5]\d):([0-5]\d)(?:\.(\d))?$/.exec(String(value).trim());
  return match ? Number(match[1])*3600+Number(match[2])*60+Number(match[3])+Number(match[4] || 0)/10 : NaN;
}

function localDateTimeInput(value) {
  const local = new Date(value.getTime()-value.getTimezoneOffset()*60*1000);
  return local.toISOString().slice(0,19);
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
    ? "单路高保真和原画均使用 H.265（HEVC）硬编，避免 RK3568 H.264 编码残块；原画保留源分辨率。"
    : "高保真多路合成会导出 H.265（HEVC）MP4，以避开 RKMPP H.264 多路残块；请使用支持 HEVC 的本地播放器。需要最广兼容或与网页原画完全一致时，请切换到“多路原画”下载原始文件和离线浏览页。";
	renderExportOverlayOptions();
}

function renderExportOverlayOptions() {
  const burnTimestamp = $("export-burn-timestamp").checked;
  const burnSEI = $("export-burn-sei").checked;
  const field = $("export-overlay-mode-field");
  field.hidden = !burnTimestamp && !burnSEI;
  const dashcam = document.querySelector('[data-export-overlay-mode="dashcam"]');
  dashcam.hidden = !burnTimestamp || burnSEI;
  let selected = document.querySelector('input[name="export-overlay-mode"]:checked');
  if (!selected || selected.closest("label").hidden) {
    const fallback = burnTimestamp && !burnSEI ? "dashcam" : "top_bar";
    selected = document.querySelector(`input[name="export-overlay-mode"][value="${fallback}"]`);
    selected.checked = true;
  }
  const help = {
    dashcam:"时间直接绘制在左上角，不增加黑边；适合单路和多路的常规行车记录仪成片。",
    top_bar:"在画面上方扩展独立信息栏，完全不遮挡录像内容。",
    floating:"信息覆盖在画面顶部，成片尺寸不变，适合更紧凑的展示。",
  };
  $("export-overlay-mode-help").textContent = help[selected?.value] || "";
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
    anchor.href = sessionMediaURL(`/api/v1/videos/${encodeURIComponent(video.id)}/download`);
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
        body: JSON.stringify({mode:"source", session_id:activeSession.id, snapshot_id:activeSession.snapshot_id, moment_ids:[...sourceExportMomentIDs], cameras:[...sourceExportCameras]}),
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
  const start = Number($("export-range-start").value);
  const end = Number($("export-range-end").value);
  const duration = end-start;
  $("export-error").textContent = "";
  if (cameras.length === 0) {
    $("export-error").textContent = "请至少选择一个摄像头视角";
    return;
  }
  if (!Number.isFinite(start) || !Number.isFinite(end) || start < 0 || duration < 1 || end > exportTimeBounds().duration+.01) {
    $("export-error").textContent = "导出范围必须位于当前完整录像内，且结束时间必须晚于开始时间至少 1 秒";
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
        mode:"clip", session_id:activeSession.id, snapshot_id:activeSession.snapshot_id, start_seconds:start, duration_seconds:duration, layout, cameras, quality:$("export-quality").value,
        burn_timestamp:$("export-burn-timestamp").checked, burn_sei:$("export-burn-sei").checked,
        overlay_mode:document.querySelector('input[name="export-overlay-mode"]:checked')?.value || "",
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
  globalThis.TeslaBoxSettingsLayout?.renderExportTask(task);
  const source = task.request?.mode === "source";
  const labels = {queued:"正在排队", running:source ? "正在打包" : "正在导出", completed:source ? "打包完成" : "导出完成", failed:source ? "打包失败" : "导出失败", canceled:"已取消"};
  const running = ["queued", "running"].includes(task.state);
  $("export-task-status").hidden = false;
  $("export-state").textContent = task.state === "running" && task.phase === "preparing" ? "正在准备录像" : (labels[task.state] || task.state);
  $("export-progress").value = task.progress_percent || 0;
  const size = task.output_bytes ? ` · 实际 ${formatBytes(task.output_bytes)}` : (task.estimated_bytes ? ` · 预计 ${formatBytes(task.estimated_bytes)}` : "");
  const files = source && task.file_count ? ` · ${task.file_count} 个源文件` : "";
  const copyDetail = task.phase === "preparing" ? `准备录像 ${formatBytes(task.copied_bytes || 0)} / ${formatBytes(task.copy_total_bytes || 0)}` : `已完成 ${task.progress_percent || 0}%`;
  $("export-detail").textContent = task.error || (running ? `${copyDetail}${files}${size}` : `可以修改参数后再次导出${files}${size}`);
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
  const frameHeight = TeslaBoxPageDisplay.rect(mainVideoFrame).height;
  const fixedHeight = Math.max(0, panel.scrollHeight-frameHeight);
  const availableHeight = Math.max(180, TeslaBoxPageDisplay.viewport().height-40-fixedHeight);
  const availableWidth = Math.max(240, panel.clientWidth-2);
  mainVideoFrame.style.width = `${Math.floor(Math.min(availableWidth, availableHeight*activeCompositeAspect))}px`;
}

function handlePlayerViewportChange() {
  if (playerModal.hidden) return;
  updatePlayerFullscreenDensity();
  updateFullscreenButton();
  window.requestAnimationFrame(() => {
    if (isPlayerFullscreen()) fitFullscreenCompositeGrid();
    else fitCompositeFrame();
  });
}

function fitFullscreenCompositeGrid() {
  if (!isPlayerFullscreen()) return;
  const telemetryHeight = TeslaBoxPageDisplay.rect($("playback-telemetry")).height;
  const controlsHeight = TeslaBoxPageDisplay.rect(document.querySelector(".unified-controls")).height;
  const availableWidth = sessionPlayerView.clientWidth || window.innerWidth;
  const availableHeight = Math.max(120,(sessionPlayerView.clientHeight || window.innerHeight)-telemetryHeight-controlsHeight);
  const aspect = fullscreenPlaybackAspect();
  const width = Math.min(availableWidth,availableHeight*aspect);
  const height = width/aspect;
  const minimumChromeWidth = sessionPlayerView.dataset.fullscreenDensity === "compact" ? 372 : 520;
  const stageWidth = Math.min(availableWidth,Math.max(width,minimumChromeWidth));
  sessionPlayerView.style.setProperty("--fullscreen-stage-width",`${Math.floor(stageWidth)}px`);
  sessionPlayerView.style.setProperty("--fullscreen-video-width",`${Math.floor(width)}px`);
  sessionPlayerView.style.setProperty("--fullscreen-video-height",`${Math.floor(height)}px`);
  const content = isCompositeDisplay(activeDisplay) && !usesServerComposite() ? compositeGrid : sessionVideo;
  content.style.width = `${Math.floor(width)}px`;
  content.style.height = `${Math.floor(height)}px`;
}

function fullscreenPlaybackAspect() {
  if (isCompositeDisplay(activeDisplay) && activeCompositeAspect > 0) return activeCompositeAspect;
  if (sessionVideo.videoWidth > 0 && sessionVideo.videoHeight > 0) return sessionVideo.videoWidth/sessionVideo.videoHeight;
  return isCompositeDisplay(activeDisplay) ? (724/469)*compositeLayoutFactor(activeDisplay) : 4/3;
}

function resetFullscreenCompositeGrid() {
  sessionPlayerView.style.removeProperty("--fullscreen-stage-width");
  sessionPlayerView.style.removeProperty("--fullscreen-video-width");
  sessionPlayerView.style.removeProperty("--fullscreen-video-height");
  compositeGrid.style.removeProperty("width");
  compositeGrid.style.removeProperty("height");
  sessionVideo.style.removeProperty("width");
  sessionVideo.style.removeProperty("height");
}

function isPlayerFullscreen() {
  if (sessionPlayerView.classList.contains("player-fullscreen-fallback")) return true;
  const fullscreenElement = document.fullscreenElement || document.webkitFullscreenElement;
  if (fullscreenElement && (fullscreenElement === sessionPlayerView || sessionPlayerView.contains(fullscreenElement))) return true;
  try {
    if (sessionPlayerView.matches(":fullscreen") || sessionPlayerView.matches(":-webkit-full-screen")) return true;
  } catch (_) {
    // 不支持全屏伪类的浏览器仍可通过 fullscreenElement 或页面级回退类判断。
  }
  return false;
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
  const token = loadToken;
  playbackWasPlaying = true;
  await Promise.all([...compositeTracks.values()].map((track) => playVideoSafely(track.element, token)));
}

async function playVideoSafely(video, token = loadToken) {
  const source = video.getAttribute("src");
  const endTrace = globalThis.TeslaBoxPlaybackTrace?.span(video,"play") || (()=>{});
  try {
    await video.play();
    endTrace(token === loadToken && video.getAttribute("src") === source ? "resolved" : "superseded");
    return token === loadToken && video.getAttribute("src") === source;
  } catch (error) {
    endTrace(error?.name === "NotAllowedError" ? "not_allowed" : error?.name === "AbortError" ? "aborted" : "rejected");
    // 旧播放请求可能在切换后才拒绝，不能覆盖新画面的提示或触发降级。
    if (token !== loadToken || video.getAttribute("src") !== source) return false;
    const name = error?.name || "Error";
    reportPlaybackDiagnostic("media_error", `play_rejected:${name}:${String(error?.message || "").slice(0, 180)}`, video);
    if (name === "NotAllowedError") showPlayerMessage("浏览器需要手动开始播放，请轻点播放继续");
    else if (name === "AbortError") showPlayerMessage("播放请求被中断，请轻点播放重试");
    else if (name === "NotSupportedError") showPlayerMessage("当前视频无法解析或格式不受支持，请重试或切换压缩画质");
    else showPlayerMessage("视频播放失败，请重试；详细原因已记录到诊断报告");
    return false;
  }
}

function pauseCompositeTracks() {
  compositeTracks.forEach((track) => {
    globalThis.TeslaBoxPlaybackTrace?.mark(track.element,"pause_request","controller_pause");
    track.element.pause();
  });
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

async function enterFullscreen() {
  if (isPlayerFullscreen()) {
    await exitPlayerFullscreen();
    return;
  }
  try {
    if (sessionPlayerView.requestFullscreen) await sessionPlayerView.requestFullscreen();
    else if (sessionPlayerView.webkitRequestFullscreen) sessionPlayerView.webkitRequestFullscreen();
    else activatePlayerFullscreenFallback();
  } catch (_) {
    activatePlayerFullscreenFallback();
  }
  handlePlayerViewportChange();
}

async function exitPlayerFullscreen() {
  sessionPlayerView.classList.remove("player-fullscreen-fallback");
  document.documentElement.classList.remove("player-fullscreen-active");
  try {
    if (document.fullscreenElement && document.exitFullscreen) await document.exitFullscreen();
    else if (document.webkitFullscreenElement && document.webkitExitFullscreen) document.webkitExitFullscreen();
  } catch (_) {
    // 全屏状态可能已由浏览器退出，继续恢复播放器布局。
  }
  handlePlayerViewportChange();
}

function activatePlayerFullscreenFallback() {
  sessionPlayerView.classList.add("player-fullscreen-fallback");
  document.documentElement.classList.add("player-fullscreen-active");
  updatePlayerFullscreenDensity();
  updateFullscreenButton();
}

function updatePlayerFullscreenDensity() {
  if (!isPlayerFullscreen()) {
    delete sessionPlayerView.dataset.fullscreenDensity;
    return;
  }
  const shortEdge = Math.min(window.innerWidth,window.innerHeight);
  sessionPlayerView.dataset.fullscreenDensity = shortEdge <= 620 ? "compact" : shortEdge <= 900 ? "medium" : "large";
}

function updateFullscreenButton() {
  const button = $("fullscreen-button");
  const fullscreen = isPlayerFullscreen();
  button.textContent = fullscreen ? "退出全屏" : "全屏";
  button.setAttribute("aria-label", fullscreen ? "退出全屏播放" : "进入全屏播放");
}

function waitForMetadata(video, timeout = 4000, token = loadToken) {
  const endTrace = globalThis.TeslaBoxPlaybackTrace?.span(video,"metadata") || (()=>{});
  const source = video.getAttribute("src");
  const current = () => token === loadToken && video.getAttribute("src") === source;
  if (!current()) { endTrace("superseded"); return Promise.resolve(false); }
  if (video.readyState >= 1 && !video.error) { endTrace("already_ready"); return Promise.resolve(true); }
  return new Promise((resolve) => {
    let settled = false;
    let timer = 0;
    let poll = 0;
    const started = performance.now();
    const done = (event) => {
      if (settled) return;
      settled = true;
      window.clearTimeout(timer);
      window.clearInterval(poll);
      video.removeEventListener("loadedmetadata", done);
      video.removeEventListener("error", done);
      const ready = current() && video.readyState >= 1 && !video.error;
      endTrace(!current() ? "superseded" : ready ? "ready" : video.error ? "media_error" : "timeout");
      if (current() && !ready) {
        const cause = video.error ? `media_error:${video.error.code}:${video.error.message || ""}` : event?.type || "timeout";
        reportPlaybackDiagnostic("load_failed", `metadata_${cause};wait_ms=${Math.round(performance.now()-started)}`, video);
      }
      resolve(ready);
    };
    video.addEventListener("loadedmetadata", done);
    video.addEventListener("error", done);
    // 主元素可被下一次切换复用；及时移除旧监听，避免旧任务等待新源的事件。
    poll = window.setInterval(() => { if (!current()) done(); }, 50);
    timer = window.setTimeout(done, timeout);
    if (video.error) done();
  });
}

function seekVideoElement(video, requestedTime, token, timeout = 4000) {
  const endTrace = globalThis.TeslaBoxPlaybackTrace?.span(video,"seek",requestedTime) || (()=>{});
  if (token !== loadToken) { endTrace("superseded"); return Promise.resolve(false); }
  const target = Number.isFinite(video.duration)
    ? Math.min(Math.max(0, requestedTime), Math.max(0, video.duration-.05))
    : Math.max(0, requestedTime);
  if (!video.seeking && Math.abs(video.currentTime-target) < .05) {
    endTrace(video.error ? "media_error" : "already_positioned");
    if (video === sessionVideo) finishPlayerLoadingFromMedia();
    return Promise.resolve(!video.error);
  }
  return new Promise((resolve) => {
    let settled = false;
    let timer = 0;
    const done = (event) => {
      if (settled) return;
      settled = true;
      window.clearTimeout(timer);
      video.removeEventListener("seeked", done);
      video.removeEventListener("error", done);
      video.removeEventListener("emptied", done);
      const positioned = !video.seeking && Math.abs(video.currentTime-target) < .25;
      const ready = token === loadToken && !video.error && positioned;
      endTrace(token !== loadToken ? "superseded" : video.error ? "media_error" : ready ? "positioned" : event?.type === "emptied" ? "source_emptied" : event?.type === "exception" ? "exception" : !event ? "timeout" : "not_positioned");
      if (ready && video === sessionVideo) finishPlayerLoadingFromMedia();
      resolve(ready);
    };
    video.addEventListener("seeked", done);
    video.addEventListener("error", done);
    video.addEventListener("emptied", done);
    timer = window.setTimeout(done, timeout);
    try {
      video.currentTime = target;
    } catch (_) {
      done({type:"exception"});
    }
  });
}

function showPlayerMessage(message) {
  hidePlayerLoading();
  $("player-message").textContent = message;
  $("player-message").hidden = false;
}

function hidePlayerMessage() { $("player-message").hidden = true; }

function showPlayerLoading(token) {
  const loading = $("player-loading");
  const display = activeDisplay === "single"
    ? cameraLabel(activeCamera)
    : (compositeLayouts[activeDisplay]?.label || "多路合成");
  loading.dataset.token = String(token);
  $("player-loading-message").textContent = `正在加载${display} · ${qualityLabel(effectivePlaybackQuality)}…`;
  loading.hidden = false;
}

function hidePlayerLoading(token = null) {
  const loading = $("player-loading");
  if (token !== null && loading.dataset.token !== String(token)) return;
  loading.hidden = true;
  delete loading.dataset.token;
}

function finishPlayerLoadingFromMedia() {
  if (sessionVideo.error || sessionVideo.readyState < 2) return;
  hidePlayerLoading(loadToken);
}

function closePlayerMedia() {
  globalThis.TeslaBoxPlaybackTrace?.stop();
  loadToken++;
  hidePlayerLoading();
  telemetryAbortController?.abort();
  telemetryAbortController = null;
  clearTimeout(telemetryPrefetchTimer);
  telemetryPrefetchTimer = 0;
  activeTelemetry = null;
  timelinePositionRequestID++;
  timelinePositionPending = false;
  timelineScrubbing = false;
  pauseCompositeTracks();
  releaseCompositeAuxiliaryTracks();
  releaseVideoElement(sessionVideo);
  compositeGrid.replaceChildren();
  resetCompositeFrameGeometry();
  activeSession = null;
  activeVideo = null;
  activeMomentIndex = -1;
  requestedPosition = 0;
  activeStreamTimelineStart = null;
  activeTelemetryMediaOffset = 0;
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
  globalThis.TeslaBoxPlaybackTrace?.beforeSwitch("quality_switch");
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
  if (!sessionVideo.error) return;
  hidePlayerLoading();
  reportPlaybackDiagnostic("media_error", `浏览器媒体错误:${sessionVideo.error.code}:${sessionVideo.error.message || ""}`);
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

function reportPlaybackDiagnostic(event, reason, video = sessionVideo) {
  if (!activeSession) return;
  const moment = activeSession.moments?.[activeMomentIndex] || null;
  const key = `${event}|${reason}|${activeSession.id}|${moment?.id || ""}|${activeDisplay}|${effectivePlaybackQuality}|${video.dataset.videoId || ""}`;
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
    probe_track: [...compositeTracks.values(), ...pendingCompositeTracks.values()].find(track => track.element === video)?.video?.camera || activeCamera || "",
    buffer_seconds: Number(bufferedSeconds(video).toFixed(2)) || 0,
    stalls: playbackStalls,
    media_error_code: video.error?.code || 0,
    ready_state: video.readyState || 0,
    network_state: video.networkState || 0,
    current_time_seconds: Number(video.currentTime) || 0,
    video_width: video.videoWidth || 0,
    video_height: video.videoHeight || 0,
    total_frames: video.getVideoPlaybackQuality?.().totalVideoFrames || 0,
    dropped_frames: video.getVideoPlaybackQuality?.().droppedVideoFrames || 0,
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

async function attachServerPlayback(source, token) {
  observePlaybackTrack(sessionVideo, activeVideo, source, true);
  const endTrace = globalThis.TeslaBoxPlaybackTrace?.span(sessionVideo,"stream_prepare") || (()=>{});
  try {
    const attached = await TeslaBoxHLS.attach(sessionVideo, source, () => token === loadToken);
    if (attached && token === loadToken) globalThis.TeslaBoxPlaybackTrace?.arm(sessionVideo);
    endTrace(attached ? "attached" : "superseded");
    return attached;
  } catch (error) {
    endTrace(token !== loadToken ? "superseded" : "failed");
    if (token !== loadToken) return false;
    finishSessionVideoLoad(token);
    reportPlaybackDiagnostic("load_failed", `分片播放准备失败：${error.message}`);
    releaseVideoElement(sessionVideo);
    showPlayerMessage(error.message || "视频准备失败，请稍后重试");
    return false;
  }
}

function releaseVideoElement(element) {
  globalThis.TeslaBoxPlaybackTrace?.unwatch(element,"released");
  TeslaBoxHLS.release(element);
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
  sessionOpening = false;
  sessionOpenSequence++;
  sessionOpenController?.abort();
  closeExportModal();
  if (isPlayerFullscreen()) void exitPlayerFullscreen();
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

function applyTeslaBrowserCompatibility(enabled) {
  const active = Boolean(enabled);
  $("tesla-browser-compatibility").checked = active;
  document.documentElement.dataset.teslaBrowserCompatibility = active ? "true" : "false";
  writePreference("teslabox-tesla-browser-compatibility", active ? "true" : "false");
  if (!active) {
    compatibilitySelectDocumentObserver?.disconnect();
    compatibilitySelectDocumentObserver = null;
    compatibilitySelects.forEach((entry, select) => {
      entry.observer.disconnect();
      select.removeEventListener("change", entry.sync);
      select.classList.remove("compat-select-native");
      entry.wrapper.remove();
    });
    compatibilitySelects.clear();
    return;
  }
  document.querySelectorAll("select").forEach(upgradeCompatibilitySelect);
  if (compatibilitySelectDocumentObserver) return;
  compatibilitySelectDocumentObserver = new MutationObserver((mutations) => {
    mutations.forEach((mutation) => mutation.addedNodes.forEach((node) => {
      if (!(node instanceof Element)) return;
      if (node.matches("select")) upgradeCompatibilitySelect(node);
      node.querySelectorAll?.("select").forEach(upgradeCompatibilitySelect);
    }));
  });
  compatibilitySelectDocumentObserver.observe(document.body, {childList:true, subtree:true});
}

function upgradeCompatibilitySelect(select) {
  if (!(select instanceof HTMLSelectElement) || compatibilitySelects.has(select)) return;
  const wrapper = document.createElement("div");
  wrapper.className = "compat-select";
  const trigger = document.createElement("button");
  trigger.type = "button";
  trigger.className = "compat-select-trigger";
  trigger.setAttribute("aria-haspopup", "listbox");
  trigger.setAttribute("aria-expanded", "false");
  const options = document.createElement("div");
  options.id = `${select.id || "select"}-compat-options-${++compatibilitySelectSequence}`;
  options.className = "compat-select-options";
  options.setAttribute("role", "listbox");
  options.hidden = true;
  trigger.setAttribute("aria-controls", options.id);
  wrapper.append(trigger, options);
  select.insertAdjacentElement("afterend", wrapper);
  select.classList.add("compat-select-native");

  const sync = () => renderCompatibilitySelect(select);
  const observer = new MutationObserver(sync);
  observer.observe(select, {attributes:true, childList:true, characterData:true, subtree:true});
  compatibilitySelects.set(select, {wrapper, trigger, options, observer, sync});
  select.addEventListener("change", sync);
  trigger.addEventListener("click", (event) => {
    event.preventDefault();
    event.stopPropagation();
    const opening = options.hidden;
    closeOpenCompatibilitySelect(select);
    renderCompatibilitySelect(select);
    options.hidden = !opening;
    wrapper.classList.toggle("open", opening);
    trigger.setAttribute("aria-expanded", opening ? "true" : "false");
  });
  options.addEventListener("click", (event) => {
    if (!(event.target instanceof Element)) return;
    const button = event.target.closest("button[data-option-index]");
    if (!button || button.disabled) return;
    event.preventDefault();
    event.stopPropagation();
    const index = Number(button.dataset.optionIndex);
    if (!Number.isInteger(index) || !select.options[index]) return;
    const changed = select.selectedIndex !== index;
    select.selectedIndex = index;
    renderCompatibilitySelect(select);
    closeOpenCompatibilitySelect();
    if (changed) {
      select.dispatchEvent(new Event("input", {bubbles:true}));
      select.dispatchEvent(new Event("change", {bubbles:true}));
    }
  });
  renderCompatibilitySelect(select);
}

function renderCompatibilitySelect(select) {
  const entry = compatibilitySelects.get(select);
  if (!entry) return;
  const selected = select.options[select.selectedIndex];
  entry.trigger.textContent = selected?.textContent?.trim() || "请选择";
  entry.trigger.disabled = select.disabled;
  entry.trigger.setAttribute("aria-label", select.getAttribute("aria-label") || selected?.textContent?.trim() || "选择选项");
  entry.wrapper.hidden = select.hidden;
  entry.options.textContent = "";
  const fragment = document.createDocumentFragment();
  Array.from(select.options).forEach((option, index) => {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "compat-select-option";
    button.dataset.optionIndex = String(index);
    button.textContent = option.textContent;
    button.disabled = option.disabled;
    button.setAttribute("role", "option");
    button.setAttribute("aria-selected", option.selected ? "true" : "false");
    fragment.appendChild(button);
  });
  entry.options.appendChild(fragment);
}

function closeOpenCompatibilitySelect(except) {
  let closed = false;
  compatibilitySelects.forEach((entry, select) => {
    if (select === except || entry.options.hidden) return;
    entry.options.hidden = true;
    entry.wrapper.classList.remove("open");
    entry.trigger.setAttribute("aria-expanded", "false");
    closed = true;
  });
  return closed;
}

document.addEventListener("click", () => closeOpenCompatibilitySelect());

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
  if (triggerType === "vehicle_emergency_braking") return "自动紧急制动触发";
  if (reason === "user_interaction_dashcam_icon_tapped") return "手动点击保存";
  if (reason === "user_interaction_dashcam_panel_save" || reason === "user_interaction_dashcam_launcher_action_tapped") return "手动保存录像";
  if (reason === "user_interaction_honk") return "鸣笛保存";
  if (reason.startsWith("sentry_panic")) return "哨兵警报触发";
  if (reason.startsWith("sentry_aware_accel")) return "哨兵检测到车辆震动";
  if (reason.startsWith("sentry_aware_object")) return "哨兵检测到周边对象";
  if (reason.startsWith("sentry_aware")) return "哨兵警戒触发";
  if (reason === "vehicle_auto_emergency_braking") return "自动紧急制动触发";
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

function formatDurationMS(value) {
  const milliseconds = Math.max(0, Number(value || 0));
  if (milliseconds >= 1000) return `${(milliseconds/1000).toFixed(2)} 秒`;
  return `${Math.round(milliseconds)} 毫秒`;
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

let storageScanActive = false;
let storageProgressLoading = false;
// 进度查询只读取内存状态，不触发快照或重复采样系统指标。
window.setInterval(async () => {
  if (appView.hidden || document.hidden || state.page !== "dashboard" || storageProgressLoading || !(storageScanActive || state.loading)) return;
  storageProgressLoading = true;
  try { renderStatus(await request("/api/v1/storage", {loading:false})); }
  catch (error) { console.warn("读取索引进度失败", error); }
  finally { storageProgressLoading = false; }
}, 2000);
window.setInterval(() => { if (!appView.hidden) loadStatus(); }, 10000);
// 校准只更新当前索引的显示结果，不触发车机盘快照刷新。
window.setInterval(() => {
  if (!appView.hidden && !document.hidden && state.page === "dashboard" && !recordingInteractionActive() &&
      recordingIndexCached) { loadSessions(true, true); return; }
  refreshRecordingDurations();
}, 5000);
scheduleRecordingRefresh();
initialize();
