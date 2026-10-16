"use strict";

const {test} = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const path = require("node:path");
const script = fs.readFileSync(path.join(__dirname, "assets", "playback-hls.js"), "utf8");
const base = "/api/v1/playback-hls/0123456789abcdef0123456789abcdef";
const reply = () => ({ok:true, json:async () => ({session_url:base, url:base+"/index.m3u8"})});

function setup(agent, fetch) {
  const intervals = new Map();
  let next = 0;
  const context = vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,navigator:{userAgent:agent}, location:{href:"http://box/"},
    URL, AbortController, fetch, setInterval:fn => {intervals.set(++next, fn); return next;},
    clearInterval:id => intervals.delete(id)});
  vm.runInContext(script, context);
  const video = {src:"", load(){}, pause(){}, removeAttribute(){this.src="";}, canPlayType(){return "maybe";}};
  return {api:context.TeslaBoxHLS, video, intervals};
}

test("Android and desktop retain the existing stream without session requests", async () => {
  for (const agent of ["Android Chrome", "Windows Chrome", "Macintosh Safari"]) {
    const {api, video} = setup(agent, () => {throw new Error("unexpected HLS request");});
    assert.equal(api.supported(video), false);
    assert.equal(await api.attach(video, "/composite?quality=smooth"), true);
    assert.equal(video.src, "/composite?quality=smooth");
    await api.release(video);
  }
});

test("iPhone creates HLS retaining seek and snapshot query then deletes on release", async () => {
  const calls = [];
  const {api, video, intervals} = setup("iPhone Safari", async (url, options) => {
    calls.push({url, options}); return reply();
  });
  await api.attach(video, "/api/v1/video-groups/g/composite?start_seconds=12&snapshot_id=s&quality=smooth");
  assert.equal(calls[0].url, "/api/v1/video-groups/g/composite/hls?start_seconds=12&snapshot_id=s&quality=smooth");
  assert.equal(calls[0].options.method, "POST");
  assert.equal(video.src, base+"/index.m3u8");
  assert.equal(intervals.size, 1);
  await api.release(video);
  assert.equal(calls.at(-1).options.method, "DELETE");
  assert.equal(calls.at(-1).url, base);
  assert.equal(intervals.size, 0);
});

test("late create response after closing cannot overwrite a newer video source", async () => {
  let resolve;
  const calls = [];
  const {api, video} = setup("iPhone Safari", (url, options) => {
    calls.push({url, options});
    return options.method === "POST" ? new Promise(done => {resolve=done;}) : Promise.resolve(reply());
  });
  const pending = api.attach(video, "/composite");
  await Promise.resolve();
  await api.release(video);
  video.src="/original.mp4";
  resolve(reply());
  assert.equal(await pending, false);
  assert.equal(video.src, "/original.mp4");
  assert.equal(calls.at(-1).options.method, "DELETE");
});

test("unsupported HLS and invalid server URLs do not switch devices to HLS", async () => {
  const {api, video} = setup("iPhone", async () => ({ok:true, json:async () => ({url:"https://other/media", session_url:"https://other"})}));
  await assert.rejects(api.attach(video, "/composite"), /地址无效/);
  video.canPlayType=()=>"";
  assert.equal(api.supported(video), false);
  assert.equal(await api.attach(video, "/composite"), true);
  assert.equal(video.src, "/composite");
});
