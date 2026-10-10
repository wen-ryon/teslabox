/* 连接代次隔离和短间隔重试只作用于浏览器；不主动重启板端或重新登记手机。 */
(function (root) {
  'use strict';
  class Connection {
    constructor(callbacks, environment = root) {
      this.callbacks = callbacks; this.environment = environment;
      this.socket = null; this.timer = null; this.timeout = null; this.abort = null;
      this.generation = 0; this.attempt = 0; this.running = false; this.disposed = false;
      this.pageHidden = false;
      this.resume = () => {
        if (this.running && this.available() && !this.socket && this.timer !== null) this.reset();
        this.changed();
      };
      this.show = () => { this.pageHidden = false; this.changed(); };
      this.leave = () => { this.pageHidden = true; this.pause(); };
      environment.addEventListener('online', this.resume);
      environment.addEventListener('offline', this.resume);
      environment.addEventListener('pageshow', this.show);
      environment.addEventListener('pagehide', this.leave);
      environment.document.addEventListener('visibilitychange', this.resume);
    }
    // 浏览器离线提示不可靠：无互联网的车载 AP 仍可能到达本机服务，实际请求才是依据。
    available() { return !this.pageHidden && !this.environment.document.hidden; }
    state(name, detail) { this.callbacks.state(name, detail); }
    start() { if (this.disposed) return; this.running = true; this.attempt = 0; this.reset(); this.changed(); }
    healthy() { this.attempt = 0; }
    // 先释放触控，再关闭旧连接；旧代次回调不能干扰新连接。
    reset() {
      this.generation++;
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
    pause() { this.reset(); if (this.running) this.state('paused'); }
    changed() {
      if (!this.running) return;
      if (!this.available()) { this.pause(); return; }
      if (!this.socket && this.timer === null && !this.abort) this.probe();
    }
    stop(error) {
      this.running = false; this.reset();
      this.state(error ? 'failed' : 'stopped', error);
    }
    retry() {
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
      this.state('checking');
      this.timeout = this.environment.setTimeout(() => { if (generation === this.generation) abort.abort(); }, 8000);
      let response;
      try {
        response = await this.environment.fetch('/carplay/api/status', {signal: abort.signal, cache: 'no-store'});
      } catch (_) {
        if (generation === this.generation) this.retry();
        return;
      }
      if (generation !== this.generation) return;
      this.environment.clearTimeout(this.timeout); this.timeout = null; this.abort = null;
      if ([401, 403].includes(response.status)) {
        this.running = false; this.reset(); this.state('unauthorized'); return;
      }
      if (!response.ok) { this.retry(); return; }
      this.open();
    }
    open() {
      if (!this.running || !this.available()) { this.changed(); return; }
      const generation = this.generation;
      let socket;
      try {
        const location = this.environment.location;
        socket = new this.environment.WebSocket((location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/carplay/video');
      } catch (_) { this.retry(); return; }
      this.socket = socket; socket.binaryType = 'arraybuffer'; this.callbacks.socket(socket);
      this.state('connecting');
      this.timeout = this.environment.setTimeout(() => { if (generation === this.generation) this.retry(); }, 8000);
      socket.onopen = () => {
        if (generation !== this.generation) return;
        this.environment.clearTimeout(this.timeout); this.timeout = null;
        // 握手成功立即清除重试提示；断开后仍按固定间隔重试。
        this.state('connected');
      };
      socket.onmessage = event => {
        if (generation !== this.generation) return;
        try { this.callbacks.message(event); } catch (error) { this.stop(error); }
      };
      socket.onerror = () => { if (generation === this.generation) this.retry(); };
      socket.onclose = event => {
        if (generation !== this.generation) return;
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
  root.TeslaBoxConnection = {Connection};
  if (typeof module === 'object' && module.exports) module.exports = root.TeslaBoxConnection;
})(typeof globalThis !== 'undefined' ? globalThis : this);
