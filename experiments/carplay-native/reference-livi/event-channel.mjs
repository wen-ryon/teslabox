import net from 'node:net';
import { validWindow } from './window-policy.mjs';
import { loadPinnedEventCodecs } from './pinned-auth-loader.mjs';
import { createIapReceiver, createIapRelay } from './iap-receiver.mjs';

const MAX_RECORD = 16384;
const MAX_HEADER = 8192;
const MAX_BODY = 128 * 1024;
const MAX_QUEUE = 256 * 1024;
const MAX_COUNTER = (1n << 64n) - 1n;
const canonical = ip => ip?.startsWith('::ffff:') ? ip.slice(7) : ip;
const privateIPv4 = ip => {
  if (net.isIP(ip) !== 4) return false;
  const [a, b] = ip.split('.').map(Number);
  return a === 10 || a === 127 || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168);
};

/** 控制和事件共用分帧预算，完整报文才可交给固定 RTSP 解析器。 */
export function messageSize(bytes, allowUnsequencedResponse = false) {
  const end = bytes.indexOf('\r\n\r\n');
  if (end < 0) { if (bytes.length > MAX_HEADER) throw new Error('header_size'); return null; }
  if (end + 4 > MAX_HEADER) throw new Error('header_size');
  const header = bytes.subarray(0, end);
  // 字段值允许 HTAB 和不透明的高位字节；latin1 防止 ascii 解码清除高位后误判语法。
  if (header.some(b => b < 32 && b !== 9 && b !== 13 && b !== 10 || b === 127)) throw new Error('header_control_byte');
  const [first, ...lines] = header.toString('latin1').split('\r\n');
  if (!/^(?:[A-Z_]{1,32} [\x21-\x7e]{1,512} (?:RTSP\/1\.0|HTTP\/1\.[01])|(?:RTSP\/1\.0|HTTP\/1\.[01]) [1-5][0-9]{2} [\x20-\x7e]{1,128})$/.test(first)) throw new Error('header_start_line');
  const names = new Set();
  let length = 0;
  for (const line of lines) {
    const match = /^([A-Za-z0-9-]{1,64}):[ \t]*([\t\x20-\x7e\x80-\xff]*)$/.exec(line);
    if (!match || names.has(match[1].toLowerCase()) || ['constructor', 'prototype'].includes(match[1].toLowerCase())) throw new Error('header_field');
    const name = match[1].toLowerCase();
    const value = match[2].replace(/^[ \t]+|[ \t]+$/g, '');
    names.add(name);
    if (name === 'content-length') {
      if (!/^[0-9]{1,6}$/.test(value) || Number(value) > MAX_BODY) throw new Error('header_content_length');
      length = Number(value);
    }
    if (name === 'cseq' && (!/^[0-9]{1,10}$/.test(value) || Number(value) > 0xffffffff)) throw new Error('header_cseq_value');
    if (name === 'transfer-encoding') throw new Error('header_transfer_encoding');
  }
  if (!names.has('cseq') && !(allowUnsequencedResponse && /^(?:RTSP\/|HTTP\/)/.test(first))) throw new Error('header_cseq_missing');
  return end + 4 + length;
}

/** 仅接受已完成 Pair-Verify 的 sharedSecret；来源限制不能替代握手认证。 */
export async function createEventChannel(options) {
  const { sharedSecret, host, peer, onRequest, windowMs = 120000, ioTimeoutMs = 2000 } = options;
  if (!Buffer.isBuffer(sharedSecret) || sharedSecret.length !== 32 || !privateIPv4(host) || !privateIPv4(peer)
      || typeof onRequest !== 'function' || !validWindow(windowMs, options.longRunning, options.daemon)
      || !Number.isInteger(ioTimeoutMs) || ioTimeoutMs < 1 || ioTimeoutMs > 2000) throw new Error('event_options_invalid');
  const secret = Buffer.from(sharedSecret);
  let channel;
  try {
    const codecs = await loadPinnedEventCodecs();
    const readKey = codecs.hkdfSha512(secret, 'Events-Salt', 'Events-Read-Encryption-Key', 32);
    const writeKey = codecs.hkdfSha512(secret, 'Events-Salt', 'Events-Write-Encryption-Key', 32);
    channel = new EventChannel({ host, peer, onRequest, windowMs, ioTimeoutMs }, codecs, readKey, writeKey, secret);
    await channel.start();
    return channel;
  } catch { channel?.stop('event_start_failed'); throw new Error('event_not_ready'); }
  finally { secret.fill(0); }
}

