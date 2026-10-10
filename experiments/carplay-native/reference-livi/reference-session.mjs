import dgram from 'node:dgram';
import { loadPinnedStack, loadPinnedEventCodecs, loadPinnedPairSetup, loadPinnedPairVerify, loadPinnedHid } from './pinned-auth-loader.mjs';
import { createSessionAutoplay } from './media-autoplay.mjs';
import { createEventChannel } from './event-channel.mjs';
import { createScreenReceiver } from './screen-receiver.mjs';
import { clearNativeObservers } from './native-boundary.mjs';
import { requestHostUI } from './host-ui.mjs';

const IAP_UUID = 'E9459FD0-BCAD-4C45-820F-1E72447EF2F2';
const CONTENT_TYPE = 'application/x-apple-binary-plist';

/** 给固定时钟算法绑定实际 AP 地址及唯一手机，避免上游默认监听所有接口。 */
async function bindTiming(TimingSync, options, peerPort) {
  const timing = new TimingSync();
  const socket = dgram.createSocket('udp4');
  timing._sock = socket;
  socket.on('message', (bytes, remote) => {
    if (remote.address === options.peer && (!peerPort || remote.port === peerPort) && bytes.length === 32) {
      timing._onMessage(bytes, remote);
    }
  });
  socket.on('error', () => options.onFailure('timing_socket_failed'));
  try {
    await new Promise((resolve, reject) => {
      socket.once('error', reject);
      socket.bind(0, options.host, () => { socket.removeListener('error', reject); resolve(); });
    });
    if (peerPort) timing.start(options.peer, peerPort);
    return { timing, port: socket.address().port };
  } catch { timing.stop(); throw new Error('timing_not_ready'); }
}

