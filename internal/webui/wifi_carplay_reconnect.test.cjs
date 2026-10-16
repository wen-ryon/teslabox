const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(__dirname + '/assets/app.js', 'utf8');

test('网络请求保留后端原因码，避免把其他 409 当成 CarPlay 占用', async () => {
  const context = vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,requestResponse:async () => ({ok:false,status:409,json:async () => ({error:'忙',reason_code:'carplay_network_reserved'})})});
  vm.runInContext(source.slice(source.indexOf('async function request(path,'), source.indexOf('async function request(path,') + source.slice(source.indexOf('async function request(path,')).indexOf('\n}\n') + 3), context);
  await assert.rejects(context.request('/network'), error => error.reasonCode === 'carplay_network_reserved' && error.status === 409);
});

for (const confirm of [false,true]) {
  test(confirm ? '确认后只重试一次，并传递强制重连授权' : '取消后保持 CarPlay，不发强制请求', async () => {
    const calls = [], questions = [];
    const context = vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,
      setSettingsNotice(){}, loadSettings:async () => {},
      askConfirmation:async (...args) => {questions.push(args); return confirm;},
      postJSON:async (url,payload) => {calls.push({...payload}); if(calls.length===1) throw Object.assign(new Error('忙'),{reasonCode:'carplay_network_reserved'});},
    });
    vm.runInContext(source.slice(source.indexOf('async function runWiFiConnect('),source.indexOf('async function forgetWiFi(')),context);
    await context.runWiFiConnect({uuid:'saved'});
    assert.equal(questions.length,1);
    assert.match(questions[0][1],/暂时中断 CarPlay/);
    assert.equal(calls.length,confirm ? 2 : 1);
    if(confirm) assert.equal(calls[1].confirm_carplay_reconnect,true);
  });
}

test('普通错误或已确认的重连失败不反复弹窗', async () => {
  for (const payload of [{uuid:'saved'}, {uuid:'saved',confirm_carplay_reconnect:true}]) {
    let questions=0;
    const context=vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,setSettingsNotice(){},loadSettings:async()=>{},askConfirmation:async()=>{questions++;return true;},postJSON:async()=>{throw Object.assign(new Error('失败'),{reasonCode:payload.confirm_carplay_reconnect?'carplay_network_reserved':'other_conflict'});}});
    vm.runInContext(source.slice(source.indexOf('async function runWiFiConnect('),source.indexOf('async function forgetWiFi(')),context);
    await context.runWiFiConnect(payload);
    assert.equal(questions,0);
  }
});
