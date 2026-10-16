"use strict";
const {test} = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const source = fs.readFileSync(path.join(__dirname,"assets/app.js"),"utf8");
const language = require('./i18n_test_helper.cjs')();
test("全部设置分类的功能图标都能实际构造，CarPlay 不阻断管理页初始化", () => {
  const layout = fs.readFileSync(path.join(__dirname,"assets/settings-layout.js"),"utf8");
  const element = () => ({setAttribute(){},append(){}});
  const context = vm.createContext({t:require('./i18n_test_helper.cjs')('zh-CN').t,TeslaBoxI18n:language,window:{matchMedia:() => ({})},document:{createElement:element,createElementNS:element}});
  vm.runInContext(layout.replace("function read(key, fallback)", "globalThis.layoutProbe={groups,functionIcons,functionDescriptions,categoryIcon};\n  function read(key, fallback)"),context);
  for (const group of context.layoutProbe.groups) {
    for (const id of group.titles) {
      assert.ok(context.layoutProbe.functionDescriptions[id], `${id} 缺少说明`);
      assert.doesNotThrow(() => context.layoutProbe.categoryIcon(context.layoutProbe.functionIcons[id]), id);
    }
  }
});
test("图标保留清晰造型，所有入口统一跟随主题而非固定彩色", () => {
  const layout = fs.readFileSync(path.join(__dirname,"assets/settings-layout.js"),"utf8");
  const element = () => ({attributes:{},children:[],setAttribute(key,value){this.attributes[key]=value;},append(value){this.children.push(value);}});
  const context = vm.createContext({TeslaBoxI18n:language,window:{matchMedia:() => ({})},document:{createElement:element,createElementNS:element}});
  vm.runInContext(layout.replace("function read(key, fallback)", "globalThis.iconProbe={groups,functionIcons,categoryIcon};\n  function read(key, fallback)"),context);
  const {groups,functionIcons,categoryIcon}=context.iconProbe;
  for(const group of groups){
    for(const id of group.titles){
      const icon=categoryIcon(functionIcons[id]);
      assert.equal(icon.attributes['aria-hidden'],'true');
      assert.equal(icon.attributes['data-icon-tone'],undefined);
      assert.equal(icon.children[0].attributes.viewBox,'0 0 24 24');
      assert.equal(icon.children[0].attributes.fill,id==='wifi-settings-title'?'currentColor':'none');
      assert.ok(icon.children[0].children.every(path=>path.attributes.d));
    }
  }
  assert.equal(categoryIcon(groups[0].icon,'network').children[0].attributes.fill,'currentColor');
  const css=fs.readFileSync(path.join(__dirname,'assets/settings-layout.css'),'utf8');
  assert.match(css,/\.settings-category-icon, \.settings-function-icon[^}]+color: var\(--accent\); background: var\(--accent-soft\)/);
  assert.doesNotMatch(css,/\.settings-function-entry:hover \.settings-function-icon/);
  assert.doesNotMatch(css,/data-icon-tone/);
});
function extract(name) {
  const start = source.indexOf(`function ${name}(`);
  return source.slice(start,source.indexOf("\n}",start)+2);
}
function retentionModel(dirty) {
  const controls = new Map();
  for (const id of ["export-delete-after-download","export-cache-idle-hours","export-retain-count","export-settings-save","export-history-refresh","export-retention-status","export-settings-detail","export-history-list"]) {
    controls.set(id,{value:"1",checked:false,options:[1,3,5,8,12,20].map(value => ({value:String(value)})),replaceChildren(){},append(){}});
  }
  const state = {exportSettings:{delete_after_download:false,retain_count:8,cache_idle_hours:24},exportHistory:[]};
  const context = vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:language.t,state,$:id => controls.get(id),TeslaBoxSettingsLayout:{isDirty:() => dirty},document:{createElement:() => ({})}});
  vm.runInContext(extract("renderExportRetentionControls")+"\n"+extract("renderExportSettings"),context);
  return {controls,state,context};
}
test("保留成品策略可正常显示并独立读取缓存留存时长", () => {
  const {controls,context} = retentionModel(false);
  context.renderExportSettings();
  assert.equal(controls.get("export-retain-count").value,"8");
  assert.equal(controls.get("export-cache-idle-hours").value,"24");
  assert.equal(controls.get("export-retention-status").textContent,"保留 8 个");
  assert.equal(controls.get("export-retain-count").disabled,false);
});
test("刷新历史和重新进入导出页保留未保存的策略草稿", () => {
  const {controls,context} = retentionModel(true);
  controls.get("export-delete-after-download").checked = true;
  controls.get("export-retain-count").value = "12";
  controls.get("export-cache-idle-hours").value = "48";
  context.renderExportSettings();
  assert.equal(controls.get("export-delete-after-download").checked,true);
  assert.equal(controls.get("export-retain-count").value,"12");
  assert.equal(controls.get("export-cache-idle-hours").value,"48");
  assert.equal(controls.get("export-retention-status").textContent,"保留 8 个");
});
test("刷新设备状态不覆盖风扇草稿的模式和手动档位", async () => {
  let dirty = true, rendered = 0;
  const draft = {temperature_mode:"manual",temperature_levels:[{temperature_c:42,percent:35}]};
  const state = {fan:draft};
  const noop = () => {};
  const context = vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:language.t,state,settingsLoadSequence:0,$:() => ({}),setRequestControlLoading:() => noop,setSettingsNotice:noop,
    request:async path => path === "/api/v1/fan" ? {temperature_mode:"automatic"} : {},
    TeslaBoxSettingsLayout:{isDirty:() => dirty},renderFanSettings:() => rendered++,
    renderWiFiSettings:noop,renderWiFiUnavailable:noop,renderAPSettings:noop,renderAPUnavailable:noop,renderStorageDevices:noop,renderStorageUnavailable:noop,renderUpdate:noop,renderUpdateUnavailable:noop,renderMaintenanceSettings:noop,renderMaintenanceUnavailable:noop,renderSnapshotReserveStatus:noop,renderFanUnavailable:noop,
  });
  vm.runInContext("async "+extract("loadSettings"),context);
  await context.loadSettings();
  assert.equal(state.fan,draft);
  assert.equal(rendered,0);
  dirty = false;
  await context.loadSettings();
  assert.equal(state.fan.temperature_mode,"automatic");
  assert.equal(rendered,1);
});
