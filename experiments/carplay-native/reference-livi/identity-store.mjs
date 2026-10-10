// 独立参考身份只写自有目录；坏文件不能被静默重建，也不能把未落盘登记报为成功。
import fs from 'node:fs';
import path from 'node:path';
import { createPrivateKey, createPublicKey, generateKeyPairSync, randomUUID, timingSafeEqual } from 'node:crypto';
import { TextDecoder } from 'node:util';
import { performance } from 'node:perf_hooks';

const FILE = 'livi-reference-state.json';
const LOCK = '.livi-reference-owner';
const MAX_BYTES = 16384;
const MAX_PHONES = 16;
const ED_SEED = Buffer.from('302e020100300506032b657004220420', 'hex');
const decoder = new TextDecoder('utf-8', { fatal: true });
const fail = (code) => { throw new Error(code); };
const hexKey = (value) => typeof value === 'string' && /^[0-9a-f]{64}$/.test(value);

function identifier(value) {
  if (typeof value !== 'string' || value.length === 0 || Buffer.byteLength(value) > 128 ||
      /[\u0000-\u001f\u007f]/u.test(value) || Buffer.from(value).toString('utf8') !== value) {
    fail('livi_store_identifier_invalid');
  }
  return value;
}

function exactKeys(value, keys) {
  return value && typeof value === 'object' && !Array.isArray(value) &&
    Object.keys(value).length === keys.length && keys.every((key) => Object.hasOwn(value, key));
}

function validateState(state) {
  if (!exactKeys(state, ['version', 'identity', 'pairings']) || state.version !== 1 ||
      !exactKeys(state.identity, ['priv', 'pub', 'pi']) ||
      !hexKey(state.identity.priv) || !hexKey(state.identity.pub) ||
      !/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(state.identity.pi) ||
      !Array.isArray(state.pairings) || state.pairings.length > MAX_PHONES) {
    fail('livi_store_state_invalid');
  }
  const seed = Buffer.from(state.identity.priv, 'hex');
  try {
    const key = createPrivateKey({ key: Buffer.concat([ED_SEED, seed]), format: 'der', type: 'pkcs8' });
    const pub = createPublicKey(key).export({ format: 'der', type: 'spki' }).subarray(-32);
    if (!timingSafeEqual(pub, Buffer.from(state.identity.pub, 'hex'))) fail('livi_store_key_mismatch');
  } finally { seed.fill(0); }
  const seen = new Set();
  for (const entry of state.pairings) {
    if (!exactKeys(entry, ['identifier', 'publicKey']) || !hexKey(entry.publicKey)) fail('livi_store_pairing_invalid');
    identifier(entry.identifier);
    if (seen.has(entry.identifier)) fail('livi_store_pairing_duplicate');
    seen.add(entry.identifier);
  }
  return state;
}

