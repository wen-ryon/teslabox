"use strict";

const {test} = require("node:test");
const assert = require("node:assert/strict");
const {buildCases} = require("./assets/playback-probe-plan.js");

const cameras = ["left_pillar", "front", "right_pillar", "left_repeater", "back", "right_repeater"];
const layouts = {
  composite6:{cameras},
  composite4a:{cameras:["front", "back", "left_repeater", "right_repeater"]},
  composite4b:{cameras:["front", "right_repeater", "back", "left_repeater"]},
  composite3:{cameras:["front", "right_repeater", "left_repeater"]},
};
const moment = {videos:cameras.map(camera => ({id:`video-${camera}`, camera}))};

test("full matrix covers every camera and original/transcoded layouts", () => {
  const plan = buildCases(moment, cameras, layouts, ["smooth", "high"], "full");
  assert.equal(plan.cases.length, 31);
  assert.equal(plan.skipped.length, 0);
  for (const camera of cameras) {
    for (const quality of ["original", "smooth", "high"]) {
      assert.ok(plan.cases.some(item => item.id === `single-${camera}-${quality}`));
    }
  }
  for (const layout of Object.keys(layouts)) {
    for (const quality of ["original", "smooth", "high"]) {
      assert.ok(plan.cases.some(item => item.id === `${layout}-${quality}`));
    }
  }
  assert.equal(plan.cases.find(item => item.id === "single-front-smooth-finite")?.delivery, "finite");
});

test("missing cameras are explicit and never sent to transcoder", () => {
  const limited = {videos:moment.videos.filter(video => video.camera !== "back")};
  const plan = buildCases(limited, cameras, layouts, ["smooth"], "full");
  assert.ok(plan.skipped.some(item => item.id === "single-back"));
  assert.ok(plan.skipped.some(item => item.id === "composite6-smooth"));
  assert.ok(!plan.cases.some(item => item.id === "composite6-smooth"));
  assert.equal(plan.cases.find(item => item.id === "composite6-original").videos.length, 5);
});

test("quick scope and optional standard quality stay bounded", () => {
  const quick = buildCases(moment, cameras, layouts, ["smooth", "high"], "quick");
  assert.equal(quick.cases.length, 7);
  assert.equal(quick.cases[0].camera, "front");
  const full = buildCases(moment, cameras, layouts, ["smooth", "standard", "high"], "full");
  assert.equal(full.cases.length, 41);
});
