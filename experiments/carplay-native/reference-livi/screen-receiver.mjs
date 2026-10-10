import net from 'node:net';
import { uptime } from 'node:os';
import { validWindow } from './window-policy.mjs';
import { EventEmitter } from 'node:events';
import { loadPinnedScreenStream } from './pinned-auth-loader.mjs';

const MAX_FRAME = 4 * 1024 * 1024;
const MAX_CODEC = 64 * 1024;
const MAX_QUEUED = 8 * 1024 * 1024;
const MAX_COUNTER = (1n << 64n) - 1n;
const canonical = ip => ip?.startsWith('::ffff:') ? ip.slice(7) : ip;

function privateLiteral(ip) {
  const value = canonical(ip);
  if (net.isIP(value) === 4) {
    const [first, second] = value.split('.').map(Number);
    return first === 10 || first === 127 || (first === 172 && second >= 16 && second <= 31)
      || (first === 192 && second === 168);
  }
  return net.isIP(value) === 6 && (value === '::1' || /^[fF][cCdD]/.test(value));
}

function validate(options) {
  const { key, host, peer, ingestHost = '127.0.0.1', ingestPort, windowMs = 120000, ioTimeoutMs = 2000 } = options;
  const startupGraceMs = options.startupGraceMs ?? (process.platform === 'linux' ? Math.max(0, Math.ceil((120 - uptime()) * 1000)) : 0);
  if (!Buffer.isBuffer(key) || key.length !== 32 || !privateLiteral(host) || !privateLiteral(peer)
      || ['0.0.0.0', '::'].includes(host) || ['0.0.0.0', '::'].includes(peer)
      || !['127.0.0.1', '::1'].includes(ingestHost) || !Number.isInteger(ingestPort)
      || ingestPort < 1 || ingestPort > 65535 || !validWindow(windowMs, options.longRunning, options.daemon)
      || !Number.isInteger(ioTimeoutMs) || ioTimeoutMs < 1 || ioTimeoutMs > 2000
      || !Number.isInteger(startupGraceMs) || startupGraceMs < 0 || startupGraceMs > 120000) {
    throw new Error('screen_options_invalid');
  }
  return { key: Buffer.from(key), host, peer: canonical(peer), ingestHost, ingestPort, windowMs, ioTimeoutMs, startupGraceMs, onCodecConfig: options.onCodecConfig, onFirstFrame: options.onFirstFrame };
}

// 固定参考从 avcC 标记一直切到负载末尾；按盒长截断，避免把后续盒误作配置扩展。
function boundedConfig(body) {
  for (let offset = 4; offset + 4 <= body.length; offset++) {
    const type = body.toString('ascii', offset, offset + 4);
    if (type !== 'avcC' && type !== 'hvcC') continue;
    const size = body.readUInt32BE(offset - 4);
    const end = offset - 4 + size;
    if (size < 8 || end > body.length) throw new Error('screen_config_box_invalid');
    return body.subarray(0, end);
  }
  return body;
}
/** 独立参考媒体接口：先连接真实本机消费者，才允许返回给 SETUP 一个接收端口。 */
export async function createScreenReceiver(options) {
  const config = validate(options);
  try {
    const { ScreenStream } = await loadPinnedScreenStream();
    const receiver = new ReferenceScreenReceiver(config, new ScreenStream(config.key));
    await receiver.start();
    return receiver;
  } catch (error) {
    config.key.fill(0);
    throw error;
  }
}

class ReferenceScreenReceiver extends EventEmitter {
  #config;
  #upstream;
  #server;
  #output;
  #input = null;
  #closed = false;
  #ending = false;
  #timers = new Set();
  #partial = null;
  #partialBudget = null;
  #partialStarted = 0;
  #lastInput = 0;
  #blocked = null;
  #keepConsumer = null;
  #header = Buffer.alloc(128);
  #headerUsed = 0;
  #bodyLeft = 0;
  #timestamp = 0n;
  #codec = false;
  #hasFrame = false;
  #attempts = 0;
  #end;
  #startupDeadline = 0;
  port = null;

