const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/assets/wireless-region.js', 'utf8');
function setup(confirm=true) {
  const nodes = new Map();
  const get = id => { if (!nodes.has(id)) nodes.set(id, {value:'', textContent:'', disabled:false, events:{}, addEventListener(name,fn){this.events[name]=fn;}}); return nodes.get(id); };
  const calls = [];
  const context = vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,document:{getElementById:get},state:{},askConfirmation:async()=>confirm,setSettingsNotice(){},postJSON:async(url,input)=>{calls.push({url,input});return {configured_country:input.country,effective_country:'00',state:'pending',message:'未生效'};}});
  vm.runInContext(source,context);
  return {get,calls,context};
}
test('保存的国家码不冒充实际全局或自管理 PHY',()=>{
  const {get,context}=setup();
  context.TeslaBoxWirelessRegion.render({configured_country:'CN',effective_country:'00',state:'pending',phy_domains:[{phy:'phy#0',country:'US',self_managed:true}]});
  assert.equal(get('wireless-region-configured').textContent,'CN');
  assert.equal(get('wireless-region-effective').textContent,'00');
  assert.match(get('wireless-region-detail').textContent,/尚未生效/);
  assert.match(get('wireless-region-phys').textContent,/US（网卡自管理）/);
});
test('刷新不覆盖尚未保存的地区草稿',()=>{
  const {get,context}=setup();get('wireless-region-country').value='JP';get('wireless-region-country').events.input();
  context.TeslaBoxWirelessRegion.render({configured_country:'CN',effective_country:'CN',state:'applied'});
  assert.equal(get('wireless-region-country').value,'JP');
});
test('读取失败后清除过时的实际监管域',()=>{
  const {get,context}=setup();
  context.TeslaBoxWirelessRegion.render({configured_country:'CN',effective_country:'CN',state:'applied'});
  context.TeslaBoxWirelessRegion.unavailable('连接中断');
  assert.equal(get('wireless-region-effective').textContent,'未知');
  assert.equal(get('wireless-region-phys').textContent,'');
  assert.match(get('wireless-region-detail').textContent,/未确认实际生效/);
});
test('用户取消确认时不更改网络',async()=>{
  const {get,calls}=setup(false);get('wireless-region-country').value='CN';await get('wireless-region-form').events.submit({preventDefault(){}});assert.equal(calls.length,0);
});
test('提交明确地区及确认，运行失败显示待生效而非成功',async()=>{
  const {get,calls}=setup();get('wireless-region-country').value='jp';await get('wireless-region-form').events.submit({preventDefault(){}});
  assert.equal(calls[0].input.country,'JP');assert.equal(calls[0].input.confirm_network_change,true);
  assert.match(get('wireless-region-detail').textContent,/尚未生效/);assert.equal(get('wireless-region-save').disabled,false);
});
