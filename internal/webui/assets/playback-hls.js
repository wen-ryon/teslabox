"use strict";

(function (root) {
  const sessions = new Map();

  function supported(video) {
    return /iPhone/.test(root.navigator?.userAgent || "") && !!video?.canPlayType?.("application/vnd.apple.mpegurl");
  }

  async function dispose(session) {
    if (!session) return;
    session.controller.abort();
    root.clearInterval(session.heartbeat);
    if (session.url) {
      await root.fetch(session.url, {method:"DELETE", credentials:"same-origin", keepalive:true}).catch(() => {});
    }
  }

  function release(video) {
    const session = sessions.get(video);
    sessions.delete(video);
    return dispose(session);
  }

  async function attach(video, source, current = () => true) {
    const previous = sessions.get(video);
    const session = {controller:new AbortController(), heartbeat:0, url:""};
    sessions.set(video, session);
    if (supported(video)) {
      video.pause();
      video.removeAttribute("src");
      video.load();
    }
    await dispose(previous);
    const valid = () => current() && sessions.get(video) === session;
    if (!valid()) {
      if (sessions.get(video) === session) sessions.delete(video);
      return false;
    }
    if (!supported(video)) {
      sessions.delete(video);
      video.src = source;
      video.load();
      return true;
    }
    // 切换视角和拖动使用新会话，旧请求迟到时不能覆盖当前画面。
    const endpoint = new URL(source, root.location.href);
    endpoint.pathname += "/hls";
    try {
      const response = await root.fetch(endpoint.pathname + endpoint.search, {
        method:"POST", credentials:"same-origin", signal:session.controller.signal,
      });
      const body = await response.json();
      if (valid()) root.TeslaBoxPlaybackTrace?.mark(video,"hls_response",`http_${response.status}`);
      if (!response.ok) throw new Error(body.error || "分片播放准备失败");
      if (!/^\/api\/v1\/playback-hls\/[0-9a-f]{32}$/.test(body.session_url) || body.url !== body.session_url + "/index.m3u8") {
        throw new Error("分片播放地址无效");
      }
      session.url = body.session_url;
      if (!valid()) { await dispose(session); return false; }
      root.TeslaBoxPlaybackTrace?.hls(video,session.url.split("/").pop());
      session.heartbeat = root.setInterval(() => {
        root.fetch(session.url + "/keepalive", {method:"POST", credentials:"same-origin"}).catch(() => {});
      }, 15000);
      video.src = body.url;
      video.load();
      return true;
    } catch (error) {
      if (!valid() || error.name === "AbortError") return false;
      sessions.delete(video);
      await dispose(session);
      throw error;
    }
  }

  root.addEventListener?.("pagehide", () => {
    for (const video of [...sessions.keys()]) release(video);
  });
  root.TeslaBoxHLS = Object.freeze({supported, attach, release});
})(typeof window === "undefined" ? globalThis : window);
