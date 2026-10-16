const test = require('node:test'), assert = require('node:assert/strict'), fs = require('node:fs'), vm = require('node:vm');
const source = fs.readFileSync(__dirname+'/assets/ethernet.js','utf8');

class Element {
  constructor(tag='div') {this.tagName=tag;this.children=[];this.listeners={};this.value='';this.name='';this._text='';this.disabled=false;this.checked=false;}
  set textContent(value) {this._text=String(value);this.children=[];}
  get textContent() {return this._text+this.children.map(v=>v.textContent).join('');}
  append(...nodes) {for(const n of nodes){n.parent=this;this.children.push(n);}}
  replaceChildren(...nodes) {this.children=[];this._text='';this.append(...nodes);}
  addEventListener(name,fn) {(this.listeners[name] ||= []).push(fn);}
  async fire(name,event={}) {for(const fn of this.listeners[name]||[]) await fn({target:this,preventDefault(){},...event});}
  get options() {return this.children;}
  get elements() {return {namedItem:name=>this.all().find(v=>v.name===name)};}
  all() {return this.children.flatMap(v=>[v,...v.all()]);}
  remove() {if(this.parent)this.parent.children=this.parent.children.filter(v=>v!==this);}
}

function fixture(language='zh-CN') {
  const ids=Object.fromEntries(['ethernet-devices','ethernet-trial','ethernet-trial-detail','ethernet-notice','ethernet-refresh','ethernet-confirm','ethernet-rollback','shared-usb-devices','shared-network-notice','shared-usb-trial','shared-usb-trial-detail','shared-usb-confirm','shared-usb-rollback'].map(id=>[id,new Element()]));
  let confirm=true, requests=[], questions=[], saved=[];
  const settings={name:'<img src=x> 未翻译的用户配置',auto_connect:true,mtu:0,auto_negotiate:false,speed:0,duplex:'',wake_on_lan:1,
    ipv4:{method:'auto',addresses:[],gateway:'',dns:[],route_metric:-1},ipv6:{method:'auto',addresses:[],gateway:'',dns:[],route_metric:-1}};
  const data={devices:[{interface:'eth0',managed:true,state:'connected',connection_uuid:'uuid-a',profiles:[{uuid:'uuid-a',active:true,editable:true,settings}],
    ipv4:{addresses:['192.168.3.1/24'],dns:['223.5.5.5']},ipv6:{addresses:[],dns:[]},link_modes:[],hardware_notice:'能力未知'}]};
  const context=vm.createContext({document:{documentElement:{dataset:{deviceLanguage:language}},readyState:'loading',getElementById:id=>ids[id],createElement:tag=>new Element(tag),addEventListener(){},hidden:false},
    window:{clearTimeout(){},setTimeout(){return 1;}},Date,
    askConfirmation:async(...args)=>{questions.push(args);return confirm;},
    request:async()=>structuredClone(data),postJSON:async(path,body)=>{requests.push({path,body:JSON.parse(JSON.stringify(body))});return {};},
    TeslaBoxSettingsLayout:{markClean:id=>saved.push(id)}});
  context.TeslaBoxEnglish=JSON.parse(fs.readFileSync(__dirname+'/assets/i18n-en.json','utf8'));
  vm.runInContext(fs.readFileSync(__dirname+'/assets/i18n.js','utf8'),context);
  vm.runInContext(source,context);context.TeslaBoxEthernet.render(structuredClone(data));
  const form=()=>ids['ethernet-devices'].all().find(v=>v.tagName==='form');
  return {ids,context,data,requests,questions,saved,form,setConfirm:value=>{confirm=value;}};
}

test('硬件未知时只保留旧值，连接名只作为文本，活动配置走试用流程',async()=>{
  const f=fixture(),form=f.form();
  assert.equal(form.elements.namedItem('link_mode').disabled,true);
  assert.equal(form.elements.namedItem('wake_on_lan').disabled,true);
  assert.match(form.textContent,/<img src=x>/);
  assert.equal(form.all().filter(v=>v.tagName==='img').length,0);
  await form.fire('submit');
  assert.equal(f.requests[0].path,'/api/v1/network/ethernet');
  assert.equal(f.requests[0].body.apply,true);assert.equal(f.requests[0].body.uuid,'uuid-a');
  assert.match(f.questions[0][1],/120 秒/);
});

