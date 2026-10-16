const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/assets/app.js', 'utf8');

function model(wifi) {
  const context = vm.createContext({});
  vm.runInContext(source.slice(source.indexOf('function wifiConnectionState('), source.indexOf('function renderWiFiSettings(')), context);
  return JSON.parse(JSON.stringify(context.partitionWiFiNetworks(wifi)));
}

test('三栏按可见性及保存状态划分，不重复展示已保存网络', () => {
  const result = model({connection_state:'connected', connection_uuid:'b', current_connection:'Home',
    saved_networks:[{uuid:'a',ssid:'Office'},{uuid:'b',ssid:'Home',active:true},{uuid:'c',ssid:'Away'}],
    available_networks:[{ssid:'Home',signal:50},{ssid:'Office',signal:90},{ssid:'Cafe',signal:70}]});
  assert.deepEqual(result.known.map(n => n.ssid), ['Home','Office']);
  assert.deepEqual(result.remaining.map(n => n.ssid), ['Away']);
  assert.deepEqual(result.available.map(n => n.ssid), ['Cafe']);
});

test('重复或过时的活动标记不能产生两个已连接行', () => {
  const result = model({connection_state:'connected',connection_uuid:'b',current_connection:'New',
    saved_networks:[{uuid:'a',ssid:'Old',active:true},{uuid:'b',ssid:'New',active:true}],available_networks:[{ssid:'Old'},{ssid:'New'}]});
  assert.deepEqual(result.known.filter(n => n.active).map(n => n.uuid), ['b']);
});

test('缺失状态、缺失 UUID 或未知新状态不能靠 SSID 及旧标记猜测已连接',()=>{
  for(const input of [{},{connection_state:'connected'},{connection_state:'future',connection_uuid:'a'}]) {
    const result=model({...input,current_connection:'Home',saved_networks:[{uuid:'a',ssid:'Home',active:true}],available_networks:[{ssid:'Home'}]});
    assert.equal(result.known.filter(value=>value.active).length,0);
  }
});

test('状态卡片与三栏共用连接判定，响应不完整时显示状态待确认',()=>{
  const nodes=new Map(),get=id=>{if(!nodes.has(id))nodes.set(id,{textContent:'',className:'',hidden:false,disabled:false,title:''});return nodes.get(id);};
  const context=vm.createContext({$:get,t:require('./i18n_test_helper.cjs')().t,renderSavedNetworks(){},renderAvailableNetworks(){},partitionWiFiNetworks:()=>({available:[]})});
  const helper=source.slice(source.indexOf('function wifiConnectionState('),source.indexOf('function partitionWiFiNetworks('));
  const renderer=source.slice(source.indexOf('function renderWiFiSettings('),source.indexOf('function renderWiFiUnavailable('));
  vm.runInContext(helper+renderer,context);
  for(const input of [{},{connection_state:'connected'},{connection_state:'future',connection_uuid:'a'}]) {
    context.renderWiFiSettings({available:true,current_connection:'Home',...input});
    assert.equal(get('wifi-status').textContent,'状态待确认');assert.doesNotMatch(get('wifi-status').className,/\bok\b/);
  }
  context.renderWiFiSettings({available:true,current_connection:'Home',connection_state:'connected',connection_uuid:'a'});
  assert.equal(get('wifi-status').textContent,'已连接 · Home');assert.match(get('wifi-status').className,/\bok\b/);
});

test('获取地址、未知及断开状态不显示已连接，已连但不在扫描缓存中仍保留', () => {
  for (const state of ['connecting','unknown','disconnected']) {
    const result = model({connection_state:state,connection_uuid:'a',saved_networks:[{uuid:'a',ssid:'Home',active:true}],available_networks:[]});
    assert.equal([...result.known,...result.remaining].filter(n => n.active).length, 0);
    if (state === 'connecting') assert.equal(result.known[0].connecting, true);
  }
  const result = model({connection_state:'connected',connection_uuid:'a',saved_networks:[{uuid:'a',ssid:'Home',active:true}],available_networks:[]});
  assert.equal(result.known[0].active, true);
});

