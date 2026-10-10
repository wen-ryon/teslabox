import net from 'node:net';
import { validWindow } from './window-policy.mjs';
import path from 'node:path';
import { lstatSync } from 'node:fs';
import { loadPinnedIapTunnel } from './pinned-auth-loader.mjs';

const MAX_RECORD = 16384;
const MAX_PACKAGE = 4 * 1024 * 1024;
const MAX_QUEUE = 256 * 1024;
const MAX_COUNTER = (1n << 64n) - 1n;
const canonical = ip => ip?.startsWith('::ffff:') ? ip.slice(7) : ip;

function privateIPv4(ip) {
  if (net.isIP(ip) !== 4) return false;
  const [a, b] = ip.split('.').map(Number);
  return a === 10 || a === 127 || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168);
}

function helperAddress(options) {
  if (options.testOnlyLoopbackHelperPort !== undefined) {
    if (process.platform !== 'win32' || options.allowWindowsTestHelper !== true || options.helperPath !== undefined
        || !Number.isInteger(options.testOnlyLoopbackHelperPort) || options.testOnlyLoopbackHelperPort < 1
        || options.testOnlyLoopbackHelperPort > 65535) throw new Error('iap_helper_options_invalid');
    return { host: '127.0.0.1', port: options.testOnlyLoopbackHelperPort };
  }
  if (process.platform !== 'linux' || options.allowWindowsTestHelper !== undefined
      || typeof options.helperPath !== 'string' || !path.isAbsolute(options.helperPath)) {
    throw new Error('iap_helper_options_invalid');
  }
  try {
    const uid = process.getuid();
    const socket = lstatSync(options.helperPath);
    const parent = lstatSync(path.dirname(options.helperPath));
    if (!socket.isSocket() || socket.isSymbolicLink() || socket.uid !== uid || (socket.mode & 0o077)
        || !parent.isDirectory() || parent.isSymbolicLink() || parent.uid !== uid || (parent.mode & 0o077)) {
      throw new Error();
    }
    let current = path.dirname(options.helperPath);
    for (;;) {
      const stat = lstatSync(current);
      if (!stat.isDirectory() || stat.isSymbolicLink() || ![0, uid].includes(stat.uid)
          || ((stat.mode & 0o022) && !(stat.uid === 0 && (stat.mode & 0o1000)))) throw new Error();
      const next = path.dirname(current);
      if (next === current) break;
      current = next;
    }
  } catch { throw new Error('iap_helper_path_rejected'); }
  return { path: options.helperPath };
}

