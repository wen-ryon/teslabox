const assert=require('node:assert/strict');
const test=require('node:test');
const vm=require('node:vm');
const fs=require('node:fs');

test('自动播放默认关闭并作为布尔保存，关闭功能和画面优先不修改画质草稿',async()=>{
  const fields=new Map(['network','quality','fps','recovery_priority','enabled','auto_play'].map(name=>['carplay-'+name,{value:''}]));
  const notice={textContent:''},button={disabled:false};let submit,saved;
  fields.set('carplay-settings-form',{offsetParent:{},addEventListener:(_,fn)=>{submit=fn;},querySelector:()=>button});
  fields.set('carplay-settings-notice',notice);fields.set('carplay-network-status',{textContent:''});
  const original={revision:1,enabled:true,recovery_priority:'internet',network:'wifi',quality:720,fps:30};
  const context={TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,document:{getElementById:id=>fields.get(id),hidden:false},AbortController,setTimeout,clearTimeout,setInterval:()=>0,
    fetch:async(path,options={})=>{
      if(path==='/carplay/api/network')return {ok:true,json:async()=>({internet:false})};
      if(options.method==='PUT'){saved={...JSON.parse(options.body),revision:2};}
      return {ok:true,json:async()=>saved||original};
    }};
  vm.runInNewContext(fs.readFileSync('internal/webui/assets/carplay-settings.js','utf8'),context);
  await context.TeslaBoxCarPlaySettings.load();
  assert.equal(fields.get('carplay-enabled').checked,true);
  assert.equal(fields.get('carplay-auto_play').checked,false);
  fields.get('carplay-auto_play').checked=true;
  fields.get('carplay-enabled').checked=false;fields.get('carplay-recovery_priority').value='carplay';
  await submit({preventDefault(){}});
  assert.equal(saved.enabled,false);assert.equal(saved.recovery_priority,'carplay');assert.equal(saved.fps,30);
  assert.equal(saved.auto_play,true);
  assert.match(notice.textContent,/CarPlay 将关闭，录像继续运行/);
});

test('登录后重试读取，刷新不覆盖草稿，保存使用当前版本并提示来源等待应用',async()=>{
  const fields=new Map(['network','quality','fps','recovery_priority','enabled'].map(name=>['carplay-'+name,{value:''}]));
  const notice={textContent:''},status={textContent:''},button={disabled:false};
  let submit,reads=0,saved;
  const form={offsetParent:{},addEventListener:(_,callback)=>{submit=callback;},querySelector:()=>button};
  fields.set('carplay-settings-form',form);fields.set('carplay-settings-notice',notice);fields.set('carplay-network-status',status);
  const requests=[];
  const context={TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,document:{getElementById:id=>fields.get(id),hidden:false},AbortController,setTimeout,clearTimeout,setInterval:()=>0,
    fetch:async(path,options={})=>{
      requests.push({path,...options});
      if(path==='/carplay/api/network')return {ok:true,json:async()=>({internet:false})};
      if(options.method==='PUT'){const value=JSON.parse(options.body);assert.equal(value.revision,2);assert.equal(value.fps,60);assert.equal('scale' in value,false);saved={...value,revision:3};return {ok:true,json:async()=>saved};}
      if(saved)return {ok:true,json:async()=>saved};
      reads++;
      return reads===1?{ok:false,json:async()=>({error:'请先登录'})}:{ok:true,json:async()=>({revision:2,enabled:true,recovery_priority:'internet',network:'wifi',quality:1080,fps:30})};
    }};
  vm.runInNewContext(fs.readFileSync('internal/webui/assets/carplay-settings.js','utf8'),context);
  assert.equal(requests.length,0);
  await context.TeslaBoxCarPlaySettings.load();assert.equal(notice.textContent,'请先登录');
  await context.TeslaBoxCarPlaySettings.load();assert.equal(fields.get('carplay-quality').value,1080);
  fields.get('carplay-fps').value=60;
  await context.TeslaBoxCarPlaySettings.load();assert.equal(reads,2);assert.equal(fields.get('carplay-fps').value,60);
  await submit({preventDefault(){}});
  assert.equal(button.disabled,false);assert.match(status.textContent,/等待应用网络来源/);
  assert.match(notice.textContent,/已保存/);assert.doesNotMatch(notice.textContent,/缩放/);
});

