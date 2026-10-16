/* 移动现有表单节点，切换页面和分类时保留草稿及独立保存逻辑。 */
(() => {
  "use strict";
  const t = globalThis.TeslaBoxI18n.t;
  const byID = id => document.getElementById(id);
  // 小尺寸下用实心弧带保留 WiFi 辨识度，避免细线缩小后像灰色噪点。
  const wifiIcon = [
    "M2 8.5C7.5 3.8 16.5 3.8 22 8.5L19.8 10.8C15.4 7.1 8.6 7.1 4.2 10.8Z",
    "M6.2 12.7C9.4 10 14.6 10 17.8 12.7L15.5 15C13.6 13.5 10.4 13.5 8.5 15Z",
    "M10 16.8C11.2 15.8 12.8 15.8 14 16.8L12 19Z",
  ];
  const carPlayIcon = ["M5 10 7 5h10l2 5", "M4 10h16v8H4Z", "M7 18v2m10-2v2M7 13h2m6 0h2"];
  const groups = [
    {id:"network", title:t("网络连接"), detail:t("有线、移动共享、WiFi 与 AP 热点"), icon:wifiIcon, titles:["wifi-settings-title","shared-network-title","ethernet-settings-title","ap-settings-title"]},
    {id:"carplay",title:"CarPlay",detail:t("网络来源、画质、帧率与主页面布局"),icon:carPlayIcon,titles:["carplay-settings-title"]},
    {id:"storage", title:t("存储管理"), detail:t("外接存储与媒体写入恢复"), icon:["M5 4h14l3 12v4H2v-4L5 4Z", "M2 16h20", "M6 18h.01M10 18h.01"], titles:["storage-settings-title","maintenance-settings-title"]},
    {id:"thermal", title:t("散热与风扇"), detail:t("温度、风扇转速与场景策略"), icon:["M14 12a2 2 0 1 1-4 0 2 2 0 0 1 4 0Z", "M12 10V5a3 3 0 0 1 6 0c0 3-3 4-4 5", "M14 12h5a3 3 0 0 1 0 6c-3 0-4-3-5-4", "M12 14v5a3 3 0 0 1-6 0c0-3 3-4 4-5", "M10 12H5a3 3 0 0 1 0-6c3 0 4 3 5 4"], titles:["fan-settings-title"]},
    {id:"security", title:t("安全与账户"), detail:t("管理登录与账户安全"), icon:["M12 3 3 7v6c0 5 9 9 9 9s9-4 9-9V7l-9-4Z", "m8 12 3 3 5-6"], titles:["security-title"]},
    {id:"system", title:t("系统"), detail:t("地区、语言、页面设置、更新与电源"), icon:["M4 7h7m4 0h5M4 17h3m4 0h9", "M15 7a2 2 0 1 1-4 0 2 2 0 0 1 4 0Z", "M11 17a2 2 0 1 1-4 0 2 2 0 0 1 4 0Z"], titles:["region-settings-title","language-settings-title","appearance-settings-title","update-title","power-title"]},
  ];
  const drafts = new Set();
  const mobile = window.matchMedia("(max-width: 759px)");
  let selected = "network";
  let selectedSection = "";
  let backupTab = "history";
  let initialized = false;
  const sections = new Map();
  const functionDescriptions = {
    "shared-network-title": t("USB 网络、蜂窝 modem 与蓝牙网络共享"),
    "ethernet-settings-title": t("连接状态、IP、DNS、链路与自动回退"),
    "region-settings-title": t("无线使用地区、监管域与信道规则"),
    "language-settings-title": t("设备共享的中文与英文界面"),
    "carplay-settings-title": t("网络来源、画质、帧率与主页面布局"),
    "wifi-settings-title": t("连接 WiFi，管理已保存的网络"),
    "ap-settings-title": t("设置热点名称、频段与自动启动"),
    "storage-settings-title": t("查看录像盘，管理空间与容量"),
    "maintenance-settings-title": t("诊断媒体写入异常，选择恢复方式"),
    "fan-settings-title": t("查看温度，调整风扇与场景策略"),
    "security-title": t("管理登录密码与账户安全"),
    "appearance-settings-title": t("主题、显示大小、字体与车机兼容"),
    "update-title": t("查看版本，检查并安装系统更新"),
    "power-title": t("查看设备信息，管理断开与电源"),
  };
  const functionIcons = {
    "shared-network-title": ["M10 8H8a4 4 0 0 0 0 8h2", "M14 8h2a4 4 0 0 1 0 8h-2", "M8 12h8"],
    "ethernet-settings-title": ["M4 5h16v10h-5v4H9v-4H4Z", "M8 8v3m4-3v3m4-3v3"],
    "region-settings-title": ["M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0Z", "M3 12h18M12 3c5 5 5 13 0 18-5-5-5-13 0-18Z"],
    "language-settings-title": ["M3 5h12M9 3v2M5 5c0 6 6 10 10 11M13 5c0 6-6 10-10 11", "m13 21 4-10 4 10m-6-4h4"],
    "carplay-settings-title": carPlayIcon,
    "wifi-settings-title": wifiIcon,
    "ap-settings-title": ["M12 13v8M8 21h8", "M14 11a2 2 0 1 1-4 0 2 2 0 0 1 4 0Z", "M7 7a6 6 0 0 0 0 8M17 7a6 6 0 0 1 0 8", "M4 4a10 10 0 0 0 0 14M20 4a10 10 0 0 1 0 14"],
    "storage-settings-title": ["M3 7h7l2 2h9v11H3V7Z", "M3 7V4h7l2 3h7v2"],
    "maintenance-settings-title": ["M14 6a5 5 0 0 0-6 6L3 17a3 3 0 0 0 4 4l5-5a5 5 0 0 0 6-6l-3 3-4-4 3-3Z"],
    "fan-settings-title": ["M9 14.5V5a3 3 0 0 1 6 0v9.5a5 5 0 1 1-6 0Z", "M12 8v10M17 6h3M17 10h2"],
    "security-title": ["M13 9a4 4 0 1 1 8 0 4 4 0 0 1-8 0Z", "m14 12-9 9H2v-3l9-9M7 16l2 2"],
    "appearance-settings-title": ["M4 4h16v12H4Z", "M8 20h8M12 16v4"],
    "update-title": ["M12 3v12m-4-4 4 4 4-4", "M4 16v5h16v-5"],
    "power-title": ["M12 3v9", "M6 5a9 9 0 1 0 12 0"],
  };
  function read(key, fallback) { try { return localStorage.getItem(key) || fallback; } catch (_) { return fallback; } }
  function remember(key, value) { try { localStorage.setItem(key, value); } catch (_) { /* 隐私模式下仍可切换。 */ } }
  function node(tag, text, className) {
    const element = document.createElement(tag);
    if (text) element.textContent = text;
    if (className) element.className = className;
    return element;
  }
  // 只统一造型和笔画，符号与底色通过 CSS 跟随当前主题，不为功能固定配色。
  function categoryIcon(paths) {
    const icon = node("span", "", "settings-category-icon");
    icon.setAttribute("aria-hidden", "true");
    const filled = paths === wifiIcon;
    const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    for (const [name, value] of Object.entries({viewBox:"0 0 24 24", fill:filled ? "currentColor" : "none", stroke:filled ? "none" : "currentColor", "stroke-width":"2", "stroke-linecap":"round", "stroke-linejoin":"round", focusable:"false"})) svg.setAttribute(name, value);
    for (const d of paths) {
      const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
      path.setAttribute("d", d); svg.append(path);
    }
    icon.append(svg);
    return icon;
  }
  function markClean(id) { drafts.delete(id); renderDrafts(); }
  function renderDrafts() {
    byID("settings-tabs").querySelectorAll("[data-settings-group]").forEach(button => {
      const group = groups.find(item => item.id === button.dataset.settingsGroup);
      const dirty = group.titles.some(id => drafts.has(id));
      button.querySelector(".settings-draft-dot").hidden = !dirty;
    });
    const group = groups.find(item => item.id === selected);
    byID("settings-draft-notice").hidden = !group.titles.some(id => drafts.has(id));
    document.querySelectorAll("[data-settings-section]").forEach(button => {
      button.querySelector(".settings-draft-dot").hidden = !drafts.has(button.dataset.settingsTitle);
    });
    for (const name of ["targets","rules"]) {
      const dirty = name === "targets" ? ["nas-target-form","nas-default-form"].some(id => drafts.has(id)) : ["nas-quality-form","nas-automatic-form"].some(id => drafts.has(id));
      byID(`backup-${name}-tab`).querySelector(".settings-draft-dot").hidden = !dirty;
      byID(`backup-${name}-draft`).hidden = !dirty;
    }
    byID("export-draft-notice").hidden = !drafts.has("export-settings-title");
  }
  function showGroup(id, detail = true, updateHistory = false, sectionID = "") {
    const group = groups.find(item => item.id === id);
    if (!group) return;
    selected = id;
    const singleFunction = group.titles.length === 1;
    // 单项分类直接使用已有详情节点，避免增加没有选择意义的菜单层级。
    selectedSection = sections.get(sectionID) === id ? sectionID : singleFunction ? byID(group.titles[0]).closest(".settings-section").id : "";
    remember("teslabox-settings-group", id);
    groups.forEach(item => { byID(`settings-group-${item.id}`).hidden = item.id !== id; });
    byID("settings-tabs").querySelectorAll("[data-settings-group]").forEach(button => {
      const active = button.dataset.settingsGroup === id;
      button.classList.toggle("active", active);
      if (active) button.setAttribute("aria-current", "true"); else button.removeAttribute("aria-current");
    });
    byID("settings-detail-title").textContent = group.title;
    const page = byID("settings-page");
    page.classList.toggle("settings-detail-open", detail);
    page.classList.toggle("settings-function-open", Boolean(selectedSection));
    page.classList.toggle("settings-single-function", singleFunction);
    byID("settings-back").textContent = selectedSection && !singleFunction ? t`‹ 返回${group.title}` : t("‹ 返回设置");
    byID(`settings-menu-${id}`).hidden = Boolean(selectedSection);
    for (const titleID of group.titles) {
      const section = byID(titleID).closest(".settings-section");
      section.hidden = section.id !== selectedSection;
    }
    if (updateHistory) window.history.pushState({page:"settings"}, "", `/settings#${id}${selectedSection && !singleFunction ? `/${selectedSection}` : ""}`);
    byID("settings-content").scrollTop = 0;
    renderDrafts();
  }
  function showFunction(sectionID, updateHistory = true) {
    const group = sections.get(sectionID);
    if (group) showGroup(group, true, updateHistory, sectionID);
  }
  function showList(updateHistory = true) {
    byID("settings-page").classList.remove("settings-detail-open");
    if (updateHistory) window.history.pushState({page:"settings"}, "", "/settings");
    byID("settings-tabs").querySelector(`[data-settings-group="${selected}"]`)?.focus();
  }
  function selectBackup(id) {
    if (!["history","targets","rules"].includes(id)) return;
    backupTab = id;
    ["history","targets","rules"].forEach(name => {
      byID(`backup-${name}-panel`).hidden = name !== id;
      const button = byID(`backup-${name}-tab`);
      button.classList.toggle("active", name === id);
      button.setAttribute("aria-selected", String(name === id));
      button.tabIndex = name === id ? 0 : -1;
    });
  }
  function mountBackup() {
    const settings = byID("nas-upload-settings");
    byID("backup-content").append(settings);
    settings.querySelector(".settings-section-heading").remove();
    for (const name of ["history","targets","rules"]) {
      const panel = node("div", "", "backup-panel");
      panel.id = `backup-${name}-panel`;
      panel.setAttribute("role", "tabpanel");
      panel.setAttribute("aria-labelledby", `backup-${name}-tab`);
      settings.append(panel);
      if (name !== "history") {
        const notice = node("p", t("有未保存的修改；切换标签会保留草稿，请在对应板块单独保存。"), "draft-notice");
        notice.id = `backup-${name}-draft`; notice.hidden = true; panel.append(notice);
      }
      const button = byID(`backup-${name}-tab`);
      button.addEventListener("click", () => selectBackup(name));
      button.addEventListener("keydown", event => {
        const names = ["history","targets","rules"];
        let index = names.indexOf(backupTab);
        if (event.key === "ArrowRight") index = (index + 1) % 3;
        else if (event.key === "ArrowLeft") index = (index + 2) % 3;
        else if (event.key === "Home") index = 0;
        else if (event.key === "End") index = 2;
        else return;
        event.preventDefault(); selectBackup(names[index]); byID(`backup-${names[index]}-tab`).focus();
      });
    }
    byID("backup-targets-panel").append(settings.querySelector(".nas-server-card"));
    byID("backup-rules-panel").append(settings.querySelector(".nas-quality-card"), settings.querySelector(".nas-automatic-card"));
    byID("backup-history-panel").append(settings.querySelector(".nas-history-card"));
    const events = node("article", "", "settings-card");
    events.append(node("h3", t("事件处理记录")), node("p", t("查看事件等待、任务创建与补传结果；自动上传规则在“上传规则”中配置。")), byID("nas-auto-events"));
    byID("backup-history-panel").append(events);
    selectBackup("history");
  }
  function mountExports() {
    const section = byID("export-settings-title").closest(".settings-section");
    section.id = "export-settings-section";
    section.removeAttribute("aria-labelledby");
    section.setAttribute("aria-label", t("导出文件与保留设置"));
    byID("exports-content").append(section);
    byID("export-status-slot").append(byID("export-retention-status"));
    section.querySelector(".settings-section-heading").remove();
    const history = node("article", "", "settings-card export-history-card");
    history.append(section.querySelector(".export-history-head"), byID("export-history-list"));
    section.prepend(history);
  }
  let lastExportTask = null;
  let exportRefreshBusy = false;
  function renderExportTask(task) {
    lastExportTask = task;
    remember("teslabox-last-export-task", task.id);
    const root = byID("export-live-task");
    root.replaceChildren();
    const labels = {queued:t("正在排队"),running:task.phase === "preparing" ? t("正在准备录像") : t("正在导出"),completed:t("导出完成"),failed:t("导出失败"),canceled:t("已取消")};
    root.append(node("strong", `${task.request?.mode === "source" ? t("多路原画压缩包") : exportTaskLabel(task.request)} · ${labels[task.state] || task.state}`));
    root.append(node("p", task.error || t`已完成 ${task.progress_percent || 0}%`, "muted"));
    const actions = node("div", "", "export-history-actions");
    if (["queued","running"].includes(task.state)) {
      const progress = node("progress"); progress.max = 100; progress.value = task.progress_percent || 0; progress.setAttribute("aria-label", t("最近导出任务进度")); root.append(progress);
      const cancel = node("button", t("取消任务"), "secondary compact"); cancel.type = "button";
      cancel.addEventListener("click", async () => {
        cancel.disabled = true;
        try { renderExportTask(await request(`/api/v1/exports/${encodeURIComponent(task.id)}`, {method:"DELETE"})); }
        catch (error) { setSettingsNotice(error.message, true); cancel.disabled = false; }
      });
      actions.append(cancel);
    }
    if (task.state === "completed" && task.download_ready) {
      const download = node("a", t("下载成品"), "secondary compact"); download.href = `/api/v1/exports/${encodeURIComponent(task.id)}/download`; download.download = ""; actions.append(download);
    }
    root.append(actions);
  }
  async function refreshExportTask() {
    if (exportRefreshBusy || byID("exports-page").hidden || byID("app-view").hidden) return;
    const id = lastExportTask?.id || read("teslabox-last-export-task", "");
    if (!id) return;
    exportRefreshBusy = true;
    try { renderExportTask(await request(`/api/v1/exports/${encodeURIComponent(id)}`, {loading:false})); }
    catch (error) {
      if (error.status === 404) { remember("teslabox-last-export-task", ""); lastExportTask = null; byID("export-live-task").replaceChildren(node("p", t("任务已清理，可在下方查看仍可下载的历史成品。"), "muted")); }
      else setSettingsNotice(t`任务状态暂不可用：${error.message}`, true, false);
    } finally { exportRefreshBusy = false; }
  }
  function initialize() {
    if (initialized) return;
    globalThis.TeslaBoxI18n.bindStatic();
    initialized = true;
    const tabs = byID("settings-tabs");
    tabs.replaceChildren();
    for (const group of groups) {
      const panel = node("div", "", "settings-group");
      panel.id = `settings-group-${group.id}`;
      panel.setAttribute("aria-label", group.title);
      byID("settings-content").append(panel);
      const menu = node("div", "", "settings-function-menu");
      menu.id = `settings-menu-${group.id}`;
      menu.setAttribute("aria-label", t`${group.title}功能`);
      panel.append(menu);
      group.titles.forEach(id => {
        const section = byID(id).closest(".settings-section");
        if (!section.id) section.id = id.replace("-title", "-section");
        sections.set(section.id, group.id); section.hidden = true; panel.append(section);
        const entry = node("button", "", "settings-function-entry");
        entry.type = "button";
        entry.dataset.settingsSection = section.id; entry.dataset.settingsTitle = id;
        entry.setAttribute("aria-controls", section.id);
        const copy = node("span", "", "settings-function-copy");
        copy.append(node("strong", id === "wifi-settings-title" ? "WiFi" : byID(id).textContent), node("small", functionDescriptions[id]));
        const dirty = node("span", "", "settings-draft-dot"); dirty.hidden = true; dirty.setAttribute("aria-label", t("有未保存的修改"));
        const arrow = node("span", "›", "settings-function-arrow"); arrow.setAttribute("aria-hidden", "true");
        const icon = categoryIcon(functionIcons[id]); icon.className = "settings-function-icon";
        entry.append(icon, copy, dirty, arrow); menu.append(entry);
        entry.addEventListener("click", () => showFunction(section.id));
      });
      const button = node("button", "", "settings-category");
      button.type = "button"; button.dataset.settingsGroup = group.id;
      button.setAttribute("aria-controls", panel.id);
      const icon = categoryIcon(group.icon);
      const copy = node("span", "", "settings-category-copy"); copy.append(node("strong", group.title), node("small", group.detail));
      const dirty = node("span", "", "settings-draft-dot"); dirty.hidden = true; dirty.setAttribute("aria-label", t("有未保存的修改"));
      const arrow = node("span", "›", "settings-category-arrow"); arrow.setAttribute("aria-hidden", "true");
      button.append(icon, copy, dirty, arrow); tabs.append(button);
      button.addEventListener("click", () => showGroup(group.id, true, true));
    }
    mountBackup(); mountExports();
    selected = read("teslabox-settings-group", "network");
    if (!groups.some(group => group.id === selected)) selected = "network";
    byID("settings-back").addEventListener("click", () => {
      if (!selectedSection || groups.find(group => group.id === selected).titles.length === 1) { showList(); return; }
      const previous = selectedSection;
      showGroup(selected, true, true);
      byID(`settings-menu-${selected}`).querySelector(`[data-settings-section="${previous}"]`)?.focus();
    });
    document.addEventListener("input", trackDraft);
    document.addEventListener("change", trackDraft);
    document.addEventListener("teslabox-config-saved", event => markClean(event.detail));
    ["fan-mode-automatic","fan-mode-manual"].forEach(id => byID(id).addEventListener("click", () => { drafts.add("fan-settings-title"); renderDrafts(); }));
    mobile.addEventListener("change", () => showGroup(selected, Boolean(window.location.hash) || !mobile.matches, false, selectedSection));
    showGroup(selected, !mobile.matches);
    enterPage();
    window.setInterval(() => { if (!document.hidden) void refreshExportTask(); }, 3000);
  }
  function trackDraft(event) {
    if (event.target.form?.id === "wireless-region-form") return;
    const input = event.target;
    if (input.name === "profile" && input.closest(".ethernet-card")) return;
    if (!input.matches("input,select,textarea")) return;
    if (["nas-target-form","nas-default-form","nas-quality-form","nas-automatic-form"].includes(input.form?.id)) drafts.add(input.form.id);
    else {
      const section = input.closest(".settings-section");
      const title = section?.querySelector("h2")?.id;
      if (["ap-settings-title","fan-settings-title","ethernet-settings-title"].includes(title)) drafts.add(title);
      if (section?.id === "export-settings-section") drafts.add("export-settings-title");
    }
    renderDrafts();
  }
  function enterPage() {
    if (!initialized) return;
    if (window.location.pathname !== "/settings") return;
    const hash = window.location.hash.slice(1);
    const legacy = hash.startsWith("settings-tab=") ? hash.slice("settings-tab=".length) : hash;
    if (sections.has(legacy)) { showFunction(legacy,false); return; }
    if (["export-settings-title", "settings-panel-5", "storage/settings-panel-5"].includes(legacy) || !hash && read("teslabox-settings-tab", "") === "settings-panel-5") {
      remember("teslabox-settings-tab", ""); showPage("exports",true); return;
    }
    const [id, sectionID] = hash.split("/");
    showGroup(groups.some(group => group.id === id) ? id : selected, Boolean(id) || !mobile.matches, false, sectionID);
  }
  function selectSection(id, rememberSelection = true) {
    if (["settings-panel-5", "export-settings-section"].includes(id)) { showPage("exports",true); return; }
    if (id === "nas-upload-settings") { showPage("backup", true); selectBackup("targets"); return; }
    showFunction(id, rememberSelection);
  }
  globalThis.TeslaBoxSettingsLayout = {initialize, enterPage, selectSection, selectBackup, markClean, renderExportTask, isDirty:id => drafts.has(id)};
})();
