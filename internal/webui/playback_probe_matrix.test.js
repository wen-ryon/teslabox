"use strict";

const {test} = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const path = require("node:path");

const script = fs.readFileSync(path.join(__dirname, "assets", "playback-probe-matrix.js"), "utf8");

function matrixWithRequest(request) {
  const context = vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,request, encodeURIComponent, URLSearchParams});
  vm.runInContext(script, context);
  return context;
}

function source() {
  return {
    clipType:"archive",
    session:{id:"session-1", snapshot_id:"snapshot-old"},
    moment:{id:"moment-1", videos:[{id:"front-old", camera:"front"}]},
  };
}

test("archive snapshot rotation refreshes source and video IDs", async () => {
  const calls = [];
  const context = matrixWithRequest(async url => {
    calls.push(url);
    if (url.startsWith("/api/v1/video-sessions?")) return {items:[{id:"session-1", snapshot_id:"snapshot-new"}]};
    return {id:"session-1", snapshot_id:"snapshot-new", moments:[{id:"moment-1", videos:[{id:"front-new", camera:"front"}]}]};
  });
  const selected = source();
  const testCase = {id:"single-front-original", stage:"original", camera:"front", cameras:["front"]};
  const changed = await context.playbackMatrixRefreshSnapshot(selected, testCase);
  assert.equal(changed, true);
  assert.equal(selected.session.snapshot_id, "snapshot-new");
  assert.equal(selected.moment.videos[0].id, "front-new");
  assert.equal(calls.length, 2);
  const mediaURL = context.playbackMatrixSource(selected, testCase, "front", "0123456789abcdef0123456789abcdef");
  assert.match(mediaURL, /front-new\/content/);
  assert.match(mediaURL, /snapshot_id=snapshot-new/);
});

test("unchanged snapshot does not reload details", async () => {
  const calls = [];
  const context = matrixWithRequest(async url => {
    calls.push(url);
    return {items:[{id:"session-1", snapshot_id:"snapshot-old"}]};
  });
  const changed = await context.playbackMatrixRefreshSnapshot(source(), {cameras:["front"]});
  assert.equal(changed, false);
  assert.equal(calls.length, 1);
});

test("rotated-away moment stops instead of reporting playback failure", async () => {
  const context = matrixWithRequest(async url => url.startsWith("/api/v1/video-sessions?")
    ? {items:[{id:"session-1", snapshot_id:"snapshot-new"}]}
    : {id:"session-1", snapshot_id:"snapshot-new", moments:[]});
  await assert.rejects(context.playbackMatrixRefreshSnapshot(source(), {cameras:["front"]}), /所选片段已变化/);
});

test("rotated-away camera and cached-only index stop the matrix", async () => {
  const missingCamera = matrixWithRequest(async url => url.startsWith("/api/v1/video-sessions?")
    ? {items:[{id:"session-1", snapshot_id:"snapshot-new"}]}
    : {id:"session-1", snapshot_id:"snapshot-new", moments:[{id:"moment-1", videos:[]}]});
  await assert.rejects(missingCamera.playbackMatrixRefreshSnapshot(source(), {cameras:["front"]}), /缺少待测视角/);
  const cachedOnly = matrixWithRequest(async () => ({items:[{id:"session-1", snapshot_id:"cache:old", cached:true}]}));
  await assert.rejects(cachedOnly.playbackMatrixRefreshSnapshot(source(), {cameras:["front"]}), /快照暂不可用/);
});

test("case ends early only when every track has failed or ended", () => {
  const context = matrixWithRequest(async () => ({}));
  assert.equal(context.playbackMatrixTracksDone([{error:"媒体错误", video:{ended:false}}, {error:"", video:{ended:false}}]), false);
  assert.equal(context.playbackMatrixTracksDone([{error:"媒体错误", video:{ended:false}}, {error:"", video:{ended:true}}]), true);
});

