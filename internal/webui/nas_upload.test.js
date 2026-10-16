"use strict";
const {test}=require("node:test");
const assert=require("node:assert/strict");
const fs=require("node:fs");
const vm=require("node:vm");
const path=require("node:path");
const source=fs.readFileSync(path.join(__dirname,"assets/nas-upload.js"),"utf8");
const model=vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,});
vm.runInContext("const savedQuality = new WeakMap();",model);
for(const name of ["automaticBitrate","readQuality","recordMatchesMoment","qualityMatches","matchingUploadRecord","selectionPlan"]){
  const start=source.indexOf(`  function ${name}(`),end=source.indexOf("\n  }",start)+4;
  vm.runInContext(source.slice(start,end),model);
}
const at="2026-10-02T12:00:00Z",moment={id:"minute",recorded_at:at,videos:[{camera:"front"},{camera:"back"}]};
const original={mode:"original"},compressed={mode:"compressed",layout:"separate",height:720,fps:24,bitrate_kbps:3000};
const merged={...compressed,layout:"composite4a"};
const record={id:"file",moment_id:"minute",recorded_at:at,cameras:["front"],quality:original,is_default_target:true};
test("分视角文件逐个计数，合成仅计一个成品，部分成功只匹配对应视角",()=>{
  const session={moments:[moment]},files=[record,{...record,id:"merged",cameras:["back","front"],quality:merged}];
  assert.equal(model.selectionPlan(session,["minute"],["front","back"],original,files).total,2);
  assert.equal(model.selectionPlan(session,["minute"],["front","back"],original,files).recorded,1);
  assert.equal(model.selectionPlan(session,["minute"],["front","back"],merged,files).total,1);
  assert.equal(model.selectionPlan(session,["minute"],["front","back"],merged,files).recorded,1);
  assert.equal(model.selectionPlan(session,[],["front"],original,files).total,0);
  assert.equal(model.selectionPlan(session,["minute"],[],original,files).total,0);
  assert.equal(model.selectionPlan(session,["minute"],["front","missing"],original,files).missing,1);
});
test("不同目标、画质或合成视角不能当作当前选择的成功记录",()=>{
  assert.equal(model.matchingUploadRecord({...record,is_default_target:false},moment,["front"],original),false);
  assert.equal(model.matchingUploadRecord(record,moment,["front"],compressed),false);
  for(const key of ["height","fps","bitrate_kbps","layout"]){
    assert.equal(model.matchingUploadRecord({...record,quality:compressed},moment,["front"],{...compressed,[key]:"different"}),false);
  }
  assert.equal(model.matchingUploadRecord({...record,quality:merged,cameras:["front","back"]},moment,["front"],merged),false);
});
test("循环窗口变更可按录像时间匹配，同一分钟相隔较远的录像不能混算",()=>{
  assert.equal(model.recordMatchesMoment({...record,moment_id:"old"},{...moment,recorded_at:"2026-10-02T12:00:01Z"}),true);
  assert.equal(model.recordMatchesMoment({...record,moment_id:"old"},{...moment,recorded_at:"2026-10-02T12:00:30Z"}),false);
});
test("画质档位自动匹配有效码率，提高分辨率和帧率时增加预算",()=>{
  let previous=0;
  for(const height of [240,360,480,720,1080]){
    const rate=model.automaticBitrate(height,24);assert.ok(rate>previous);previous=rate;
    let previousFPS=0;
    for(const fps of [10,15,24,30,36]){const rate=model.automaticBitrate(height,fps);assert.ok(rate>=256&&rate<=20000);assert.ok(rate>previousFPS);previousFPS=rate;}
  }
  assert.equal(model.automaticBitrate(720,24),3000);
  assert.equal(model.automaticBitrate(1080,30),7500);
});
test("旧画质保持精确匹配，更换档位后使用自动码率且不改变另一表单",()=>{
  const makeForm=()=>({elements:{quality_mode:{value:"compressed"},quality_height:{value:"480"},quality_fps:{value:"15"},quality_layout:{value:"separate"}}});
  model.settingsForm=makeForm();model.manualForm=makeForm();
  vm.runInContext("savedQuality.set(settingsForm,{height:480,fps:15,bitrate_kbps:1500}); savedQuality.set(manualForm,{height:480,fps:15,bitrate_kbps:1500});",model);
  assert.equal(model.readQuality(model.settingsForm).bitrate_kbps,1500);
  model.settingsForm.elements.quality_fps.value="30";
  vm.runInContext("savedQuality.delete(settingsForm);",model);
  assert.equal(model.readQuality(model.settingsForm).bitrate_kbps,1900);
  assert.equal(model.readQuality(model.manualForm).bitrate_kbps,1500);
  model.settingsForm.elements.quality_fps.value="15";
  assert.equal(model.readQuality(model.settingsForm).bitrate_kbps,900);
});