test('保存后读回旧帧率时不得显示已保存',async()=>{
  const fields=new Map(['network','quality','fps','recovery_priority','enabled'].map(name=>['carplay-'+name,{value:''}]));
  const notice={textContent:''},button={disabled:false};
  let submit,revision=1;
  fields.set('carplay-settings-form',{offsetParent:{},addEventListener:(_,callback)=>{submit=callback;},querySelector:()=>button});
  fields.set('carplay-settings-notice',notice);fields.set('carplay-network-status',{textContent:''});
  const context={TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,document:{getElementById:id=>fields.get(id),hidden:false},AbortController,setTimeout,clearTimeout,setInterval:()=>0,
    fetch:async(path,options={})=>{
      if(path==='/carplay/api/network')return {ok:true,json:async()=>({internet:true})};
      if(options.method==='PUT'){revision=2;return {ok:true,json:async()=>({...JSON.parse(options.body),revision})};}
      return {ok:true,json:async()=>({revision,enabled:true,recovery_priority:'internet',network:'auto',quality:720,fps:60})};
    }};
  vm.runInNewContext(fs.readFileSync('internal/webui/assets/carplay-settings.js','utf8'),context);
  await context.TeslaBoxCarPlaySettings.load();
  fields.get('carplay-fps').value=30;
  await submit({preventDefault(){}});
  assert.equal(notice.textContent,'设置未确认保存，请刷新后重试');
  assert.equal(button.disabled,false);
});

test('隐藏多手机入口后不获取列表，保存画质仍保留既有手机身份',async()=>{
  const fields=new Map(['network','quality','fps','recovery_priority','enabled'].map(name=>['carplay-'+name,{value:''}]));
  const children=[];
  const phone={value:'',get firstChild(){return children[0];},removeChild(){children.shift();},appendChild(node){children.push(node);}};
  fields.set('carplay-phone_id',phone);fields.set('carplay-phone-status',{textContent:''});
  let submit,saved;
  const original={revision:1,phone_id:'02:00:00:00:00:01',enabled:true,recovery_priority:'internet',network:'auto',quality:720,fps:30};
  fields.set('carplay-settings-form',{offsetParent:{},addEventListener:(_,callback)=>{submit=callback;},querySelector:()=>({disabled:false})});
  fields.set('carplay-settings-notice',{textContent:''});fields.set('carplay-network-status',{textContent:''});
  const context={TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,document:{getElementById:id=>fields.get(id),createElement:()=>({value:'',textContent:''}),hidden:false},AbortController,setTimeout,clearTimeout,setInterval:()=>0,
    fetch:async(path,options={})=>{
      if(path==='/carplay/api/network')return {ok:true,json:async()=>({internet:true})};
      if(path==='/carplay/api/phones')assert.fail('隐藏入口不应获取手机列表');
      if(options.method==='PUT')saved={...JSON.parse(options.body),revision:(saved?.revision||1)+1};
      return {ok:true,json:async()=>saved||original};
    }};
  vm.runInNewContext(fs.readFileSync('internal/webui/assets/carplay-settings.js','utf8'),context);
  await context.TeslaBoxCarPlaySettings.load();
  assert.deepEqual(children,[]);
  phone.value='02:00:00:00:00:02';await submit({preventDefault(){}});assert.equal(saved.phone_id,original.phone_id);
  phone.value='';await submit({preventDefault(){}});assert.equal(saved.phone_id,original.phone_id);
});
