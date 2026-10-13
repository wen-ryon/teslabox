"use strict";
const {test} = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const path = require("node:path");
const app = fs.readFileSync(path.join(__dirname, "assets/app.js"), "utf8");
function extract(name) {
  const start = app.indexOf(`function ${name}(`);
  const end = app.indexOf("\n}\n", start)+2;
  return (app.slice(Math.max(0,start-6), start)==="async " ? "async " : "")+app.slice(start,end);
}
function fixture() {
  const messages=[], events=[];
  const context=vm.createContext({loadToken:1,performance,window:{setTimeout,clearTimeout,setInterval,clearInterval},
    showPlayerMessage:message=>messages.push(message),reportPlaybackDiagnostic:(...args)=>events.push(args)});
  for(const name of ["playVideoSafely","waitForMetadata"]) vm.runInContext(extract(name),context);
  const listeners=new Map();
  const video={src:"/old",readyState:0,error:null,
    getAttribute(){return this.src;},
    addEventListener(name,fn){listeners.set(name,fn);},
    removeEventListener(name,fn){if(listeners.get(name)===fn)listeners.delete(name);},
    play(){return Promise.resolve();}};
  return {context,video,listeners,messages,events};
}
test("play failures distinguish permission, format and abort errors",async()=>{
  for(const [name,expected] of [["NotAllowedError","手动"],["NotSupportedError","格式"],["AbortError","中断"]]){
    const f=fixture(); f.video.play=()=>Promise.reject({name,message:"detail"});
    assert.equal(await f.context.playVideoSafely(f.video,1),false);
    assert.ok(f.messages[0].includes(expected));
    assert.ok(f.events[0][1].includes(name));
  }
});
test("late old play rejection cannot change new player message",async()=>{
  const f=fixture();let reject;
  f.video.play=()=>new Promise((_,fail)=>{reject=fail;});
  const result=f.context.playVideoSafely(f.video,1);
  f.context.loadToken=2;f.video.src="/new";
  reject({name:"AbortError"});
  assert.equal(await result,false);assert.equal(f.messages.length,0);assert.equal(f.events.length,0);
});
test("metadata timeout and media error have distinct diagnostics",async()=>{
  const f=fixture();assert.equal(await f.context.waitForMetadata(f.video,5,1),false);
  assert.match(f.events[0][1],/metadata_timeout/);assert.equal(f.listeners.size,0);
  f.events.length=0;f.video.error={code:4,message:"unsupported"};
  assert.equal(await f.context.waitForMetadata(f.video,500,1),false);
  assert.match(f.events[0][1],/media_error:4:unsupported/);assert.equal(f.listeners.size,0);
});
test("superseded metadata wait releases listeners without reporting failure",async()=>{
  const f=fixture();const result=f.context.waitForMetadata(f.video,1000,1);
  f.context.loadToken=2;
  assert.equal(await result,false);assert.equal(f.listeners.size,0);assert.equal(f.events.length,0);
});
test("metadata success remains compatible",async()=>{
  const f=fixture();const result=f.context.waitForMetadata(f.video,1000,1);
  f.video.readyState=1;f.listeners.get("loadedmetadata")({type:"loadedmetadata"});
  assert.equal(await result,true);assert.equal(f.listeners.size,0);assert.equal(f.events.length,0);
});

function snapshotFixture(request) {
  const moment={id:"minute",recorded_at:"2026-09-30T08:00:00Z",timeline_start_seconds:0,duration_seconds:60};
  const session={id:"archive",snapshot_id:"old",moments:[moment]};
  const context=vm.createContext({request,encodeURIComponent,
    selectMomentAtPosition:()=>({moment,localTime:12}),
    timelineMomentDuration:m=>m.duration_seconds,timelineMomentStart:m=>m.timeline_start_seconds});
  vm.runInContext(extract("refreshPlaybackSessionSnapshot"),context);
  return {context,session,moment};
}

function compositePositionFixture(prepare = async()=>true) {
  const recorded_at="2026-09-30T08:00:00Z";
  const front={id:"front",camera:"front",recorded_at},back={id:"back",camera:"back",recorded_at};
  const moment={videos:[front,back],recorded_at};
  let resolveBack;
  const pendingPlay=new Promise(resolve=>{resolveBack=resolve;});
  const element=play=>({dataset:{},currentTime:4.798,getAttribute:()=>"/video",play});
  const main=element(()=>Promise.resolve()),aux=element(()=>pendingPlay);
  const noop=()=>{};
  const context=vm.createContext({loadToken:0,timelinePositionRequestID:0,timelinePositionPending:false,
    requestedPosition:0,activeSession:{duration_seconds:60,moments:[moment]},activeVideo:front,
    activeMomentIndex:0,activeDisplay:"composite6",activeStreamTimelineStart:null,
    sessionVideo:main,compositeTracks:new Map(),pendingCompositeTracks:new Map(),
    compositeLayouts:{composite6:{cameras:["front","back"],label:"双路"}},
    selectMomentAtPosition:()=>({moment,momentIndex:0,absolute:0}),
    isCompositeDisplay:()=>true,usesServerComposite:()=>false,videoCoversAbsolutePosition:()=>true,
    compositeVideoElement:()=>aux,prepareVideoElement:prepare,canAutomaticallyFallback:()=>false,
    timelineMomentStart:()=>0,sessionMediaURL:u=>u,qualityLabel:()=>"原画",effectivePlaybackQuality:"original",
    $:()=>({}),showPlayerLoading:noop,hidePlayerMessage:noop,showPlayerMessage:noop,
    resetPlaybackTelemetryLoad:noop,releaseCompositeAuxiliaryTracks:noop,resetCompositePlaceholders:noop,
    attachCompositeElement:noop,applyCompositeFrameGeometry:noop,clearUnusedCompositeTracks:noop,
    startPlaybackTelemetryLoad:noop,renderCameraSwitcher:noop,reportPlaybackDiagnostic:noop});
  for(const name of ["playVideoSafely","playCompositeTracks","loadCompositePosition","loadSessionPositionAttempt","currentSessionPosition"])
    vm.runInContext(extract(name),context);
  return {context,main,resolveBack};
}

