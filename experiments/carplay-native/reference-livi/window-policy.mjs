// 长测必须显式开启，短测和登记窗口仍保持原来的两分钟边界。
export function validWindow(windowMs, longRunning = false, daemon = false) {
  if (daemon === true) return windowMs === 0;
  return Number.isInteger(windowMs) && windowMs >= 1 && windowMs <= (longRunning === true ? 3600000 : 120000);
}
