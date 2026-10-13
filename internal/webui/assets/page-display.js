/* 在首屏绘制前恢复浏览器自己的显示大小，避免影响设备和其他客户端。 */
(() => {
  "use strict";
  const key = "teslabox-display-scale";
  const normalize = value => value !== null && String(value).trim() !== "" && Number.isFinite(Number(value))
    ? Math.max(50, Math.min(200, Math.round(Number(value)))) : 100;
  let percent = 100;
  try { percent = normalize(localStorage.getItem(key)); } catch (_) { /* 无存储权限时使用标准大小。 */ }
  function apply(value) {
    percent = normalize(value);
    document.documentElement.style.setProperty("--ui-zoom", String(percent / 100));
    try { localStorage.setItem(key, String(percent)); } catch (_) { /* 当前页面仍可调整。 */ }
    return percent;
  }
  // DOM 矩形包含视觉缩放，固定定位和播放器尺寸需要还原到布局坐标。
  function rect(element) {
    const bounds = element.getBoundingClientRect(), scale = percent / 100;
    return Object.fromEntries(["left", "top", "right", "bottom", "width", "height"].map(name => [name, bounds[name] / scale]));
  }
  globalThis.TeslaBoxPageDisplay = {apply, rect, getPercent: () => percent,
    viewport: () => ({width: window.innerWidth / (percent / 100), height: window.innerHeight / (percent / 100)})};
  apply(percent);
})();
