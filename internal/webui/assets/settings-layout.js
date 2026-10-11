/* 移动现有表单节点，切换页面和分类时保留草稿及独立保存逻辑。 */
(() => {
  "use strict";
  const byID = id => document.getElementById(id);
  const groups = [
    {id:"network", title:"网络连接", detail:"WiFi 无线局域网与 AP 热点", icon:["M2 8.5a16 16 0 0 1 20 0", "M5 12a11 11 0 0 1 14 0", "M8.5 15.5a5.5 5.5 0 0 1 7 0", "M12 19h.01"], titles:["wifi-settings-title","ap-settings-title"]},
    {id:"carplay",title:"CarPlay",detail:"网络来源、画质、帧率与主页面布局",icon:["M3 5h18v12H3Z","M8 21h8M12 17v4"],titles:["carplay-settings-title"]},
    {id:"storage", title:"存储管理", detail:"外接存储与媒体写入恢复", icon:["M5 4h14l3 12v4H2v-4L5 4Z", "M2 16h20", "M6 18h.01M10 18h.01"], titles:["storage-settings-title","maintenance-settings-title"]},
    {id:"thermal", title:"散热与风扇", detail:"温度、风扇转速与场景策略", icon:["M14 12a2 2 0 1 1-4 0 2 2 0 0 1 4 0Z", "M12 10V5a3 3 0 0 1 6 0c0 3-3 4-4 5", "M14 12h5a3 3 0 0 1 0 6c-3 0-4-3-5-4", "M12 14v5a3 3 0 0 1-6 0c0-3 3-4 4-5", "M10 12H5a3 3 0 0 1 0-6c3 0 4 3 5 4"], titles:["fan-settings-title"]},
    {id:"security", title:"安全与账户", detail:"管理登录与账户安全", icon:["M12 3 3 7v6c0 5 9 9 9 9s9-4 9-9V7l-9-4Z", "m8 12 3 3 5-6"], titles:["security-title"]},
    {id:"system", title:"系统", detail:"系统更新、设备信息与电源", icon:["M4 7h7m4 0h5M4 17h3m4 0h9", "M15 7a2 2 0 1 1-4 0 2 2 0 0 1 4 0Z", "M11 17a2 2 0 1 1-4 0 2 2 0 0 1 4 0Z"], titles:["update-title","power-title"]},
  ];
  const drafts = new Set();
  const mobile = window.matchMedia("(max-width: 759px)");
  let selected = "network";
  let selectedSection = "";
  let backupTab = "history";
  let initialized = false;
  const sections = new Map();
  const functionDescriptions = {
    "carplay-settings-title": "网络来源、画质、帧率与主页面布局",
    "wifi-settings-title": "连接 WiFi，管理已保存的网络",
    "ap-settings-title": "设置热点名称、频段与自动启动",
    "storage-settings-title": "查看录像盘，管理空间与容量",
    "maintenance-settings-title": "诊断媒体写入异常，选择恢复方式",
    "fan-settings-title": "查看温度，调整风扇与场景策略",
    "security-title": "管理登录密码与账户安全",
    "update-title": "查看版本，检查并安装系统更新",
    "power-title": "查看设备信息，管理断开与电源",
  };
  const functionIcons = {
    "carplay-settings-title": ["M3 5h18v12H3Z","M8 21h8M12 17v4"],
    "wifi-settings-title": ["M5 12a11 11 0 0 1 14 0", "M8.5 15.5a5.5 5.5 0 0 1 7 0", "M12 19h.01"],
    "ap-settings-title": ["M12 13v8M8 21h8", "M14 11a2 2 0 1 1-4 0 2 2 0 0 1 4 0Z", "M7 7a6 6 0 0 0 0 8M17 7a6 6 0 0 1 0 8", "M4 4a10 10 0 0 0 0 14M20 4a10 10 0 0 1 0 14"],
    "storage-settings-title": ["M3 7h7l2 2h9v11H3V7Z", "M3 7V4h7l2 3h7v2"],
    "maintenance-settings-title": ["M14 6a5 5 0 0 0-6 6L3 17a3 3 0 0 0 4 4l5-5a5 5 0 0 0 6-6l-3 3-4-4 3-3Z"],
    "fan-settings-title": ["M9 14.5V5a3 3 0 0 1 6 0v9.5a5 5 0 1 1-6 0Z", "M12 8v10M17 6h3M17 10h2"],
    "security-title": ["M13 9a4 4 0 1 1 8 0 4 4 0 0 1-8 0Z", "m14 12-9 9H2v-3l9-9M7 16l2 2"],
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
  // 使用相同坐标系和线宽，避免字符图标随系统字体产生尺寸差异。
  function categoryIcon(paths) {
    const icon = node("span", "", "settings-category-icon");
    icon.setAttribute("aria-hidden", "true");
    const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    for (const [name, value] of Object.entries({viewBox:"0 0 24 24", fill:"none", stroke:"currentColor", "stroke-width":"1.75", "stroke-linecap":"round", "stroke-linejoin":"round", focusable:"false"})) svg.setAttribute(name, value);
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
    byID("settings-back").textContent = selectedSection && !singleFunction ? `‹ 返回${group.title}` : "‹ 返回设置";
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
        const notice = node("p", "有未保存的修改；切换标签会保留草稿，请在对应板块单独保存。", "draft-notice");
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
    events.append(node("h3", "事件处理记录"), node("p", "查看事件等待、任务创建与补传结果；自动上传规则在“上传规则”中配置。"), byID("nas-auto-events"));
    byID("backup-history-panel").append(events);
    selectBackup("history");
  }
  function mountExports() {
    const section = byID("export-settings-title").closest(".settings-section");
    section.id = "export-settings-section";
    section.removeAttribute("aria-labelledby");
    section.setAttribute("aria-label", "导出文件与保留设置");
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
    const labels = {queued:"正在排队",running:task.phase === "preparing" ? "正在准备录像" : "正在导出",completed:"导出完成",failed:"导出失败",canceled:"已取消"};
    root.append(node("strong", `${task.request?.mode === "source" ? "多路原画压缩包" : exportTaskLabel(task.request)} · ${labels[task.state] || task.state}`));
    root.append(node("p", task.error || `已完成 ${task.progress_percent || 0}%`, "muted"));
    const actions = node("div", "", "export-history-actions");
    if (["queued","running"].includes(task.state)) {
      const progress = node("progress"); progress.max = 100; progress.value = task.progress_percent || 0; progress.setAttribute("aria-label", "最近导出任务进度"); root.append(progress);
      const cancel = node("button", "取消任务", "secondary compact"); cancel.type = "button";
      cancel.addEventListener("click", async () => {
        cancel.disabled = true;
        try { renderExportTask(await request(`/api/v1/exports/${encodeURIComponent(task.id)}`, {method:"DELETE"})); }
        catch (error) { setSettingsNotice(error.message, true); cancel.disabled = false; }
      });
      actions.append(cancel);
    }
    if (task.state === "completed" && task.download_ready) {
      const download = node("a", "下载成品", "secondary compact"); download.href = `/api/v1/exports/${encodeURIComponent(task.id)}/download`; download.download = ""; actions.append(download);
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
      if (error.status === 404) { remember("teslabox-last-export-task", ""); lastExportTask = null; byID("export-live-task").replaceChildren(node("p", "任务已清理，可在下方查看仍可下载的历史成品。", "muted")); }
      else setSettingsNotice(`任务状态暂不可用：${error.message}`, true, false);
    } finally { exportRefreshBusy = false; }
  }
  function initialize() {
    if (initialized) return;
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
      menu.setAttribute("aria-label", `${group.title}功能`);
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
        const dirty = node("span", "", "settings-draft-dot"); dirty.hidden = true; dirty.setAttribute("aria-label", "有未保存的修改");
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
      const dirty = node("span", "", "settings-draft-dot"); dirty.hidden = true; dirty.setAttribute("aria-label", "有未保存的修改");
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
    const input = event.target;
    if (!input.matches("input,select,textarea")) return;
    if (["nas-target-form","nas-default-form","nas-quality-form","nas-automatic-form"].includes(input.form?.id)) drafts.add(input.form.id);
    else {
      const section = input.closest(".settings-section");
      const title = section?.querySelector("h2")?.id;
      if (title === "ap-settings-title" || title === "fan-settings-title") drafts.add(title);
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
