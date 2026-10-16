/* 网络来源只依据系统设备类型，绝不通过接口名字猜测手机或运营商。 */
(() => {
  "use strict";
  const root = document.getElementById("shared-network-devices");
  if (!root) return;
  const t = globalThis.TeslaBoxI18n.t;
  const labels = {cellular:t("移动网络"),bluetooth:t("蓝牙网络共享"),usb:t("USB 网络")};
  const states = {connected:t("已连接"),connecting:t("正在连接／获取地址"),disconnecting:t("正在断开"),disconnected:t("未连接"),unmanaged:t("未由 NetworkManager 管理"),unavailable:t("链路不可用"),failed:t("连接失败"),unknown:t("状态未知")};
  const node = (tag,text,cls) => {const el=document.createElement(tag); el.textContent=text; if(cls)el.className=cls; return el;};
  function unavailable(message) {document.getElementById("shared-network-notice").textContent=message;if(message)root.replaceChildren(node("p",t("状态暂不可用，请刷新后重试"),"empty-state"));}
  function render(status) {
    root.replaceChildren(); unavailable("");
    for (const device of status.devices || []) {
      // USB 以太网沿用有检查点回退的编辑器，只在本栏出现一次。
      if (device.kind === "usb" && globalThis.TeslaBoxEthernet) continue;
      const card=node("article","","network-device"), overview=node("div","","hardware-overview network-overview");
      const metric=(name,value,tone="")=>{const item=node("div","");item.append(node("span",name),node("strong",value,`hardware-value${tone}`));overview.append(item);};
      // 蓝牙设备可能只提供地址标识；它不是已创建的 PAN 网络接口，不能当作接口名展示。
      const addressOnly=device.kind === "bluetooth" && /^(?:[0-9a-f]{2}:){5}[0-9a-f]{2}$/i.test(device.interface || "");
      const iface=addressOnly ? "" : device.interface || "";
      const kind=Object.prototype.hasOwnProperty.call(labels,device.kind) ? device.kind : "ethernet";
      const heading=node("h3","",`network-device-heading network-heading-${kind}`);
      heading.append(node("span",labels[device.kind] || t("网络连接"),"network-device-title"));
      if(iface) heading.append(node("span",iface,"network-device-interface"));
      card.append(heading);
      const tone=device.state === "connected" ? " ok" : ["connecting","disconnecting"].includes(device.state) ? "" : " error";
      metric(t("连接状态"),states[device.state] || states.unknown,tone);
      metric(t("网络接口"),iface || "—");
      metric(t("网络类型"),labels[device.kind] || t("未知"));
      metric(t("IP 地址"),[...(device.ipv4?.addresses || []),...(device.ipv6?.addresses || [])].join("\n") || t("尚未取得 IP 地址"));
      metric(t("连接配置"),device.connection_name || t("无活动配置"));
      metric(t("默认路由"),[device.default_ipv4 ? "IPv4" : "",device.default_ipv6 ? "IPv6" : ""].filter(Boolean).join(" / ") || t("无"));
      const connected=device.state === "connected";
      metric(t("连接速率"),!connected ? "—" : device.speed > 0 ? `${device.speed} Mbps` : t("系统未提供"));
      metric(t("网关"),!connected ? "—" : [device.ipv4?.gateway,device.ipv6?.gateway].filter(Boolean).join("\n") || t("无"));
      metric("DNS",!connected ? "—" : [...new Set([...(device.ipv4?.dns || []),...(device.ipv6?.dns || [])])].join("\n") || t("无"));
      metric("MTU",device.mtu > 0 ? String(device.mtu) : t("未知"));
      card.append(overview);
      if(device.notice) card.append(node("p",t(device.notice),"settings-warning"));
      root.append(card);
    }
    if (!(status.devices || []).length) root.append(node("p",t("未检测到移动或共享网络设备；请先连接设备并开启网络共享。"),"empty-state"));
  }
  document.getElementById("shared-network-refresh").addEventListener("click",async()=>{
    try {render(await request("/api/v1/network/shared",{loading:false})); if(globalThis.TeslaBoxEthernet)globalThis.TeslaBoxEthernet.render(await request("/api/v1/network/ethernet",{loading:false}));}
    catch(error){unavailable(error.message);}
  });
  globalThis.TeslaBoxSharedNetwork={render,unavailable};
})();
