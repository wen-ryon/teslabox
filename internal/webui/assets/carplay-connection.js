/* 连接代次隔离和短间隔重试只作用于浏览器；不主动重启板端或重新登记手机。 */
(function (root) {
  'use strict';
  // 断网记录保留在有界内存队列，下一次事件触发补传；不额外建立重试定时器。
  function createReporter(environment = root) {
    const started = Date.now(), page = started.toString(36) + '-' + Math.random().toString(36).slice(2, 14);
    let sequence = 0, count = 0, dropped = 0, windowStart = started, busy = false;
    const pending = [];
    function flush() {
      if (busy || !pending.length) return;
      busy = true;
      let timer, accepted = false;
      try {
        const abort = new environment.AbortController();
        timer = environment.setTimeout(() => abort.abort(), 2000);
        Promise.resolve(environment.fetch('/api/v1/diagnostics/carplay-connection', {
          method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(pending[0]),
          signal: abort.signal, keepalive: true, cache: 'no-store'
        })).then(response => {
          if (response.ok) { pending.shift(); accepted = true; }
        }).catch(() => {}).finally(() => {
          environment.clearTimeout(timer); busy = false;
          if (accepted) flush();
        });
      } catch (_) { environment.clearTimeout(timer); busy = false; }
    }
    return event => {
      if (Date.now() - windowStart >= 60000) { windowStart = Date.now(); count = 0; }
      if (count >= 60 || pending.length >= 60) { dropped++; flush(); return; }
      count++;
      pending.push({...event, page_id: page, seq: ++sequence, dropped,
		client_time_ms: Date.now(),
        page_elapsed_ms: Math.min(86400000, Math.max(0, Date.now() - started))});
      dropped = 0; flush();
    };
  }
  class Connection {
    constructor(callbacks, environment = root) {
      this.callbacks = callbacks; this.environment = environment;
      this.socket = null; this.timer = null; this.timeout = null; this.abort = null;
      this.generation = 0; this.attempt = 0; this.running = false; this.disposed = false;
      this.startedAt = 0; this.traceAttempt = 0; this.firstFrame = false; this.firstPacket = false; this.frameTimer = null;
      this.pageHidden = false;
      this.resume = () => {
        this.diagnostic(this.available() ? 'resumed' : 'paused', this.available() ? 'visible' : 'hidden');
        if (this.running && this.available() && !this.socket && this.timer !== null) this.reset();
        this.changed();
      };
      this.show = () => { this.pageHidden = false; this.diagnostic('resumed', 'visible'); this.changed(); };
      this.leave = () => { this.pageHidden = true; this.pause(); };
      environment.addEventListener('online', this.resume);
      environment.addEventListener('offline', this.resume);
      environment.addEventListener('pageshow', this.show);
      environment.addEventListener('pagehide', this.leave);
      environment.document.addEventListener('visibilitychange', this.resume);
    }
    // 浏览器离线提示不可靠：无互联网的车载 AP 仍可能到达本机服务，实际请求才是依据。
    available() { return !this.pageHidden && !this.environment.document.hidden; }
    now() { return this.environment.performance?.now() ?? Date.now(); }
    diagnostic(phase, reason = 'none', status = 0, closeCode = 0) {
      try {
        this.callbacks.diagnostic?.({phase, reason, status, close_code: closeCode,
          attempt: this.traceAttempt, generation: this.generation,
          elapsed_ms: Math.min(86400000, Math.max(0, Math.round(this.now() - this.startedAt))),
          hidden: !this.available()});
      } catch (_) { /* 诊断失败不能影响播放。 */ }
    }
    state(name, detail) { this.callbacks.state(name, detail); }
    start() { if (this.disposed) return; this.running = true; this.attempt = 0; this.reset(); this.changed(); }
    healthy() { this.attempt = 0; if (!this.firstFrame) { this.firstFrame = true; this.environment.clearTimeout(this.frameTimer); this.frameTimer = null; this.diagnostic('first_frame'); } }
    packet() { if (!this.firstPacket) { this.firstPacket = true; this.diagnostic('first_packet'); } }
    // 先释放触控，再关闭旧连接；旧代次回调不能干扰新连接。
    reset() {
      this.generation++; this.firstFrame = false; this.firstPacket = false;
      this.environment.clearTimeout(this.frameTimer); this.frameTimer = null;
      this.environment.clearTimeout(this.timer); this.timer = null;
      this.environment.clearTimeout(this.timeout); this.timeout = null;
      if (this.abort) this.abort.abort(); this.abort = null;
      this.callbacks.reset();
      if (this.socket) {
        const previous = this.socket; this.socket = null;
        previous.onopen = previous.onmessage = previous.onclose = previous.onerror = null;
        previous.close();
      }
      this.callbacks.socket(null);
    }
    pause() { this.diagnostic('paused', 'hidden'); this.reset(); if (this.running) this.state('paused'); }
    changed() {
      if (!this.running) return;
      if (!this.available()) { this.pause(); return; }
      if (!this.socket && this.timer === null && !this.abort) this.probe();
    }
    stop(error) {
      this.diagnostic('stopped', error ? 'player_error' : 'client_stop');
      this.running = false; this.reset();
      this.state(error ? 'failed' : 'stopped', error);
    }
    retry() {
      this.diagnostic('retry');
      this.reset();
      if (!this.running) return;
      if (!this.available()) { this.state('paused'); return; }
      // 车载网络恢复后尽快接回，单请求和代次隔离仍避免并发堆叠。
      const delay = 1000;
      const generation = this.generation;
      this.state('retry', delay);
      this.timer = this.environment.setTimeout(() => { if (generation !== this.generation) return; this.timer = null; this.probe(); }, delay);
    }
    async probe() {
      if (!this.running || !this.available()) return;
      const generation = this.generation;
      const abort = new this.environment.AbortController(); this.abort = abort;
      this.traceAttempt++; this.startedAt = this.now(); this.diagnostic('probe_start');
      this.state('checking');
      this.timeout = this.environment.setTimeout(() => { if (generation === this.generation) { this.diagnostic('timeout', 'probe_timeout'); abort.abort(); } }, 8000);
      let response;
      try {
        response = await this.environment.fetch('/carplay/api/status', {signal: abort.signal, cache: 'no-store'});
      } catch (_) {
        if (generation === this.generation) this.diagnostic('probe_error', abort.signal.aborted ? 'probe_timeout' : 'network');
        if (generation === this.generation) this.retry();
        return;
      }
      if (generation !== this.generation) return;
      this.environment.clearTimeout(this.timeout); this.timeout = null; this.abort = null;
      this.diagnostic('probe_result', response.ok ? 'none' : 'http', response.status);
      if ([401, 403].includes(response.status)) {
        this.running = false; this.reset(); this.state('unauthorized'); return;
      }
      if (!response.ok) { this.retry(); return; }
      this.open();
    }
    open() {
      if (!this.running || !this.available()) { this.changed(); return; }
      const generation = this.generation;
      let socket; this.diagnostic('ws_start');
      try {
        const location = this.environment.location;
        socket = new this.environment.WebSocket((location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/carplay/video');
      } catch (_) { this.diagnostic('ws_error', 'network'); this.retry(); return; }
      this.socket = socket; socket.binaryType = 'arraybuffer'; this.callbacks.socket(socket);
      this.state('connecting');
      this.timeout = this.environment.setTimeout(() => { if (generation === this.generation) { this.diagnostic('timeout', 'ws_timeout'); this.retry(); } }, 8000);
      socket.onopen = () => {
        if (generation !== this.generation) return;
        this.environment.clearTimeout(this.timeout); this.timeout = null;
        // 握手成功立即清除重试提示；断开后仍按固定间隔重试。
        this.diagnostic('ws_open');
        this.frameTimer = this.environment.setTimeout(() => {
          if (generation === this.generation && !this.firstFrame) this.diagnostic('first_frame_wait');
        }, 8000);
        this.state('connected');
      };
      socket.onmessage = event => {
        if (generation !== this.generation) return;
        try { this.callbacks.message(event); } catch (error) { this.stop(error); }
      };
      socket.onerror = () => { if (generation === this.generation) { this.diagnostic('ws_error', 'network'); this.retry(); } };
      socket.onclose = event => {
        if (generation !== this.generation) return;
        this.diagnostic('ws_close', 'none', 0, event.code || 0);
        if (event.code === 1008) {
          this.running = false; this.reset(); this.state('unauthorized');
        } else this.retry();
      };
    }
    dispose() {
      if (this.disposed) return;
      this.disposed = true;
      this.stop();
      this.environment.removeEventListener('online', this.resume);
      this.environment.removeEventListener('offline', this.resume);
      this.environment.removeEventListener('pageshow', this.show);
      this.environment.removeEventListener('pagehide', this.leave);
      this.environment.document.removeEventListener('visibilitychange', this.resume);
    }
  }
  root.TeslaBoxConnection = {Connection, createReporter};
  if (typeof module === 'object' && module.exports) module.exports = root.TeslaBoxConnection;
})(typeof globalThis !== 'undefined' ? globalThis : this);
