const test=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');

test('分类下载忽略旧播放编号，完整报告保留播放编号',async()=>{
  const source=fs.readFileSync('internal/webui/assets/app.js','utf8');
  const routine=source.slice(source.indexOf('async function downloadDiagnosticReport()'),source.indexOf('function playbackProbeID()'));
  for(const category of ['', 'usb','media','network','application','carplay']){
    const button={textContent:'下载日志',disabled:false};
    const calls=[];
    const context={TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,URL,URLSearchParams,Blob,playbackProbeLastID:'a'.repeat(32),
      $:id=>id==='logs-download-source'?{value:category}:button,
      requestResponse:async path=>{calls.push(path);return {ok:true,headers:{get:()=>''},blob:async()=>new Blob(['log'])};},
      document:{createElement:()=>({click(){},remove(){}}),body:{append(){}}},window:{setTimeout:fn=>fn()},showToast(){}};
    vm.runInNewContext(routine,context);
    await context.downloadDiagnosticReport();
    const url=new URL(calls[0],'http://teslabox.local');
    assert.equal(url.searchParams.get('source'),category||null);
    assert.equal(url.searchParams.get('probe_id'),category?null:'a'.repeat(32));
    assert.equal(button.disabled,false);
  }
});

test('返回按钮属于 CarPlay 设置，播放诊断位于日志页面最后',()=>{
  const html=fs.readFileSync('internal/webui/assets/index.html','utf8');
  const carplay=html.slice(html.indexOf('aria-labelledby="carplay-settings-title"'),html.indexOf('</section>',html.indexOf('aria-labelledby="carplay-settings-title"')));
  assert.match(carplay,/id="settings-carplay-return"/);
  const start=html.indexOf('id="logs-page"');
  const logs=html.slice(start,html.indexOf('</main>',start));
  const sections=[...logs.matchAll(/<section\s[^>]*aria-labelledby="([^"]+)"/g)].map(match=>match[1]);
  assert.equal(sections.at(-1),'playback-probe-title');
  assert.match(logs,/data-log-source="carplay"/);
});
