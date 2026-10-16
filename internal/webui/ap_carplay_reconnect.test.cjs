const test=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm');
const source=fs.readFileSync(__dirname+'/assets/app.js','utf8');
function fixture({confirm=true,reason='carplay_network_reserved',retryFailure=false,language='zh-CN',refreshFailure=false}={}) {
  const i18n=require('./i18n_test_helper.cjs')(language),nodes=new Map(),calls=[],questions=[],notices=[],clean=[];
  const get=id=>{if(!nodes.has(id))nodes.set(id,{value:'',checked:false,textContent:'',className:''});return nodes.get(id);};
  const context=vm.createContext({state:{},$:get,t:i18n.t,
    askConfirmation:async(...args)=>{questions.push(args);return questions.length===1 || confirm;},
    setSettingsNotice:(...args)=>notices.push(args),TeslaBoxSettingsLayout:{markClean:id=>clean.push(id)},
    postJSON:async(url,payload)=>{calls.push({url,payload:{...payload}});if(calls.length===1||retryFailure)throw Object.assign(new Error('rejected'),{reasonCode:reason});},
    loadSettings:async()=>{if(refreshFailure)throw Object.assign(new Error('refresh failed'),{reasonCode:'carplay_network_reserved'});},
  });
  vm.runInContext(source.slice(source.indexOf('async function saveAPSettings('),source.indexOf('async function loadStorageDevices(')),context);
  get('ap-enabled').checked=true;get('ap-autoconnect').checked=true;
  get('ap-ssid').value='User Hotspot';get('ap-password').value='not-for-logs';get('ap-band').value='a';get('ap-channel').value='161';get('ap-security').value='wpa2';
  return {context,get,calls,questions,notices,clean,i18n,save:()=>context.saveAPSettings({preventDefault(){}})};
}
for(const confirm of [false,true])test(confirm?'AP 确认中断后携带授权重试一次':'取消 AP 中断确认保留草稿、密码和 CarPlay',async()=>{
  const f=fixture({confirm});await f.save();
  assert.equal(f.questions.length,2);assert.equal(f.questions[1][0],'中断 CarPlay 并应用热点设置');assert.equal(f.questions[1][2],'中断并应用');
  assert.match(f.questions[1][1],/录像继续运行/);assert.match(f.questions[1][1],/专用热点/);
  assert.equal(f.calls.length,confirm?2:1);assert.equal(f.context.state.apSaving,false);
  assert.equal(f.calls[0].payload.confirm_carplay_reconnect,undefined);
  if(confirm){assert.deepEqual(f.calls[1].payload,{...f.calls[0].payload,confirm_carplay_reconnect:true});assert.equal(f.get('ap-password').value,'');assert.deepEqual(f.clean,['ap-settings-title']);}
  else{assert.equal(f.get('ap-password').value,'not-for-logs');assert.equal(f.clean.length,0);assert.match(f.notices.at(-1)[0],/已取消/);}
});
test('普通错误及确认后的失败不循环确认、不清理草稿或假称恢复成功',async()=>{
  for(const options of [{reason:'channel_no_ir'},{retryFailure:true}]){
    const f=fixture(options);await f.save();assert.equal(f.questions.length,options.retryFailure?2:1);
    assert.equal(f.calls.length,options.retryFailure?2:1);assert.equal(f.clean.length,0);assert.equal(f.get('ap-password').value,'not-for-logs');
    assert.deepEqual(f.notices.at(-1),['rejected',true]);assert.equal(f.context.state.apSaving,false);
  }
});
test('保存成功后刷新失败不能再次提交 AP 设置',async()=>{
  const f=fixture({refreshFailure:true});await f.save();assert.equal(f.calls.length,2);assert.equal(f.questions.length,2);
  assert.deepEqual(f.notices.at(-1),['refresh failed',true]);assert.equal(f.clean.length,1);
});
test('英文 AP 确认按钮及说明完整翻译，不把密码放进弹窗或提示',async()=>{
  const f=fixture({language:'en'});await f.save();assert.equal(f.questions[1][2],'Interrupt and apply');
  assert.match(f.questions[1][0],/CarPlay/);assert.match(f.questions[1][1],/Recording continues/);
  assert.doesNotMatch(JSON.stringify([f.questions,f.notices]),/not-for-logs/);assert.deepEqual(Array.from(f.i18n.missing()),[]);
});
test('重复点击保存不会并发提交两个确认事务',async()=>{
  const f=fixture();await Promise.all([f.save(),f.save()]);assert.equal(f.calls.length,2);assert.equal(f.questions.length,2);
});
