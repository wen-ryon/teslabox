"use strict";
const {test} = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const html = fs.readFileSync(path.join(__dirname, "assets", "index.html"), "utf8");
const source = fs.readFileSync(path.join(__dirname, "assets", "settings-layout.js"), "utf8");
const app = fs.readFileSync(path.join(__dirname, "assets", "app.js"), "utf8");

// 对完整初始化脚本运行最小 DOM 模型，验证节点移动和路由，不冒充浏览器截图验收。
function environment({hash = "", saved = "", mobile = false} = {}) {
  class Node {
    constructor(tag) { this.tagName = tag; this.children = []; this.dataset = {}; this.listeners = {}; this.className = ""; this.id = ""; this.hidden = false; }
    get classList() { return {toggle:(name, on) => {const set = new Set(this.className.split(/\s+/).filter(Boolean)); if (on) set.add(name); else set.delete(name); this.className = [...set].join(" ");}, contains:name => this.className.split(/\s+/).includes(name), remove:name => this.classList.toggle(name, false)}; }
    append(...children) { for (const child of children) { if (child.parent) child.parent.children = child.parent.children.filter(node => node !== child); child.parent = this; this.children.push(child); } }
    replaceChildren(...children) { for (const child of this.children) child.parent = null; this.children = []; this.append(...children); }
    setAttribute(key, value) { if (key === "class") this.className = value; else this[key] = value; }
    removeAttribute(key) { delete this[key]; }
    addEventListener(name, callback) { this.listeners[name] = callback; }
    focus() {}
    matches(selector) { return selector.split(",").some(value => {value = value.trim(); if (value.startsWith(".")) return this.classList.contains(value.slice(1)); const match = value.match(/^\[data-([a-z-]+)(?:="([^"]+)")?\]$/); if (match) {const key = match[1].replace(/-([a-z])/g, (_,c) => c.toUpperCase()); return this.dataset[key] !== undefined && (match[2] === undefined || this.dataset[key] === match[2]);} return this.tagName === value;}); }
    closest(selector) { return this.matches(selector) ? this : this.parent?.closest(selector); }
    querySelectorAll(selector) { const all = this.children.flatMap(node => [node, ...node.querySelectorAll("*")]); return selector === "*" ? all : all.filter(node => node.matches(selector)); }
    querySelector(selector) { return this.querySelectorAll(selector)[0] || null; }
  }
  const root = new Node("body");
  const add = (parent, tag, id, cls = "") => {const node = new Node(tag); node.id = id; node.className = cls; parent.append(node); return node;};
  const settings = add(root, "div", "settings-page", "settings-page");
  const content = add(settings, "div", "settings-content", "settings-content");
  const tabs = add(settings, "nav", "settings-tabs");
  for (const id of ["settings-back", "settings-detail-title", "settings-draft-notice"]) add(settings,"div",id);
  const sections = new Map();
  for (const match of html.matchAll(/<section([^>]*class="[^"]*settings-section[^>]*aria-labelledby="([^"]+)"[^>]*)>/g)) {
    const title = match[2], id = match[1].match(/\bid="([^"]+)"/)?.[1];
    if (!id || !title.endsWith("settings-title") && !["security-title","update-title","power-title"].includes(title)) continue;
    const section = add(content, "section", id, "settings-section"); sections.set(id, section);
    const heading = add(section,"h2",title); heading.textContent = title;
  }
  for (const id of ["fan-mode-automatic", "fan-mode-manual"]) add(sections.get("settings-panel-6"),"button",id);
  const retain = add(sections.get("settings-panel-5"),"select","export-retain-count");
  const store = new Map(saved ? [["teslabox-settings-tab",saved]] : []);
  const location = {pathname:"/settings",hash};
  const document = {getElementById:id => [root,...root.querySelectorAll("*")].find(node => node.id === id) || null, createElement:tag => new Node(tag), createElementNS:(_,tag) => new Node(tag), querySelectorAll:selector => root.querySelectorAll(selector), addEventListener(name,callback) {this[name] = callback;}};
  const context = vm.createContext({document,window:{location,matchMedia:() => ({matches:mobile,addEventListener(){}}),history:{pushState(_,unused,url){const [pathname,hash=""] = url.split("#");location.pathname=pathname;location.hash=hash ? "#"+hash : "";}}},localStorage:{getItem:key => store.get(key),setItem:(key,value) => store.set(key,value)}});
  vm.runInContext(source,context);
  return {document,context,layout:context.TeslaBoxSettingsLayout,sections,store,tabs,content,retain};
}

test("完整初始化保留导出节点，CarPlay 位于网络下面，重复初始化不重复菜单", () => {
  const model = environment();
  model.layout.initialize(); model.layout.initialize();
  assert.deepEqual(model.tabs.children.map(node => node.dataset.settingsGroup),["network","carplay","storage","thermal","security","system"]);
  assert.equal(model.sections.get("settings-panel-5").parent.id,"settings-group-storage");
  assert.equal(model.document.getElementById("export-retain-count"),model.retain);
});
test("旧设置 localStorage 区段 ID 恢复导出，旧区段书签同样有效", () => {
  for (const options of [{saved:"settings-panel-5"},{hash:"#settings-panel-5"},{hash:"#settings-tab=settings-panel-5"},{hash:"#storage/settings-panel-5"}]) {
    const model = environment(options);model.layout.initialize();
    assert.equal(model.sections.get("settings-panel-5").hidden,false,JSON.stringify(options));
    assert.equal(model.store.get("teslabox-settings-tab"),"settings-panel-5");
  }
});
test("CarPlay URL 与程序选择都打开实际设置区段，旧存储跳转仍有效", () => {
  const model = environment({hash:"#carplay"});model.layout.initialize();
  assert.equal(model.sections.get("carplay-settings-section").hidden,false);
  model.layout.selectSection("settings-panel-5");
  assert.equal(model.sections.get("settings-panel-5").hidden,false);
  model.layout.selectSection("storage-settings-section");
  assert.equal(model.sections.get("storage-settings-section").hidden,false);
  model.layout.selectSection("carplay-settings-section");
  assert.equal(model.sections.get("carplay-settings-section").hidden,false);
});
test("导出草稿与原节点在分类切换中保留，保存通知清除草稿", () => {
  const model = environment();model.layout.initialize();
  model.retain.value = "12"; model.document.change({target:model.retain});
  model.layout.selectSection("carplay-settings-section");model.layout.selectSection("settings-panel-5");
  assert.equal(model.retain.value,"12");assert.equal(model.layout.isDirty("export-settings-title"),true);
  model.layout.markClean("export-settings-title");assert.equal(model.layout.isDirty("export-settings-title"),false);
});
test("手机版旧导出书签直接显示详情而非隐藏菜单", () => {
  const model = environment({mobile:true,hash:"#settings-panel-5"});model.layout.initialize();
  assert.equal(model.sections.get("settings-panel-5").hidden,false);
  assert.equal(model.document.getElementById("settings-page").classList.contains("settings-detail-open"),true);
});
test("app 初始化、设置跳转与认证后加载使用实际模块钩子", () => {
  const body = name => app.match(new RegExp("^function "+name+"\\([^\\n]*\\{.*?^}","ms"))[0];
  const calls = [];const nodes = new Map();
  const lookup = id => {if (!nodes.has(id)) nodes.set(id,{hidden:false});return nodes.get(id);};
  const context = vm.createContext({globalThis:{TeslaBoxSettingsLayout:{initialize:() => calls.push("init"),enterPage:() => calls.push("enter"),selectSection:id => calls.push(id)},TeslaBoxCarPlaySettings:{load:() => calls.push("load")}},$ : lookup,loginView:lookup("login-view"),appView:lookup("app-view"),state:{authEnabled:true},document:{querySelectorAll:() => []},window:{history:{pushState(){}},scrollTo(){}},closeNavigation(){},closeAppearanceMenu(){},loadSettings:() => calls.push("settings"),loadMediaAssets(){},loadLogs(){},scheduleLogsRefresh(){}});
  for (const name of ["initializeSettingsTabs","activateSettingsTab","showApp","showPage"]) vm.runInContext(body(name),context);
  context.initializeSettingsTabs();context.activateSettingsTab("settings-panel-5");context.showApp();context.showPage("settings",false);
  assert.deepEqual(calls,["init","settings-panel-5","load","enter","settings"]);
});
test("导出区域保持数据盘语义和既有 API，未混入新 workspace 或录像缓存", () => {
  assert.ok(html.includes("导出完成后会保存在数据盘"));
  assert.ok(html.includes('id="export-retain-count"'));
  assert.ok(app.includes('request("/api/v1/export-settings"'));
  assert.ok(app.includes('method:"POST", headers:{"Content-Type":"application/json"}'));
  for (const text of [source,html,app]) for (const marker of ["nas-upload", "exports-page", "cache_idle_hours", "playbackTraceMarker"]) assert.ok(!text.includes(marker),marker);
});
