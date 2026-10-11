import fs from 'node:fs';
import { randomUUID } from 'node:crypto';
import path from 'node:path';
import net from 'node:net';
import { createTouchInput } from './control-input.mjs';
import { parseArgs } from 'node:util';
import { spawn } from 'node:child_process';
import { createInterface } from 'node:readline';
import { MfiOracleSigner } from './mfi-oracle.mjs';
import { openPhoneRegistry } from './phone-registry.mjs';
import { openIdentityStore } from './identity-store.mjs';
import { bindIdentityStore } from './identity-binding.mjs';
import { createControlServer } from './control-server.mjs';
import { loadPinnedStack, loadPinnedEventCodecs } from './pinned-auth-loader.mjs';
import { validWindow } from './window-policy.mjs';
import { superviseReference } from './reference-supervisor.mjs';
import { teslaBoxBranding } from './oem-branding.mjs';
import { splitAutoPlayProfile } from './media-autoplay.mjs';
import { CODEC_DIAGNOSTIC_FILE, writeCodecDiagnostic } from './codec-diagnostic.mjs';

const { values } = parseArgs({ options: {
  binary: { type: 'string' }, 'hardware-config': { type: 'string' }, profile: { type: 'string' },
  host: { type: 'string' }, peer: { type: 'string' }, 'peer-network': { type: 'string' },
  'legacy-phone-bt': { type: 'string' }, 'phone-bt': { type: 'string' },
  'identity-directory': { type: 'string' }, seconds: { type: 'string', default: '120' },
  enroll: { type: 'boolean', default: false },
  'long-running': { type: 'boolean', default: false },
  'session-child': { type: 'boolean', default: false },
  daemon: { type: 'boolean', default: false },
  'auto-connect': { type: 'boolean', default: false },
  'classic-display': { type: 'boolean', default: false },
  'mfi-protocol': { type: 'string', default: '2' }
} });
if ((values['long-running'] || values.daemon) && !values['session-child']) {
  const seconds = Number(values.seconds);
  if (process.platform !== 'linux' || values.enroll || values.daemon && values['long-running']
      || !validWindow(values.daemon ? 0 : seconds * 1000, true, values.daemon)) {
    throw new Error('reference_long_run_invalid');
  }
  const abort = new AbortController();
  for (const name of ['SIGINT', 'SIGTERM', 'SIGHUP']) process.on(name, () => abort.abort());
  const result = await superviseReference({ file: process.execPath,
    durationMs: values.daemon ? null : seconds * 1000, daemon: values.daemon,
    signal: abort.signal, output: line => process.stderr.write(`${line}\n`),
    args: remaining => {
      const args = [process.argv[1]];
      for (const [key, value] of Object.entries(values)) {
        if (key === 'seconds' || key === 'session-child' || value === false) continue;
        args.push(`--${key}`); if (typeof value !== 'boolean') args.push(value);
      }
      return [...args, '--session-child', ...(values.daemon ? [] : ['--seconds', String(remaining)])];
    } });
  process.stderr.write(`REFERENCE_STAGE supervisor_${result.reason}\n`);
  process.exit(['expired', 'stopped'].includes(result.reason) ? 0 : 1);
}
const children = [], sockets = new Set();
let control, touch, store, phoneRegistry, unbind, temporary, timer, stopping = false;
let observedMediaFrames = null, phoneMediaObserver = null;
let finish;
const finished = new Promise(resolve => { finish = resolve; });
const mediaAbort = new AbortController();
const stage = name => process.stderr.write(`REFERENCE_STAGE ${name}\n`);
const stop = () => { if (!stopping) { stopping = true; finish(); } };
for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) process.on(signal, stop);