test('取消确认不发送任何写请求；手动 IPv4/IPv6 与 DNS 可序列化',async()=>{
  const f=fixture(),form=f.form();f.setConfirm(false);await form.fire('submit');assert.equal(f.requests.length,0);
  const v=name=>form.elements.namedItem(name);
  v('ipv4.method').value='manual';v('ipv4.addresses').value='192.168.5.2/24，192.168.6.2/24';v('ipv4.gateway').value='192.168.5.1';
  v('ipv4.dns').value='1.1.1.1, 223.5.5.5';v('ipv4.route_metric').value='25';
  v('ipv6.method').value='manual';v('ipv6.addresses').value='2001:db8::2/64';v('ipv6.gateway').value='fe80::1';
  f.setConfirm(true);await form.fire('submit');
  assert.deepEqual(f.requests[0].body.settings.ipv4.addresses,['192.168.5.2/24','192.168.6.2/24']);
  assert.equal(f.requests[0].body.settings.ipv4.route_metric,25);assert.equal(f.requests[0].body.settings.ipv6.gateway,'fe80::1');
});

test('刷新保留有线草稿，试用期间锁定编辑，确认使用服务端不透明标识',async()=>{
  const f=fixture(),form=f.form(),name=form.elements.namedItem('name');name.value='草稿';await form.fire('input',{target:name});
  f.context.TeslaBoxEthernet.render(structuredClone(f.data));assert.equal(f.form().elements.namedItem('name').value,'草稿');
  f.data.pending={token:'opaque',interface:'eth0',expires_at:new Date(Date.now()+120000).toISOString()};
  f.context.TeslaBoxEthernet.render(structuredClone(f.data));assert.equal(f.form().inert,true);assert.equal(f.ids['ethernet-trial'].hidden,false);
  await f.ids['ethernet-confirm'].fire('click');await new Promise(resolve=>setImmediate(resolve));
  assert.deepEqual(f.requests[0],{path:'/api/v1/network/ethernet/finish',body:{token:'opaque',keep:true}});
});

test('有线入口与后端刷新已接入，离线配置不冒充已连接',()=>{
  const html=fs.readFileSync(__dirname+'/assets/index.html','utf8'),layout=fs.readFileSync(__dirname+'/assets/settings-layout.js','utf8');
  assert.match(html,/id="ethernet-settings-title"/);assert.match(layout,/titles:\["wifi-settings-title","shared-network-title","ethernet-settings-title","ap-settings-title"\]/);
  const f=fixture();f.data.devices[0].state='connecting';f.data.devices[0].profiles[0].active=false;
  f.context.TeslaBoxEthernet.render(f.data);assert.match(f.ids['ethernet-devices'].textContent,/正在连接／获取地址/);
});

test('USB 网络移动到共享栏目，试用确认仍可在当前栏目完成，拔出清理旧卡',async()=>{
  const f=fixture();f.data.devices[0].kind='usb';
  // 同名接口的驱动分类补齐后，必须迁移原有卡片，不能两边重复或遗留旧标题。
  f.context.TeslaBoxEthernet.render(f.data);
  assert.match(f.ids['shared-usb-devices'].textContent,/USB 网络/);
  assert.equal(f.ids['ethernet-devices'].all().some(el=>el.tagName==='form'),false);
  f.data.pending={token:'usb-trial',interface:'usb0',expires_at:new Date(Date.now()+120000).toISOString()};
  f.context.TeslaBoxEthernet.render(f.data);assert.equal(f.ids['shared-usb-trial'].hidden,false);
  await f.ids['shared-usb-confirm'].fire('click');await new Promise(resolve=>setImmediate(resolve));
  assert.deepEqual(f.requests[0],{path:'/api/v1/network/ethernet/finish',body:{token:'usb-trial',keep:true}});
  f.data.devices=[];f.context.TeslaBoxEthernet.render(f.data);assert.equal(f.ids['shared-usb-devices'].children.length,0);
});

test('英文有线页面翻译控制文案，不翻译用户配置名称或改动提交参数',async()=>{
  const f=fixture('en');
  assert.match(f.ids['ethernet-devices'].textContent,/Connection profile/);
  assert.match(f.ids['ethernet-devices'].textContent,/未翻译的用户配置/);
  assert.equal(f.form().elements.namedItem('name').value,'<img src=x> 未翻译的用户配置');
  await f.form().fire('submit');
  assert.match(f.questions[0][0],/Try Ethernet/);
  assert.equal(f.requests[0].body.settings.ipv4.method,'auto');
  assert.deepEqual(Array.from(f.context.TeslaBoxI18n.missing()),[]);
});

