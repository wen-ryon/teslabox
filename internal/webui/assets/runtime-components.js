/* 组件状态只查询本机受认证接口，不做远程心跳。 */
(() => {
 "use strict";
const t = globalThis.TeslaBoxI18n.t;
 const panel = document.getElementById("runtime-components");
 const list = document.getElementById("runtime-components-list");
 if (!panel || !list) return;
 const names = {media:t("视频与转码"), nas:t("NAS 上传"), carplay:"CarPlay"};
 const labels = {pending:t("等待安装"), downloading:t("正在下载"), installing:t("正在安装和自检"), ready:t("已就绪"), waiting_network:t("等待联网"), failed:t("安装失败，稍后自动重试")};
 let active = false;
 async function refresh() {
  if (active || document.hidden || !location.pathname.startsWith("/settings")) return;
  active = true;
  try {
   const response = await fetch("/api/v1/runtime-components", {cache:"no-store", credentials:"same-origin"});
   if (response.status === 404) { panel.hidden = true; return; }
   if (!response.ok) return;
   const states = await response.json();
   if (!Array.isArray(states)) return;
   const nodes = states.map(state => {
    const item = document.createElement("li");
    item.textContent = `${names[state.id] || state.id} · ${state.version}：${labels[state.state] || t("尚未就绪")}${state.error ? "（" + state.error + "）" : ""}`;
    return item;
   });
   list.replaceChildren(...nodes); panel.hidden = false;
  } catch (_) { /* 网络切换时保留上次状态，下一轮重新查询。 */ }
  finally { active = false; }
 }
 refresh(); setInterval(refresh, 15000); document.addEventListener("visibilitychange", refresh);
})();