function start(args, readyMarker) {
  const child = spawn(values.binary, args, { stdio: ['ignore', 'pipe', 'pipe'] });
  const exited = new Promise(resolve => { child.once('error', () => resolve(-1)); child.once('exit', resolve); });
  const lines = [createInterface({ input: child.stdout }), createInterface({ input: child.stderr })];
  let ready;
  const started = new Promise(resolve => { ready = resolve; });
  for (const reader of lines) reader.on('line', line => {
    // 保留内部诊断到调用者私有日志；公开窗口只使用单独的阶段白名单。
    process.stderr.write(`${line}\n`);
    if (line.includes(readyMarker)) ready(true);
  });
  void exited.then(() => { ready(false); if (!stopping) { stage('child_exited'); process.exitCode = 1; stop(); } });
  const entry = { child, exited, lines };
  children.push(entry);
  return { ...entry, async ready() {
    let deadline;
    const ok = await Promise.race([started, new Promise(resolve => { deadline = setTimeout(() => resolve(false), 12000); }), finished.then(() => false)]);
    clearTimeout(deadline); if (!ok) throw new Error('reference_child_not_ready');
  } };
}

async function helperCommand(line) {
  if (!/^(?:claim [A-Za-z0-9_-]{1,128} (?:[A-Fa-f0-9]{2}:){5}[A-Fa-f0-9]{2}|release [A-Za-z0-9_-]{1,128})$/.test(line)) {
    throw new Error('reference_helper_command_invalid');
  }
  const socket = net.connect(path.join(temporary, 'helper.sock'));
  sockets.add(socket);
  try {
    await new Promise((resolve, reject) => {
      let bytes = '';
      const deadline = setTimeout(() => reject(new Error('reference_helper_timeout')), 2500);
      const done = (error) => { clearTimeout(deadline); if (error) reject(error); else resolve(); };
      socket.once('error', () => done(new Error('reference_helper_failed')));
      socket.once('connect', () => socket.write(`${line}\n`));
      socket.on('data', data => {
        bytes += data.toString('ascii');
        if (bytes === 'OK\n') done();
        else if (bytes.length >= 3) done(new Error('reference_helper_rejected'));
      });
      socket.once('end', () => { if (bytes !== 'OK\n') done(new Error('reference_helper_eof')); });
    });
  } finally { socket.destroy(); sockets.delete(socket); }
}

async function observeMedia(tokenFile) {
  const token = fs.readFileSync(tokenFile, 'utf8').trim();
  if (!/^[a-f0-9]{64}$/i.test(token)) throw new Error('reference_bridge_token_invalid');
  let previous = '';
  while (!stopping) {
    try {
      const response = await fetch('http://127.0.0.1:8767/api/status', {
        headers: { Cookie: `tbcp=${token}` }, signal: AbortSignal.any([mediaAbort.signal, AbortSignal.timeout(1000)]) });
      if (!response.ok) throw new Error('reference_bridge_status_failed');
      const text = await response.text();
      if (text.length > 4096) throw new Error('reference_bridge_status_budget');
      const value = JSON.parse(text);
      if (![value.frames, value.input_bytes].every(n => Number.isSafeInteger(n) && n >= 0)
          || typeof value.source_connected !== 'boolean') throw new Error('reference_bridge_status_invalid');
      // 桥接累计计数可能来自旧会话，必须先建立本轮基线，再等新视频增长。
      observedMediaFrames = value.frames;
      if (phoneMediaObserver?.(value)) stage('phone_media_success');
      const line = `REFERENCE_MEDIA frames=${value.frames} input_bytes=${value.input_bytes} source_connected=${value.source_connected} codec_ready=${typeof value.codec === 'string'}`;
      if (!stopping && line !== previous) { process.stderr.write(`${line}\n`); previous = line; }
    } catch { if (!stopping) { stage('bridge_status_failed'); process.exitCode = 1; stop(); } }
    await Promise.race([new Promise(resolve => setTimeout(resolve, 1000)), finished]);
  }
}