class EventChannel {
  #config;
  #codecs;
  #cipher;
  #readKey;
  #writeKey;
  #sharedSecret;
  #server;
  #socket;
  #closed = false;
  #peerEnded = false;
  #finishing = false;
  #record = Buffer.alloc(0);
  #plain = Buffer.alloc(0);
  #messages = [];
  #messageBytes = 0;
  #processing = false;
  #timers = new Set();
  #partial;
  #writes = new Set();
  #pending = new Set();
  #cseq = 0;
  #attempts = 0;
  #abort = new AbortController();
  #resolveDone;
  #resolveConnected;
  port = null;

  constructor(config, codecs, readKey, writeKey, sharedSecret) {
    this.#config = config;
    this.#codecs = codecs;
    this.#readKey = readKey;
    this.#writeKey = writeKey;
    this.#sharedSecret = Buffer.from(sharedSecret);
    this.#cipher = new codecs.ControlCipher(readKey, writeKey);
    this.done = new Promise(resolve => { this.#resolveDone = resolve; });
    this.connected = new Promise(resolve => { this.#resolveConnected = resolve; });
    this.#server = net.createServer({ allowHalfOpen: true }, socket => this.#accept(socket));
    this.#server.on('error', () => this.stop('event_listen_failed'));
  }

  #timer(reason, delay) {
    const timer = setTimeout(() => { this.#timers.delete(timer); this.stop(reason); }, delay);
    this.#timers.add(timer);
    return timer;
  }
  #clear(timer) { if (timer) { clearTimeout(timer); this.#timers.delete(timer); } }

  async start() {
    const listening = new Promise(resolve => this.#server.listen({ host: this.#config.host, port: 0 }, () => {
      if (this.#closed) { this.#server.close(); resolve(false); }
      else resolve(true);
    }));
    if (!await Promise.race([listening, this.done.then(() => false)])) throw new Error();
    this.port = this.#server.address().port;
    if (this.#config.windowMs !== 0) this.#timer('event_window_expired', this.#config.windowMs);
  }

  #accept(socket) {
    socket.on('error', () => { if (socket === this.#socket) this.stop('event_socket_failed'); });
    if (this.#closed || this.#socket || canonical(socket.remoteAddress) !== this.#config.peer) {
      socket.destroy();
      if (++this.#attempts > 16) this.stop('event_peer_budget');
      return;
    }
    this.#socket = socket;
    this.#server.close();
    socket.setNoDelay(true);
    socket.on('data', bytes => this.#receive(bytes));
    socket.on('end', () => {
      this.#peerEnded = true;
      if (this.#record.length || this.#plain.length) this.stop('event_truncated_input');
      else this.#finishPeer();
    });
    socket.on('close', () => this.stop('event_socket_closed'));
    this.#resolveConnected(true);
  }

  #receive(chunk) {
    if (this.#closed) return;
    let offset = 0;
    let rejection = 'record';
    try {
      while (offset < chunk.length && !this.#closed) {
        rejection = 'record';
        const wanted = this.#record.length < 2 ? 2 : 18 + this.#record.readUInt16LE(0);
        const count = Math.min(wanted - this.#record.length, chunk.length - offset);
        this.#record = Buffer.concat([this.#record, chunk.subarray(offset, offset + count)]);
        offset += count;
        if (this.#record.length < 2) break;
        const size = this.#record.readUInt16LE(0);
        if (!size || size > MAX_RECORD || this.#cipher.readCtr > MAX_COUNTER) throw new Error();
        if (this.#record.length !== size + 18) continue;
        rejection = 'decrypt';
        const { data, rest } = this.#cipher.decrypt(this.#record);
        rejection = 'budget';
        if (rest.length) throw new Error();
        this.#record.fill(0);
        this.#record = Buffer.alloc(0);
        if (this.#plain.length + data.length > MAX_BODY + MAX_HEADER + MAX_RECORD) throw new Error();
        this.#plain = Buffer.concat([this.#plain, data]);
        data.fill(0);
        for (;;) {
          rejection = 'header';
          const length = messageSize(this.#plain, true);
          if (length === null || this.#plain.length < length) break;
          rejection = 'parse';
          const { messages, rest: trailing } = this.#codecs.parseMessages(this.#plain.subarray(0, length));
          if (messages.length !== 1 || trailing.length) throw new Error();
          this.#plain = Buffer.from(this.#plain.subarray(length));
          rejection = 'budget';
          if (this.#messages.length >= 64 || this.#messageBytes + length > MAX_QUEUE) throw new Error();
          this.#messages.push({ message: messages[0], size: length });
          this.#messageBytes += length;
        }
      }
      if (this.#record.length || this.#plain.length) {
        if (!this.#partial) this.#partial = this.#timer('event_partial_timeout', this.#config.ioTimeoutMs);
      } else { this.#clear(this.#partial); this.#partial = null; }
      if (this.#messages.length) { this.#socket.pause(); void this.#process(); }
    } catch (error) {
      // 只输出固定校验项，避免为定位报头问题泄露手机标识或原始正文。
      const detail = rejection === 'header' && /^header_(size|control_byte|start_line|field|content_length|cseq_value|transfer_encoding|cseq_missing)$/.test(error?.message)
        ? error.message : rejection;
      this.stop(`event_record_rejected_${detail}`);
    }
  }

  async #process() {
    if (this.#processing || this.#closed) return;
    this.#processing = true;
    while (this.#messages.length && !this.#closed) {
      const { message, size } = this.#messages.shift();
      this.#messageBytes -= size;
      try {
        if (message.method.startsWith('RTSP/') || message.method.startsWith('HTTP/')) {
          // 实机事件响应可省略 CSeq；同一 TCP 上按发送顺序归还待确认预算。
          // 显式 CSeq 仍精确匹配，不能把未知序号响应当作已有命令的 ACK。
          const cseq = message.headers.cseq === undefined
            ? this.#pending.values().next().value : Number(message.headers.cseq);
          if (this.#pending.has(cseq)) {
            this.#pending.delete(cseq);
            if (message.path !== '200') { this.stop('event_peer_rejected'); break; }
          }
        } else {
          const timer = this.#timer('event_handler_timeout', this.#config.ioTimeoutMs);
          let status;
          try {
            status = await Promise.race([Promise.resolve(this.#config.onRequest(message, this.#abort.signal)), this.done.then(() => null)]);
          } finally { this.#clear(timer); }
          if (this.#closed) break;
          if (![200, 400, 404, 500].includes(status)) throw new Error();
          await this.#write(this.#codecs.buildResponse(message, { status }));
        }
      } catch { this.stop('event_handler_failed'); }
      finally { message.body.fill(0); }
    }
    this.#processing = false;
    if (!this.#closed) { this.#socket.resume(); this.#finishPeer(); }
  }

  #finishPeer() {
    if (this.#closed || !this.#peerEnded || this.#finishing || this.#processing || this.#messages.length || this.#writes.size) return;
    this.#finishing = true;
    this.#timer('event_finish_timeout', this.#config.ioTimeoutMs);
    this.#socket.end(() => this.stop('event_peer_eof'));
  }

  #write(plain, signal) {
    if (this.#closed || !this.#socket || signal?.aborted) return Promise.reject(new Error('event_not_connected'));
    const records = Math.ceil(plain.length / MAX_RECORD);
    if (this.#cipher.writeCtr + BigInt(records) - 1n > MAX_COUNTER) {
      this.stop('event_counter_exhausted'); return Promise.reject(new Error('event_write_failed'));
    }
    const wire = this.#cipher.encrypt(plain);
    plain.fill(0);
    if (this.#socket.writableLength + wire.length > MAX_QUEUE) {
      wire.fill(0); this.stop('event_write_budget'); return Promise.reject(new Error('event_write_failed'));
    }
    return new Promise((resolve, reject) => {
      const abort = () => this.stop('event_write_cancelled');
      const timer = this.#timer('event_write_timeout', this.#config.ioTimeoutMs);
      const finish = error => {
        if (!this.#writes.delete(finish)) return;
        this.#clear(timer);
        signal?.removeEventListener('abort', abort);
        wire.fill(0);
        if (error) reject(new Error('event_write_failed'));
        else resolve();
        this.#finishPeer();
      };
      this.#writes.add(finish);
      signal?.addEventListener('abort', abort, { once: true });
      this.#socket.write(wire, error => { if (error) this.stop('event_write_failed'); finish(error); });
    });
  }

  /** 成功只表示本机 TCP 写出，不代表手机 ACK、iAP 认证或屏幕流已成功。 */
  async sendIap(bytes, signal) {
    if (!Buffer.isBuffer(bytes) || !bytes.length || bytes.length > 65536) throw new Error('event_iap_invalid');
    return this.sendCommand(this.#codecs.encodeBplist({ type: 'iAPSendMessage', params: { data: Buffer.from(bytes) } }), signal);
  }

  /** 控制引擎生成的二进制命令沿同一事件序列发送，保留 ACK 跟踪和写入预算。 */
  async sendCommand(body, signal) {
    if (!Buffer.isBuffer(body) || !body.length || body.length > MAX_BODY) throw new Error('event_command_invalid');
    if (signal?.aborted) throw new Error('event_write_cancelled');
    if (this.#closed || this.#peerEnded || !this.#socket || this.#pending.size >= 64 || this.#cseq >= 0xffffffff) {
      this.stop('event_command_budget'); throw new Error('event_command_unavailable');
    }
    const cseq = ++this.#cseq;
    const head = Buffer.from(`POST /command RTSP/1.0\r\nContent-Type: application/x-apple-binary-plist\r\nContent-Length: ${body.length}\r\nCSeq: ${cseq}\r\n\r\n`, 'ascii');
    this.#pending.add(cseq);
    const plain = Buffer.concat([head, body]);
    await this.#write(plain, signal);
  }

  /** iAP 只能绑定同一已连入的认证事件通道；事件退场后回收下行，不把它留成孤儿。 */
  async createIapReceiver(options) {
    return this.#bindIap(options, createIapReceiver);
  }

  /** RECORD 阶段使用同一认证事件通道启动 helper，不等待 type 130。 */
  async createIapRelay(options) {
    return this.#bindIap(options, createIapRelay);
  }

  async #bindIap(options, create) {
    if (options.host !== this.#config.host || options.peer !== this.#config.peer
        || Object.hasOwn(options, 'onAccessoryMessage') || (Object.hasOwn(options, 'sharedSecret')
          && (!Buffer.isBuffer(options.sharedSecret) || !options.sharedSecret.equals(this.#sharedSecret)))) {
      throw new Error('event_iap_binding_invalid');
    }
    const timer = this.#timer('event_connect_timeout', this.#config.ioTimeoutMs);
    try {
      if (!await this.connected || this.#closed) throw new Error('event_not_connected');
    } finally { this.#clear(timer); }
    const receiver = await create({ ...options, sharedSecret: this.#sharedSecret,
      onAccessoryMessage: (bytes, signal) => this.sendIap(bytes, signal) });
    if (this.#closed) { receiver.stop('iap_event_closed'); throw new Error('event_not_connected'); }
    void this.done.then(() => receiver.stop('iap_event_closed'));
    return receiver;
  }

  /** 事件连接退场必须通知所属会话清理媒体；本实例只关闭自己的资源。 */
  stop(reason = 'event_local_stop') {
    if (this.#closed) return;
    this.#closed = true;
    this.#abort.abort();
    for (const timer of this.#timers) clearTimeout(timer);
    this.#timers.clear();
    for (const finish of [...this.#writes]) finish(new Error());
    this.#socket?.destroy();
    this.#server.close();
    this.#record.fill(0);
    this.#plain.fill(0);
    for (const { message } of this.#messages) message.body.fill(0);
    this.#messages.length = 0;
    this.#pending.clear();
    this.#readKey.fill(0);
    this.#writeKey.fill(0);
    this.#sharedSecret.fill(0);
    this.#resolveConnected(false);
    this.#resolveDone({ reason });
  }
}
