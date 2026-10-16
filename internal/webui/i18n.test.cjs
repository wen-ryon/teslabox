const test=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm');
const source=fs.readFileSync(__dirname+'/assets/i18n.js','utf8');
function setup(language='en',elements=[]) {
  const calls=[];
  const context=vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,document:{documentElement:{dataset:{deviceLanguage:language}},readyState:'complete',querySelectorAll:selector=>selector==='[data-i18n]'?elements:[]},
    localStorage:{getItem(){throw Error('不得读取客户端语言缓存');},setItem(){throw Error('不得写客户端语言缓存');}},
    fetch(...args){calls.push(args);throw Error('不得写板端网络');}});
  context.TeslaBoxEnglish=JSON.parse(fs.readFileSync(__dirname+'/assets/i18n-en.json','utf8'));
  vm.runInContext(source,context);return {context,api:context.TeslaBoxI18n,calls};
}
test('只依赖板端语言，不推测浏览器或地区；中文不改变业务文案',()=>{
  const f=setup('zh-CN');assert.equal(f.api.t('已连接'),'已连接');assert.equal(f.api.locale,'zh-CN');assert.equal(f.calls.length,0);
  const en=setup();assert.equal(en.api.t('已连接'),'Connected');assert.equal(en.api.locale,'en');assert.equal(en.calls.length,0);
});
test('模板替换不再次解析用户 SSID、花括号、中文文件名或 HTML',()=>{
  const {context,api}=setup();
  context.ssid='{1}<img src=x>中文';context.end='12:00';
  const result=vm.runInContext('TeslaBoxI18n.t`${ssid} · 请在 ${end} 前确认；到期由 NetworkManager 自动回退。`',context);
  assert.match(result,/^\{1\}<img src=x>中文/);assert.match(result,/Confirm before 12:00/);
  assert.equal(api.t('未收录文案'),'未收录文案');assert.deepEqual(Array.from(api.missing()),['未收录文案']);
});

test('静态 HTML 翻译拒绝用户插值，中文模式保留原始固定标记',()=>{
  const {context}=setup('zh-CN');context.user='用户文件名';
  assert.equal(vm.runInContext('TeslaBoxI18n.t.html`<label>用户名<input name="user"></label>`',context),'<label>用户名<input name="user"></label>');
  assert.throws(()=>vm.runInContext('TeslaBoxI18n.t.html`<p>${user}</p>`',context),/cannot contain interpolated/);
});
test('显式静态标记只更新自己的文案，保留表单子节点和未标记用户数据',()=>{
  const text={nodeType:3,nodeValue:'  界面语言 '},user={nodeType:1,textContent:'已连接（这是用户 SSID）'};
  const label={getAttribute:()=> '界面语言',childNodes:[text,user]};
  setup('en',[label]);assert.equal(text.nodeValue,'  Interface language ');assert.equal(user.textContent,'已连接（这是用户 SSID）');
});
test('页面先加载翻译器，语言配置与网络模块不依赖客户端私有设置',()=>{
  const html=fs.readFileSync(__dirname+'/assets/index.html','utf8');
  assert.ok(html.indexOf('/i18n.js')<html.indexOf('/ethernet.js'));
  assert.ok(html.indexOf('/i18n.js')<html.indexOf('/language-settings.js'));
  assert.match(html,/data-i18n="语言"/);assert.match(html,/data-i18n="有线网络"/);
});

test('所有英文词条保留插值与控件标记，不带未说明的中文残片',()=>{
  const json=fs.readFileSync(__dirname+'/assets/i18n-en.json','utf8'),keys=new Set();
  for(const match of json.matchAll(/^  ("(?:[^"\\]|\\.)*"):/gm)) {
    const key=JSON.parse(match[1]);assert.ok(!keys.has(key),'词条重复: '+key);keys.add(key);
  }
  const context=vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,document:{documentElement:{dataset:{deviceLanguage:'en'}},readyState:'loading',addEventListener(){}}});
  context.TeslaBoxEnglish=JSON.parse(fs.readFileSync(__dirname+'/assets/i18n-en.json','utf8'));
  const exposed=source.replace('const missing = new Set();','globalThis.catalogueForTest = en; const missing = new Set();');
  assert.notEqual(exposed,source);vm.runInContext(exposed,context);
  const placeholders=value=>(value.match(/\{\d+\}/g)||[]).sort();
  const tags=value=>(value.match(/<\/?[a-z][^>]*>/gi)||[]);
  for(const [key,value]of Object.entries(context.catalogueForTest)){
    assert.deepEqual(placeholders(value),placeholders(key),'占位符改变: '+key);
    assert.deepEqual(tags(value),tags(key),'控件 HTML 改变: '+key);
    if(key!=='简体中文')assert.doesNotMatch(value,/[\u3400-\u9fff]/,'英文残留中文: '+key);
  }
});

test('界面语言不能改变诊断原因、原始日志或中文业务判断',()=>{
  const app=fs.readFileSync(__dirname+'/assets/app.js','utf8');
  for(const text of ['stopPlaybackProbeMatrix("用户停止")','stopPlaybackProbeMatrix("页面进入后台")','stopPlaybackProbeMatrix("离开日志中心")','includes("快照空间不足")'])assert.ok(app.includes(text),text);
  const language=require('./i18n_test_helper.cjs')('en'),elements=new Map();
  const get=id=>{if(!elements.has(id))elements.set(id,{textContent:'',hidden:false,replaceChildren(...items){this.children=items;}});return elements.get(id);};
  const context=vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:language.t,$:get,document:{createElement:()=>({})},formatDate:()=> '2026-10-15',renderGadgetServiceStates(){}});
  const start=app.indexOf('function renderLogs('),end=app.indexOf('function renderGadgetServiceStates(');
  vm.runInContext(app.slice(start,end),context);
  const lines=['原始日志：已连接 {1}<img src=x>','中文错误没有被界面翻译器改写'];
  context.renderLogs({source:'network',lines,summaries:['原始诊断摘要'],generated_at:'x'});
  assert.equal(get('logs-output').textContent,lines.join('\n'));assert.match(get('logs-meta').textContent,/Network and hotspot/);
  assert.equal(get('logs-summary-list').children[0].textContent,'原始诊断摘要');
});
