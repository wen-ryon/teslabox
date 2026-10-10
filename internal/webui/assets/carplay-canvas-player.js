/* 独立原码流播放器；不依赖 HTMLVideoElement，也不复制商业盒子的解码器。 */
(function (root) {
  'use strict';
  const MAX_FRAME = 4 * 1024 * 1024;
  const MAX_PENDING_OUTPUT = 12, MAX_BACKLOG_US = 200000, MAX_DECODE_MS = 250;
  const MAX_OUTPUT_STALL_MS = 1000;

  function packet(buffer) {
    if (!(buffer instanceof ArrayBuffer) || buffer.byteLength <= 16 || buffer.byteLength > MAX_FRAME + 16) {
      throw new Error('Canvas 视频帧长度不合法');
    }
    const view = new DataView(buffer);
    if (view.getUint32(0) !== 0x54424331 || view.getUint32(4) > 1) {
      throw new Error('Canvas 视频标识或关键帧标记不合法');
    }
    const timestamp = view.getUint32(8) * 4294967296 + view.getUint32(12);
    if (!Number.isSafeInteger(timestamp)) throw new Error('Canvas 时间戳超过安全整数范围');
    const data = new Uint8Array(buffer, 16);
    if (!(data[0] === 0 && data[1] === 0 && (data[2] === 1 || (data[2] === 0 && data[3] === 1)))) {
      throw new Error('Canvas 视频不是 Annex-B');
    }
    return { type: view.getUint32(4) === 1 ? 'key' : 'delta', timestamp, data };
  }

  class CanvasPlayer {
    constructor(canvas, onFrame, onError, environment = root, onKeyframeNeeded = () => {}, onConsumed = () => {}) {
      this.canvas = canvas; this.environment = environment;
      this.onFrame = onFrame; this.onError = onError; this.onKeyframeNeeded = onKeyframeNeeded;
      this.onConsumed = onConsumed;
      this.decoder = null; this.config = null; this.queue = [];
      this.queuedBytes = 0; this.waitingKey = true; this.needsReset = false; this.generation = 0;
      this.lastTimestamp = -1; this.closed = false;
      this.inFlight = new Map(); this.renderedOnce = false; this.droppedFrames = 0;
      this.stallTimer = null;
      if (environment.TeslaBoxRenderer) {
        this.renderer = environment.TeslaBoxRenderer.create(canvas);
      } else {
        const context = canvas.getContext('2d', { alpha: false });
        if (!context) throw new Error('浏览器没有可用 Canvas 2D 上下文');
        this.renderer = { mode: '2d', draw: (frame, width, height) => context.drawImage(frame, 0, 0, width, height), close() {} };
      }
    }

    async configure(codec) {
      this.reset();
      const generation = this.generation;
      if (!/^avc1\.[0-9a-fA-F]{6}$/.test(codec)) { this.fail(new Error('H.264 codec 不合法')); return; }
      const env = this.environment;
      if (typeof env.VideoDecoder !== 'function' || typeof env.EncodedVideoChunk !== 'function') {
        this.fail(new Error('此页面没有 WebCodecs；请检查 HTTPS、安全上下文和车机浏览器版本。不会自动切回 video 播放。'));
        return;
      }
      try {
        // 不提供 AVC description 时，标准规定输入为 Annex-B；不能把 avcC 与它混用。
        const config = { codec, optimizeForLatency: true, hardwareAcceleration: 'prefer-hardware' };
        const supported = await env.VideoDecoder.isConfigSupported(config);
        if (generation !== this.generation || this.closed) return;
        if (!supported.supported) throw new Error('浏览器不支持实际 H.264 profile/level：' + codec);
        const decoder = new env.VideoDecoder({
          output: frame => {
            let consumed = false;
            try {
              if (this.decoder !== decoder || generation !== this.generation || this.closed) return;
              const width = frame.displayWidth, height = frame.displayHeight;
              if (!width || !height || width > 8192 || height > 8192 || width * height > 16777216) {
                throw new Error('解码画面尺寸超过预算');
              }
              const submitted = this.inFlight.get(frame.timestamp);
              this.inFlight.delete(frame.timestamp);
              // decodeQueueSize 归零不代表硬件已输出；旧参考代次的迟到输出也不能绘制。
              if (submitted === undefined) return;
              consumed = true;
              const latencyMs = this.now() - submitted;
              if (this.renderedOnce && (this.lastTimestamp - frame.timestamp > MAX_BACKLOG_US || latencyMs > MAX_DECODE_MS)) {
                this.droppedFrames++; return;
              }
              if (this.canvas.width !== width) this.canvas.width = width;
              if (this.canvas.height !== height) this.canvas.height = height;
              const drawStarted = this.now();
              this.renderer.draw(frame, width, height);
              const drawMs = this.now() - drawStarted;
              this.renderedOnce = true;
              this.onFrame({ width, height, timestamp: frame.timestamp, renderer: this.renderer.mode, pendingOutput: this.inFlight.size, droppedFrames: this.droppedFrames, latencyMs, drawMs });
            } catch (error) { this.fail(error); } finally {
              const timestamp = frame.timestamp; frame.close();
              if (consumed) this.onConsumed(timestamp);
              if (!this.inFlight.size) this.clearStallTimer();
            }
          },
          error: error => { if (this.decoder === decoder && !this.closed) this.fail(error); }
        });
        this.decoder = decoder; this.config = config;
        decoder.configure(config);
        const pending = this.queue; this.queue = []; this.queuedBytes = 0;
        for (const frame of pending) this.decode(frame);
      } catch (error) { if (generation === this.generation && !this.closed) this.fail(error); }
    }

    push(buffer) {
      if (this.closed) return;
      try {
        const frame = packet(buffer);
        if (frame.timestamp <= this.lastTimestamp) throw new Error('Canvas 时间戳重复或倒退');
        this.lastTimestamp = frame.timestamp;
        if (!this.decoder) {
          if (this.queue.length >= 6 || this.queuedBytes + buffer.byteLength > 8 * 1024 * 1024) {
            throw new Error('浏览器初始化期间视频队列超过预算');
          }
          this.queue.push(frame); this.queuedBytes += buffer.byteLength;
          return;
        }
        this.decode(frame);
      } catch (error) { this.fail(error); }
    }

    decode(frame) {
      if (!this.decoder || this.closed) return;
      // 同时限制已提交但尚未输出的帧和源时间跨度，覆盖硬件内部缓冲。
      const oldest = this.inFlight.keys().next().value;
      if (this.decoder.decodeQueueSize >= MAX_PENDING_OUTPUT || this.inFlight.size >= MAX_PENDING_OUTPUT ||
          (oldest !== undefined && frame.timestamp - oldest > MAX_BACKLOG_US)) {
        this.waitingKey = true; this.needsReset = true;
      }
      if (this.waitingKey && frame.type !== 'key') { this.onConsumed(frame.timestamp); this.onKeyframeNeeded(); return; }
      // 先让已提交的首帧完成输出；下一关键帧到来时才丢弃旧参考链。
      if (this.needsReset) this.resync();
      this.waitingKey = false;
      this.inFlight.set(frame.timestamp, this.now());
      try { this.decoder.decode(new this.environment.EncodedVideoChunk(frame)); }
      catch (error) { this.inFlight.delete(frame.timestamp); throw error; }
      this.armStallTimer();
    }

    now() { return this.environment.performance?.now ? this.environment.performance.now() : Date.now(); }

    // 消费窗口满而硬件永不输出时，发送端已停止送帧，不能再靠下一输入触发恢复。
    armStallTimer() {
      if (this.closed || this.stallTimer !== null || !this.inFlight.size || typeof this.environment.setTimeout !== 'function') return;
      const oldest = this.inFlight.values().next().value;
      this.stallTimer = this.environment.setTimeout(() => {
        this.stallTimer = null;
        if (this.closed || !this.inFlight.size) return;
        const submitted = this.inFlight.values().next().value;
        if (this.now() - submitted < MAX_OUTPUT_STALL_MS) { this.armStallTimer(); return; }
        this.resync();
        this.onKeyframeNeeded();
      }, Math.max(1, MAX_OUTPUT_STALL_MS - (this.now() - oldest)));
    }

    clearStallTimer() {
      if (this.stallTimer !== null) this.environment.clearTimeout(this.stallTimer);
      this.stallTimer = null;
    }

    reset() {
      this.clearStallTimer();
      for (const timestamp of this.inFlight.keys()) this.onConsumed(timestamp);
      for (const frame of this.queue) this.onConsumed(frame.timestamp);
      this.inFlight.clear(); this.renderedOnce = false; this.droppedFrames = 0;
      this.generation += 1; this.queue = []; this.queuedBytes = 0;
      this.waitingKey = true; this.needsReset = false; this.lastTimestamp = -1; this.config = null;
      if (this.decoder) { if (this.decoder.state !== 'closed') this.decoder.close(); this.decoder = null; }
    }

    // 丢包后清空参考链，但不能重置对源时间戳倒退的检查。
    resync() {
      this.clearStallTimer();
      for (const timestamp of this.inFlight.keys()) this.onConsumed(timestamp);
      this.inFlight.clear();
      this.renderedOnce = false;
      this.queue = []; this.queuedBytes = 0; this.waitingKey = true; this.needsReset = false;
      if (this.decoder && this.decoder.state !== 'closed') { this.decoder.reset(); this.decoder.configure(this.config); }
    }

    fail(error) { this.close(); this.onError(error); }
    close() { if (!this.closed) { this.closed = true; this.reset(); this.renderer.close(); } }
  }

  root.TeslaBoxCanvas = { CanvasPlayer, packet };
  if (typeof module === 'object' && module.exports) module.exports = root.TeslaBoxCanvas;
})(typeof globalThis !== 'undefined' ? globalThis : self);