test('WiFi 摘要使用实际运行指标，零信号不丢失，连接切换和读取失败不保留旧详情',()=>{
  const language=require('./i18n_test_helper.cjs')('en'),nodes=new Map();
  const get=id=>{if(!nodes.has(id))nodes.set(id,{textContent:'',className:'',hidden:false});return nodes.get(id);};
  const context=vm.createContext({$:get,t:language.t,renderSavedNetworks(){},renderAvailableNetworks(){},partitionWiFiNetworks:()=>({available:[]})});
  vm.runInContext(source.slice(source.indexOf('function wifiConnectionState('),source.indexOf('function partitionWiFiNetworks('))+
    source.slice(source.indexOf('function renderWiFiSettings('),source.indexOf('function renderSavedNetworks(')),context);
  const connected={available:true,interface:'sta0',connection_state:'connected',connection_uuid:'a',current_connection:'Home',
    details:{available:true,bitrate_kbps:866700,frequency_mhz:5220,signal:0,mtu:1500,addresses:['192.168.1.2/24','2001:db8::2/64'],gateways:['192.168.1.1','fe80::1'],dns:['1.1.1.1','1.1.1.1']}};
  context.renderWiFiSettings(connected);
  assert.equal(get('wifi-link-speed').textContent,'866.7 Mbps');assert.equal(get('wifi-frequency').textContent,'5220 MHz');
  assert.equal(get('wifi-signal').textContent,'0%');assert.equal(get('wifi-mtu').textContent,'1500');
  assert.equal(get('wifi-gateway').textContent,'192.168.1.1\nfe80::1');assert.equal(get('wifi-dns').textContent,'1.1.1.1');
  assert.match(get('wifi-ip-address').textContent,/2001:db8/);
  context.renderWiFiSettings({...connected,details:{available:false}});
  assert.equal(get('wifi-gateway').textContent,'Unknown');assert.equal(get('wifi-link-speed').textContent,'Unknown');
  context.renderWiFiSettings({...connected,connection_state:'connecting'});
  for(const name of ['link-speed','frequency','signal','gateway','dns','mtu']) assert.equal(get('wifi-'+name).textContent,'—');
  context.renderWiFiSettings(connected);context.renderWiFiUnavailable('offline');
  for(const name of ['link-speed','frequency','signal','gateway','dns','mtu']) assert.equal(get('wifi-'+name).textContent,'—');
  assert.deepEqual(Array.from(language.missing()),[]);
});

test('WiFi AP 和动态设备共用宽屏五列两行布局，不影响其他诊断摘要',()=>{
  const html=fs.readFileSync(__dirname+'/assets/index.html','utf8'),css=fs.readFileSync(__dirname+'/assets/settings-layout.css','utf8');
  const wifi=html.slice(html.indexOf('aria-label="WiFi 硬件信息"'),html.indexOf('id="wifi-warning"'));
  assert.equal((wifi.match(/<strong /g)||[]).length,10);
  assert.match(html,/class="hardware-overview network-overview" aria-label="AP 热点硬件信息"/);
  assert.match(css,/@media \(min-width: 1001px\)[^}]+\.hardware-overview\.network-overview[^}]+repeat\(5/);
});

test('当前网络可以断开或忘记，取消操作不发送请求，确认包含精确UUID', async () => {
  for (const action of ['forgetWiFi','disconnectWiFi']) {
    for (const confirm of [false,true]) {
      const calls = [], questions = [];
      const context = vm.createContext({t:require('./i18n_test_helper.cjs')().t,askConfirmation:async (...args) => {questions.push(args);return confirm;},
        postJSON:async (path,body) => calls.push({path,body}),setSettingsNotice(){},loadSettings:async()=>{}});
      const start = source.indexOf('async function ' + action + '(');
      const end = source.indexOf('\n}', start) + 2;
      vm.runInContext(source.slice(start,end), context);
      await context[action]({uuid:'a',ssid:'Home',active:true});
      assert.match(questions[0][1], /网线/);
      assert.equal(calls.length, confirm ? 1 : 0);
      if (confirm) assert.equal(calls[0].body.uuid, 'a');
    }
  }
});

test('已保存栏目放在最后，已连接按钮不再被永久禁用', () => {
  const html = fs.readFileSync(__dirname + '/assets/index.html', 'utf8');
  assert.ok(html.indexOf('known-network-list') < html.indexOf('available-network-list'));
  assert.ok(html.indexOf('available-network-list') < html.indexOf('saved-network-list'));
  const rows = source.slice(source.indexOf('function renderSavedNetworkRows('),source.indexOf('function toggleNetworkPanel('));
  assert.doesNotMatch(rows,/forget\.disabled\s*=\s*network\.active/);
  assert.doesNotMatch(rows,/connect\.disabled\s*=.*network\.active/);
  assert.match(rows,/disconnectWiFi\(network\)/);
});

test('英文断开和忘记提示保留真实 SSID，UUID 不受语言影响',async()=>{
  for (const action of ['forgetWiFi','disconnectWiFi']) {
    const language=require('./i18n_test_helper.cjs')('en'),calls=[],questions=[];
    const context=vm.createContext({t:language.t,askConfirmation:async(...args)=>{questions.push(args);return true;},
      postJSON:async(path,body)=>calls.push({path,body}),setSettingsNotice(){},loadSettings:async()=>{}});
    const start=source.indexOf('async function '+action+'('),end=source.indexOf('\n}',start)+2;
    vm.runInContext(source.slice(start,end),context);
    await context[action]({uuid:'fixed-uuid',ssid:'已连接{1}<img src=x>',active:true});
    assert.match(questions[0][1],/已连接\{1\}<img src=x>/);
    assert.match(questions[0][0],/Forget|Disconnect/);
    assert.equal(calls[0].body.uuid,'fixed-uuid');
    assert.deepEqual(Array.from(language.missing()),[]);
  }
});
