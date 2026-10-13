"use strict";
const {test}=require("node:test");
const assert=require("node:assert/strict");
const vm=require("node:vm");
const fs=require("node:fs");
const path=require("node:path");
const source=fs.readFileSync(path.join(__dirname,"assets/playback-trace.js"),"utf8");
function fixture() {
  let clock=1000, counter=0;
  const requests=[],intervals=[],timeouts=new Map();
  const root={performance:{now:()=>clock},crypto:{getRandomValues:a=>{a.fill(++counter);return a;}},
    setInterval:fn=>intervals.push(fn),setTimeout:fn=>{const id=++counter;timeouts.set(id,fn);return id;},clearTimeout:id=>timeouts.delete(id),
    fetch:async(url,options)=>{requests.push(JSON.parse(options.body));return {ok:true};}};
  vm.runInNewContext(source,{window:root,AbortController});
  const listeners=new Map(),frames=new Map();let frameID=0;
  const video={currentTime:0,readyState:0,networkState:0,paused:true,seeking:false,buffered:{length:0},
    addEventListener:(n,f)=>{listeners.set(n,f);},removeEventListener:(n,f)=>{if(listeners.get(n)===f)listeners.delete(n);},
    requestVideoFrameCallback:f=>{frames.set(++frameID,f);return frameID;},cancelVideoFrameCallback:id=>frames.delete(id),
    play(){throw Error("observer must not play");},pause(){throw Error("observer must not pause");},load(){throw Error("observer must not load");}};
  return {trace:root.TeslaBoxPlaybackTrace,root,requests,intervals,video,listeners,frames,advance:n=>{clock+=n;},entries:()=>requests.flatMap(r=>r.events)};
}
test("metadata and play resolution never masquerade as presented frames",async()=>{
  const f=fixture();f.trace.beginOperation();f.trace.watch(f.video,{camera:"front"},false);
  assert.equal(f.frames.size,0);
  f.listeners.get("loadedmetadata")();f.trace.span(f.video,"play")("resolved");
  await f.trace.flush();assert.ok(!f.entries().some(e=>e.phase==="first_frame"));
  f.trace.arm(f.video);f.advance(1800);f.frames.get(1)(0,{mediaTime:0});
  await f.trace.flush();assert.ok(f.entries().some(e=>e.phase==="first_frame" && e.elapsed_ms===1800));
});
test("late phase completion retains old immutable batch identity",async()=>{
  const f=fixture();f.trace.beginOperation();f.trace.watch(f.video,{video_id:"old"},false);
  const finish=f.trace.span(f.video,"seek"); const old=f.trace.context().batch_id;
  f.trace.beginBatch();f.trace.watch(f.video,{video_id:"new"},false);finish("superseded");await f.trace.flush();
  const event=f.entries().find(e=>e.phase==="seek" && e.reason_code==="superseded");
  assert.equal(event.batch_id,old);assert.equal(event.video_id,"old");
});
test("release cancels callbacks and stale callback cannot pollute new source",async()=>{
  const f=fixture();f.trace.beginOperation();f.trace.watch(f.video,{},false);f.trace.arm(f.video);
  const callback=f.frames.get(1);f.trace.unwatch(f.video);callback(0,{mediaTime:10});await f.trace.flush();
  assert.equal(f.listeners.size,0);assert.equal(f.frames.size,0);
  assert.ok(!f.entries().some(e=>e.phase==="first_frame"));
});
test("bounded queue reports loss and events are batched",async()=>{
  const f=fixture();f.trace.beginOperation();
  for(let i=0;i<200;i++)f.trace.mark(null,"test","observed");
  await f.trace.flush();assert.equal(f.requests[0].events.length,24);assert.equal(f.requests[0].dropped_events,72);
});
test("no frame API uses explicitly labelled clock observation",async()=>{
  const f=fixture();delete f.video.requestVideoFrameCallback;
  f.trace.beginOperation();f.trace.watch(f.video,{},false);f.trace.arm(f.video);
  f.advance(2000);f.video.currentTime=1;f.video.paused=false;f.intervals[0]();await f.trace.flush();
  assert.ok(f.entries().some(e=>e.reason_code==="media_clock_fallback"));
  assert.ok(!f.entries().some(e=>e.phase==="first_frame"));
});
test("transport failure never retries infinitely and losses are visible",async()=>{
  const f=fixture();f.trace.beginOperation();f.trace.mark(null,"test","observed");
  f.root.fetch=async()=>{throw Error("offline");};await f.trace.flush();
  f.root.fetch=async(_,o)=>{f.requests.push(JSON.parse(o.body));return {ok:true};};
  f.trace.mark(null,"test","observed");await f.trace.flush();assert.equal(f.requests[0].dropped_events,1);
});

