"use strict";

const cameraOrder = ["left_pillar", "front", "right_pillar", "left_repeater", "back", "right_repeater"];
const savedView = readPreference("teslabox-view", "multi");
const savedPeriod = readPreference("teslabox-period", "month");
const state = {
  offset: 0, total: 0, loading: false, authEnabled: true,
  view: savedView === "single" ? "single" : "multi",
  period: ["month", "week", "day"].includes(savedPeriod) ? savedPeriod : "month",
};
const $ = (id) => document.getElementById(id);

const loginView = $("login-view");
const appView = $("app-view");
const loginForm = $("login-form");
const loginError = $("login-error");
const timelineList = $("video-grid");
const notice = $("notice");
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
let activeDisplay = "composite";
let allSessions = [];
let compositeTracks = new Map();
let lastCompositeSync = 0;

applyTheme(normalizeTheme(readPreference("teslabox-theme", "blue")));
applyViewState();

async function request(path, options = {}) {
  const response = await fetch(path, { credentials: "same-origin", ...options });
  if (response.status === 401) {
    showLogin();
    throw new Error("登录状态已失效");
  }
  if (!response.ok) {
    let message = `请求失败（HTTP ${response.status}）`;
    try { message = (await response.json()).error || message; } catch (_) { /* 保留通用提示 */ }
    throw new Error(message);
  }
  if (response.status === 204) return null;
  return response.json();
}

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
    const session = await fetch("/api/v1/auth/session", { credentials: "same-origin" }).then((response) => response.json());
    state.authEnabled = session.enabled;
    if (session.enabled && !session.authenticated) {
      showLogin();
      return;
    }
    showApp();
    await refreshAll();
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
    const response = await fetch("/api/v1/auth/login", {
      method: "POST", credentials: "same-origin", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ password: $("password").value }),
    });
    const payload = await response.json();
    if (!response.ok) throw new Error(payload.error || "登录失败");
    $("password").value = "";
    showApp();
    await refreshAll();
  } catch (error) {
    loginError.textContent = error.message;
  } finally {
    button.disabled = false;
  }
});

$("logout-button").addEventListener("click", async () => {
  await fetch("/api/v1/auth/logout", { method: "POST", credentials: "same-origin" });
  closePlayer();
  showLogin();
});
$("theme-select").addEventListener("change", (event) => applyTheme(event.target.value));
$("refresh-button").addEventListener("click", refreshAll);
$("filter-form").addEventListener("submit", (event) => { event.preventDefault(); loadSessions(false); });
$("play-toggle").addEventListener("click", togglePlayback);
$("session-timeline").addEventListener("input", previewTimelinePosition);
$("session-timeline").addEventListener("change", seekTimelinePosition);
$("fullscreen-button").addEventListener("click", enterFullscreen);
document.querySelectorAll("[data-view]").forEach((button) => button.addEventListener("click", () => setView(button.dataset.view)));
document.querySelectorAll("[data-period]").forEach((button) => button.addEventListener("click", () => setPeriod(button.dataset.period)));
document.querySelectorAll("[data-close-player]").forEach((element) => element.addEventListener("click", closePlayer));
document.addEventListener("keydown", (event) => { if (event.key === "Escape" && !playerModal.hidden) closePlayer(); });
sessionVideo.addEventListener("timeupdate", updateUnifiedTimeline);
sessionVideo.addEventListener("ended", advanceToNextSegment);
sessionVideo.addEventListener("play", updatePlayButton);
sessionVideo.addEventListener("pause", updatePlayButton);

async function refreshAll() {
  await Promise.allSettled([loadStatus(), loadSessions(false)]);
}

async function loadStatus() {
  try {
    const storage = await request("/api/v1/storage");
    renderStatus(storage);
    $("live-indicator").classList.add("online");
    $("live-indicator").querySelector("span").textContent = "服务在线";
  } catch (error) {
    $("live-indicator").classList.remove("online");
    $("live-indicator").querySelector("span").textContent = "连接异常";
    setNotice(error.message, true);
  }
}