test("finite control captures the same stream only through two complete fragments", async () => {
  const box = (type, payload) => {
    const data = Buffer.alloc(8 + payload.length);
    data.writeUInt32BE(data.length, 0);
    data.write(type, 4, "ascii");
    Buffer.from(payload).copy(data, 8);
    return data;
  };
  const first = Buffer.concat([box("ftyp", [1]), box("moov", [2]), box("moof", [3]), box("mdat", [4])]);
  const second = Buffer.concat([box("moof", [5]), box("mdat", [6])]);
  const third = box("moof", [7]);
  const context = matrixWithRequest(async () => ({}));
  context.playbackProbeRun = 1;
  context.AbortController = AbortController;
  context.Blob = Blob;
  context.window = {setTimeout:() => 1, clearTimeout() {}};
  let reads = 0;
  let canceled = false;
  context.fetch = async () => ({ok:true, body:{getReader:() => ({
    async read() {reads++; return {done:false, value:new Uint8Array([first, second, third][reads - 1])};},
    async cancel() {canceled = true;},
  })}});
  const result = await context.playbackMatrixCaptureFinite("/composite", 1);
  assert.equal(result.bytes, first.length + second.length);
  assert.equal(result.blob.size, first.length + second.length);
  assert.equal(reads, 2);
  assert.equal(canceled, true);
});

test("finite control rejects incomplete media fragments", () => {
  const context = matrixWithRequest(async () => ({}));
  const bytes = new Uint8Array([0, 0, 0, 12, 109, 111, 111, 102, 1, 2]);
  assert.equal(context.playbackMatrixCompleteFragments(bytes, bytes.length), 0);
});

test("unsupported media finishes without waiting for the 12 second deadline", {timeout:1000}, async () => {
  const timers = [];
  const container = {hidden:true, append() {}};
  const context = matrixWithRequest(async () => ({}));
  context.$ = () => container;
  context.document = {createElement:() => ({
    error:null, ended:false, currentTime:0,
    setAttribute() {}, addEventListener() {}, removeEventListener() {}, load() {},
    play() {return Promise.reject(Object.assign(new Error("unsupported"), {name:"NotSupportedError"}));},
  })};
  context.performance = {now:() => 100};
  context.window = {
    setTimeout(callback, delay) {
      timers.push(delay);
      if (delay === 200) queueMicrotask(callback);
      return timers.length;
    },
    clearTimeout() {},
  };
  context.playbackProbeRun = 1;
  context.playbackMatrixClearVideos = () => {};
  context.playbackMatrixEvent = () => {};
  const result = await context.playbackMatrixRunCase(source(), {
    id:"single-front-smooth", layout:"single", quality:"smooth", stage:"transcoded",
    camera:"front", cameras:["front"],
  }, "0123456789abcdef0123456789abcdef", [], 1);
  assert.match(result, /异常/);
  assert.ok(timers.includes(200));
  assert.ok(timers.includes(12000));
});

test("snapshot rotation retries one failed case only once", async () => {
  const context = matrixWithRequest(async () => ({}));
  context.playbackProbeRun = 1;
  context.performance = {now:() => 0};
  context.playbackMatrixEvent = () => {};
  let runs = 0;
  let retries = 0;
  context.playbackMatrixRunCase = async () => ++runs === 1 ? "1/1 路异常" : "通过";
  context.playbackMatrixRefreshSnapshot = async () => true;
  const outcome = await context.playbackMatrixRunCaseWithSnapshotRetry(source(), {cameras:["front"]}, "id", [], 1, () => retries++);
  assert.equal(outcome, "通过");
  assert.equal(runs, 2);
  assert.equal(retries, 1);
});

test("failed case without rotation is not retried", async () => {
  const context = matrixWithRequest(async () => ({}));
  context.playbackProbeRun = 1;
  let runs = 0;
  context.playbackMatrixRunCase = async () => {runs++; return "1/1 路异常";};
  context.playbackMatrixRefreshSnapshot = async () => false;
  const outcome = await context.playbackMatrixRunCaseWithSnapshotRetry(source(), {cameras:["front"]}, "id", [], 1, () => {});
  assert.equal(outcome, "1/1 路异常");
  assert.equal(runs, 1);
});