/** 打开明确的独立身份目录；首次生成必须由调用者显式授权，Windows 仅允许离线测试。 */
export function openIdentityStore({ directory, initialize = false, allowInsecureTestPlatform = false } = {}) {
  if (typeof directory !== 'string' || !path.isAbsolute(directory)) fail('livi_store_directory_invalid');
  const unix = process.platform === 'linux';
  if (!unix && !allowInsecureTestPlatform) fail('livi_store_platform_unverified');
  const root = path.resolve(directory);
  // 不沿软链接穿入另一个目录；私有父目录内的同 UID 恶意进程不在此边界的防护承诺中。
  for (let current = root; ; current = path.dirname(current)) {
    let stat;
    try { stat = fs.lstatSync(current); } catch { fail('livi_store_directory_unavailable'); }
    if (!stat.isDirectory() || stat.isSymbolicLink()) fail('livi_store_directory_invalid');
    if (unix && current !== root &&
        ((stat.uid !== 0 && stat.uid !== process.geteuid()) ||
         ((stat.mode & 0o022) !== 0 && (stat.mode & 0o1000) === 0))) {
      fail('livi_store_parent_permissions_invalid');
    }
    if (current === path.dirname(current)) break;
  }
  function checkPrivate(stat, directoryEntry = false) {
    if ((directoryEntry ? !stat.isDirectory() : !stat.isFile()) ||
        (unix && (stat.uid !== process.geteuid() || (stat.mode & 0o077) !== 0))) {
      fail('livi_store_permissions_invalid');
    }
  }
  checkPrivate(fs.lstatSync(root), true);
  const file = path.join(root, FILE);
  const lock = path.join(root, LOCK);
  let lockFd;
  try { lockFd = fs.openSync(lock, fs.constants.O_WRONLY | fs.constants.O_CREAT | fs.constants.O_EXCL, 0o600); }
  catch { fail('livi_store_owned_or_lock_unavailable'); }
  const lockStat = fs.fstatSync(lockFd);
  let closed = false;
  let poisoned = false;
  let enrollment = null;
  let state;

  function ready() {
    if (closed || poisoned) fail('livi_store_unavailable');
    try {
      const stat = fs.lstatSync(root);
      checkPrivate(stat, true);
      const current = fs.lstatSync(lock);
      if (current.isSymbolicLink() || current.ino !== lockStat.ino || current.dev !== lockStat.dev) fail('livi_store_owner_changed');
      checkPrivate(current);
    } catch (error) {
      poisoned = true;
      if (error instanceof Error && error.message.startsWith('livi_store_')) throw error;
      fail('livi_store_owner_unavailable');
    }
  }
  function read() {
    let fd;
    try {
      const before = fs.lstatSync(file);
      if (before.isSymbolicLink()) fail('livi_store_file_invalid');
      fd = fs.openSync(file, fs.constants.O_RDONLY | (fs.constants.O_NOFOLLOW ?? 0));
      const stat = fs.fstatSync(fd);
      checkPrivate(stat);
      if (stat.ino !== before.ino || stat.dev !== before.dev || stat.size < 1 || stat.size > MAX_BYTES) fail('livi_store_file_invalid');
      // 即使文件在检查尺寸后发生增长，实际读取也不能超过预算。
      const allocation = Buffer.alloc(MAX_BYTES + 1);
      let length = 0;
      while (length < allocation.length) {
        const count = fs.readSync(fd, allocation, length, allocation.length - length, null);
        if (count === 0) break;
        length += count;
      }
      const bytes = allocation.subarray(0, length);
      try {
        if (bytes.length > MAX_BYTES || bytes.subarray(0, 3).equals(Buffer.from([0xef, 0xbb, 0xbf]))) fail('livi_store_encoding_invalid');
        return validateState(JSON.parse(decoder.decode(bytes)));
      } finally { allocation.fill(0); }
    } catch (error) {
      if (error instanceof Error && error.message.startsWith('livi_store_')) throw error;
      fail('livi_store_read_failed');
    } finally { if (fd !== undefined) fs.closeSync(fd); }
  }
  function persist(next, first = false) {
    ready();
    validateState(next);
    const temp = path.join(root, `.livi-reference-${randomUUID()}.tmp`);
    const bytes = Buffer.from(JSON.stringify(next));
    let fd;
    let renamed = false;
    try {
      if (bytes.length > MAX_BYTES) fail('livi_store_budget_exceeded');
      fd = fs.openSync(temp, fs.constants.O_WRONLY | fs.constants.O_CREAT | fs.constants.O_EXCL, 0o600);
      fs.writeFileSync(fd, bytes);
      fs.fsyncSync(fd);
      fs.closeSync(fd);
      fd = undefined;
      if (first) {
        // 硬链接是原子“不覆盖”创建，避免授权初始化覆盖意外出现的既有身份。
        fs.linkSync(temp, file);
        renamed = true;
        fs.unlinkSync(temp);
      } else {
        read();
        fs.renameSync(temp, file);
        renamed = true;
      }
      if (unix) {
        const dirFd = fs.openSync(root, fs.constants.O_RDONLY | fs.constants.O_DIRECTORY);
        try { fs.fsyncSync(dirFd); } finally { fs.closeSync(dirFd); }
      }
    } catch {
      // rename 后同步失败不能确认是否持久化；禁止本实例继续覆盖或握手。
      if (renamed) poisoned = true;
      fail('livi_store_write_failed');
    } finally {
      bytes.fill(0);
      if (fd !== undefined) fs.closeSync(fd);
      try { fs.unlinkSync(temp); } catch (error) {
        if (error.code !== 'ENOENT') {
          poisoned = true;
          fail('livi_store_cleanup_failed');
        }
      }
    }
  }
  function refresh() {
    ready();
    try {
      const next = read();
      if (next.identity.priv !== state.identity.priv || next.identity.pub !== state.identity.pub ||
          next.identity.pi !== state.identity.pi) fail('livi_store_identity_changed');
      state = next;
    } catch (error) { poisoned = true; throw error; }
  }
  function close() {
    if (closed) return;
    closed = true;
    enrollment = null;
    if (state) state.identity.priv = '';
    try {
      const current = fs.lstatSync(lock);
      if (!current.isSymbolicLink() && current.ino === lockStat.ino && current.dev === lockStat.dev) fs.unlinkSync(lock);
    } catch { fail('livi_store_close_failed');
    } finally { fs.closeSync(lockFd); }
  }
  try {
    let exists = true;
    try { fs.lstatSync(file); } catch (error) { if (error.code === 'ENOENT') exists = false; else throw error; }
    if (!exists) {
      if (!initialize) fail('livi_store_initialization_required');
      const kp = generateKeyPairSync('ed25519');
      const privateBytes = kp.privateKey.export({ type: 'pkcs8', format: 'der' });
      try {
        state = { version: 1, identity: {
          priv: privateBytes.subarray(-32).toString('hex'),
          pub: kp.publicKey.export({ type: 'spki', format: 'der' }).subarray(-32).toString('hex'),
          pi: randomUUID()
        }, pairings: [] };
        persist(state, true);
      } finally { privateBytes.fill(0); }
    }
    state = read();
  } catch (error) { close(); throw error; }

  return {
    /** 返回隔离副本，避免参考会话意外改写持久身份的内存缓存。 */
    loadOrCreateIdentity() {
      refresh();
      return { privRaw: Buffer.from(state.identity.priv, 'hex'), pubRaw: Buffer.from(state.identity.pub, 'hex'),
        pairingId: state.identity.pi, pkHex: state.identity.pub };
    },
    /** 旧单手机迁移只读取登记标识，不对调用者复制密钥。 */
    pairingIdentifiers() { refresh(); return state.pairings.map(entry => entry.identifier); },
    /** 已登记手机可重连；查找不创建文件、不隐式打开登记窗口。 */
    getPairing(id) {
      identifier(id);
      refresh();
      const entry = state.pairings.find((value) => value.identifier === id);
      return entry ? Buffer.from(entry.publicKey, 'hex') : null;
    },
    /** 仅在调用者已经完成验签后保存；仍需当前单次限时登记授权。 */
    savePairing(id, key) {
      identifier(id);
      if (!Buffer.isBuffer(key) || key.length !== 32) fail('livi_store_pairing_invalid');
      refresh();
      const publicKey = key.toString('hex');
      const existing = state.pairings.find((value) => value.identifier === id);
      if (existing) {
        if (existing.publicKey !== publicKey) fail('livi_store_pairing_key_changed');
        return;
      }
      if (!enrollment || performance.now() >= enrollment.deadline ||
          (enrollment.identifier !== null && enrollment.identifier !== id)) fail('livi_store_enrollment_closed');
      if (state.pairings.length >= MAX_PHONES) fail('livi_store_pairing_budget');
      const next = { ...state, pairings: [...state.pairings, { identifier: id, publicKey }] };
      persist(next);
      state = next;
      enrollment = null;
    },
    /** 授权仅允许一个新手机，最多 120 秒；目标 IP 仍须由控制会话入口独立约束。 */
    openEnrollment({ timeoutMs, controllerIdentifier = null } = {}) {
      ready();
      if (!Number.isInteger(timeoutMs) || timeoutMs < 1 || timeoutMs > 120000) fail('livi_store_window_invalid');
      if (controllerIdentifier !== null) identifier(controllerIdentifier);
      if (enrollment) fail('livi_store_window_already_open');
      const lease = {};
      enrollment = { deadline: performance.now() + timeoutMs, identifier: controllerIdentifier, lease };
      return () => { if (enrollment?.lease === lease) enrollment = null; };
    },
    close
  };
}
