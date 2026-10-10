import fs from 'node:fs';
import path from 'node:path';
import { randomUUID } from 'node:crypto';

const FILE = 'carplay-phone-registry.json';
const MAX_BYTES = 8192, MAX_PHONES = 8;
const fail = () => { throw new Error('reference_phone_registry_invalid'); };
const phone = value => typeof value === 'string' && /^(?:[A-F0-9]{2}:){5}[A-F0-9]{2}$/.test(value);
const controller = value => typeof value === 'string' && /^[A-Za-z0-9_-]{1,128}$/.test(value);
const exact = (value, keys) => value && typeof value === 'object' && !Array.isArray(value)
  && Object.keys(value).length === keys.length && keys.every(key => Object.hasOwn(value, key));
function validate(value) {
  if (!exact(value, ['version', 'phones', 'last_successful']) || value.version !== 1 ||
      !Array.isArray(value.phones) || value.phones.length > MAX_PHONES) fail();
  const bluetooth = new Set(), identifiers = new Set();
  for (const entry of value.phones) {
    if (!exact(entry, ['bluetooth', 'controller']) || !phone(entry.bluetooth) || !controller(entry.controller)
        || bluetooth.has(entry.bluetooth) || identifiers.has(entry.controller)) fail();
    bluetooth.add(entry.bluetooth); identifiers.add(entry.controller);
  }
  if (value.last_successful !== null && !bluetooth.has(value.last_successful)) fail();
  return value;
}

/** 复用已持有身份存储锁的调用者，单独保存手机映射，旧身份文件和回退格式保持兼容。 */
export function openPhoneRegistry({ directory, identityStore, allowInsecureTestPlatform = false }) {
  const unix = process.platform === 'linux';
  if (!path.isAbsolute(directory) || !identityStore || typeof identityStore.getPairing !== 'function'
      || !unix && !allowInsecureTestPlatform) fail();
  const root = fs.lstatSync(directory);
  if (!root.isDirectory() || root.isSymbolicLink() || unix && (root.uid !== process.geteuid() || root.mode & 0o077)) fail();
  const file = path.join(directory, FILE);
  let poisoned = false;
  function read() {
    if (poisoned) fail();
    let fd;
    try {
      const before = fs.lstatSync(file);
      if (before.isSymbolicLink()) fail();
      fd = fs.openSync(file, fs.constants.O_RDONLY | (fs.constants.O_NOFOLLOW ?? 0));
      const stat = fs.fstatSync(fd);
      if (stat.ino !== before.ino || stat.dev !== before.dev || !stat.isFile() || stat.size < 1 || stat.size > MAX_BYTES ||
          unix && (stat.uid !== process.geteuid() || stat.mode & 0o077)) fail();
      const bytes = Buffer.alloc(MAX_BYTES + 1);
      let length = 0;
      while (length < bytes.length) {
        const count = fs.readSync(fd, bytes, length, bytes.length - length, null);
        if (!count) break;
        length += count;
      }
      if (length > MAX_BYTES || bytes.subarray(0, 3).equals(Buffer.from([239, 187, 191]))) fail();
      return validate(JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(bytes.subarray(0, length))));
    } catch (error) {
      if (error.code === 'ENOENT') return { version: 1, phones: [], last_successful: null };
      fail();
    } finally { if (fd !== undefined) fs.closeSync(fd); }
  }
  function trusted(id) {
    if (!controller(id)) fail();
    const key = identityStore.getPairing(id);
    const known = Buffer.isBuffer(key) && key.length === 32;
    key?.fill(0);
    return known;
  }
  function persist(value) {
    validate(value);
    const temporary = path.join(directory, `.phone-registry-${randomUUID()}.tmp`);
    let fd;
    try {
      fd = fs.openSync(temporary, fs.constants.O_WRONLY | fs.constants.O_CREAT | fs.constants.O_EXCL, 0o600);
      fs.writeFileSync(fd, JSON.stringify(value)); fs.fsyncSync(fd); fs.closeSync(fd); fd = undefined;
      fs.renameSync(temporary, file);
      if (unix) {
        const dir = fs.openSync(directory, fs.constants.O_RDONLY | fs.constants.O_DIRECTORY);
        try { fs.fsyncSync(dir); } finally { fs.closeSync(dir); }
      }
    } catch { poisoned = true; fail(); }
    finally {
      if (fd !== undefined) fs.closeSync(fd);
      try { fs.unlinkSync(temporary); } catch (error) { if (error.code !== 'ENOENT') fail(); }
    }
  }
  read();
  return {
    /** 地址不能授予身份，只允许已验签登记且映射到所选蓝牙手机的控制器。 */
    authorizes(id, bluetooth) {
      if (!phone(bluetooth) || !trusted(id)) return false;
      return read().phones.some(entry => entry.bluetooth === bluetooth && entry.controller === id);
    },
    /** 仅供明确授权的登记或单手机旧配置迁移，不在任意入站握手中自动绑定。 */
    bindVerified(id, bluetooth) {
      if (!phone(bluetooth) || !trusted(id)) fail();
      const value = read(), existing = value.phones.find(entry => entry.bluetooth === bluetooth || entry.controller === id);
      if (existing) { if (existing.bluetooth !== bluetooth || existing.controller !== id) fail(); return; }
      if (value.phones.length >= MAX_PHONES) fail();
      persist({ ...value, phones: [...value.phones, { bluetooth, controller: id }] });
    },
    /** 只有本轮认证后的新视频到达才调用；蓝牙成功或开始握手不改变默认手机。 */
    markMediaSuccess(id, bluetooth) {
      if (!this.authorizes(id, bluetooth)) fail();
      const value = read();
      if (value.last_successful !== bluetooth) persist({ ...value, last_successful: bluetooth });
    },
    /** 本轮认证只建立观察器，旧累计帧或断开的来源不能更新成功手机。 */
    mediaSession(id, bluetooth, baseline = null) {
      if (!this.authorizes(id, bluetooth) || baseline !== null && (!Number.isSafeInteger(baseline) || baseline < 0)) fail();
      const registry = this;
      let remembered = false;
      return value => {
        if (!value || !Number.isSafeInteger(value.frames) || value.frames < 0 || typeof value.source_connected !== 'boolean') fail();
        if (remembered) return false;
        if (baseline === null || value.frames < baseline) { baseline = value.frames; return false; }
        if (!value.source_connected || typeof value.codec !== 'string' || !value.codec.length || value.frames <= baseline) return false;
        registry.markMediaSuccess(id, bluetooth); remembered = true; return true;
      };
    },
    /** 明确选择优先，其次上次成功手机；多部首次登记手机不能按列表顺序猜测。 */
    select(requested = '') {
      const value = read();
      const selected = requested || value.last_successful || (value.phones.length === 1 ? value.phones[0].bluetooth : null);
      return selected && value.phones.some(entry => entry.bluetooth === selected && trusted(entry.controller)) ? selected : null;
    },
    /** 给设置入口的快照只含蓝牙标识，不含协议控制器标识或公钥。 */
    snapshot() {
      const value = read();
      return { phones: value.phones.filter(entry => trusted(entry.controller)).map(entry => ({ id: entry.bluetooth })),
        last_successful: value.last_successful };
    }
  };
}
