const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(require('node:path').join(__dirname, 'assets/app.js'), 'utf8');
const code = source.slice(source.indexOf('let durationRefreshInFlight'), source.indexOf('function recordingInteractionActive'));

function fixture(request) {
  const session = {id:'recent',snapshot_id:'one',duration_state:'estimated'};
  const nodes = {};
  const card = {dataset:{sessionId:'recent'},querySelector:key => nodes[key] ||= {}};
  const context = vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,appView:{hidden:false},document:{hidden:false,querySelectorAll:()=>[card]},state:{page:'dashboard'},recentSessions:[session],archivedSessions:[],URLSearchParams,request,console:{warn:()=>{}},sessionDurationLabel:s=>s.duration_state,formatSessionRange:()=> 'range'});
  vm.runInContext(code, context);
  return {context,session,nodes};
}

test('播放或选择期间也能静默合并时长，不依赖列表刷新',async()=>{
  const f=fixture(async(url,options)=>{
    assert.match(url,/metadata_only=true/);assert.match(url,/session_id=recent/);assert.equal(options.loading,false);
    return {snapshot_id:'one',items:[{id:'recent',duration_state:'verified',duration_seconds:123}]};
  });
  await f.context.refreshRecordingDurations();
  assert.equal(f.session.duration_seconds,123);assert.equal(f.nodes['.duration-pill'].textContent,'verified');
});

test('快照变化或列表已更换时丢弃迟到响应',async()=>{
  let resolve;
  const f=fixture(()=>new Promise(r=>resolve=r));
  const task=f.context.refreshRecordingDurations();
  f.context.recentSessions=[];
  resolve({snapshot_id:'one',items:[{id:'recent',duration_state:'verified'}]});
  await task;assert.equal(f.session.duration_state,'estimated');
  const other=fixture(async()=>({snapshot_id:'two',items:[{id:'recent',duration_state:'verified'}]}));
  await other.context.refreshRecordingDurations();assert.equal(other.session.duration_state,'estimated');
});

test('失败后可重试，混合不可读取状态仍继续校准',async()=>{
  let calls=0;
  const f=fixture(async()=>{if(++calls===1)throw Error('offline');return {snapshot_id:'one',items:[{id:'recent',duration_state:'unavailable',duration_pending:false}]};});
  f.session.duration_state='unavailable';f.session.duration_pending=true;
  await f.context.refreshRecordingDurations();await f.context.refreshRecordingDurations();
  assert.equal(calls,2);assert.equal(f.session.duration_pending,false);
});
