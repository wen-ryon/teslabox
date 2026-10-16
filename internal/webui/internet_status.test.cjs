const test=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm');
class Element {
  constructor(){this.children=[];this.events={};this.attributes={};this.hidden=true;this.textContent='';}
  appendChild(node){this.children.push(node);}
  replaceChildren(){this.children=[];}
  addEventListener(name,fn){this.events[name]=fn;}
  setAttribute(name,value){this.attributes[name]=value;}
  contains(node){return node===this||this.children.includes(node);}
  text(){return this.textContent+this.children.map(n=>n.text()).join('\n');}
}
function fixture(language='zh-CN'){
  const button=new Element(),panel=new Element(),wrapper=new Element();wrapper.children=[button,panel];button.parentElement=wrapper;
  const doc={readyState:'complete',activeElement:null,events:{},getElementById:id=>({'internet-indicator':button,'internet-egress':panel}[id]),createElement:()=>new Element(),addEventListener(name,fn){this.events[name]=fn;}};
  const context=vm.createContext({document:doc,TeslaBoxI18n:require('./i18n_test_helper.cjs')(language)});
  vm.runInContext(fs.readFileSync(__dirname+'/assets/internet-status.js','utf8'),context);
  return{button,panel,wrapper,doc,api:context.TeslaBoxInternetStatus};
}
test('悬停、点击固定、再次点击、Escape 和点击外部均可控制出口说明',()=>{
  const f=fixture();assert.equal(f.panel.hidden,true);
  f.wrapper.events.pointerenter();assert.equal(f.panel.hidden,false);
  f.wrapper.events.pointerleave();assert.equal(f.panel.hidden,true);
  f.button.events.click();f.wrapper.events.pointerleave();assert.equal(f.panel.hidden,false);
  f.button.events.click();assert.equal(f.panel.hidden,true);
  f.button.events.click();f.doc.events.keydown({key:'Escape'});assert.equal(f.panel.hidden,true);
  f.button.events.click();f.doc.events.pointerdown({target:new Element()});assert.equal(f.panel.hidden,true);
  f.wrapper.events.focusin();assert.equal(f.button.attributes['aria-expanded'],'true');
  f.wrapper.events.focusout({relatedTarget:null});assert.equal(f.panel.hidden,true);
});
test('显示探测来源和时间；未知或接口失败不得保留旧出口',()=>{
  const f=fixture();f.api.render({internet_available:true,internet_checked_at:'2026-10-16T00:00:00Z',internet_egress:{kind:'ethernet',interface:'eth0',local_ip:'192.168.1.2',family:'ipv4'}});
  assert.match(f.panel.text(),/有线网络 · eth0/);assert.match(f.panel.text(),/192.168.1.2 \(IPv4\)/);
  assert.match(f.panel.text(),/不同目标或 IPv4\/IPv6/);
  f.api.render({internet_available:false,internet_egress:{kind:'wifi',interface:'sta0'}});assert.doesNotMatch(f.panel.text(),/eth0|sta0/);
  f.api.render({internet_available:true});assert.match(f.panel.text(),/出口信息不可用/);
  f.api.unavailable();assert.match(f.panel.text(),/未检测/);assert.doesNotMatch(f.panel.text(),/192\.168/);
});
test('英文界面、未知隧道及不可信接口名只作为文本展示，不发起请求',()=>{
  const f=fixture('en');f.api.render({internet_available:true,internet_egress:{kind:'unknown',interface:'<img src=x>',local_ip:'2001:db8::2',family:'ipv6'}});
  assert.match(f.panel.text(),/Unknown type · <img src=x>/);assert.match(f.panel.text(),/IPv6/);assert.match(f.panel.text(),/Last observed egress/);
  assert.ok(f.panel.children.every(n=>n.children.length===0));
});
test('实际管理页加载资源并由状态成功与失败路径更新，不增加网络操作',()=>{
  const html=fs.readFileSync(__dirname+'/assets/index.html','utf8'),app=fs.readFileSync(__dirname+'/assets/app.js','utf8');
  assert.match(html,/internet-status\.js\?v=/);assert.match(html,/<button id="internet-indicator"[^>]+aria-controls="internet-egress"/);
  assert.match(app,/function renderSystemStatus\(system\)\s*\{\s*globalThis\.TeslaBoxInternetStatus\?\.render\(system\)/);
  assert.match(app,/function renderSystemUnavailable\(\)\s*\{\s*globalThis\.TeslaBoxInternetStatus\?\.unavailable\(\)/);
  assert.doesNotMatch(fs.readFileSync(__dirname+'/assets/internet-status.js','utf8'),/\bfetch\(|\brequest\(/);
});