function renderStatus(storage) {
  const gadget = storage.gadget_status || {};
  const badge = $("usb-badge");
  badge.className = "badge";
  if (gadget.host_connected) {
    badge.textContent = "车机已连接";
    badge.classList.add("ok");
    $("usb-title").textContent = gadget.current_speed || "USB 已连接";
  } else if (gadget.bound) {
    badge.textContent = "等待车机";
    $("usb-title").textContent = "Gadget 已就绪";
  } else {
    badge.textContent = "未就绪";
    badge.classList.add("error");
    $("usb-title").textContent = "USB Gadget 未绑定";
  }
  $("usb-detail").textContent = `${gadget.udc_state || "状态未知"} · ${storage.phase || "UNKNOWN"}`;
  $("video-count").textContent = formatNumber(storage.video_count || 0);

  const space = storage.snapshot_space || {};
  const freePercent = Number(space.free_percent || 0);
  $("space-value").textContent = formatBytes(space.free_bytes || 0);
  $("space-progress").style.width = `${Math.max(0, Math.min(100, 100-freePercent))}%`;
  $("space-progress").style.background = freePercent < 10 ? "var(--warning)" : "var(--ok)";
  $("space-detail").textContent = `可用 ${freePercent.toFixed(1)}% · 总计 ${formatBytes(space.total_bytes || 0)}`;

  const snapshot = storage.media_snapshot;
  $("snapshot-state").textContent = snapshot ? "可播放" : "尚未建立";
  $("snapshot-detail").textContent = storage.snapshot_error || (snapshot ? formatDate(snapshot.created_at) : "等待首次快照");
}

async function loadSessions() {
  if (state.loading) return;
  state.loading = true;
  timelineList.replaceChildren();
  setNotice("正在整理完整录像时间轴…", false);
  try {
    const params = new URLSearchParams({ limit: "1000" });
    for (const id of ["clip-type", "date", "event"]) {
      const value = $(id).value.trim();
      if (value) params.set(id.replace("-", "_"), value);
    }
    const payload = await request(`/api/v1/video-sessions?${params}`);
    state.total = payload.total;
    allSessions = payload.items;
    renderArchiveTimeline();
    $("result-count").textContent = `${formatNumber(payload.total)} 段完整录像`;
    $("load-more").hidden = true;
    setNotice(payload.total === 0 ? "当前筛选条件下没有录像" : "", false);
  } catch (error) {
    setNotice(error.message, true);
  } finally {
    state.loading = false;
  }
}

function renderArchiveTimeline() {
  timelineList.replaceChildren();
  const buckets = archiveSessions(allSessions, state.period);
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
}

function createSessionTimeline(session) {
  const card = document.createElement("article");
  card.className = `session-card clip-${clipTypeClass(session.clip_type)}`;
  const head = document.createElement("div");
  head.className = "session-head";
  const heading = document.createElement("div");
  const title = document.createElement("h3");
  title.className = "session-title";
  title.textContent = formatSessionRange(session.start_at, session.end_at);
  const meta = document.createElement("p");
  meta.className = "session-meta";
  const clipBadge = document.createElement("span");
  clipBadge.className = `clip-badge clip-${clipTypeClass(session.clip_type)}`;
  clipBadge.textContent = clipLabel(session.clip_type);
  meta.append(clipBadge, document.createTextNode(`${session.segment_count} 个分钟片段 · ${session.cameras.length} 路视角${session.event && !session.event_metadata ? ` · ${session.event}` : ""}`));
  const duration = document.createElement("span");
  duration.className = "duration-pill";
  duration.textContent = formatDuration(session.duration_seconds);
  heading.append(title, meta);
  const eventSummary = createEventSummary(session.event_metadata);
  if (eventSummary) heading.append(eventSummary);
  head.append(heading, duration);

  const body = document.createElement("div");
  body.className = "session-timeline-body";
  if (state.view === "multi") {
    body.append(createTrackRow(session, "六路合并", ""));
  } else {
    const cameras = cameraOrder.filter((camera) => session.cameras.includes(camera));
    cameras.forEach((camera) => body.append(createTrackRow(session, cameraLabel(camera), camera)));
  }

  const footer = document.createElement("div");
  footer.className = "session-footer";
  const status = document.createElement("span");
  status.className = "muted";
  status.textContent = session.complete ? "所有分钟均包含完整六路" : "部分分钟存在视角缺失";
  const open = document.createElement("button");
  open.className = "primary session-open";
  open.type = "button";
  open.textContent = "打开完整录像";
  open.addEventListener("click", () => {
    const display = state.view === "multi" ? "composite" : "single";
    openSessionPlayer(session, preferredCamera(session), 0, display);
  });
  footer.append(status, open);
  body.append(footer);
  card.append(head, body);
  return card;
}

