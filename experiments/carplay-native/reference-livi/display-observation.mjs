/** 只记录实际报文的显示数值，避免把配置值当成发送证据或泄露身份与正文。 */
export function observeInfoDisplay(body, decodeBplist) {
  if (!Buffer.isBuffer(body) || body.length > 256 * 1024) return null;
  try {
    const info = decodeBplist(body);
    if (!Array.isArray(info?.displays) || info.displays.length !== 1) return null;
    const display = info.displays[0];
    const fields = { width: 'widthPixels', height: 'heightPixels', width_mm: 'widthPhysical',
      height_mm: 'heightPhysical', fps: 'maxFPS' };
    const result = {};
    for (const [name, key] of Object.entries(fields)) {
      const value = display?.[key];
      if (!Number.isSafeInteger(value) || value < 1 || value > 10000) return null;
      result[name] = value;
    }
    return result;
  } catch { return null; }
}
