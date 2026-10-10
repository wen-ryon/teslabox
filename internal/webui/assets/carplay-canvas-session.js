/* 主线程只处理连接与有界转移；不让 Worker 消息队列无限积压。 */
(function (root) {
  'use strict';
  class WorkerPlayer {
    constructor(canvas, onFrame, onError, environment = root, onKeyframeNeeded = () => {}, onConsumed = () => {}) {
      this.onFrame = onFrame; this.onError = onError; this.onKeyframeNeeded = onKeyframeNeeded; this.environment = environment;
      this.onConsumed = onConsumed;
      this.generation = 0; this.nextId = 0; this.pending = new Map(); this.pendingBytes = 0;
      this.closed = false; this.waitingKey = true; this.needsReset = false; this.lastTimestamp = -1;
      this.configurationTimer = null;
      this.worker = new environment.Worker('/carplay/canvas-worker.js');
      this.worker.onmessage = event => this.message(event.data);
      this.worker.onerror = () => this.fail(new Error('视频 Worker 启动或执行失败'));
      this.worker.onmessageerror = () => this.fail(new Error('视频 Worker 消息反序列化失败'));
      this.timer = environment.setTimeout(() => this.fail(new Error('视频 Worker 初始化超时')), 8000);
      try {
        const offscreen = canvas.transferControlToOffscreen();
        this.worker.postMessage({ type: 'init', canvas: offscreen }, [offscreen]);
      } catch (error) { this.close(); throw error; }
    }
    configure(codec) {
      if (this.closed) return;
      if (!/^avc1\.[0-9a-fA-F]{6}$/.test(codec)) { this.fail(new Error('H.264 codec 不合法')); return; }
      // 旧代次的待确认帧仍占预算，不能靠反复切换配置无限扩大 Worker 邮箱。
      this.generation++;
      this.waitingKey = true; this.needsReset = false; this.lastTimestamp = -1;
      this.environment.clearTimeout(this.configurationTimer);
      this.configurationTimer = this.environment.setTimeout(() => this.fail(new Error('视频解码器配置超时')), 8000);
      this.post({ type: 'configure', codec });
    }
    post(message, transfers = []) {
      try { this.worker.postMessage({ generation: this.generation, ...message }, transfers); }
      catch (error) { this.fail(error); }
    }
    push(buffer) {
      if (this.closed) return;
      try {
        const frame = this.environment.TeslaBoxCanvas.packet(buffer);
        if (frame.timestamp <= this.lastTimestamp) throw new Error('Worker 输入时间戳重复或倒退');
        this.lastTimestamp = frame.timestamp;
        if (this.pending.size >= 6 || this.pendingBytes + buffer.byteLength > 8 * 1024 * 1024) {
          this.waitingKey = true; this.needsReset = true; this.onConsumed(frame.timestamp); this.onKeyframeNeeded(); return;
        }
        if (this.waitingKey && frame.type !== 'key') { this.onConsumed(frame.timestamp); this.onKeyframeNeeded(); return; }
        if (this.needsReset) { this.post({ type: 'resync' }); this.needsReset = false; }
        this.waitingKey = false;
        const id = ++this.nextId;
        this.pending.set(id, buffer.byteLength); this.pendingBytes += buffer.byteLength;
        this.post({ type: 'frame', id, buffer }, [buffer]);
      } catch (error) { this.fail(error); }
    }
    message(message) {
      if (this.closed) return;
      if (message.type === 'ready') { this.environment.clearTimeout(this.timer); this.timer = null; return; }
      if (message.type === 'ack') {
        const length = this.pending.get(message.id);
        if (length !== undefined) { this.pending.delete(message.id); this.pendingBytes -= length; }
        return;
      }
      if (message.generation !== this.generation) return;
      if (message.type === 'consumed') { this.onConsumed(message.timestamp); return; }
      if (message.type === 'keyframe-needed') { this.onKeyframeNeeded(); return; }
      if (message.type === 'configured') {
        this.environment.clearTimeout(this.configurationTimer); this.configurationTimer = null;
      } else if (message.type === 'frame') {
        this.onFrame({ width: message.width, height: message.height, timestamp: message.timestamp, renderer: message.renderer, pendingOutput: message.pendingOutput, droppedFrames: message.droppedFrames, latencyMs: message.latencyMs, drawMs: message.drawMs, worker: true });
      } else if (message.type === 'error') {
        this.fail(new Error(message.message));
      }
    }
    fail(error) { if (!this.closed) { this.close(); this.onError(error); } }
    close() {
      if (this.closed) return;
      this.closed = true; this.environment.clearTimeout(this.timer); this.environment.clearTimeout(this.configurationTimer);
      this.worker.terminate(); this.pending.clear(); this.pendingBytes = 0;
    }
  }
  // 优先隔离解码线程；旧浏览器回退时不改变原码流协议或偷偷切换 video。
  function create(canvas, onFrame, onError, environment = root, onKeyframeNeeded = () => {}, onConsumed = () => {}) {
    if (typeof environment.Worker === 'function' && typeof canvas.transferControlToOffscreen === 'function') {
      return new WorkerPlayer(canvas, onFrame, onError, environment, onKeyframeNeeded, onConsumed);
    }
    return new environment.TeslaBoxCanvas.CanvasPlayer(canvas,
      frame => onFrame({ ...frame, worker: false }), onError, environment, onKeyframeNeeded, onConsumed);
  }
  root.TeslaBoxSession = { create, WorkerPlayer };
  if (typeof module === 'object' && module.exports) module.exports = root.TeslaBoxSession;
})(typeof globalThis !== 'undefined' ? globalThis : self);
