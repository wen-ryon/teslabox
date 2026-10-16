"use strict";

let playbackProbeCaptureAbort = null;

function playbackMatrixClearVideos() {
  const container = $("playback-probe-videos");
  for (const video of container.querySelectorAll("video")) {
    globalThis.TeslaBoxHLS?.release(video);
    video.pause();
    video.removeAttribute("src");
    video.load();
    if (video.playbackProbeObjectURL) URL.revokeObjectURL(video.playbackProbeObjectURL);
  }
  container.replaceChildren();
  container.hidden = true;
}

function playbackMatrixLabel(test) {
  const view = test.layout === "single" ? cameraLabel(test.camera) : (compositeLayouts[test.layout]?.label || test.layout);
  const quality = t(({original:"原画", smooth:"流畅", standard:"标清", high:"高清"})[test.quality] || test.quality);
  return `${view} · ${quality}${test.delivery === "finite" ? t("（有限文件对照）") : test.delivery === "hls" ? t("（iPhone 分片播放）") : ""}${test.stage === "original" && test.layout !== "single" ? t`（${test.cameras.length} 路并发）` : ""}`;
}

function playbackMatrixCompleteFragments(bytes, length) {
  const view = new DataView(bytes.buffer, bytes.byteOffset, length);
  let offset = 0;
  let moofs = 0;
  let mdats = 0;
  while (offset + 8 <= length) {
    const size = view.getUint32(offset);
    if (size < 8 || size > bytes.length || offset + size > length) break;
    const type = String.fromCharCode(...bytes.subarray(offset + 4, offset + 8));
    if (type === "moof") moofs++;
    if (type === "mdat") mdats++;
    offset += size;
    if (moofs >= 2 && mdats >= 2) return offset;
  }
  return 0;
}

async function playbackMatrixCaptureFinite(url, run) {
  const limit = 8 * 1024 * 1024;
  const controller = new AbortController();
  playbackProbeCaptureAbort = controller;
  const timeout = window.setTimeout(() => controller.abort(), 10000);
  let reader;
  try {
    const response = await fetch(url, {credentials:"same-origin", signal:controller.signal});
    if (!response.ok || !response.body) throw new Error(`HTTP ${response.status} 或浏览器不支持流式读取`);
    reader = response.body.getReader();
    const bytes = new Uint8Array(limit);
    let length = 0;
    while (run === playbackProbeRun) {
      const {done, value} = await reader.read();
      if (done) break;
      if (length + value.length > limit) throw new Error("首两个媒体片段超过 8 MiB 上限");
      bytes.set(value, length);
      length += value.length;
      const complete = playbackMatrixCompleteFragments(bytes, length);
      if (complete) return {blob:new Blob([bytes.slice(0, complete)], {type:"video/mp4"}), bytes:complete};
    }
    throw new Error(run === playbackProbeRun ? "10 秒内未收到两个完整媒体片段" : "诊断已停止");
  } finally {
    window.clearTimeout(timeout);
    if (reader) await reader.cancel().catch(() => {});
    controller.abort();
    if (playbackProbeCaptureAbort === controller) playbackProbeCaptureAbort = null;
  }
}

async function playbackMatrixFindSource() {
  let best = null;
  for (const clipType of ["archive", "recent"]) {
    const page = await request(`/api/v1/video-sessions?clip_type=${clipType}&limit=20`, {loading:false});
    for (const summary of page.items || []) {
      if (summary.cached || !summary.snapshot_id) continue;
      try {
        const session = await request(`/api/v1/video-sessions/${encodeURIComponent(summary.id)}?snapshot_id=${encodeURIComponent(summary.snapshot_id)}`, {loading:false});
        for (const moment of session.moments || []) {
          const count = new Set((moment.videos || []).filter(video => video.id).map(video => video.camera)).size;
          if (!moment.id || !count) continue;
          if (count === cameraOrder.length) return {session, moment, clipType};
          if (!best || count > best.count) best = {session, moment, clipType, count};
        }
      } catch (error) {
        if (error.status !== 404 && error.status !== 409) throw error;
      }
    }
    if (best) break;
  }
  if (best) return best;
  throw new Error("没有可用于诊断的录像，请先确认媒体中心能打开录像");
}