/** 同一个真实握手会话调用固定 CpStack 控制处理器；端口与媒体消费者由明确适配层拥有。 */
export async function createReferenceSession(config, options) {
  if (config.disableAudioOutput !== true || config.hevc || !config.h264 || config.cluster
      || typeof options.onFailure !== 'function') throw new Error('reference_configuration_unsupported');
  const [{ CpStack, TimingSync }, codecs, { PairSetup }, { PairVerify }] = await Promise.all([
    loadPinnedStack(), loadPinnedEventCodecs(), loadPinnedPairSetup(), loadPinnedPairVerify()
  ]);
  let closed = false;
  let established = false;
  let recorded = false;
  let event = null;
  let relay = null;
  let relayReady = null;
  let keepalive = null;
  const { MEDIA_HID_UID, mediaReport } = await loadPinnedHid();
  const autoplay = createSessionAutoplay(config.autoPlay === true, index => {
    if (closed || !event || !state.pairVerify.verified) throw new Error('media_not_ready');
    return event.sendCommand(codecs.encodeBplist({ type: 'hidSendReport',
      uuid: MEDIA_HID_UID.toString(16), hidReport: mediaReport(index) }));
  }, options.onStage);
  const retired = new WeakSet();
  const state = {
    pairSetup: new PairSetup(), pairVerify: new PairVerify(), cipher: null, encBuf: Buffer.alloc(0),
    peerHost: options.peer, deviceBtMac: '', timing: null, keepAlive: null, screen: null,
    clusterScreen: null, screenNativeId: null, clusterScreenNativeId: null,
    screenInProcess: false, clusterScreenInProcess: false, clusterCodecEmitted: false,
    audioMeta: [], iapTunnel: null, iapRelay: null, event: null, eventSock: null,
    eventCipher: null, eventCseq: 0, codecEmitted: false, mainStreamReady: false,
    lastCtrlReadNs: process.hrtime.bigint(), heartbeat: null
  };
  const watch = resource => {
    void resource.done.then(({ reason }) => { if (!closed && !retired.has(resource)) options.onFailure(reason); });
    return resource;
  };
  const alive = resource => {
    if (closed) { resource.stop(); throw new Error('reference_session_closed'); }
    return watch(resource);
  };
  class ReferenceStack extends CpStack {
    async _handleSetup(req, session) {
      const dict = codecs.decodeBplist(req.body);
      if (!dict || typeof dict !== 'object' || Array.isArray(dict) || Buffer.isBuffer(dict)) return { status: 400 };
      if (Array.isArray(dict.streams)) {
        if (!established || dict.streams.length > 2 || !dict.streams.length) return { status: 455 };
        const types = dict.streams.map(stream => Number(stream?.type));
        if (new Set(types).size !== types.length || types.some(type => ![110, 130].includes(type))) return { status: 461 };
        if (types.includes(110) && session.screen || types.includes(130) && session.iapTunnel) return { status: 455 };
        return super._handleSetup(req, session);
      }
      if (established) return { status: 455 };
      const peerPort = Number(dict.timingPort ?? 0);
      if (!Number.isInteger(peerPort) || peerPort < 0 || peerPort > 65535) return { status: 400 };
      session.deviceBtMac = typeof dict.deviceID === 'string' ? dict.deviceID : '';
      const timing = await bindTiming(TimingSync, options, peerPort);
      if (closed) { timing.timing.stop(); throw new Error('reference_session_closed'); }
      session.timing = timing.timing;
      event = alive(await createEventChannel({ ...options, sharedSecret: session.pairVerify.sharedSecret,
        // 固定参考仅确认入站事件；保留相同响应语义，不输出原始正文。
        onRequest: async () => 200 }));
      this._active = session;
      // 能力声明必须与实际显示字典一致，经典显示不声明没有提供的视区。
      const response = { timingPort: timing.port, eventPort: event.port,
        enabledFeatures: ['iAPChannel', ...(config.main.viewArea ? ['viewAreas'] : [])] };
      if (dict.keepAliveLowPower) {
        keepalive = dgram.createSocket('udp4');
        keepalive.on('error', () => options.onFailure('keepalive_socket_failed'));
        await new Promise((resolve, reject) => {
          keepalive.once('error', reject);
          keepalive.bind(0, options.host, () => { keepalive.removeListener('error', reject); resolve(); });
        });
        if (closed) { try { keepalive.close(); } catch {} throw new Error('reference_session_closed'); }
        response.keepAlivePort = keepalive.address().port;
      }
      established = true;
      return { headers: { 'Content-Type': CONTENT_TYPE }, body: codecs.encodeBplist(response) };
    }

    _openIapMessageRelay(session) {
      if (relayReady) return;
      relayReady = event.createIapRelay({ ...options, controllerIdentifier: session.pairVerify.controllerId,
        phoneBtMac: session.deviceBtMac || config.phoneBtMac?.() || '' }).then(value => {
        relay = alive(value);
        session.iapRelay = { write: bytes => relay.write(bytes), destroy: () => relay.stop() };
      });
      // 上游 RECORD 调用是同步的；公共 handle 会等待实际连接，失败不能变成未处理拒绝。
      void relayReady.catch(() => options.onFailure('iap_relay_not_ready'));
    }

    async _setupDataStream(sd, session) {
      if (String(sd.clientTypeUUID ?? '').toUpperCase() !== IAP_UUID || !recorded) throw new Error('iap_stream_not_negotiated');
      await relayReady;
      if (!relay || closed) throw new Error('iap_relay_not_ready');
      const seed = sd.seed;
      if (typeof seed !== 'bigint' && !(Number.isSafeInteger(seed) && seed >= 0)) throw new Error('iap_seed_invalid');
      const port = await relay.attachStream(BigInt(seed));
      session.iapTunnel = relay;
      return { type: 130, streamID: 1, dataPort: port };
    }

    async _setupScreen(sd, session, cluster = false) {
      if (cluster || session.screen) throw new Error('screen_stream_unsupported');
      const id = sd.streamConnectionID;
      if (typeof id !== 'bigint' && !(Number.isSafeInteger(id) && id >= 0)) throw new Error('screen_id_invalid');
      const key = codecs.hkdfSha512(session.pairVerify.sharedSecret, `DataStream-Salt${id}`, 'DataStream-Output-Encryption-Key', 32);
      try {
        session.screen = alive(await createScreenReceiver({ ...options, key,
          onFirstFrame: () => { void autoplay.firstFrame(); },
          onCodecConfig: bytes => { session.mainStreamReady = true; options.onCodecConfig?.(bytes); } }));
        // 监听就绪只记录 SETUP；首帧由真实媒体消费者确认。
        this.emit('screen-listening');
        return session.screen.port;
      } finally { key.fill(0); }
    }

    _sendEventCommand(_session, body) {
      if (!event) throw new Error('event_not_ready');
      void event.sendCommand(body).catch(() => options.onFailure('event_command_failed'));
    }

    _handleTeardown(req, session) {
      const body = req.body.length ? codecs.decodeBplist(req.body) : {};
      const streams = body.streams;
      if (!Array.isArray(streams) || streams.length === 0) { this._teardown(session); return { status: 200 }; }
      for (const stream of streams) {
        if (stream.type === 110 && session.screen) { const screen = session.screen; session.screen = null; retired.add(screen); screen.stop(); }
        else if (stream.type === 130 && relay) { retired.add(relay); relay.stop(); session.iapTunnel = null; }
      }
      return { status: 200 };
    }

    _teardown(session) {
      if (closed) return;
      closed = true;
      autoplay.stop();
      event?.stop();
      relay?.stop();
      try { keepalive?.close(); } catch {}
      super._teardown(session);
      this._active = null;
      this._liveSession = null;
      clearNativeObservers();
    }
  }
  const stack = new ReferenceStack(config);
  let releaseRequested = false;
  stack.on('disable-bluetooth', mac => {
    if (!options.phoneBtMac || mac.toUpperCase() !== options.phoneBtMac.toUpperCase()) {
      throw new Error('reference_bluetooth_target_invalid');
    }
    if (typeof options.releaseBluetooth !== 'function') throw new Error('reference_bluetooth_release_unavailable');
    releaseRequested = true;
  });
  return {
    stack,
    state,
    codecs,
    get closed() { return closed; },
    async afterResponse() {
      // 手机可能先发视频再 RECORD；两个条件都满足且响应已写出后才播放。
      if (recorded) void autoplay.recordReady();
      if (releaseRequested) {
        releaseRequested = false;
        await options.releaseBluetooth(state.pairVerify.controllerId);
      }
    },
    async handle(req) {
      if (closed) throw new Error('reference_session_closed');
      const path = req.path.toLowerCase();
      const handshake = ['/pair-setup', '/pair-verify', '/auth-setup', '/info'].some(suffix => path.endsWith(suffix));
      if (!state.pairVerify.verified && !handshake && req.method !== 'OPTIONS') return { status: 470 };
      if (state.pairVerify.verified && ['/pair-setup', '/pair-verify'].some(suffix => path.endsWith(suffix))) return { status: 455 };
      if (req.method === 'RECORD' && (!established || recorded)) return { status: 455 };
      if (req.method==='POST' && path==='/command' && req.body.length<=4096 && state.pairVerify.verified) {
        const body=codecs.decodeBplist(req.body);
        if(body?.type==='requestUI'){
          if(closed)return {status:455};
          return {status:await requestHostUI()?200:503};
        }
      }
      try {
        const response = await stack._handle(req, state);
        if (req.method === 'RECORD') { await relayReady; recorded = true; }
        return response;
      } catch { stack._teardown(state); throw new Error('reference_request_failed'); }
    },
    stop() { stack._teardown(state); }
  };
}