test('所有有线复选项使用同行标签，小尺寸控件不会变成整行输入框',()=>{
  const f=fixture(),form=f.form();
  for(const name of ['auto_connect','ipv4.ignore_auto_dns','ipv4.never_default','ipv6.ignore_auto_dns','ipv6.never_default','auto_negotiate']){
    const input=form.elements.namedItem(name),label=input.parent;
    assert.equal(input.type,'checkbox');assert.equal(label.className,'inline-check');
    assert.equal(label.children[0],input);assert.equal(label.children[1].tagName,'span');
    assert.ok(label.children[1].textContent);
  }
  assert.equal(form.elements.namedItem('auto_connect').checked,true);
  assert.equal(form.elements.namedItem('auto_negotiate').disabled,true);
  assert.equal(form.all().find(element=>element.type==='submit').className,'primary');
  const css=fs.readFileSync(__dirname+'/assets/settings-layout.css','utf8');
  assert.match(css,/\.ethernet-card label\.inline-check[^}]+flex-direction: row/);
  assert.match(css,/\.ethernet-card \.inline-check input\[type="checkbox"\][^}]+width: 18px; height: 18px/);
  assert.match(css,/\.ethernet-card \.button-row \{[^}]+display: flex/);
  assert.match(css,/@media \(max-width: 759px\) \{\s*\.ethernet-fields, \.ethernet-addresses \{ grid-template-columns: minmax\(0, 1fr\)/);
});

test('有线信息复用 WiFi/AP 状态卡片，地址与链路细节完整保留',()=>{
  const f=fixture(),device=f.data.devices[0];
  Object.assign(device,{carrier:true,speed:1000,duplex:'full',mac:'00:11:22:33:44:55',permanent_mac:'00:11:22:33:44:66',mtu:1500,driver:'example'});
  device.ipv4.gateway='192.168.3.254';device.ipv6.addresses=['2001:db8::1/64'];
  f.context.TeslaBoxEthernet.render(f.data);
  const container=f.ids['ethernet-devices'].children[0],overview=container.children.find(el=>el.className==='hardware-overview network-overview');
  const fields=Object.fromEntries(overview.children.map(item=>[item.children[0].textContent,item.children[1]]));
  assert.equal(container.className,'network-device');assert.equal(overview.children.length,10);
  assert.equal(fields['连接状态'].className,'hardware-value ok');
  assert.equal(fields['网络接口'].textContent,'eth0');assert.match(fields['IP 地址'].textContent,/2001:db8/);
  assert.equal(fields['速率／双工'].textContent,'1000 Mbps · 全双工');
  assert.equal(fields['网关'].textContent,'192.168.3.254');assert.equal(fields.DNS.textContent,'223.5.5.5');
  assert.equal(fields['永久 MAC'],undefined);assert.equal(fields['驱动'],undefined);
  assert.equal(fields['连接配置'].children.length,0);assert.match(fields['连接配置'].textContent,/<img src=x>/);
  assert.equal(f.requests.length,0);
});

test('有线读取失败清除旧状态卡数值，但不丢弃编辑草稿',async()=>{
  const f=fixture(),name=f.form().elements.namedItem('name');name.value='未保存草稿';await f.form().fire('input',{target:name});
  f.context.TeslaBoxEthernet.unavailable('读取失败');
  const overview=f.ids['ethernet-devices'].children[0].children.find(el=>el.className==='hardware-overview network-overview');
  assert.match(overview.textContent,/状态暂不可用/);assert.doesNotMatch(overview.textContent,/192\.168\.3\.1|223\.5\.5\.5|已连接/);
  assert.equal(overview.children[0].children[1].className,'hardware-value error');
  assert.equal(f.form().elements.namedItem('name').value,'未保存草稿');assert.equal(f.form().inert,true);
});

test('有线页面不重复小标题，共享列表 USB 标题保留且迁移可恢复',()=>{
  const f=fixture(),heading=()=>f.ids['ethernet-devices'].children.find(el=>el.className==='network-device')?.children[0];
  assert.equal(heading().hidden,true);assert.equal(heading().textContent,'');
  const overview=f.ids['ethernet-devices'].children[0].children.find(el=>el.className==='hardware-overview network-overview');
  assert.equal(overview.children[1].children[1].textContent,'eth0');
  f.data.devices[0].kind='usb';f.context.TeslaBoxEthernet.render(f.data);
  const usbHeading=f.ids['shared-usb-devices'].children[0].children[0];
  assert.equal(usbHeading.hidden,false);
  assert.equal(usbHeading.className,'network-device-heading network-heading-usb');
  assert.equal(usbHeading.children[0].textContent,'USB 网络');assert.equal(usbHeading.children[1].textContent,'eth0');
  f.data.devices[0].kind='ethernet';f.context.TeslaBoxEthernet.render(f.data);
  assert.equal(heading().hidden,true);assert.equal(heading().textContent,'');
  assert.equal(f.requests.length,0);
});