test("pending auxiliary play cannot retain stale switch position after preparation",async()=>{
  const f=compositePositionFixture();
  let completed=false;
  const loading=f.context.loadSessionPositionAttempt(4.798,true).then(value=>{completed=true;return value;});
  // 固定微任务轮次，不依赖真实时间；旧实现会一直等辅助 play 完成。
  for(let i=0;i<12;i++)await Promise.resolve();
  f.main.currentTime=10.665;
  const position=f.context.currentSessionPosition(),pending=f.context.timelinePositionPending,finishedBeforeAuxiliary=completed;
  f.resolveBack();await loading;
  assert.equal(pending,false);assert.equal(finishedBeforeAuxiliary,true);assert.equal(position,10.665);
});

test("real unfinished seek still keeps the requested target instead of old media time",async()=>{
  let ready;
  const wait=new Promise(resolve=>{ready=resolve;});
  const f=compositePositionFixture(()=>wait);
  const loading=f.context.loadSessionPositionAttempt(30,false);
  f.main.currentTime=10;
  assert.equal(f.context.timelinePositionPending,true);
  assert.equal(f.context.currentSessionPosition(),30);
  ready(true);await loading;
  assert.equal(f.context.timelinePositionPending,false);
});

test("superseded preparation cannot clear a newer positioning operation",async()=>{
  let ready;
  const f=compositePositionFixture();
  // 两路准备共享同一个可控承诺，便于一次完成旧请求。
  const wait=new Promise(resolve=>{ready=resolve;});f.context.prepareVideoElement=()=>wait;
  const loading=f.context.loadSessionPositionAttempt(4.798,false);
  f.context.loadToken++;f.context.timelinePositionRequestID++;f.context.requestedPosition=30;
  ready(true);assert.equal(await loading,false);
  assert.equal(f.context.timelinePositionPending,true);
  assert.equal(f.context.currentSessionPosition(),30);
});
test("409 refresh preserves the original minute and offset",async()=>{
  let calls=0;let f;
  f=snapshotFixture(async url=>{
    calls++;
    if(url.includes("snapshot_id=old"))throw {status:409};
    return {...f.session,snapshot_id:"new",moments:[{...f.moment,timeline_start_seconds:60}]};
  });
  const next=await f.context.refreshPlaybackSessionSnapshot(f.session,12,-1,()=>true);
  assert.equal(next.session.snapshot_id,"new");assert.equal(next.position,72);assert.equal(calls,2);
});
test("unchanged snapshot does not retry codec errors",async()=>{
  let calls=0;const f=snapshotFixture(async()=>{calls++;return {};});
  assert.equal(await f.context.refreshPlaybackSessionSnapshot(f.session,12,-1,()=>true),null);
  assert.equal(calls,1);
});
test("deleted minute and rotating recent window cannot silently switch recordings",async()=>{
  const f=snapshotFixture(async url=>{
    if(url.includes("?"))throw {status:409};
    return {...f.session,snapshot_id:"new",moments:[]};
  });
  await assert.rejects(f.context.refreshPlaybackSessionSnapshot(f.session,12,-1,()=>true),/删除或更新/);
  f.session.id="rw-old";
  await assert.rejects(f.context.refreshPlaybackSessionSnapshot(f.session,12,-1,()=>true),/循环覆盖/);
});
test("late snapshot response cannot take over a newer selection",async()=>{
  let current=true;const f=snapshotFixture(async()=>{current=false;throw {status:409};});
  assert.equal(await f.context.refreshPlaybackSessionSnapshot(f.session,12,-1,()=>current),null);
});

test("player recovery retries once even when the refreshed load fails",async()=>{
  let attempts=0,refreshes=0;
  const session={id:"archive",snapshot_id:"old"};
  const context=vm.createContext({activeSession:session,loadToken:0,
    loadSessionPositionAttempt:async()=>{context.loadToken++;attempts++;return false;},
    refreshPlaybackSessionSnapshot:async()=>{refreshes++;return {session:{...session,snapshot_id:"new"},position:12,momentIndex:0};},
    reportPlaybackDiagnostic(){},$:()=>({}),formatDuration:()=>"",
    renderPlayerMeta(){},renderCameraSwitcher(){},renderSegmentMarkers(){},renderEventMarker(){},showPlayerMessage(){}});
  vm.runInContext(extract("loadSessionPosition"),context);
  assert.equal(await context.loadSessionPosition(12,true),false);
  assert.equal(attempts,2);assert.equal(refreshes,1);
  assert.equal(context.activeSession.snapshot_id,"new");
});