function createTrackRow(session, labelText, camera) {
  const row = document.createElement("div");
  row.className = "track-row";
  const label = document.createElement("span");
  label.className = "track-label";
  label.textContent = labelText;
  const track = document.createElement("div");
  track.className = "session-track";
  for (let index=0; index<session.segment_count; index++) {
    const hasCamera = !camera || session.cameras.includes(camera);
    const segment = document.createElement("button");
    segment.type = "button";
    segment.className = `track-segment${hasCamera ? "" : " missing"}${session.complete ? "" : " incomplete"}`;
    segment.style.flexGrow = "1";
    segment.title = `第 ${index+1} 个分钟片段`;
    segment.setAttribute("aria-label", segment.title);
    segment.addEventListener("click", () => {
      const position = session.duration_seconds*index/session.segment_count;
      openSessionPlayer(session, camera || preferredCamera(session), position, camera ? "single" : "composite");
    });
    track.append(segment);
  }
  const eventPosition = sessionEventPosition(session);
  if (eventPosition !== null) {
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
    activeCamera = session.cameras.includes(camera) ? camera : preferredCamera(session);
    activeDisplay = display === "composite" ? "composite" : "single";
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
    await loadSessionPosition(position, false);
  } catch (error) {
    setNotice(`无法打开录像：${error.message}`, true);
  }
}

function renderCameraSwitcher() {
  const switcher = $("camera-switcher");
  switcher.replaceChildren();
  const compositeButton = document.createElement("button");
  compositeButton.type = "button";
  compositeButton.textContent = "六路合成";
  compositeButton.classList.toggle("active", activeDisplay === "composite");
  compositeButton.addEventListener("click", () => setDisplayMode("composite"));
  switcher.append(compositeButton);
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
    marker.style.flexGrow = String(Math.max(1, Number(moment.duration_seconds || 60)));
    markers.append(marker);
  });
}

function renderEventMarker() {
  const layer = $("event-marker-layer");
  layer.replaceChildren();
  const position = sessionEventPosition(activeSession);
  if (position === null) return;
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
  if (!activeSession) return;
  requestedPosition = Math.max(0, Math.min(Number(position), Number(activeSession.duration_seconds)));
  if (activeDisplay === "composite") {
    await loadCompositePosition(requestedPosition, autoplay);
    return;
  }
  const selection = selectVideoAtPosition(activeSession, activeCamera, requestedPosition);
  $("session-timeline").value = String(requestedPosition);
  $("current-time").textContent = formatDuration(requestedPosition);
  if (!selection) {
    showPlayerMessage("这个时刻缺少当前视角录像");
    sessionVideo.pause();
    return;
  }
  hidePlayerMessage();
  activeMomentIndex = selection.momentIndex;
  activeVideo = selection.video;
  $("segment-status").textContent = `第 ${activeMomentIndex+1} / ${activeSession.moments.length} 分钟 · ${cameraLabel(activeCamera)}`;
  $("download-link").href = `/api/v1/videos/${encodeURIComponent(activeVideo.id)}/download`;
  const token = ++loadToken;
  const source = `/api/v1/videos/${encodeURIComponent(activeVideo.id)}/content`;
  if (sessionVideo.dataset.videoId !== activeVideo.id) {
    sessionVideo.dataset.videoId = activeVideo.id;
    sessionVideo.src = source;
    sessionVideo.load();
    await waitForMetadata(sessionVideo);
  }
  if (token !== loadToken) return;
  const localTime = Math.max(0, selection.localTime);
  if (Number.isFinite(sessionVideo.duration)) {
    sessionVideo.currentTime = Math.min(localTime, Math.max(0, sessionVideo.duration-.05));
  } else {
    sessionVideo.currentTime = localTime;
  }
  if (autoplay) sessionVideo.play().catch(() => showPlayerMessage("浏览器阻止了自动续播，请点播放继续"));
}