async function playbackMatrixRefreshSnapshot(source, test) {
  const page = await request(`/api/v1/video-sessions?session_id=${encodeURIComponent(source.session.id)}&limit=1`, {loading:false});
  const updated = (page.items || []).find(item => item.id === source.session.id);
  if (!updated?.snapshot_id || updated.cached) throw new Error("当前录像快照暂不可用，请稍后重新诊断");
  const changed = updated.snapshot_id !== source.session.snapshot_id;
  if (changed) {
    let session;
    try {
      session = await request(`/api/v1/video-sessions/${encodeURIComponent(source.session.id)}?snapshot_id=${encodeURIComponent(updated.snapshot_id)}`, {loading:false});
    } catch (error) {
      if (error.status === 404 || error.status === 409) throw new Error("快照轮换后所选录像已变化，请重新开始诊断");
      throw error;
    }
    const moment = (session.moments || []).find(item => item.id === source.moment.id);
    if (!moment) throw new Error("快照轮换后所选片段已变化，请重新开始诊断");
    source.session = session;
    source.moment = moment;
  }
  if (test && test.cameras.some(camera => !(source.moment.videos || []).some(video => video.id && video.camera === camera))) {
    throw new Error("快照轮换后该片段缺少待测视角，请重新开始诊断");
  }
  return changed;
}

function playbackMatrixSource(source, test, track, id) {
  const query = new URLSearchParams({snapshot_id:source.session.snapshot_id, probe_id:id,
    probe_case:test.id, probe_track:track});
  if (test.stage === "original") {
    const video = source.moment.videos.find(item => item.camera === track);
    return `/api/v1/videos/${encodeURIComponent(video.id)}/content?${query}`;
  }
  query.set("layout", test.layout);
  query.set("quality", test.quality);
  query.set("start_seconds", "0");
  if (test.layout === "single") query.set("camera", test.camera);
  return `/api/v1/video-groups/${encodeURIComponent(source.moment.id)}/composite?${query}`;
}

function playbackMatrixEvent(pending, source, test, id, event, reason, started, item = null) {
  const connection = navigator.connection;
  const video = item?.video;
  const payload = {
    event, reason, probe_id:id, probe_stage:test.stage, probe_case:test.id,
    probe_track:item?.track || test.camera || "",
    elapsed_ms:Math.max(0, Math.round(performance.now()-started)),
    session_id:source.session.id, group_id:source.moment.id,
    display:test.layout, quality:test.quality, effective_quality:test.quality,
    access_path:classifyAccessPath(), network_type:connection?.effectiveType || "",
    downlink_mbps:Number(connection?.downlink) || 0, rtt_ms:Number(connection?.rtt) || 0,
    buffer_seconds:video ? Number(bufferedSeconds(video).toFixed(2)) || 0 : 0,
    media_error_code:video?.error?.code || 0, ready_state:video?.readyState || 0,
    stalls:item?.stalls || 0,
    current_time_seconds:video ? Number(video.currentTime.toFixed(2)) || 0 : 0,
    video_width:video?.videoWidth || 0, video_height:video?.videoHeight || 0,
    network_state:video?.networkState || 0,
    total_frames:video?.getVideoPlaybackQuality?.().totalVideoFrames || 0,
    dropped_frames:video?.getVideoPlaybackQuality?.().droppedVideoFrames || 0,
  };
  pending.push(fetch("/api/v1/diagnostics/client-event", {
    method:"POST", credentials:"same-origin", keepalive:true,
    headers:{"Content-Type":"application/json"}, body:JSON.stringify(payload),
  }).then(response => response.ok).catch(() => false));
}

function stopPlaybackProbeMatrix(reason) {
  if (!playbackProbeActive) return;
  playbackProbeRun++;
  playbackProbeCaptureAbort?.abort();
  playbackProbeFinish?.(reason);
  playbackMatrixClearVideos();
  $("playback-probe-status").textContent = t`诊断已停止：${reason}。可下载已有诊断日志，或保持页面在前台后重试。`;
}

function playbackMatrixTracksDone(tracks) {
  return tracks.every(track => track.error || track.video.ended);
}

