const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const source = fs.readFileSync(path.join(__dirname, 'assets/app.js'), 'utf8');
const html = fs.readFileSync(path.join(__dirname, 'assets/index.html'), 'utf8');

test('恢复原版完整录像入口，不请求窗口列表或拍摄时间导出',()=>{
  assert.ok(source.includes('new URLSearchParams({clip_type: "recent", limit: "1000"})'));
  assert.ok(source.includes('open.textContent = t("打开完整录像")'));
  for(const removed of ['windowed=true','createRecentNavigation','range_start_at','recentPlaybackWindows'])assert.ok(!source.includes(removed),removed);
  assert.ok(!html.includes('recent-player-navigation'));
});

test('完整录像的导出范围采用原会话真实总时长',()=>{
  const context=vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,activeSession:{start_at:'2026-01-01T00:00:00Z',duration_seconds:86400}});
  vm.runInContext(source.slice(source.indexOf('function exportTimeBounds('),source.indexOf('function initializeExportRange(')),context);
  assert.equal(context.exportTimeBounds().duration,86400);
});

test('近期和归档视频流都绑定详情快照',()=>{
  const context=vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,activeSession:{snapshot_id:'one'}});
  vm.runInContext(source.slice(source.indexOf('function sessionMediaURL('),source.indexOf('async function openSessionPlayer(')),context);
  assert.equal(context.sessionMediaURL('/content'),'/content?snapshot_id=one');
  context.activeSession={};assert.equal(context.sessionMediaURL('/content'),'/content');
});

test('快速打开不同录像时旧请求不能覆盖新请求',async()=>{
  const requests=[];const notices=[];
  const context=vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,sessionOpenSequence:0,sessionOpening:false,sessionOpenController:null,AbortController,activeSession:null,playerModal:{hidden:true},request:()=>new Promise((resolve,reject)=>requests.push({resolve,reject})),setNotice:message=>notices.push(message)});
  vm.runInContext(source.slice(source.indexOf('async function openSessionPlayer('),source.indexOf('function renderCameraSwitcher(')),context);
  const first=context.openSessionPlayer({id:'old'},'front',0,'single');
  const second=context.openSessionPlayer({id:'new'},'front',0,'single');
  requests[1].reject(new Error('新请求失败'));await second;
  requests[0].resolve({id:'old'});await first;
  assert.equal(notices.length,1);assert.match(notices[0],/新请求失败/);assert.equal(context.activeSession,null);
});

test('播放遇到旧快照时自动刷新并按事件目录重试',async()=>{
  const requests=[];const notices=[];
  const context=vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,
    sessionOpenSequence:0,sessionOpening:false,sessionOpenController:null,AbortController,
    activeSession:null,playerModal:{hidden:true},recentSessions:[],archivedSessions:[],
    request:(path)=>new Promise((resolve,reject)=>requests.push({path,resolve,reject})),
    loadSessions:async()=>{context.archivedSessions=[{id:'new-id',clip_type:'sentry',event:'2026-09-18_13-46-59',snapshot_id:'new'}]},
    setNotice:message=>notices.push(message),
  });
  vm.runInContext(source.slice(source.indexOf('async function openSessionPlayer('),source.indexOf('function renderCameraSwitcher(')),context);
  const opening=context.openSessionPlayer({id:'old-id',clip_type:'sentry',event:'2026-09-18_13-46-59',snapshot_id:'old'},'front',0,'single');
  const stale=new Error('录像已更新');stale.status=409;requests[0].reject(stale);
  await new Promise(resolve=>setImmediate(resolve));
  assert.match(requests[0].path,/old-id\?snapshot_id=old/);
  assert.match(requests[1].path,/new-id\?snapshot_id=new/);
  requests[1].reject(new Error('重试已发出'));
  await opening;
  assert.equal(notices.length,1);assert.match(notices[0],/重试已发出/);
});

test('播放遇到旧快照且目录已删除时自动移除旧卡片',async()=>{
  const notices=[];
  const context=vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,
    sessionOpenSequence:0,sessionOpening:false,sessionOpenController:null,AbortController,
    activeSession:null,playerModal:{hidden:true},recentSessions:[],archivedSessions:[],
    request:async()=>{const error=new Error('录像已更新');error.status=409;throw error},
    loadSessions:async()=>{},setNotice:(...args)=>notices.push(args),
  });
  vm.runInContext(source.slice(source.indexOf('async function openSessionPlayer('),source.indexOf('function renderCameraSwitcher(')),context);
  await context.openSessionPlayer({id:'old-id',clip_type:'sentry',event:'2026-09-18_13-46-59',snapshot_id:'old'},'front',0,'single');
  assert.deepEqual(notices,[['该录像已被删除',false]]);
});