async function loadCompositePosition(position, autoplay) {
  const selection = selectMomentAtPosition(activeSession, position);
  if (!selection || selection.moment.videos.length === 0) {
    showPlayerMessage("这个时刻没有可播放的摄像头画面");
    pauseCompositeTracks();
    return;
  }
  hidePlayerMessage();
  activeMomentIndex = selection.momentIndex;
  const masterVideo = selection.moment.videos.find((video) => video.camera === "front") || selection.moment.videos[0];
  activeCamera = masterVideo.camera;
  activeVideo = masterVideo;
  const token = ++loadToken;
  const nextTracks = new Map();
  const preparations = selection.moment.videos.map(async (video) => {
    const element = video.id === masterVideo.id ? sessionVideo : compositeVideoElement(video.camera);
    element.muted = true;
    element.preload = "auto";
    attachCompositeElement(video.camera, element);
    await prepareVideoElement(element, video, selection.absolute);
    nextTracks.set(video.camera, {element, video, lastHardSync: 0});
  });
  await Promise.all(preparations);
  if (token !== loadToken) return;
  clearUnusedCompositeTracks(nextTracks);
  compositeTracks = nextTracks;
  $("segment-status").textContent = `第 ${activeMomentIndex+1} / ${activeSession.moments.length} 分钟 · ${selection.moment.videos.length} 路同窗`;
  $("download-link").href = `/api/v1/videos/${encodeURIComponent(masterVideo.id)}/download`;
  renderCameraSwitcher();
  if (autoplay) await playCompositeTracks();
}