async function playbackMatrixRunCase(source, test, id, pending, run) {
  playbackMatrixClearVideos();
  const container = $("playback-probe-videos");
  container.hidden = false;
  const started = performance.now();
  playbackMatrixEvent(pending, source, test, id, "probe_start", `开始加载 ${test.cameras.join(",")}`, started);
  let finiteURL = "";
  if (test.delivery === "finite") {
    try {
      const captured = await playbackMatrixCaptureFinite(playbackMatrixSource(source, test, test.camera, id), run);
      if (run !== playbackProbeRun) throw new Error("诊断已停止");
      finiteURL = URL.createObjectURL(captured.blob);
      playbackMatrixEvent(pending, source, test, id, "probe_event", `finite_capture:bytes=${captured.bytes},ms=${Math.round(performance.now()-started)}`, started);
    } catch (error) {
      const outcome = run === playbackProbeRun ? `捕获失败：${error.message}` : "已中止";
      playbackMatrixEvent(pending, source, test, id, "probe_end", outcome, started);
      playbackMatrixClearVideos();
      return outcome;
    }
  }
  const tracks = (test.stage === "original" ? test.cameras : [test.camera || test.layout]).map(track => {
    const video = document.createElement("video");
    video.className = "playback-probe-video";
    video.muted = true;
    video.playsInline = true;
    video.setAttribute("webkit-playsinline", "");
    video.preload = "auto";
    container.append(video);
    return {track, video, stalls:0, startupWaits:0, progress:false, firstProgress:0, error:"", eventCount:0};
  });
  const listeners = [];
  const result = await new Promise(resolve => {
    let finished = false;
    let timer = 0;
    let errorTimer = 0;
    const finish = reason => {
      if (finished) return;
      finished = true;
      window.clearTimeout(timer);
      window.clearTimeout(errorTimer);
      listeners.forEach(([video, name, listener]) => video.removeEventListener(name, listener));
      playbackProbeFinish = null;
      resolve(reason);
    };
    playbackProbeFinish = finish;
    for (const item of tracks) {
      for (const name of ["loadstart", "loadedmetadata", "loadeddata", "canplay", "playing", "waiting", "stalled", "suspend", "error", "ended", "timeupdate"]) {
        const listener = () => {
          if (finished || run !== playbackProbeRun) return;
          if (name === "timeupdate") {
            if (item.progress || item.video.currentTime < 1) return;
            item.progress = true;
            item.firstProgress = Math.round(performance.now() - started);
          }
          if (name === "waiting" || name === "stalled") {
            if (item.progress) item.stalls++;
            else item.startupWaits++;
          }
          if (name === "error") item.error = `媒体错误 ${item.video.error?.code || 0}`;
          if (item.eventCount++ < 16) playbackMatrixEvent(pending, source, test, id, "probe_event", name, started, item);
          if (name === "error" && playbackMatrixTracksDone(tracks) && !errorTimer) {
            errorTimer = window.setTimeout(() => finish("媒体错误"), 200);
          }
          if (name === "ended" && playbackMatrixTracksDone(tracks)) finish("播放结束");
        };
        item.video.addEventListener(name, listener);
        listeners.push([item.video, name, listener]);
      }
      const start = async () => {
        const sourceURL = playbackMatrixSource(source, test, item.track, id);
        if (test.delivery === "hls") {
          if (!await TeslaBoxHLS.attach(item.video, sourceURL, () => !finished && run === playbackProbeRun)) return;
          playbackMatrixEvent(pending, source, test, id, "probe_event", "hls_ready", started, item);
        } else {
          item.video.src = finiteURL || sourceURL;
          if (finiteURL) item.video.playbackProbeObjectURL = finiteURL;
          item.video.load();
        }
        if (!finished) await item.video.play();
      };
      start().catch(error => {
        if (finished) return;
        item.error = `浏览器拒绝播放 ${error.name || "unknown"}`;
        playbackMatrixEvent(pending, source, test, id, "probe_event", `play_rejected:${error.name || "unknown"}`, started, item);
        if (playbackMatrixTracksDone(tracks) && !errorTimer) {
          errorTimer = window.setTimeout(() => finish("浏览器拒绝播放"), 200);
        }
      });
    }
    const limit = test.delivery === "hls" ? 27000 : 12000;
    timer = window.setTimeout(() => finish(`达到 ${limit/1000} 秒测试时长`), limit);
  });
  const failed = tracks.filter(item => item.error || !item.progress || item.stalls > 0 ||
    (!item.video.ended && item.video.currentTime < 4));
  for (const item of tracks) {
    playbackMatrixEvent(pending, source, test, id, "probe_event",
      `track_summary:progress=${item.progress},first_progress_ms=${item.firstProgress},startup_waits=${item.startupWaits},stalls=${item.stalls},error=${item.error || "none"}`,
      started, item);
  }
  const aborted = run !== playbackProbeRun;
  const outcome = aborted ? "已中止" : failed.length ? `${failed.length}/${tracks.length} 路异常` : "通过";
  playbackMatrixEvent(pending, source, test, id, "probe_end", `${outcome}；${result}`, started);
  playbackMatrixClearVideos();
  return outcome;
}