  constructor(config, upstream) {
    super();
    this.#config = config;
    this.#startupDeadline = performance.now() + config.startupGraceMs;
    this.#upstream = upstream;
    this.done = new Promise(resolve => { this.#end = resolve; });
    this.#output = new net.Socket();
    this.#server = net.createServer(socket => this.#accept(socket));
    this.#server.on('error', () => this.stop('screen_listen_failed'));
    this.#output.on('error', () => this.stop('screen_consumer_failed'));
    this.#output.on('end', () => this.stop(this.#ending ? 'screen_input_closed' : 'screen_consumer_closed'));
    this.#output.on('close', () => this.stop(this.#ending ? 'screen_input_closed' : 'screen_consumer_closed'));
    this.#output.on('drain', () => {
      this.#clear(this.#blocked);
      this.#blocked = null;
      if (!this.#closed) this.#input?.resume();
    });
    const original = upstream._onMessage.bind(upstream);
    upstream._onMessage = (header, body) => {
      if (this.#closed || this.#ending) return;
      try {
        this.#validateHeader(header);
        if (body.length !== header.readUInt32LE(0) || upstream._counter > MAX_COUNTER) {
          throw new Error('screen_record_invalid');
        }
        this.#timestamp = header.readBigUInt64LE(8);
        original(header, header[4] === 1 ? boundedConfig(body) : body);
        this.#armConsumerHeartbeat();
      } catch {
        this.stop('screen_record_rejected');
        throw new Error('screen_record_rejected');
      }
    };
    upstream.on('codec', codec => {
      if (codec !== 'h264') this.stop('screen_codec_unsupported');
    });
    upstream.on('config', bytes => {
      if (this.#closed) return;
      this.#config.onCodecConfig?.(Buffer.from(bytes));
      this.#codec = true;
      this.#writePacket(1, bytes, 0n);
    });
    upstream.on('frame', bytes => {
      if (!this.#codec) { this.stop('screen_config_missing'); return; }
      this.#writePacket(0, bytes, this.#timestamp);
      if (!this.#closed && bytes.length && !this.#hasFrame) {
        this.#hasFrame = true;
        this.#config.onFirstFrame?.();
      }
    });
  }

  #timer(reason, delay) {
    const timer = setTimeout(() => { this.#timers.delete(timer); this.stop(reason); }, delay);
    this.#timers.add(timer);
    return timer;
  }

  #clear(timer) { if (timer) { clearTimeout(timer); this.#timers.delete(timer); } }

  async start() {
    const { ingestHost, ingestPort, host, windowMs, ioTimeoutMs } = this.#config;
    const connectTimer = this.#timer('screen_consumer_timeout', ioTimeoutMs);
    const connected = new Promise(resolve => this.#output.once('connect', () => resolve(true)));
    this.#output.connect(ingestPort, ingestHost);
    if (!await Promise.race([connected, this.done.then(() => false)])) throw new Error('screen_consumer_not_ready');
    this.#clear(connectTimer);
    this.#output.setNoDelay(true);
    const listening = new Promise(resolve => this.#server.listen({ port: 0, host, ipv6Only: net.isIP(host) === 6 }, () => {
      if (this.#closed) { this.#server.close(); resolve(false); }
      else resolve(true);
    }));
    if (!await Promise.race([listening, this.done.then(() => false)])) throw new Error('screen_listener_not_ready');
    this.port = this.#server.address().port;
    if (windowMs !== 0) this.#timer('screen_window_expired', windowMs);
  }

  #accept(socket) {
    socket.on('error', () => {});
    if (this.#closed || this.#input || canonical(socket.remoteAddress) !== this.#config.peer) {
      socket.destroy();
      if (++this.#attempts > 16) this.stop('screen_peer_budget');
      return;
    }
    this.#input = socket;
    socket.setNoDelay(true);
    this.#server.close(); // 同一媒体密钥只允许一条输入，重连必须重新协商并创建实例。
    socket.on('data', chunk => this.#observe(chunk));
    socket.on('close', () => this.#finishInput());
    this.#upstream._onConnection(socket);
    this.#armConsumerHeartbeat();
  }

  #armConsumerHeartbeat() {
    // 手机空心跳没有写入消费者，不能反复推迟本机保活截止时间。
    if (this.#closed || this.#ending || this.#keepConsumer) return;
    // 这是本机 IPC 的无媒体心跳，不代表收到手机帧，不消费手机 nonce 或合成画面。
    // 静止导航时避免下游 15 秒空闲回收；只有限时且自有的媒体实例会保持消费者。
    this.#keepConsumer = setTimeout(() => {
      this.#timers.delete(this.#keepConsumer);
      this.#keepConsumer = null;
      if (this.#closed || this.#ending) return;
      if (!this.#partial && !this.#blocked) this.#writePacket(2, Buffer.alloc(0), 0n);
      this.#armConsumerHeartbeat();
    }, 10000);
    this.#timers.add(this.#keepConsumer);
  }

  #finishInput() {
    if (this.#closed || this.#ending) return;
    if (this.#headerUsed || this.#bodyLeft) { this.stop('screen_truncated_input'); return; }
    this.#ending = true;
    this.#clear(this.#keepConsumer);
    // 正常 EOF 先 flush 完本机输出；立即 destroy 会丢掉末尾仍在用户态队列中的帧。
    this.#timer('screen_output_finish_timeout', this.#config.ioTimeoutMs);
    this.#output.end();
  }

  #validateHeader(header) {
    // 固定参考按单字节 opcode 解析；后续字节仍原样参与帧 AEAD 认证。
    const kind = header[4];
    const length = header.readUInt32LE(0);
    if ((kind === 0 && (length <= 16 || length > MAX_FRAME + 16))
        || (kind === 1 && (length < 7 || length > MAX_CODEC)) || (kind === 2 && length !== 0)
        || kind > 2) throw new Error('screen_header_invalid');
  }

  #observe(chunk) {
    if (this.#closed || this.#ending) return;
    if (!chunk.length) return;
    this.#lastInput = performance.now();
    let offset = 0;
    try {
      while (offset < chunk.length) {
        if (this.#bodyLeft) {
          const used = Math.min(this.#bodyLeft, chunk.length - offset);
          offset += used;
          this.#bodyLeft -= used;
          if (this.#bodyLeft) break;
          this.#clear(this.#partial);
          this.#partial = null;
          this.#clear(this.#partialBudget);
          this.#partialBudget = null;
        } else {
          const used = Math.min(128 - this.#headerUsed, chunk.length - offset);
          chunk.copy(this.#header, this.#headerUsed, offset, offset + used);
          this.#headerUsed += used;
          offset += used;
          if (this.#headerUsed < 128) break;
          this.#validateHeader(this.#header);
          this.#bodyLeft = this.#header.readUInt32LE(0);
          this.#headerUsed = 0;
          if (!this.#bodyLeft) {
            this.#clear(this.#partial); this.#partial = null;
            this.#clear(this.#partialBudget); this.#partialBudget = null;
          }
        }
      }
      if (this.#headerUsed || this.#bodyLeft) {
        // 无线分片只要仍有进度就更新空闲期限；总期限防止滴流无限占用会话。
        this.#clear(this.#partial);
        // 开机扫描同一射频时可能短暂停收；只延长半包空闲等待，总时限和输出背压仍保持原限制。
        const idleTimeout = this.#config.ioTimeoutMs * (performance.now() < this.#startupDeadline ? 3 : 1);
        this.#partial = this.#timer('screen_partial_timeout', idleTimeout);
        if (!this.#partialBudget) {
          this.#partialStarted = this.#lastInput;
          this.#partialBudget = this.#timer('screen_partial_budget_timeout', this.#config.ioTimeoutMs * 5);
        }
      }
    } catch { this.stop('screen_header_rejected'); }
  }

  #writePacket(kind, bytes, timestamp) {
    if (this.#closed || this.#ending) return;
    if (!Buffer.isBuffer(bytes) || (kind === 2 ? bytes.length !== 0 : !bytes.length)
        || bytes.length > (kind === 1 ? MAX_CODEC : MAX_FRAME)) {
      this.stop('screen_payload_budget');
      return;
    }
    const header = Buffer.alloc(128);
    header.writeUInt32LE(bytes.length, 0);
    header.writeUInt16LE(kind, 4);
    header.writeBigUInt64LE(timestamp, 8);
    if (this.#output.writableLength + header.length + bytes.length > MAX_QUEUED) {
      this.stop('screen_output_budget');
      return;
    }
    const accepted = this.#output.write(Buffer.concat([header, bytes]));
    if (!accepted) {
      this.#input?.pause();
      if (!this.#blocked) this.#blocked = this.#timer('screen_output_timeout', this.#config.ioTimeoutMs);
    }
  }

  /** 关闭所有自有端口和 socket；停止后不可复用媒体密钥或产生后续输出。 */
  stop(reason = 'screen_local_stop') {
    if (this.#closed) return;
    if (reason === 'screen_partial_timeout' || reason === 'screen_partial_budget_timeout') {
      const now = performance.now();
      // 只记录计数和耗时，区分停收、慢传输与本机背压，不输出视频或密钥。
      console.log(`REFERENCE_SCREEN_TIMEOUT reason=${reason} header_bytes=${this.#headerUsed} body_left=${this.#bodyLeft} idle_ms=${Math.round(now - this.#lastInput)} elapsed_ms=${Math.round(now - this.#partialStarted)} blocked=${Boolean(this.#blocked)}`);
    }
    this.#closed = true;
    for (const timer of this.#timers) clearTimeout(timer);
    this.#timers.clear();
    this.#input?.destroy();
    this.#output.destroy();
    this.#server.close();
    this.#upstream.stop();
    this.#config.key.fill(0);
    this.#header.fill(0);
    this.#end({ reason });
  }
}