function selectMomentAtPosition(session, position) {
  if (!session || session.moments.length === 0) return null;
  const absolute = new Date(session.start_at).getTime()+position*1000;
  let momentIndex = 0;
  for (let index=0; index<session.moments.length; index++) {
    if (new Date(session.moments[index].recorded_at).getTime() <= absolute) momentIndex = index;
    else break;
  }
  return {absolute, momentIndex, moment: session.moments[momentIndex]};
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
  if (compositeGrid.childElementCount === cameraOrder.length) return;
  compositeGrid.replaceChildren();
  cameraOrder.forEach((camera) => {
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

function attachCompositeElement(camera, element) {
  ensureCompositeTiles();
  const tile = compositeGrid.querySelector(`[data-camera="${camera}"]`);
  tile.querySelector(".composite-tile-placeholder").hidden = true;
  tile.append(element);
  element.hidden = false;
}

async function prepareVideoElement(element, video, absolute) {
  if (element.dataset.videoId !== video.id) {
    element.pause();
    element.dataset.videoId = video.id;
    element.src = `/api/v1/videos/${encodeURIComponent(video.id)}/content`;
    element.load();
    await waitForMetadata(element);
  }
  const localTime = Math.max(0, (absolute-new Date(video.recorded_at).getTime())/1000);
  element.currentTime = Number.isFinite(element.duration)
    ? Math.min(localTime, Math.max(0, element.duration-.05))
    : localTime;
}

function clearUnusedCompositeTracks(nextTracks) {
  compositeTracks.forEach((track, camera) => {
    if (nextTracks.has(camera) || track.element === sessionVideo) return;
    releaseVideoElement(track.element);
  });
  cameraOrder.forEach((camera) => {
    if (nextTracks.has(camera)) return;
    const placeholder = compositeGrid.querySelector(`[data-camera="${camera}"] .composite-tile-placeholder`);
    if (placeholder) placeholder.hidden = false;
  });
}

function selectVideoAtPosition(session, camera, position) {
  const absolute = new Date(session.start_at).getTime() + position*1000;
  let momentIndex = 0;
  for (let index=0; index<session.moments.length; index++) {
    if (new Date(session.moments[index].recorded_at).getTime() <= absolute) momentIndex = index;
    else break;
  }
  const moment = session.moments[momentIndex];
  const video = moment.videos.find((item) => item.camera === camera);
  if (!video) return null;
  return {
    video, momentIndex,
    localTime: (absolute-new Date(video.recorded_at).getTime())/1000,
  };
}

async function switchCamera(camera) {
  if (!activeSession || (activeDisplay === "single" && camera === activeCamera)) return;
  const position = currentSessionPosition();
  const autoplay = !sessionVideo.paused;
  activeCamera = camera;
  if (activeDisplay === "composite") {
    sessionVideo.pause();
    activeDisplay = "single";
    releaseCompositeAuxiliaryTracks();
  }
  pauseCompositeTracks();
  applyDisplayState();
  renderCameraSwitcher();
  await loadSessionPosition(position, autoplay);
}

function togglePlayback() {
  if (!activeSession) return;
  if (!sessionVideo.src) {
    loadSessionPosition(Number($("session-timeline").value), true);
    return;
  }
  if (activeDisplay === "composite") {
    if (sessionVideo.paused) playCompositeTracks();
    else pauseCompositeTracks();
    return;
  }
  if (sessionVideo.paused) sessionVideo.play().catch(() => showPlayerMessage("请再次点击播放"));
  else sessionVideo.pause();
}

function previewTimelinePosition() {
  $("current-time").textContent = formatDuration(Number($("session-timeline").value));
}

function seekTimelinePosition() {
  const autoplay = !sessionVideo.paused;
  if (activeDisplay === "composite") pauseCompositeTracks();
  else sessionVideo.pause();
  loadSessionPosition(Number($("session-timeline").value), autoplay);
}

function updateUnifiedTimeline() {
  if (!activeSession || !activeVideo || sessionVideo.seeking) return;
  const position = Math.max(0, (new Date(activeVideo.recorded_at)-new Date(activeSession.start_at))/1000 + sessionVideo.currentTime);
  requestedPosition = Math.min(position, activeSession.duration_seconds);
  $("session-timeline").value = String(requestedPosition);
  $("current-time").textContent = formatDuration(requestedPosition);
  if (activeDisplay === "composite") synchronizeCompositeTracks();
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
  const nextPosition = Math.max(0, (new Date(activeSession.moments[next].recorded_at)-new Date(activeSession.start_at))/1000);
  loadSessionPosition(nextPosition, true);
}

function updatePlayButton() {
  $("play-toggle").textContent = sessionVideo.paused ? "▶" : "Ⅱ";
  $("play-toggle").setAttribute("aria-label", sessionVideo.paused ? "播放" : "暂停");
}

function currentSessionPosition() {
  if (!activeSession || !activeVideo) return requestedPosition;
  return Math.max(0, Math.min(activeSession.duration_seconds,
    (new Date(activeVideo.recorded_at)-new Date(activeSession.start_at))/1000 + sessionVideo.currentTime));
}

async function setDisplayMode(display) {
  if (!["composite", "single"].includes(display) || display === activeDisplay || !activeSession) return;
  const position = currentSessionPosition();
  const autoplay = !sessionVideo.paused;
  pauseCompositeTracks();
  sessionVideo.pause();
  activeDisplay = display;
  if (display === "single") releaseCompositeAuxiliaryTracks();
  applyDisplayState();
  renderCameraSwitcher();
  await loadSessionPosition(position, autoplay);
}

function applyDisplayState() {
  const composite = activeDisplay === "composite";
  compositeGrid.hidden = !composite;
  if (composite) {
    ensureCompositeTiles();
    sessionVideo.hidden = true;
  } else {
    mainVideoFrame.insertBefore(sessionVideo, $("active-camera-label"));
    sessionVideo.hidden = false;
    sessionVideo.preload = "metadata";
    sessionVideo.muted = false;
  }
  $("active-camera-label").hidden = composite;
}

async function playCompositeTracks() {
  const results = await Promise.allSettled([...compositeTracks.values()].map((track) => track.element.play()));
  if (results.some((result) => result.status === "rejected")) showPlayerMessage("浏览器阻止了自动播放，请再次点击播放");
}

function pauseCompositeTracks() {
  compositeTracks.forEach((track) => track.element.pause());
}

function releaseCompositeAuxiliaryTracks() {
  compositeTracks.forEach((track) => {
    if (track.element !== sessionVideo) releaseVideoElement(track.element);
  });
  compositeTracks.clear();
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
  if (frame.requestFullscreen) frame.requestFullscreen().catch(() => {});
  else if (sessionVideo.webkitEnterFullscreen) sessionVideo.webkitEnterFullscreen();
}

function waitForMetadata(video) {
  if (video.readyState >= 1) return Promise.resolve();
  return new Promise((resolve) => {
    const done = () => resolve();
    video.addEventListener("loadedmetadata", done, {once:true});
    video.addEventListener("error", done, {once:true});
    window.setTimeout(done, 4000);
  });
}

function showPlayerMessage(message) {
  $("player-message").textContent = message;
  $("player-message").hidden = false;
}

function hidePlayerMessage() { $("player-message").hidden = true; }

function closePlayerMedia() {
  loadToken++;
  pauseCompositeTracks();
  compositeTracks.forEach((track) => {
    if (track.element !== sessionVideo) releaseVideoElement(track.element);
  });
  compositeTracks.clear();
  releaseVideoElement(sessionVideo);
  compositeGrid.replaceChildren();
  activeSession = null;
  activeVideo = null;
  activeMomentIndex = -1;
  requestedPosition = 0;
  $("camera-switcher").replaceChildren();
  $("segment-markers").replaceChildren();
  $("event-marker-layer").replaceChildren();
  hidePlayerMessage();
}

function releaseVideoElement(element) {
  const placeholder = element.parentElement?.querySelector(".composite-tile-placeholder");
  element.pause();
  element.removeAttribute("src");
  element.removeAttribute("data-video-id");
  element.load();
  if (element !== sessionVideo) element.remove();
  if (placeholder) placeholder.hidden = false;
}

function closePlayer() {
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

function preferredCamera(session) {
  return session.cameras.includes("front") ? "front" : session.cameras[0];
}

function readPreference(key, fallback) {
  try { return window.localStorage.getItem(key) || fallback; } catch (_) { return fallback; }
}

function writePreference(key, value) {
  try { window.localStorage.setItem(key, value); } catch (_) { /* 无持久存储时仍保持当前页面状态 */ }
}

function setNotice(message, error) {
  notice.textContent = message;
  notice.classList.toggle("error", Boolean(error));
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
  summary.append(dot, document.createTextNode(eventMetadataText(metadata)));
  return summary;
}

function sessionEventPosition(session) {
  if (!session?.event_metadata?.occurred_at || !session.duration_seconds) return null;
  const position = (new Date(session.event_metadata.occurred_at)-new Date(session.start_at))/1000;
  if (!Number.isFinite(position) || position < 0 || position > session.duration_seconds) return null;
  return position;
}

function eventMetadataText(metadata) {
  const parts = [];
  if (metadata.occurred_at) parts.push(`触发于 ${formatTime(metadata.occurred_at)}`);
  const location = eventLocation(metadata);
  if (location) parts.push(location);
  if (metadata.reason) parts.push(eventReasonLabel(metadata.reason));
  if (cameraOrder.includes(metadata.camera)) parts.push(cameraLabel(metadata.camera));
  return parts.join(" · ") || "事件详情不可用";
}

function eventLocation(metadata) {
  const place = [metadata.city, metadata.street].filter(Boolean).join(" ");
  const latitude = Number(metadata.latitude);
  const longitude = Number(metadata.longitude);
  const coordinates = Number.isFinite(latitude) && Number.isFinite(longitude)
    ? `${latitude.toFixed(4)}, ${longitude.toFixed(4)}` : "";
  return [place, coordinates].filter(Boolean).join(" · ");
}

function eventReasonLabel(reason) {
  if (reason === "user_interaction_dashcam_icon_tapped") return "手动点击保存";
  if (reason === "user_interaction_honk") return "鸣笛保存";
  if (reason.startsWith("sentry_panic")) return "哨兵警报触发";
  if (reason.startsWith("sentry_aware")) return "哨兵警戒触发";
  return "车辆事件触发";
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

window.setInterval(() => { if (!appView.hidden) loadStatus(); }, 10000);
initialize();
