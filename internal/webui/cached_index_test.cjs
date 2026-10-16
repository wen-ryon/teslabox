const test=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const source=fs.readFileSync(require('node:path').join(__dirname,'assets/app.js'),'utf8');
test('历史预览不允许进入播放或批量删除',async()=>{
 const notices=[];
 const context=vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,setNotice:msg=>notices.push(msg)});
 vm.runInContext(source.slice(source.indexOf('async function openSessionPlayer('),source.indexOf('function renderCameraSwitcher(')),context);
 await context.openSessionPlayer({cached:true});assert.equal(notices.length,1);
 vm.runInContext(source.slice(source.indexOf('function isSelectableRecordingSession('),source.indexOf('function selectableRecordingSessions(')),context);
 assert.equal(context.isSelectableRecordingSession({cached:true,clip_type:'saved'}),false);
 assert.equal(context.isSelectableRecordingSession({clip_type:'saved'}),true);
});
test('空历史索引仍通过响应标记驱动后台核对',()=>{
 assert.match(source,/recordingIndexCached = Boolean\(recentPayload.cached/);
 assert.match(source,/recordingIndexCached\) \{ loadSessions\(true, true\)/);
});
test('空间保护后的历史索引不再误报索引未完成或持续读取时长',()=>{
 assert.match(source,/历史索引 · 空间保护/);
 assert.match(source,/当前只读快照因外层空间不足已回收；释放空间后自动核对并恢复时长与播放/);
 assert.match(source,/待释放空间后核对/);
  assert.match(source,/空间不足 · 暂不可播放/);
});
test('列表不暴露缓存实现且自动刷新不触发索引同步',()=>{
 assert.match(source,/duration_state === "cached"\) return formatDuration\(session\.duration_seconds\)/);
 assert.doesNotMatch(source,/formatDuration\(session\.duration_seconds\)\} · 缓存/);
 assert.match(source,/if \(automatic\) archiveParams\.set\("metadata_only", "true"\)/);
 assert.match(source,/if \(automatic\) recentParams\.set\("metadata_only", "true"\)/);
 assert.match(source,/约 \$\{formatDuration\(session\.duration_seconds\)\}/);
});
