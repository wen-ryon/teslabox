import net from 'node:net';
import { createPeerPolicy } from './peer-policy.mjs';
import { createHash } from 'node:crypto';
import { createReferenceSession } from './reference-session.mjs';
import { messageSize } from './event-channel.mjs';
import { validatePlistBudget } from './plist-budget.mjs';
import { validWindow } from './window-policy.mjs';
import { observeInfoDisplay } from './display-observation.mjs';

const canonical = value => value?.replace(/^::ffff:/, '');
const privateIPv4 = value => {
  if (net.isIP(value) !== 4) return false;
  const [a, b] = value.split('.').map(Number);
  return a === 10 || a === 127 || a === 172 && b >= 16 && b <= 31 || a === 192 && b === 168;
};
const MAX_BUFFER = 256 * 1024;

/** 实际控制 TCP 入口；固定 CpStack 负责协议响应，握手后立刻切换为真实加密记录。 */
export async function createControlServer(config, options) {
  const { host, peer, port = 0, windowMs = 120000, ioTimeoutMs = 2000, onStage = () => {} } = options;
  const peerPolicy = createPeerPolicy(options);
  if (!privateIPv4(host) || !Number.isInteger(port) || port < 0 || port > 65535
      || !validWindow(windowMs, options.longRunning, options.daemon)
      || !Number.isInteger(ioTimeoutMs) || ioTimeoutMs < 1 || ioTimeoutMs > 2000) throw new Error('control_options_invalid');
  let active = null, activeSession = null, stopped = false, attempts = 0, resolveDone;
  let windowTimer, lastKeyframe = -Infinity;
  const renewableAttempts = options.daemon || options.longRunning;
  let attemptsStarted = performance.now();
  let rejectedPeers = 0;
  const done = new Promise(resolve => { resolveDone = resolve; });
  const server = net.createServer({ allowHalfOpen: true }, socket => {
    socket.on('error', () => socket.destroy());
    const remote = canonical(socket.remoteAddress);
    if (!peerPolicy.allows(remote)) {
      // 只记录有界地址诊断，不放宽来源限制，也不接收或输出未认证正文。
      if (rejectedPeers++ < 8) {
        onStage('control_peer_rejected');
        if (net.isIP(remote) === 4) options.onPeerRejected?.({ actual: remote, expected: peerPolicy.expected });
      }
      socket.destroy(); return;
    }
    if (stopped || active) { socket.destroy(); return; }
    // 常驻进程限制短时间的握手尝试，不能把多次正常重连累计成永久拒绝。
    if (renewableAttempts && performance.now() - attemptsStarted >= ioTimeoutMs * 16) {
      attempts = 0;
      attemptsStarted = performance.now();
    }
    if (++attempts > 16) { socket.destroy(); return; }
    active = socket;
    socket.pause();
    socket.setNoDelay(true);
    // 手机离开无线范围时未必发送 FIN；由 TCP 探测回收旧会话，不能用静止画面的帧间隔判定离线。
    socket.setKeepAlive(true, 10000);
    void run(socket, remote).catch(() => { onStage('control_rejected'); socket.destroy(); });
  });
  const stop = (reason = 'control_local_stop') => {
    if (stopped) return;
    stopped = true;
    clearTimeout(windowTimer);
    active?.destroy();
    server.close();
    resolveDone({ reason });
  };
  server.on('error', () => stop('control_listener_failed'));

  async function run(socket, remote) {
    let session, raw = Buffer.alloc(0), plain = Buffer.alloc(0), cipher = null;
    let processing = false, ended = false, timer = null, failed = false;
    const started = performance.now();
    let observations = 0;
    const observe = (request, response) => {
      if (!options.onObservation || observations++ >= (options.longRunning || options.daemon ? 2048 : 128)) return;
      const routes = ['*', '/', '/info', '/pair-setup', '/pair-verify', '/auth-setup', '/command', '/feedback'];
      const methods = ['OPTIONS', 'GET', 'POST', 'SETUP', 'RECORD', 'TEARDOWN'];
      options.onObservation({ elapsed_ms: Math.round((performance.now() - started) * 1000) / 1000,
        method: methods.includes(request.method) ? request.method : 'other',
        route: routes.includes(request.path) ? request.path : 'other',
        encrypted: cipher !== null, request_bytes: request.body.length,
        status: response.status ?? 200, response_bytes: response.body?.length ?? 0,
        ...(request.path === '/info' && response.body
          ? { info_sha256: createHash('sha256').update(response.body).digest('hex'),
            info_display: observeInfoDisplay(response.body, session.codecs.decodeBplist) } : {}) });
    };
    const fail = reason => { if (failed) return; failed = true; onStage(reason); socket.destroy(); };
    const cleanup = () => {
      clearTimeout(timer);
      session?.stop();
      if (activeSession === session) activeSession = null;
      raw.fill(0); plain.fill(0);
      if (active === socket) active = null;
      options.onSessionClosed?.();
    };
    socket.once('close', cleanup);
    session = await createReferenceSession(config, { ...options, peer: remote, windowMs, ioTimeoutMs, onFailure: fail });
    if (stopped || socket.destroyed) { session.stop(); return; }
    activeSession = session;
    session.stack.on('screen-listening', () => onStage('screen_setup'));
    async function write(bytes) {
      if (bytes.length + socket.writableLength > MAX_BUFFER) throw new Error('control_output_budget');
      await new Promise((resolve, reject) => {
        const deadline = setTimeout(() => { socket.destroy(); reject(new Error('control_write_timeout')); }, ioTimeoutMs);
        const closed = () => { clearTimeout(deadline); reject(new Error('control_closed')); };
        socket.once('close', closed);
        socket.write(bytes, error => {
          clearTimeout(deadline); socket.removeListener('close', closed);
          if (error) reject(error); else resolve();
        });
      });
    }
    async function processInput() {
      if (processing || failed) return;
      processing = true; socket.pause();
      let phase = 'framing';
      try {
        for (;;) {
          phase = 'framing';
          if (socket.destroyed) return;
          if (cipher && raw.length >= 2) {
            const size = raw.readUInt16LE(0);
            if (!size || size > 16384 || cipher.readCtr >= (1n << 64n)) throw new Error('control_record_budget');
            if (raw.length >= size + 18) {
              const result = cipher.decrypt(raw.subarray(0, size + 18));
              if (result.rest.length) throw new Error('control_record_invalid');
              plain = Buffer.concat([plain, result.data]);
              raw = Buffer.from(raw.subarray(size + 18));
              if (plain.length > MAX_BUFFER) throw new Error('control_plaintext_budget');
            }
          }
          const input = cipher ? plain : raw;
          const size = messageSize(input);
          if (size === null || input.length < size) {
            if (cipher && raw.length >= 2 && raw.length >= raw.readUInt16LE(0) + 18) continue;
            break;
          }
          phase = 'parse';
          const { messages, rest } = session.codecs.parseMessages(input.subarray(0, size));
          if (messages.length !== 1 || rest.length) throw new Error('control_message_invalid');
          if (cipher) plain = Buffer.from(plain.subarray(size)); else raw = Buffer.from(raw.subarray(size));
          const request = messages[0];
          if (request.method.startsWith('HTTP/') || request.method.startsWith('RTSP/')) throw new Error('control_response_invalid');
          if (request.body.length && (['SETUP', 'TEARDOWN'].includes(request.method)
              || ['/info', '/command', '/feedback'].some(suffix => request.path.toLowerCase().endsWith(suffix)))) {
            validatePlistBudget(request.body);
          }
          phase = 'handle';
          const response = await session.handle(request);
          // 在成功握手响应与加密会话开放前，先匹配本轮选中的手机身份。
          if (!cipher && session.state.pairVerify.controlKeys && peerPolicy.dynamic) {
            phase = 'controller_authorization';
            if (await options.authorizeController(session.state.pairVerify.controllerId, remote) !== true) {
              throw new Error('control_controller_not_selected');
            }
          }
          phase = 'response';
          const output = session.codecs.buildResponse(request, response);
          await write(cipher ? cipher.encrypt(output) : output);
          observe(request, response);
          if (!cipher && session.state.pairVerify.controlKeys) {
            const keys = session.state.pairVerify.controlKeys;
            cipher = new session.codecs.ControlCipher(keys.readKey, keys.writeKey);
            phase = 'verified';
            await options.onVerified?.(session.state.pairVerify.controllerId, remote);
            if (renewableAttempts) {
              attempts = 0;
              attemptsStarted = performance.now();
            }
            onStage('pair_verified');
          }
          phase = 'after_response';
          await session.afterResponse();
          if (request.method === 'RECORD' && (response.status ?? 200) === 200) onStage('record_started');
          // TEARDOWN 响应写完即释放本地连接，不等待手机的 FIN 才归还单会话入口。
          if (session.closed) { socket.destroySoon(); return; }
        }
        if (ended) {
          if (raw.length || plain.length) throw new Error('control_truncated_input');
          socket.end();
        } else {
          if (raw.length || plain.length) {
            timer ??= setTimeout(() => fail('control_partial_timeout'), ioTimeoutMs);
          } else { clearTimeout(timer); timer = null; }
          socket.resume();
        }
      } catch (error) {
        // 仅记录固定阶段、异常类型和源码位置，不输出请求正文、身份或任意异常消息。
        const kind = error instanceof TypeError ? 'type' : error instanceof RangeError ? 'range' : 'other';
        const location = /\/(\w[\w-]*\.(?:mjs|ts)):(\d+):\d+/.exec(String(error?.stack ?? ''));
        const source = location ? `${location[1].replace('.', '_')}_L${location[2]}` : 'unknown';
        onStage(`control_error_${phase}_${kind}_${source}`);
        fail('control_request_failed');
      }
      finally { processing = false; }
    }
    socket.on('data', bytes => {
      if (raw.length + bytes.length > MAX_BUFFER) { fail('control_input_budget'); return; }
      raw = Buffer.concat([raw, bytes]);
      void processInput();
    });
    socket.on('end', () => { ended = true; void processInput(); });
    onStage('control_connected');
    socket.resume();
  }
  try {
    await new Promise((resolve, reject) => {
      server.once('error', reject);
      server.listen(port, host, () => { server.removeListener('error', reject); resolve(); });
    });
    if (windowMs !== 0) windowTimer = setTimeout(() => stop('control_window_expired'), windowMs);
    return { port: server.address().port, done, stop,
      requestKeyframe() {
        if (!activeSession?.state.pairVerify.verified || activeSession.closed
            || !activeSession.state.screen || !activeSession.state.mainStreamReady || performance.now() - lastKeyframe < 1000) return false;
        lastKeyframe = performance.now();
        activeSession.stack.forceMainKeyframe(); return true;
      },
      sendTouches(contacts) {
        if (!activeSession?.state.pairVerify.verified || !activeSession.stack._liveSession || activeSession.closed) return false;
        activeSession.stack.sendTouches(contacts); return true;
      } };
  } catch { stop('control_listener_failed'); throw new Error('control_listener_failed'); }
}
