// 固定参考模块通过此边界访问独立存储，不伪造 Electron app 或共享原 Rust 登记。
let active = null;
function store() {
  if (!active) throw new Error('livi_store_not_bound');
  return active;
}

/** 同一进程只绑定一个已打开的独立存储，释放后参考模块不能继续读取旧身份。 */
export function bindIdentityStore(value) {
  if (active || !value || ['loadOrCreateIdentity', 'getPairing', 'savePairing'].some((name) => typeof value[name] !== 'function')) {
    throw new Error('livi_store_binding_invalid');
  }
  active = value;
  return () => { if (active === value) active = null; };
}

/** 参考握手读取实际存储，未绑定或持久化失败不能降级成新身份。 */
export function loadOrCreateIdentity() { return store().loadOrCreateIdentity(); }
/** 参考验签查询已登记公钥，不对外输出标识符。 */
export function getPairing(identifier) { return store().getPairing(identifier); }
/** 参考首次握手保存验签结果；是否允许登记由存储的单次授权决定。 */
export function savePairing(identifier, key) { return store().savePairing(identifier, key); }
