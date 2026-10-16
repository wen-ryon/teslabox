const test=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm');
class Element {
  constructor(tag='div'){this.tagName=tag;this.children=[];this.listeners={};this._text='';}
  set textContent(v){this._text=String(v);this.children=[];}
  get textContent(){return this._text+this.children.map(c=>c.textContent).join('');}
  append(...nodes){this.children.push(...nodes);}
  replaceChildren(...nodes){this._text='';this.children=[...nodes];}
  addEventListener(name,fn){this.listeners[name]=fn;}
}
function fixture(language='zh-CN'){
  const ids=Object.fromEntries(['shared-network-devices','shared-network-notice','shared-network-refresh'].map(id=>[id,new Element()]));
  const ctx=vm.createContext({document:{documentElement:{dataset:{deviceLanguage:language}},readyState:'loading',getElementById:id=>ids[id],createElement:tag=>new Element(tag),addEventListener(){}},TeslaBoxEthernet:{render(){}},request:async()=>({devices:[]})});
  ctx.TeslaBoxEnglish=JSON.parse(fs.readFileSync(__dirname+'/assets/i18n-en.json','utf8'));
  vm.runInContext(fs.readFileSync(__dirname+'/assets/i18n.js','utf8'),ctx);
  vm.runInContext(fs.readFileSync(__dirname+'/assets/shared-network.js','utf8'),ctx);
  return{ids,ctx,api:ctx.TeslaBoxSharedNetwork};
}
test('移动与共享网络保留真实配置文本，获取地址不能显示已联网',()=>{
  const f=fixture('en');f.api.render({devices:[{kind:'bluetooth',interface:'bnep0',state:'connecting',connection_name:'<img src=x>中文名称',ipv4:{addresses:[]},notice:'此连接由 CarPlay 管理，请在 CarPlay 设置中调整网络来源'}]});
  const text=f.ids['shared-network-devices'].textContent;
  assert.match(text,/Bluetooth tethering/);assert.match(text,/Connecting/);assert.match(text,/No IP address/);assert.match(text,/CarPlay/);assert.match(text,/<img src=x>中文名称/);
  assert.equal(f.ids['shared-network-devices'].children[0].children.some(el=>el.tagName==='img'),false);
  assert.doesNotMatch(text,/Internet available/);
});
test('USB 只使用移动栏中的已有编辑器，不重复建卡，蓝牙无配对或抢占按钮',()=>{
  const f=fixture();f.api.render({devices:[{kind:'usb',interface:'usb0',state:'connected'}]});assert.equal(f.ids['shared-network-devices'].children.length,0);
  f.api.render({devices:[{kind:'cellular',interface:'cdc-wdm0',state:'unmanaged'}]});assert.match(f.ids['shared-network-devices'].textContent,/未由 NetworkManager 管理/);
  assert.equal(f.ids['shared-network-devices'].children[0].children.some(el=>el.tagName==='button'),false);
  const html=fs.readFileSync(__dirname+'/assets/index.html','utf8');assert.match(html,/id="shared-usb-trial"/);assert.match(html,/id="shared-usb-confirm"/);
});
test('共享设备消失时清理旧卡，读取失败显式显示，不提交网络变更',()=>{
  const f=fixture();f.api.render({devices:[]});assert.match(f.ids['shared-network-devices'].textContent,/未检测到/);
  f.api.unavailable('读取失败');assert.equal(f.ids['shared-network-notice'].textContent,'读取失败');
});

test('共享网络使用相同状态卡结构，连接中不显示绿色成功状态',()=>{
  const f=fixture();f.api.render({devices:[{kind:'bluetooth',interface:'bnep0',state:'connecting',connection_name:'手机网络',ipv4:{addresses:[]}}]});
  const card=f.ids['shared-network-devices'].children[0],overview=card.children.find(el=>el.className==='hardware-overview network-overview');
  assert.equal(card.className,'network-device');assert.equal(overview.children.length,10);
  assert.deepEqual(overview.children.slice(0,4).map(item=>item.children[0].textContent),['连接状态','网络接口','网络类型','IP 地址']);
  assert.equal(overview.children[0].children[1].className,'hardware-value');
  assert.match(overview.textContent,/尚未取得 IP 地址/);
  f.api.render({devices:[{kind:'cellular',interface:'wwan0',state:'connected',connection_name:'<b>数据</b>',ipv4:{addresses:['10.1.2.3/24']},default_ipv4:true}]});
  const connected=f.ids['shared-network-devices'].children[0].children.find(el=>el.className==='hardware-overview network-overview');
  assert.equal(connected.children[0].children[1].className,'hardware-value ok');
  assert.match(connected.textContent,/10\.1\.2\.3\/24/);assert.match(connected.textContent,/IPv4/);
  assert.equal(connected.children[4].children[1].children.length,0);
  f.api.unavailable('读取失败');assert.doesNotMatch(f.ids['shared-network-devices'].textContent,/10\.1\.2\.3|已连接/);
});

test('共享网络展示实际双栈网关 DNS 和 MTU，蓝牙不捏造理论速率，断开清理旧指标',()=>{
  const f=fixture('en');
  const device={kind:'bluetooth',interface:'bnep0',state:'connected',mtu:1500,
    ipv4:{addresses:['172.20.10.2/28'],gateway:'172.20.10.1',dns:['1.1.1.1']},ipv6:{gateway:'fe80::1',dns:['1.1.1.1','2001:4860:4860::8888']}};
  f.api.render({devices:[device]});
  const fields=()=>Object.fromEntries(f.ids['shared-network-devices'].children[0].children[1].children.map(item=>[item.children[0].textContent,item.children[1].textContent]));
  assert.equal(fields()['Link speed'],'Not reported by system');assert.equal(fields().MTU,'1500');
  assert.equal(fields().Gateway,'172.20.10.1\nfe80::1');assert.equal(fields().DNS,'1.1.1.1\n2001:4860:4860::8888');
  f.api.render({devices:[{...device,state:'disconnected',speed:1000}]});
  assert.equal(fields()['Link speed'],'—');assert.equal(fields().Gateway,'—');assert.equal(fields().DNS,'—');
  assert.deepEqual(Array.from(f.ctx.TeslaBoxI18n.missing()),[]);
});

test('蓝牙 MAC 标识不进入标题或接口卡片，真实 PAN 接口仍可辨识',()=>{
  const f=fixture();f.api.render({devices:[{kind:'bluetooth',interface:'E4:9C:67:18:0B:FB',state:'disconnected'}]});
  let card=f.ids['shared-network-devices'].children[0],heading=card.children[0];
  assert.equal(heading.className,'network-device-heading network-heading-bluetooth');
  assert.equal(heading.textContent,'蓝牙网络共享');assert.doesNotMatch(card.textContent,/E4:9C:67:18:0B:FB/);
  assert.equal(card.children[1].children[1].children[1].textContent,'—');
  f.api.render({devices:[{kind:'bluetooth',interface:'bnep0',state:'connected',connection_name:'手机'}]});
  card=f.ids['shared-network-devices'].children[0];heading=card.children[0];
  assert.equal(heading.children[0].textContent,'蓝牙网络共享');assert.equal(heading.children[1].textContent,'bnep0');
  assert.equal(card.children[1].children[1].children[1].textContent,'bnep0');
});
