"use strict";

(function (root) {
  function buildCases(moment, cameraOrder, layouts, qualities, scope) {
    const videos = new Map((moment?.videos || []).filter(video => video?.id && video?.camera).map(video => [video.camera, video]));
    const selectedQualities = [...new Set(qualities)].filter(quality => ["smooth", "standard", "high"].includes(quality));
    const available = cameraOrder.filter(camera => videos.has(camera));
    const singleCameras = scope === "quick" ? (videos.has("front") ? ["front"] : available.slice(0, 1)) : available;
    const layoutNames = scope === "quick" ? ["composite6"] : Object.keys(layouts);
    const cases = [];
    const skipped = [];
    for (const camera of cameraOrder) {
      if (scope === "quick" && !singleCameras.includes(camera)) continue;
      if (!videos.has(camera)) {
        skipped.push({id:`single-${camera}`, reason:"当前片段缺少该视角"});
        continue;
      }
      if (!singleCameras.includes(camera)) continue;
      for (const quality of ["original", ...selectedQualities]) {
        cases.push({
          id:`single-${camera}-${quality}`, layout:"single", stage:quality === "original" ? "original" : "transcoded",
          quality, camera, cameras:[camera], videos:[videos.get(camera)], label:`${camera} · 单路${quality}`,
        });
      }
    }
    if (selectedQualities.includes("smooth") && singleCameras.length) {
      const camera = singleCameras.includes("front") ? "front" : singleCameras[0];
      cases.push({
        id:`single-${camera}-smooth-finite`, layout:"single", stage:"transcoded", delivery:"finite",
        quality:"smooth", camera, cameras:[camera], videos:[videos.get(camera)],
        label:`${camera} · 单路流畅有限文件对照`,
      });
    }
    for (const layout of layoutNames) {
      const required = layouts[layout]?.cameras || [];
      const selected = required.filter(camera => videos.has(camera));
      if (selected.length < 2) {
        skipped.push({id:`${layout}-original`, reason:"当前片段少于两路可用视角"});
      } else {
        cases.push({
          id:`${layout}-original`, layout, stage:"original", quality:"original", camera:"",
          cameras:selected, videos:selected.map(camera => videos.get(camera)),
          label:`${layout} · 多路原画（${selected.length} 路）`,
        });
      }
      for (const quality of selectedQualities) {
        if (required.length === 0 || selected.length !== required.length) {
          skipped.push({id:`${layout}-${quality}`, reason:"板端合成要求该布局全部视角齐全"});
          continue;
        }
        cases.push({
          id:`${layout}-${quality}`, layout, stage:"transcoded", quality, camera:"",
          cameras:selected, videos:selected.map(camera => videos.get(camera)),
          label:`${layout} · 多路${quality}`,
        });
      }
    }
    return {cases, skipped, available};
  }

  root.TeslaBoxPlaybackProbePlan = Object.freeze({buildCases});
  if (typeof module !== "undefined" && module.exports) module.exports = root.TeslaBoxPlaybackProbePlan;
})(typeof window === "undefined" ? globalThis : window);
