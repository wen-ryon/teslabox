const test=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const source=fs.readFileSync(require('node:path').join(__dirname,'assets/app.js'),'utf8');
const code=source.slice(source.indexOf('  const scan = storage.scan_progress;'),source.indexOf('\nfunction renderSystemStatus')).replace(/}\s*$/, '');
function render(storage){
 const nodes={};
 const context=vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,storage,storageScanActive:false,$:id=>nodes[id]||=( {style:{},classList:{toggle(){},add(){}}}),formatNumber:String,formatBytes:n=>`${n} B`,formatDate:String});
 vm.runInContext(code,context);return nodes;
}
test('未知容量和索引不显示零或满进度条',()=>{
 const n=render({});assert.equal(n['video-count'].textContent,'—');assert.equal(n['space-value'].textContent,'—');assert.ok(!n['space-detail'].textContent.includes('0 B'));assert.notEqual(n['space-progress'].style.width,'100%');
});
test('扫描数量逐步显示但不冒充已发布索引',()=>{
 for(const count of [64,128]) {const n=render({scan_progress:{stage:'scanning',discovered:count}});assert.equal(n['video-count'].textContent,`${count} 个`);assert.match(n['snapshot-detail'].textContent,/扫描完成后/);}
 const n=render({media_snapshot:{created_at:'now'},video_count:99,scan_progress:{stage:'scanning',discovered:128}});assert.equal(n['video-count'].textContent,'99 个');assert.match(n['snapshot-detail'].textContent,/128/);
});
test('完成空索引才显示零，失败明确等待重试',()=>{
 assert.equal(render({media_snapshot:{created_at:'now'},video_count:0})['video-count'].textContent,'0 个');
 assert.equal(render({snapshot_error:'error'})['snapshot-state'].textContent,'等待重试');
});
