const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const source = fs.readFileSync(path.join(__dirname,"assets","app.js"),"utf8").match(/^async function downloadDiagnosticReport\(\) \{.*?^\}/ms)[0];
const html = fs.readFileSync(path.join(__dirname,"assets","index.html"),"utf8");

function model(category, response) {
  const button = {textContent:"下载日志",disabled:false};
  const calls = [], downloads = [], notices = [], revoked = [];
  const context = vm.createContext({
    $:id => id === "logs-download-source" ? {value:category} : button,
    encodeURIComponent,Date,
    // 旧播放编号故意存在，分类与完整下载都不能携带它。
    playbackProbeLastID:"a".repeat(32),
    URL:{createObjectURL:() => "blob:report",revokeObjectURL:url => revoked.push(url)},
    requestResponse:async (url,options) => {calls.push({url,options});return response || {ok:true,headers:{get:()=> 'attachment; filename="carplay.txt"'},blob:async()=>({})};},
    document:{createElement:() => {const link={click:() => downloads.push(link),remove(){}};return link;},body:{append(){}}},
    window:{setTimeout:fn => fn()},showToast:(message,type) => notices.push({message,type}),
  });
  vm.runInContext(source,context);
  return {context,button,calls,downloads,notices,revoked};
}

test("六种下载选择对应白名单分类，完整报告保留原路径且不附旧播放编号",async()=>{
  for(const category of ["","usb","media","network","application","carplay"]){
    const instance=model(category);await instance.context.downloadDiagnosticReport();
    assert.equal(instance.calls.length,1);
    assert.equal(instance.calls[0].url,"/api/v1/diagnostics/report"+(category?"?source="+category:""));
    assert.equal(instance.calls[0].options.cache,"no-store");
    assert.equal(instance.downloads.length,1);
    assert.equal(instance.downloads[0].download,"carplay.txt");
    assert.deepEqual(instance.revoked,["blob:report"]);
    assert.equal(instance.button.disabled,false);assert.equal(instance.button.textContent,"下载日志");
  }
});
test("请求过程中禁止重复点击，错误恢复按钮并显示后端原因",async()=>{
  const instance=model("carplay",{ok:false,status:503,json:async()=>({error:"已有诊断报告正在生成"})});
  instance.context.requestResponse=async(url,options)=>{
    assert.equal(instance.button.disabled,true);assert.equal(instance.button.textContent,"正在收集…");
    instance.calls.push({url,options});return {ok:false,status:503,json:async()=>({error:"已有诊断报告正在生成"})};
  };
  await instance.context.downloadDiagnosticReport();
  assert.equal(instance.downloads.length,0);assert.equal(instance.button.disabled,false);
  assert.equal(instance.notices[0].message,"已有诊断报告正在生成");assert.equal(instance.notices[0].type,"error");
});
test("失败响应无法解析时仍恢复下载按钮并保留 HTTP 错误",async()=>{
  const instance=model("network",{ok:false,status:502,json:async()=>{throw Error("invalid JSON");}});
  await instance.context.downloadDiagnosticReport();
  assert.equal(instance.button.disabled,false);assert.match(instance.notices[0].message,/HTTP 502/);
});
test("页面具备六项下载选择和 CarPlay 日志来源，不依赖播放实验",()=>{
  const selector=html.match(/<select id="logs-download-source">(.*?)<\/select>/s)[1];
  assert.deepEqual([...selector.matchAll(/value="([^"]*)"/g)].map(match=>match[1]),["","usb","media","network","application","carplay"]);
  assert.match(html,/data-log-source="carplay"/);
  for(const marker of ["probe_id","playbackProbe","PlaybackTrace"]) assert.equal(source.includes(marker),false);
});
