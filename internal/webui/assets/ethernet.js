/* 只操作明确选择的有线配置，不切换全局网络，不借用无线接口。 */
(() => {
  "use strict";
  const t = globalThis.TeslaBoxI18n.t;
  const byID = id => document.getElementById(id), root = byID("ethernet-devices");
  if (!root) return;
  const cards = new Map();
  let status = {devices:[]}, busy = false, poll = 0, sequence = 0, emptyNode = null;
  const labels = {connected:t("已连接"),connecting:t("正在连接／获取地址"),disconnecting:t("正在断开"),disconnected:t("未连接"),unavailable:t("链路不可用"),unmanaged:t("未托管"),failed:t("连接失败"),unknown:t("状态未知")};
  function node(tag, text = "", className = "") {
    const value = document.createElement(tag); value.textContent = text;
    if (className) value.className = className; return value;
  }
  function notice(text, error = false) { for (const id of ["ethernet-notice","shared-network-notice"]) {const target=byID(id); if(target){target.textContent=text;target.className=`notice ${error ? "error" : ""}`;}} }
  function option(select, value, text) { const item = node("option",text); item.value = String(value); select.append(item); }
  function field(parent, name, text, type, initial) {
    const check = type === "checkbox";
    const label = node("label",check ? "" : text,check ? "inline-check" : ""), input = node(type === "select" ? "select" : "input"); input.name = name;
    if (type !== "select") { input.type = type; if (type === "checkbox") input.checked = Boolean(initial); else input.value = String(initial ?? ""); }
    label.append(input); if (check) label.append(node("span",text)); parent.append(label); return input;
  }
  function addressEditor(parent, name, settings) {
    const group = node("fieldset","","ethernet-addresses"); group.append(node("legend",name.toUpperCase())); parent.append(group);
    const method = field(group,`${name}.method`,t("地址配置"),"select");
    for (const [value,text] of [["auto",t("自动")],["manual",t("手动")],["link-local",t("仅本地链路")],["disabled",t("停用")]]) option(method,value,text);
    if (name === "ipv6") option(method,"dhcp",t("仅 DHCPv6")); method.value = settings.method;
    const addresses = field(group,`${name}.addresses`,t("IP 地址／前缀（多个用逗号分隔）"),"text",(settings.addresses || []).join(", "));
    addresses.placeholder = name === "ipv4" ? "192.168.1.20/24" : "2001:db8::20/64";
    const gateway = field(group,`${name}.gateway`,t("网关"),"text",settings.gateway);
    const dns = field(group,`${name}.dns`,t("DNS（多个用逗号分隔）"),"text",(settings.dns || []).join(", "));
    const ignore = field(group,`${name}.ignore_auto_dns`,t("忽略自动分配的 DNS"),"checkbox",settings.ignore_auto_dns);
    field(group,`${name}.never_default`,t("不作为默认路由"),"checkbox",settings.never_default);
    const metric = field(group,`${name}.route_metric`,t("路由 metric（越小越优先；-1 为自动）"),"number",settings.route_metric ?? -1);
    metric.required = true;
    metric.min = "-1"; metric.max = "4294967295"; metric.step = "1";
    function update() { addresses.disabled = method.value !== "manual"; addresses.required = method.value === "manual"; gateway.disabled = ["disabled","link-local"].includes(method.value); dns.disabled = ignore.disabled = ["disabled","link-local"].includes(method.value); }
    method.addEventListener("change",update); update();
  }
  function renderOverview(target, device, unavailable = false) {
    target.replaceChildren();
    const metric = (name, value, tone = "") => {
      const item = node("div"); item.append(node("span",name),node("strong",value,`hardware-value${tone}`)); target.append(item);
    };
    const tone = device.state === "connected" ? " ok" : ["connecting","disconnecting"].includes(device.state) ? "" : " error";
    metric(t("连接状态"),unavailable ? t("状态暂不可用，请刷新后重试") : labels[device.state] || t("状态未知"),unavailable ? " error" : tone);
    metric(t("网络接口"),device.interface);
    if (unavailable) return;
    const active = (device.profiles || []).find(profile => device.connection_uuid && profile.uuid === device.connection_uuid);
    const duplex = device.duplex === "full" ? t("全双工") : device.duplex === "half" ? t("半双工") : device.duplex || t("双工未知");
    metric(t("MAC 地址"),device.mac || t("未知"));
    metric(t("IP 地址"),[...(device.ipv4?.addresses || []),...(device.ipv6?.addresses || [])].join("\n") || t("尚未取得 IP 地址"));
    metric(t("连接配置"),active?.settings?.name || t("无活动配置"));
    metric(t("链路状态"),device.carrier === undefined ? t("状态未知") : device.carrier ? t(device.kind === "usb" ? "USB 网络链路已接入" : "网线已接入") : t("未检测到链路"));
    metric(t("速率／双工"),`${device.speed ? `${device.speed} Mbps` : t("速率未知")} · ${duplex}`);
    metric(t("网关"),[device.ipv4?.gateway,device.ipv6?.gateway].filter(Boolean).join("\n") || t("无"));
    metric("DNS",[...(device.ipv4?.dns || []),...(device.ipv6?.dns || [])].join("\n") || t("无"));
    metric("MTU",String(device.mtu || t("未知")));
  }
  function defaultSettings(iface) {
    return {name:`TeslaBox-Ethernet-${iface}`,auto_connect:true,mtu:0,auto_negotiate:false,speed:0,duplex:"",wake_on_lan:1,
      ipv4:{method:"auto",addresses:[],gateway:"",dns:[],ignore_auto_dns:false,never_default:false,route_metric:-1},
      ipv6:{method:"auto",addresses:[],gateway:"",dns:[],ignore_auto_dns:false,never_default:false,route_metric:-1}};
  }
  function renderEditor(card, device, selectedUUID) {
    card.form.replaceChildren();
    const profiles = device.profiles || [], selected = profiles.find(value => value.uuid === selectedUUID);
    card.uuid = selected?.uuid || ""; card.dirty = false;
    const picker = field(card.form,"profile",t("连接配置"),"select");
    for (const profile of profiles) option(picker,profile.uuid,`${profile.settings.name}${profile.active ? t(" · 当前连接") : ""}`);
    option(picker,"",t("新建普通有线配置")); picker.value = card.uuid;
    picker.addEventListener("change",async () => {
      const next = picker.value;
      if (card.dirty && !await askConfirmation(t("丢弃有线草稿？"),t("切换配置会放弃本张网卡未保存的修改。"),t("丢弃并切换"),true)) { picker.value = card.uuid; return; }
      renderEditor(card,card.device,next);
    });
    const settings = selected?.settings || defaultSettings(device.interface), fields = node("fieldset","","ethernet-fields"); card.form.append(fields);
    fields.disabled = !device.managed || Boolean(selected && !selected.editable);
    const name = field(fields,"name",t("连接名称"),"text",settings.name); name.maxLength = 128; name.required = true;
    const mtu = field(fields,"mtu",t("MTU（0 为自动）"),"number",settings.mtu); mtu.min = "0"; mtu.max = "65535"; mtu.required = true;
    field(fields,"auto_connect",t("自动连接"),"checkbox",settings.auto_connect);
    addressEditor(fields,"ipv4",settings.ipv4); addressEditor(fields,"ipv6",settings.ipv6);
    const hardware = node("fieldset","","ethernet-addresses"); hardware.append(node("legend",t("链路与唤醒"))); fields.append(hardware);
    const negotiate = field(hardware,"auto_negotiate",t("强制自动协商"),"checkbox",settings.auto_negotiate);
    negotiate.disabled = device.auto_negotiate === undefined || !device.auto_negotiate && !settings.auto_negotiate;
    const mode = field(hardware,"link_mode",t("速率／双工"),"select"); option(mode,"0/",t("不指定（由驱动决定）"));
    for (const value of device.link_modes || []) option(mode,`${value.speed}/${value.duplex}`,`${value.speed} Mbps · ${value.duplex === "full" ? t("全双工") : t("半双工")}`);
    const selectedMode = `${settings.speed}/${settings.duplex || ""}`;
    if (![...mode.options].some(item => item.value === selectedMode)) option(mode,selectedMode,t("当前值（未核实支持，仅保留）"));
    mode.value = selectedMode; mode.disabled = !device.link_modes?.length;
    const wol = field(hardware,"wake_on_lan","Wake-on-LAN","select");
    for (const [value,text,key] of [[1,t("系统默认"),""],[32768,t("不干预"),""],[0,t("关闭"),""],[64,t("魔术包"),"g"],[2,t("物理链路"),"p"],[4,t("单播"),"u"],[8,t("组播"),"m"],[16,t("广播"),"b"],[32,"ARP","a"]]) if (!key || device.wake_on_lan_modes?.includes(key)) option(wol,value,text);
    if (![...wol.options].some(item => item.value === String(settings.wake_on_lan))) option(wol,settings.wake_on_lan,t("当前组合（保持）"));
    wol.value = String(settings.wake_on_lan); wol.disabled = !device.wake_on_lan_modes;
    if (device.hardware_notice) fields.append(node("p",device.hardware_notice,"muted"));
    if (selected?.reason) card.form.append(node("p",selected.reason,"settings-warning"));
    const actions = node("div","","button-row"), active = device.connection_uuid === card.uuid && Boolean(card.uuid);
    const save = node("button",active ? t("保存并试用 120 秒") : t("保存（不连接）"),"primary"); save.type = "submit";
    save.disabled = fields.disabled || active && device.state !== "connected";
    const connect = node("button",active ? t("断开") : t("连接"),"secondary"); connect.type = "button";
    connect.disabled = fields.disabled || !card.uuid || ["connecting","disconnecting"].includes(device.state);
    connect.addEventListener("click",() => act(card.device,card.uuid,active ? "disconnect" : "connect"));
    actions.append(save,connect); card.form.append(actions);
    card.form.append(node("p",active ? t("修改活动配置会短暂断网。请确认新地址可访问后保留，未确认自动回退。") : t("未连接的配置先保存，再点击连接；连接成功并确认后可应用修改。"),"muted"));
  }
  function readSettings(form) {
    const value = name => form.elements.namedItem(name), list = name => value(name).value.split(/[,，\s]+/).filter(Boolean);
    const ip = family => {
      const method = value(`${family}.method`).value;
      return {method,addresses:method === "manual" ? list(`${family}.addresses`) : [],gateway:["disabled","link-local"].includes(method) ? "" : value(`${family}.gateway`).value.trim(),
        dns:["disabled","link-local"].includes(method) ? [] : list(`${family}.dns`),ignore_auto_dns:value(`${family}.ignore_auto_dns`).checked,never_default:value(`${family}.never_default`).checked,route_metric:Number(value(`${family}.route_metric`).value || -1)};
    };
    const [speed,duplex] = value("link_mode").value.split("/");
    return {name:value("name").value.trim(),auto_connect:value("auto_connect").checked,mtu:Number(value("mtu").value),ipv4:ip("ipv4"),ipv6:ip("ipv6"),
      auto_negotiate:value("auto_negotiate").checked,speed:Number(speed),duplex,wake_on_lan:Number(value("wake_on_lan").value)};
  }
  function renderHeading(heading, device) {
    const usb = device.kind === "usb";
    // 有线页面已有主标题；只有共享网络列表中的 USB 设备需要独立标题。
    heading.hidden = !usb;
    heading.replaceChildren();
    if (!usb) return;
    heading.className = "network-device-heading network-heading-usb";
    heading.append(node("span",t("USB 网络"),"network-device-title"),node("span",device.interface,"network-device-interface"));
  }
  function makeCard(device) {
    const container = node("article","","network-device"), info = node("div","","hardware-overview network-overview"), editor = node("div","","settings-card ethernet-card"), form = node("form","","settings-form");
    const heading=node("h3"); renderHeading(heading,device);
    editor.append(form); container.append(heading,info,editor);
    (device.kind === "usb" ? byID("shared-usb-devices") || root : root).append(container);
    const card = {container,heading,kind:device.kind,info,form,uuid:"",dirty:false,device};
    form.addEventListener("input",event => {if (event.target.name !== "profile") card.dirty = true;});
    form.addEventListener("change",event => {if (event.target.name !== "profile") card.dirty = true;});
    form.addEventListener("submit",async event => {
      event.preventDefault(); const current = card.device, apply = current.connection_uuid === card.uuid && Boolean(card.uuid);
      if (!await askConfirmation(apply ? t("试用有线设置？") : t("保存有线配置？"),apply ? t("当前管理连接可能中断。120 秒内请从新地址或其他网口回到本页确认；未确认会自动回退。") : t("只保存此配置，不会立刻连接或断开当前网口。"),apply ? t("开始试用") : t("保存"),false)) return;
      await mutate("/api/v1/network/ethernet",{interface:current.interface,uuid:card.uuid,apply,settings:readSettings(form)},card);
    }); return card;
  }
  function render(value) {
    if (busy) return; status = value;
    byID("ethernet-trial").hidden = !value.pending;
    if (value.pending) byID("ethernet-trial-detail").textContent = t`${value.pending.interface} · 请在 ${new Date(value.pending.expires_at).toLocaleTimeString()} 前确认；到期由 NetworkManager 自动回退。`;
    if (byID("shared-usb-trial")) {
      byID("shared-usb-trial").hidden = !value.pending;
      if(value.pending) byID("shared-usb-trial-detail").textContent=byID("ethernet-trial-detail").textContent;
    }
    if(emptyNode){emptyNode.remove();emptyNode=null;}
    if (!cards.size) root.replaceChildren();
    const present = new Set();
    for (const device of value.devices || []) {
      present.add(device.interface); let card = cards.get(device.interface);
      if (!card) {card = makeCard(device); cards.set(device.interface,card);}
      if(card.kind!==device.kind){
        card.container.remove();(device.kind === "usb" ? byID("shared-usb-devices") || root : root).append(card.container);
        renderHeading(card.heading,device);card.kind=device.kind;
      }
      card.device = device; renderOverview(card.info,device);
      if (!card.dirty) renderEditor(card,device,card.uuid || device.profiles?.find(item => item.active)?.uuid || device.profiles?.[0]?.uuid || "");
      card.form.inert = Boolean(value.pending);
    }
    for (const [name,card] of cards) if (!present.has(name)) {card.container.remove(); cards.delete(name);}
    if (!(value.devices || []).some(device => device.kind !== "usb")) {emptyNode=node("p",t("未检测到普通以太网网卡。"),"empty-state");root.replaceChildren(emptyNode);}
    window.clearTimeout(poll); if (value.pending && !document.hidden) poll = window.setTimeout(() => void refresh(),2000);
  }
  async function refresh() {
    const current = ++sequence;
    try {const value = await request("/api/v1/network/ethernet",{loading:false}); if (current === sequence) render(value);}
    catch (error) {if (current === sequence) notice(error.message,true);}
  }
  async function mutate(path,input,card) {
    if (busy) return; busy = true; sequence++;
    notice(t("正在提交有线设置；若地址变化导致断线，请从新 IP 或其他网络重新打开页面，未确认的试用会自动回退。"));
    try {
      const result = await postJSON(path,input); if (card) card.dirty = false;
      if (![...cards.values()].some(item => item.dirty)) globalThis.TeslaBoxSettingsLayout?.markClean("ethernet-settings-title");
      notice(result.trial ? t("有线试用已启动，等待实际连接完成后确认。") : t("有线设置已完成。"));
    } catch (error) {notice(t`${error.message}。如本次应用已改变网络，请恢复访问后刷新状态；未确认的试用会自动回退。`,true);}
    finally {busy = false; await refresh();}
  }
  async function act(device,uuid,action) {
    if (!await askConfirmation(action === "disconnect" ? t("断开有线网络？") : t("连接有线网络？"),t("操作可能中断当前管理页面。请准备好通过 WiFi、AP 或新地址访问；120 秒内未确认会恢复原状态。"),t("开始试用"),false)) return;
    await mutate("/api/v1/network/ethernet/action",{interface:device.interface,uuid,action});
  }
  async function finish(keep) {
    if (!status.pending || busy) return;
    if (!await askConfirmation(keep ? t("保留有线设置？") : t("回退有线设置？"),keep ? t("请确认新地址、DNS 和所需网络已经可以使用。保留后不再自动恢复旧配置。") : t("将恢复试用前的有线连接和配置，当前访问可能短暂中断。"),keep ? t("确认保留") : t("立即回退"),false)) return;
    await mutate("/api/v1/network/ethernet/finish",{token:status.pending.token,keep});
  }
  byID("ethernet-refresh").addEventListener("click",() => void refresh());
  byID("ethernet-confirm").addEventListener("click",() => void finish(true));
  byID("ethernet-rollback").addEventListener("click",() => void finish(false));
  byID("shared-usb-confirm")?.addEventListener("click",() => void finish(true));
  byID("shared-usb-rollback")?.addEventListener("click",() => void finish(false));
  document.addEventListener("visibilitychange",() => {if (!document.hidden && status.pending) void refresh();});
  globalThis.TeslaBoxEthernet = {render,unavailable:message => {notice(message,true);for(const card of cards.values()){renderOverview(card.info,card.device,true);card.form.inert=true;}}};
})();
