/* 只展示现有连通性探测证据，不发起额外请求或修改网络。 */
(() => {
  "use strict";
  const t = text => globalThis.TeslaBoxI18n.t(text);
  let initialized = false, pinned = false;
  const byID = id => document.getElementById(id);
  function show(visible) {
    const panel = byID("internet-egress"), button = byID("internet-indicator");
    if (!panel || !button) return;
    panel.hidden = !visible;
    button.setAttribute("aria-expanded", String(visible));
  }
  function render(system) {
    const panel = byID("internet-egress"), button = byID("internet-indicator");
    if (!panel || !button) return;
    panel.replaceChildren();
    const row = (tag, text) => { const node = document.createElement(tag); node.textContent = text; panel.appendChild(node); };
    row("strong", t("互联网出口"));
    const source = system?.internet_available === true ? system.internet_egress : null;
    const kinds = {ethernet:t("有线网络"), wifi:"Wi-Fi", usb:t("USB 网络"), cellular:t("蜂窝网络"), bluetooth:t("蓝牙网络共享")};
    const summary = source?.interface ? `${kinds[source.kind] || t("类型未知")} · ${source.interface}` : t(system?.internet_available === true ? "出口信息不可用" : "未确认可用出口");
    row("p", `${t("最近探测出口")}：${summary}`);
    if (source?.local_ip) row("p", `${t("本机地址")}：${source.local_ip}${source.family === "ipv4" ? " (IPv4)" : source.family === "ipv6" ? " (IPv6)" : ""}`);
    const checked = new Date(system?.internet_checked_at || "");
    row("p", `${t("检测时间")}：${Number.isFinite(checked.getTime()) ? checked.toLocaleString() : t("未检测")}`);
    row("small", t("显示最近一次连通性探测的出口，约每分钟更新；不同目标或 IPv4/IPv6 的出口可能不同。"));
    button.setAttribute("aria-label", `${t("互联网状态")} · ${summary}`);
  }
  function unavailable() { render(null); }
  function initialize() {
    const button = byID("internet-indicator"), wrapper = button?.parentElement;
    if (initialized || !button || !wrapper) return;
    initialized = true;
    wrapper.addEventListener("pointerenter", () => show(true));
    wrapper.addEventListener("pointerleave", () => { if (!pinned && !wrapper.contains(document.activeElement)) show(false); });
    wrapper.addEventListener("focusin", () => show(true));
    wrapper.addEventListener("focusout", event => { if (!wrapper.contains(event.relatedTarget)) { pinned = false; show(false); } });
    button.addEventListener("click", () => { pinned = !pinned; show(pinned); });
    document.addEventListener("pointerdown", event => { if (!wrapper.contains(event.target)) { pinned = false; show(false); } });
    document.addEventListener("keydown", event => { if (event.key === "Escape") { pinned = false; show(false); } });
    unavailable();
  }
  globalThis.TeslaBoxInternetStatus = {render, unavailable};
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", initialize, {once:true});
  else initialize();
})();