test("one in-flight upload prevents unbounded network concurrency",async()=>{
  const f=fixture();let release,calls=0;
  f.root.fetch=()=>{calls++;return new Promise(resolve=>{release=resolve;});};
  f.trace.beginOperation();f.trace.mark(null,"test","observed");
  const pending=f.trace.flush();f.trace.mark(null,"test","observed");f.trace.flush();f.trace.flush();
  assert.equal(calls,1);release({ok:true});await pending;
});

test("observer cap releases excess listeners without changing media",async()=>{
  const f=fixture();f.trace.beginOperation();
  const all=[];
  for(let i=0;i<7;i++){const x=fixture();all.push(x);f.trace.watch(x.video,{camera:"front"},false);}
  assert.equal(all[0].listeners.size,0);assert.ok(all[6].listeners.size>0);
  f.trace.stop();for(const x of all)assert.equal(x.listeners.size,0);
});

test("switch baseline is immutable and actual instance is separate from source reuse",async()=>{
  const f=fixture();f.trace.beginOperation();
  f.trace.watch(f.video,{camera:"front",video_id:"a"},false,"/a");f.trace.arm(f.video);
  f.video.currentTime=12;f.trace.beforeSwitch("layout_switch");
  f.video.currentTime=0;f.trace.beginBatch();
  f.trace.watch(f.video,{camera:"front",video_id:"b"},false,"/b");f.trace.arm(f.video);
  await f.trace.flush();
  const before=f.entries().find(e=>e.phase==="switch_baseline");
  const after=f.entries().find(e=>e.phase==="switch_result" && e.video_id==="b");
  assert.equal(before.state.current_time_seconds,12);
  assert.equal(before.video_id,"a");assert.equal(after.reason_code,"same_instance");
  assert.equal(before.switch_id,after.switch_id);assert.equal(before.instance_id,after.instance_id);
  assert.ok(f.entries().some(e=>e.phase==="source_result" && e.video_id==="b" && e.reason_code==="source_assigned"));
});

test("stalled does not imply prior progress and seek jumps are not playback",async()=>{
  const f=fixture();f.trace.beginOperation();f.trace.watch(f.video,{},false);f.video.paused=false;
  f.listeners.get("stalled")();f.video.currentTime=40;f.listeners.get("seeking")();f.listeners.get("seeked")();
  f.advance(2000);f.intervals[0]();await f.trace.flush();
  assert.equal(f.entries().find(e=>e.reason_code==="stalled").ever_clock_advanced,false);
  assert.ok(!f.entries().some(e=>e.reason_code==="media_clock_fallback"));
  f.advance(2000);f.video.currentTime=41;f.intervals[0]();
  f.advance(2000);f.intervals[0]();f.listeners.get("stalled")();await f.trace.flush();
  assert.ok(f.entries().some(e=>e.progress_state==="stationary_after_advance" && e.ever_clock_advanced));
});

test("history outside bounded metadata window remains unknown",async()=>{
  const f=fixture();f.trace.beginOperation();
  for(let i=0;i<50;i++){f.trace.watch(f.video,{video_id:String(i)},false,"/"+i);f.trace.unwatch(f.video);await f.trace.flush();}
  f.trace.watch(f.video,{video_id:"0"},false,"/0");await f.trace.flush();
  assert.equal(f.entries().filter(e=>e.phase==="history_baseline").at(-1).history_status,"unknown");
});

test("diagnostic toggle never mutates media controls or URL",async()=>{
  const f=fixture();f.video.src="/api/v1/videos/a?snapshot_id=stable";
  f.trace.beginOperation();f.trace.watch(f.video,{},false,f.video.src);f.trace.arm(f.video);
  f.trace.setEnabled(false);f.trace.watch(f.video,{},false);f.trace.arm(f.video);
  assert.equal(f.listeners.size,0);assert.equal(f.frames.size,0);
  assert.equal(f.video.src,"/api/v1/videos/a?snapshot_id=stable");assert.equal(f.video.currentTime,0);
  f.trace.setEnabled(true);f.trace.watch(f.video,{},false);await f.trace.flush();
  assert.equal(f.video.paused,true);
});

test("unknown event survives queue pressure from auxiliary samples",async()=>{
  const f=fixture();f.trace.beginOperation();f.trace.mark(null,"future_event","new");
  for(let i=0;i<160;i++)f.trace.mark(null,"buffer_sample","observed");
  await f.trace.flush();assert.ok(f.entries().some(e=>e.phase==="future_event"));
});