try {
  if (process.platform !== 'linux') throw new Error('reference_requires_linux');
  const seconds = values.daemon ? 0 : Number(values.seconds);
  if (!Number.isInteger(seconds) || !validWindow(seconds * 1000, values['long-running'], values.daemon)
      || values.daemon && values['long-running']
      || values.enroll && (values['long-running'] || values.daemon)) throw new Error('reference_window_invalid');
  for (const key of ['binary', 'hardware-config', 'profile', 'identity-directory']) {
    if (!values[key] || !path.isAbsolute(values[key])) throw new Error('reference_path_invalid');
  }
  const { profile, autoPlay } = splitAutoPlayProfile(JSON.parse(fs.readFileSync(values.profile, 'utf8')));
  const hardware = JSON.parse(fs.readFileSync(values['hardware-config'], 'utf8'));
  const oracle = new MfiOracleSigner({ port: Number(hardware.oracle_listen.split(':').at(-1)),
    expectedMajor: Number(values['mfi-protocol']) });
  if (!hardware.oracle_listen.startsWith('127.0.0.1:')) throw new Error('reference_oracle_invalid');
  await oracle.protocolMajor();
  if (values.enroll && !fs.existsSync(values['identity-directory'])) fs.mkdirSync(values['identity-directory'], { mode: 0o700 });
  store = openIdentityStore({ directory: values['identity-directory'], initialize: values.enroll });
  if (values.enroll) store.openEnrollment({ timeoutMs: seconds * 1000 });
  if (values['peer-network']) {
    phoneRegistry = openPhoneRegistry({ directory: values['identity-directory'], identityStore: store });
    if (values['legacy-phone-bt'] && phoneRegistry.snapshot().phones.length === 0) {
      const identifiers = store.pairingIdentifiers();
      // 仅迁移旧配置明确绑定的唯一已登记手机，不在多手机场景猜测对应关系。
      if (identifiers.length !== 1 || values['legacy-phone-bt'] !== values['phone-bt']) {
        throw new Error('reference_phone_migration_requires_selection');
      }
      phoneRegistry.bindVerified(identifiers[0], values['legacy-phone-bt']);
    }
    if (!values.enroll && phoneRegistry.select(values['phone-bt']) !== values['phone-bt']) {
      throw new Error('reference_phone_not_registered');
    }
  } else if (values['legacy-phone-bt']) throw new Error('reference_phone_mode_invalid');
  unbind = bindIdentityStore(store);
  temporary = fs.mkdtempSync(path.join(path.dirname(values['identity-directory']), 'reference-run-'));
  fs.chmodSync(temporary, 0o700);
  const config = { deviceName: profile.name, deviceId: profile.device_id, btMac: profile.bluetooth_id,
    sourceVersion: '950.7.1', h264: true, hevc: false, disableAudioOutput: true,
    main: { widthPixels: profile.display.width, heightPixels: profile.display.height,
      widthPhysicalMm: profile.display.width_mm, heightPhysicalMm: profile.display.height_mm,
      // 发送的是绝对坐标触摸屏报告，避免手机按触摸板选择输入行为。
      fps: profile.display.fps, primaryInputDevice: 1,
      // 经典显示仅供固定像素的密度对照；默认继续使用完整视区。
      ...(values['classic-display'] ? {} : { viewArea: { top: 0, bottom: 0, left: 0, right: 0 } }) },
    rightHandDrive: profile.right_hand_drive, entertainmentSampleRate: 44100, autoPlay,
    ...teslaBoxBranding(), mfi: oracle, phoneBtMac: () => values['phone-bt'] };
  const ingest = start(['screen-ingest', '--listen', '127.0.0.1:9002', '--bridge', '127.0.0.1:9001', '--bridge-format', 'timestamped'], '已解密屏幕包实验输入');
  await ingest.ready();
  let codecSaved = false;
  control = await createControlServer(config, { host: values.host, peer: values.peer, peerNetwork: values['peer-network'], port: 7000,
    ...(phoneRegistry ? { authorizeController(id) {
      if (values.enroll && !phoneRegistry.authorizes(id, values['phone-bt'])) {
        phoneRegistry.bindVerified(id, values['phone-bt']);
      }
      return phoneRegistry.authorizes(id, values['phone-bt']);
    } } : {}),
    windowMs: seconds * 1000, longRunning: values['long-running'], daemon: values.daemon,
    ingestPort: 9002, helperPath: path.join(temporary, 'helper.sock'),
    onSessionClosed: () => { if (values['long-running'] || values.daemon) { stage('session_closed'); stop(); } },
    phoneBtMac: values['phone-bt'], onStage: stage,
    onPeerRejected: value => process.stderr.write(`REFERENCE_PEER actual=${value.actual} expected=${value.expected}\n`),
    onCodecConfig: bytes => {
      if (codecSaved || !Buffer.isBuffer(bytes) || bytes.length > 65536) return;
      codecSaved = true;
      // 仅保存首个编码配置，供生产解析器离线复现；不保存视频、音频或密钥。
      try {
        writeCodecDiagnostic(bytes, { directory: temporary, runtimeDirectory: path.dirname(values.binary) });
        stage('screen_codec_saved');
      } catch { stage('screen_codec_save_failed'); }
    },
    onObservation: value => process.stderr.write(`REFERENCE_CONTROL ${JSON.stringify(value)}\n`),
    onVerified: async id => {
      await helperCommand(`claim ${id} ${values['phone-bt']}`);
      if (phoneRegistry) phoneMediaObserver = phoneRegistry.mediaSession(id, values['phone-bt'], observedMediaFrames);
    },
    releaseBluetooth: id => helperCommand(`release ${id}`) });
  const identity = store.loadOrCreateIdentity();
  identity.privRaw.fill(0);
  const { buildInfoPlist } = await loadPinnedStack();
  const { encodeBplist } = await loadPinnedEventCodecs();
  const manifest = { control: `${values.host}:${control.port}`,
    profile: { ...profile, source_version: config.sourceVersion },
    pairing_identifier: identity.pairingId, public_key: [...identity.pubRaw],
    information: [...encodeBplist(buildInfoPlist(config))], phone_bluetooth: values['phone-bt'] };
  const manifestFile = path.join(temporary, 'manifest.json');
  fs.writeFileSync(manifestFile, JSON.stringify(manifest), { flag: 'wx', mode: 0o600 });
  const runtime = start(['--config', values['hardware-config'], 'reference-runtime', '--manifest', manifestFile,
    '--socket', path.join(temporary, 'helper.sock'),
    ...(values.daemon ? ['--daemon'] : ['--seconds', String(seconds)]),
    ...(values['long-running'] ? ['--long-running'] : []),
    ...(values['auto-connect'] ? ['--auto-connect'] : [])], '无线 iAP2 服务已登记');
  await runtime.ready();
  const bindTouchInput = createTouchInput(contacts => control.sendTouches(contacts), () => control.requestKeyframe());
  touch = net.createServer(socket => {
    sockets.add(socket);
    socket.on('close', () => sockets.delete(socket));
    bindTouchInput(socket);
  });
  touch.on('error', stop);
  await new Promise((resolve, reject) => { touch.once('error', reject); touch.listen(9110, '127.0.0.1', resolve); });
  stage('control_ready');
  void observeMedia(path.join(path.dirname(values.profile), 'browser-token')).catch(() => { stage('bridge_status_failed'); process.exitCode = 1; stop(); });
  if (!values.daemon) timer = setTimeout(stop, seconds * 1000);
  void control.done.then(result => { if (result.reason === 'control_window_expired') stage('window_expired'); stop(); });
  await finished;
} catch (error) {
  stage('startup_or_runtime_failed');
  // 固定本地错误码可用于诊断，未知异常不展开路径、身份或正文。
  if (error instanceof Error && /^(?:reference|livi_store|mfi)_[a-z_]+$/.test(error.message)) stage(error.message);
  process.exitCode = 1;
} finally {
  stop(); mediaAbort.abort(); clearTimeout(timer); control?.stop(); touch?.close();
  for (const socket of sockets) socket.destroy();
  for (const entry of children.reverse()) {
    if (entry.child.exitCode === null) entry.child.kill('SIGTERM');
    const guard = setTimeout(() => entry.child.kill('SIGKILL'), 5000);
    await entry.exited; clearTimeout(guard); for (const line of entry.lines) line.close();
  }
  unbind?.(); store?.close();
  if (temporary) {
    for (const file of ['manifest.json', 'helper.sock', CODEC_DIAGNOSTIC_FILE]) {
      try { fs.unlinkSync(path.join(temporary, file)); } catch (error) { if (error.code !== 'ENOENT') process.exitCode = 1; }
    }
    try { fs.rmdirSync(temporary); } catch { process.exitCode = 1; }
  }
  stage('owned_resources_stopped');
}