async function playbackMatrixRunCaseWithSnapshotRetry(source, test, id, pending, run, onRetry) {
  let outcome = await playbackMatrixRunCase(source, test, id, pending, run);
  if (run !== playbackProbeRun || outcome === "通过") return outcome;
  if (!await playbackMatrixRefreshSnapshot(source, test)) return outcome;
  if (run !== playbackProbeRun) return "已中止";
  onRetry();
  playbackMatrixEvent(pending, source, test, id, "probe_event", "snapshot_changed_retry", performance.now());
  outcome = await playbackMatrixRunCase(source, test, id, pending, run);
  if (run === playbackProbeRun && outcome !== "通过" && await playbackMatrixRefreshSnapshot(source, test)) {
    throw new Error("诊断期间快照连续轮换，请稍后重新开始；该项不计为播放异常");
  }
  return outcome;
}

async function startPlaybackProbeMatrix() {
  if (playbackProbeActive) return;
  playbackProbeActive = true;
  const run = ++playbackProbeRun;
  const pending = [];
  const results = $("playback-probe-results");
  results.replaceChildren();
  playbackProbeLastID = "";
  $("playback-probe-start").disabled = true;
  $("playback-probe-stop").disabled = false;
  $("playback-probe-status").textContent = t("正在寻找可播放的录像…");
  const qualities = Array.from(document.querySelectorAll('input[name="playback-probe-quality"]:checked'), item => item.value);
  let source = null;
  let id = "";
  const overview = {id:"matrix", stage:"original", quality:"original", layout:"matrix", camera:""};
  const started = performance.now();
  try {
    source = await playbackMatrixFindSource();
    if (run !== playbackProbeRun) return;
    id = playbackProbeID();
    playbackProbeLastID = id;
    const plan = TeslaBoxPlaybackProbePlan.buildCases(source.moment, cameraOrder, compositeLayouts, qualities, $("playback-probe-scope").value);
    if (globalThis.TeslaBoxHLS?.supported(document.createElement("video"))) {
      plan.cases = plan.cases.flatMap(test => test.stage === "transcoded" && !test.delivery
        ? [test, {...test, id:test.id+"-hls", delivery:"hls"}] : [test]);
    }
    playbackMatrixEvent(pending, source, overview, id, "probe_start", `矩阵诊断 ${plan.cases.length} 项，跳过 ${plan.skipped.length} 项`, started);
    for (const skipped of plan.skipped) {
      const row = document.createElement("li");
      row.textContent = t`${skipped.id}：跳过（${skipped.reason}）`;
      results.append(row);
      playbackMatrixEvent(pending, source, {...overview, id:skipped.id}, id, "probe_end", `跳过：${skipped.reason}`, started);
    }
    const totals = {pass:0, abnormal:0};
    for (const [index, test] of plan.cases.entries()) {
      if (run !== playbackProbeRun) break;
      if (await playbackMatrixRefreshSnapshot(source, test)) {
        playbackMatrixEvent(pending, source, test, id, "probe_event", "snapshot_changed_before_case", started);
      }
      if (run !== playbackProbeRun) break;
      $("playback-probe-status").textContent = t`第 ${index+1}/${plan.cases.length} 项：${playbackMatrixLabel(test)}。诊断编号 ${id}`;
      const outcome = await playbackMatrixRunCaseWithSnapshotRetry(source, test, id, pending, run, () => {
        $("playback-probe-status").textContent = t`快照已轮换，正在重试第 ${index+1}/${plan.cases.length} 项：${playbackMatrixLabel(test)}`;
      });
      if (run !== playbackProbeRun) break;
      if (outcome === "通过") totals.pass++; else totals.abnormal++;
      const row = document.createElement("li");
      row.textContent = t`${playbackMatrixLabel(test)}：${t(outcome)}`;
      results.append(row);
    }
    if (run === playbackProbeRun) {
      playbackMatrixEvent(pending, source, overview, id, "probe_end", `完成：通过 ${totals.pass}，异常 ${totals.abnormal}，跳过 ${plan.skipped.length}`, started);
    }
    const sent = await Promise.all(pending);
    if (run === playbackProbeRun) {
      $("playback-probe-status").textContent = t`测试完成：${totals.pass} 项通过，${totals.abnormal} 项异常，${plan.skipped.length} 项跳过，${sent.filter(value => !value).length} 条事件上报失败。诊断编号 ${id}。请下载完整诊断报告。`;
    }
  } catch (error) {
    if (run === playbackProbeRun) {
      if (source && id) playbackMatrixEvent(pending, source, overview, id, "probe_end", `诊断未完成：${error.message}`, started);
      await Promise.all(pending);
      $("playback-probe-status").textContent = t`诊断未完成：${error.message}。请下载已有诊断报告。`;
    }
  } finally {
    playbackProbeFinish = null;
    playbackMatrixClearVideos();
    playbackProbeActive = false;
    $("playback-probe-start").disabled = false;
    $("playback-probe-stop").disabled = true;
  }
}