function validate(options, deferred = false) {
  const { sharedSecret, seed, host, peer, controllerIdentifier, phoneBtMac,
    onAccessoryMessage, windowMs = 120000, ioTimeoutMs = 2000 } = options;
  if (!Buffer.isBuffer(sharedSecret) || sharedSecret.length !== 32
      || (!deferred && (typeof seed !== 'bigint' || seed < 0n || seed > MAX_COUNTER))
      || (deferred && seed !== undefined) || !privateIPv4(host) || !privateIPv4(peer)
      || typeof controllerIdentifier !== 'string' || !/^[A-Za-z0-9_-]{1,128}$/.test(controllerIdentifier)
      || typeof phoneBtMac !== 'string' || !/^(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$/.test(phoneBtMac)
      || typeof onAccessoryMessage !== 'function' || !validWindow(windowMs, options.longRunning, options.daemon)
      || !Number.isInteger(ioTimeoutMs) || ioTimeoutMs < 1 || ioTimeoutMs > 2000) {
    throw new Error('iap_options_invalid');
  }
  return { sharedSecret: Buffer.from(sharedSecret), seed, host, peer, onAccessoryMessage, windowMs, ioTimeoutMs,
    helper: helperAddress(options), header: `tunnel ${controllerIdentifier} ${phoneBtMac}\n` };
}

/** 先连接实际 iAP 状态机，手机下行解密后转交；上行必须由调用者写入真实认证事件通道。 */
export async function createIapReceiver(options) {
  const seed = options.seed;
  validate(options).sharedSecret.fill(0);
  const receiver = await createIapRelay({ ...options, seed: undefined });
  try { await receiver.attachStream(seed); return receiver; }
  catch { receiver.stop('iap_start_failed'); throw new Error('iap_receiver_not_ready'); }
}

/** RECORD 即接通真实 helper；type 130 的 seed 到达后才建立手机下行，不提前猜测派生密钥。 */
export async function createIapRelay(options) {
  const config = validate(options, true);
  let receiver;
  try {
    const { IapTunnel } = await loadPinnedIapTunnel();
    receiver = new ReferenceIapReceiver(config, IapTunnel);
    await receiver.start();
    return receiver;
  } catch {
    receiver?.stop('iap_start_failed');
    config.sharedSecret.fill(0);
    throw new Error('iap_receiver_not_ready');
  }
}

class ReferenceIapReceiver {
  #config;
  #upstream;
  #Tunnel;
  #attaching = false;
  #helper = new net.Socket();
  #server;
  #input;
  #closed = false;
  #ending = false;
  #timers = new Set();
  #partial;
  #blocked;
  #records = Buffer.alloc(0);
  #events = [];
  #eventBytes = 0;
  #eventActive = false;
  #helperClosed = false;
  #abort = new AbortController();
  #attempts = 0;
  #resolve;
  port = null;

  constructor(config, Tunnel) {
    this.#config = config;
    this.#Tunnel = Tunnel;
    this.done = new Promise(resolve => { this.#resolve = resolve; });
    this.#server = net.createServer(socket => this.#accept(socket));
    this.#server.on('error', () => this.stop('iap_listen_failed'));
    this.#helper.on('error', () => this.stop('iap_helper_failed'));
    this.#helper.on('close', () => {
      this.#helperClosed = true;
      if (!this.#eventActive && !this.#events.length) this.stop(this.#ending ? 'iap_input_closed' : 'iap_helper_closed');
    });
    this.#helper.on('data', bytes => this.#queueEvent(bytes));
    this.#helper.on('drain', () => {
      this.#clear(this.#blocked);
      this.#blocked = null;
      if (!this.#closed) this.#input?.resume();
    });
  }

  #timer(reason, delay) {
    const timer = setTimeout(() => { this.#timers.delete(timer); this.stop(reason); }, delay);
    this.#timers.add(timer);
    return timer;
  }

  #clear(timer) { if (timer) { clearTimeout(timer); this.#timers.delete(timer); } }

  async start() {
    const { helper, host, ioTimeoutMs, windowMs, header } = this.#config;
    const timer = this.#timer('iap_helper_connect_timeout', ioTimeoutMs);
    const connected = new Promise(resolve => this.#helper.once('connect', () => resolve(true)));
    this.#helper.connect(helper);
    if (!await Promise.race([connected, this.done.then(() => false)])) throw new Error('iap_helper_unavailable');
    this.#clear(timer);
    this.#helper.setNoDelay(true);
    this.#helper.write(header);
    this.#config.header = null;
    if (windowMs !== 0) this.#timer('iap_window_expired', windowMs);
  }

  /** 单会话只协商一次下行密钥；重复 SETUP 不得归零 nonce 或另连 helper。 */
  async attachStream(seed) {
    if (this.#closed || this.#attaching || typeof seed !== 'bigint' || seed < 0n || seed > MAX_COUNTER) {
      throw new Error('iap_stream_binding_invalid');
    }
    this.#attaching = true;
    this.#upstream = new this.#Tunnel(this.#config.sharedSecret, seed);
    this.#config.sharedSecret.fill(0);
    this.#upstream.on('iap', bytes => this.#writeHelper(bytes));
    const host = this.#config.host;
    const listening = new Promise(resolve => this.#server.listen({ host, port: 0 }, () => {
      if (this.#closed) { this.#server.close(); resolve(false); }
      else resolve(true);
    }));
    if (!await Promise.race([listening, this.done.then(() => false)])) throw new Error('iap_listener_unavailable');
    this.port = this.#server.address().port;
    return this.port;
  }

  #accept(socket) {
    socket.on('error', () => {});
    if (this.#closed || this.#input || canonical(socket.remoteAddress) !== this.#config.peer) {
      socket.destroy();
      if (++this.#attempts > 16) this.stop('iap_peer_budget');
      return;
    }
    this.#input = socket;
    socket.setNoDelay(true);
    this.#server.close(); // 一条派生密钥只接收一条连接，禁止上游重连把 nonce 归零。
    this.#upstream._onConnection(socket);
    socket.removeAllListeners('data');
    socket.on('data', chunk => this.#receive(chunk));
    socket.on('end', () => this.#finishInput());
    socket.on('close', () => { if (!this.#closed && !this.#ending) this.stop('iap_input_aborted'); });
  }

  #receive(chunk) {
    if (this.#closed || this.#ending) return;
    // 每条完整加密记录单独交给上游，避免一次 TCP 合包累积无界明文。
    let offset = 0;
    while (offset < chunk.length && !this.#closed) {
      const needed = this.#records.length < 2 ? 2 : 18 + this.#records.readUInt16LE(0);
      const used = Math.min(needed - this.#records.length, chunk.length - offset);
      this.#records = Buffer.concat([this.#records, chunk.subarray(offset, offset + used)]);
      offset += used;
      if (this.#records.length < 2) break;
      const length = this.#records.readUInt16LE(0);
      if (length < 1 || length > MAX_RECORD || this.#upstream._readCtr > MAX_COUNTER) {
        this.stop('iap_record_budget'); break;
      }
      if (this.#records.length !== length + 18) continue;
      this.#upstream._onData(this.#records);
      this.#records.fill(0);
      this.#records = Buffer.alloc(0);
      if (this.#input.destroyed) { this.stop('iap_record_rejected'); break; }
      if (this.#upstream._plainBuf.length > MAX_PACKAGE + MAX_RECORD) { this.stop('iap_plaintext_budget'); break; }
      this.#updatePartial();
    }
    this.#updatePartial();
  }

  #updatePartial() {
    if (this.#closed) return;
    if (this.#records.length || this.#upstream._plainBuf.length) {
      if (!this.#partial) this.#partial = this.#timer('iap_partial_timeout', this.#config.ioTimeoutMs);
    } else { this.#clear(this.#partial); this.#partial = null; }
  }

  #writeHelper(bytes) {
    if (this.#closed) return;
    if (!Buffer.isBuffer(bytes) || !bytes.length || bytes.length + this.#helper.writableLength > MAX_QUEUE) {
      this.stop('iap_helper_budget'); return;
    }
    if (!this.#helper.write(bytes)) {
      this.#input?.pause();
      if (!this.#blocked) this.#blocked = this.#timer('iap_helper_write_timeout', this.#config.ioTimeoutMs);
    }
  }

  /** 已认证控制命令也可能携带 iAP 字节，必须进入同一个真实 helper 和发送预算。 */
  write(bytes) {
    if (this.#closed) throw new Error('iap_relay_closed');
    this.#writeHelper(bytes);
    if (this.#closed) throw new Error('iap_helper_budget');
  }

  #queueEvent(bytes) {
    if (this.#closed) return;
    if (this.#eventBytes + bytes.length > MAX_QUEUE) { this.stop('iap_event_budget'); return; }
    this.#events.push(Buffer.from(bytes));
    this.#eventBytes += bytes.length;
    this.#helper.pause();
    void this.#flushEvents();
  }

  async #flushEvents() {
    if (this.#eventActive || this.#closed) return;
    this.#eventActive = true;
    while (this.#events.length && !this.#closed) {
      const bytes = this.#events[0];
      const timer = this.#timer('iap_event_write_timeout', this.#config.ioTimeoutMs);
      try {
        const sent = Promise.resolve(this.#config.onAccessoryMessage(bytes, this.#abort.signal)).then(() => true);
        if (!await Promise.race([sent, this.done.then(() => false)])) break;
      } catch { this.stop('iap_event_write_failed'); break; }
      finally { this.#clear(timer); }
      this.#events.shift();
      this.#eventBytes -= bytes.length;
      bytes.fill(0);
    }
    this.#eventActive = false;
    if (!this.#closed) {
      if (this.#helperClosed) this.stop(this.#ending ? 'iap_input_closed' : 'iap_helper_closed');
      else this.#helper.resume();
    }
  }

  #finishInput() {
    if (this.#closed || this.#ending) return;
    if (this.#records.length || this.#upstream._plainBuf.length) { this.stop('iap_truncated_input'); return; }
    this.#ending = true;
    // 正常手机 EOF 先让已经验签的 link 字节进入状态机，不能立即 destroy 丢尾包。
    this.#timer('iap_helper_finish_timeout', this.#config.ioTimeoutMs);
    this.#helper.end();
  }

  /** 关闭自有传输与异步写入；停止后必须新协商密钥，不能恢复旧计数器。 */
  stop(reason = 'iap_local_stop') {
    if (this.#closed) return;
    this.#closed = true;
    this.#abort.abort();
    for (const timer of this.#timers) clearTimeout(timer);
    this.#timers.clear();
    this.#input?.destroy();
    this.#helper.destroy();
    this.#server.close();
    this.#upstream?.stop();
    this.#upstream?._readKey.fill(0);
    this.#upstream?._plainBuf.fill(0);
    this.#upstream?._cipherBuf.fill(0);
    this.#records.fill(0);
    for (const bytes of this.#events) bytes.fill(0);
    this.#events.length = 0;
    this.#eventBytes = 0;
    this.#config.sharedSecret.fill(0);
    this.#config.header = null;
    this.#resolve({ reason });
  }
}
