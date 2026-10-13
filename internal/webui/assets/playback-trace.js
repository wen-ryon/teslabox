"use strict";

// 只观察生命周期；不得调用 play/pause/load、修改 src/currentTime 或控制重试。
(function (root) {
  const version = "20260930-6";
  const watches = new Map(), queue = [];
  const instances = new WeakMap(), history = new Map();
  let switchID = "", baseline = new Map(), pendingBaseline = false, enabled = true;
  let operation = "", batch = "", dropped = 0, inflight = null, sequence = 0;
  const now = () => root.performance.now();
  const id = () => Array.from(root.crypto.getRandomValues(new Uint8Array(16)), n => n.toString(16).padStart(2,"0")).join("");
  function state(video) {
    const ranges = [];
    for (let i=0; i<Math.min(video?.buffered?.length || 0,4); i++) ranges.push([video.buffered.start(i),video.buffered.end(i)]);
    const q = video?.getVideoPlaybackQuality?.();
    return {current_time_seconds:video?.currentTime || 0, ready_state:video?.readyState || 0,
      network_state:video?.networkState || 0, paused:!!video?.paused, seeking:!!video?.seeking,
      media_error_code:video?.error?.code || 0, total_frames:q?.totalVideoFrames || 0,
      dropped_frames:q?.droppedVideoFrames || 0, buffered_ranges:ranges,
      frame_callback_supported:!!video?.requestVideoFrameCallback, frame_counter_supported:!!video?.getVideoPlaybackQuality};
  }
  function emit(context, video, phase, reason, duration=0) {
    if (!enabled || !context?.operation_id) return;
    const auxiliary = phase === "buffer_sample";
    const entry={...context, schema_version:1, frontend_version:version, event_seq:++sequence,
      client_time_ms:Date.now(), monotonic_ms:Math.round(now()), phase, reason_code:reason,
      elapsed_ms:Math.max(0,Math.round(duration)), state_scope:phase==="history_baseline" || (phase==="lifecycle" && reason==="source_load") ? "pre_assignment" : "current_element", state:state(video)};
    const w=watches.get(video);
    if (w && w.context===context) Object.assign(entry, {progress_state:w.progressState,
      ever_clock_advanced:w.progress, last_load_ms:w.lastLoad, last_play_ms:w.lastPlay,
      last_progress_ms:w.lastProgress});
    if (queue.length >= 128) {
      const index=queue.findIndex(e=>e.phase==="buffer_sample");
      dropped++;
      if (index<0 && auxiliary) return;
      queue.splice(index<0 ? 0 : index,1);
    }
    queue.push(entry);
  }
  function context(extra={}) { return {operation_id:operation,batch_id:batch,...extra}; }
  function beginOperation() { operation=id(); batch=id();switchID="";baseline=new Map();pendingBaseline=false;return operation; }
  function beforeSwitch(reason="load") {
    switchID=id(); baseline=new Map(); pendingBaseline=true;
    for (const [video,w] of watches) {
      baseline.set(w.context.camera,w.context.instance_id);
      // 在控制器暂停、清源或释放之前复制数值，后续事件不能改写本条基线。
      emit({...w.context,switch_id:switchID,last_load_ms:w.lastLoad,last_play_ms:w.lastPlay,
        last_progress_ms:w.lastProgress,progress_state:w.progressState,ever_clock_advanced:w.progress},video,"switch_baseline",reason);
    }
  }
  function beginBatch() {
    if (!operation) beginOperation();
    if (!pendingBaseline) beforeSwitch();
    pendingBaseline=false; batch=id();
  }
  function mark(video, phase, reason, duration=0) { emit(watches.get(video)?.context || context(),video,phase,reason,duration); }
  function hls(video, sessionID) {
    const w=watches.get(video);
    if (w) w.context={...w.context,hls_session_id:sessionID};
  }
  function span(video, phase, target) {
    const owner=watches.get(video);
    if (owner && phase==="play") owner.lastPlay=Math.round(now());
    if (owner && phase==="seek") resetProgress(video,owner);
    const c={...(owner?.context || context()),span_id:id(), ...(Number.isFinite(target) ? {target_time_seconds:target} : {})}, start=now();
    emit(c,video,phase,"started");
    return reason => { try {
      const stale=owner && watches.get(video)!==owner;
      emit(c,stale ? null : video,phase,stale ? "superseded" : reason,now()-start);
    } catch (_) {} };
  }
  function unwatch(video, reason="released") {
    const w=watches.get(video);
    if (!w) return;
    emit(w.context,video,"lifecycle",reason,now()-w.started);
    history.delete(w.historyKey);
    history.set(w.historyKey,{last_load_ms:w.lastLoad,last_play_ms:w.lastPlay,last_progress_ms:w.lastProgress});
    if (history.size>48) history.delete(history.keys().next().value);
    watches.delete(video);
    w.listeners.forEach(([name,fn])=>video.removeEventListener(name,fn));
    if (w.frame != null) video.cancelVideoFrameCallback?.(w.frame);
  }
  function watch(video, extra, reused, sourceIdentity="") {
    if (!enabled) return;
    unwatch(video,"observation_replaced");
    // 只允许当前布局中的元素被观察，避免诊断本身保留无界播放器引用。
    if (watches.size >= 6) unwatch(watches.keys().next().value,"observer_limit");
    if (!instances.has(video)) instances.set(video,id());
    const historyKey=JSON.stringify([extra.session_id,extra.snapshot_id,extra.video_id,extra.transport,extra.quality,sourceIdentity]);
    const previous=history.get(historyKey);
    const w={context:context({...extra,instance_id:instances.get(video),switch_id:switchID,
      previous_instance_id:baseline.get(extra.camera) || "",history_status:previous ? "observed" : "unknown"}),
      historyKey,started:now(),listeners:[],frame:null,firstFrame:false,reused,
      lastLoad:reused ? (previous?.last_load_ms || 0) : Math.round(now()),lastPlay:previous?.last_play_ms || 0,lastProgress:previous?.last_progress_ms || 0,
      progress:false,progressState:"never_advanced",seekEpoch:0,skipSample:false,
      previousTime:video.currentTime,lastSample:0,lastEvents:new Map(),frameStart:0};
    watches.set(video,w);
    emit({...w.context,...previous},video,"history_baseline",previous ? "observed" : "unknown");
    emit(w.context,video,"lifecycle",reused ? "reused" : "source_load");
    const current=()=>watches.get(video)===w;
    for (const name of ["loadedmetadata","loadeddata","canplay","playing","waiting","stalled","pause","seeking","seeked","error","ended","emptied","abort"]) {
      const fn=()=>{
        if (!current()) return;
        if (name==="seeking" || name==="seeked" || name==="emptied") resetProgress(video,w);
        const t=now();
        if (t-(w.lastEvents.get(name) ?? -Infinity)<1000) return;
        w.lastEvents.set(name,t);
        emit(w.context,video,"media_event",name,t-w.started);
      };
      w.listeners.push([name,fn]); video.addEventListener(name,fn);
    }
  }
  function resetProgress(video,w) {
    w.previousTime=video.currentTime; w.skipSample=true; w.seekEpoch++;
    w.progressState="seeking";
  }
  function arm(video) {
    const w=watches.get(video);
    if (!w || w.armed) return;
    w.armed=true;
    emit(w.context,video,"switch_result",w.context.previous_instance_id===w.context.instance_id ? "same_instance" : "new_or_unknown_instance");
    emit(w.context,video,"source_result",w.reused ? "source_preserved" : "source_assigned");
    const current=()=>watches.get(video)===w;
    // 赋源后才注册，避免 HLS 准备期间把上一媒体的尾帧记作首帧。
    // 每次准备最多观察两次呈现回调；后续推进用低频样本，不逐帧上报。
    if (video.requestVideoFrameCallback) {
      const frameEpoch=w.seekEpoch;
      w.frame=video.requestVideoFrameCallback((_,meta)=>{
        if (!current()) return;
        w.frame=null; w.firstFrame=true; w.frameStart=meta.mediaTime; w.frameEpoch=frameEpoch;
        emit(w.context,video,"first_frame","video_frame_callback",now()-w.started);
      });
    }
  }
  function sample() {
    for (const [video,w] of watches) {
      const t=now(), elapsed=t-w.started;
      if (t-w.lastSample < (elapsed<10000 ? 2000 : 10000)) continue;
      w.lastSample=t;
      const advanced=!w.skipSample && !video.paused && !video.seeking && !root.document?.hidden && video.currentTime>w.previousTime+.1;
      const previousState=w.progressState;
      w.progressState=root.document?.hidden ? "hidden" : video.seeking ? "seeking" : video.paused ? "paused" : w.skipSample ? "unknown" : advanced ? "advancing" : w.progress ? "stationary_after_advance" : "never_advanced";
      if (advanced) w.lastProgress=Math.round(t);
      if (previousState!==w.progressState) emit(w.context,video,"progress_state",w.progressState,elapsed);
      emit(w.context,video,"buffer_sample",video.paused ? "paused" : "observed",elapsed);
      if (!w.progress && advanced) {
        w.progress=true;
        emit(w.context,video,"frame_progress","media_clock_fallback",elapsed);
      }
      if (w.firstFrame && w.frame == null && !w.secondScheduled && !video.paused && !video.seeking) {
        w.secondScheduled=true;
        const epoch=w.seekEpoch;
        w.frame=video.requestVideoFrameCallback((_,meta)=>{
          if (watches.get(video)!==w) return;
          w.frame=null;
          emit(w.context,video,"frame_progress",epoch!==w.seekEpoch || w.frameEpoch!==epoch ? "seek_invalidated" : meta.mediaTime>w.frameStart ? "video_frame_callback" : "frame_not_advanced",now()-w.started);
        });
      }
      w.previousTime=video.currentTime;
      w.skipSample=false;
    }
  }
  function flush() {
    if (inflight) return inflight;
    if (!queue.length) return Promise.resolve();
    const events=queue.splice(0,24), lost=dropped; dropped=0;
    const controller=new AbortController(), timer=root.setTimeout(()=>controller.abort(),5000);
    inflight=root.fetch("/api/v1/diagnostics/playback-trace",{method:"POST",credentials:"same-origin",
      headers:{"Content-Type":"application/json"},signal:controller.signal,
      body:JSON.stringify({events,dropped_events:lost})}).then(response=>{
        if (!response.ok) dropped+=events.length+lost;
      }).catch(()=>{dropped+=events.length+lost;}).finally(()=>{root.clearTimeout(timer);inflight=null;});
    return inflight;
  }
  async function drain() {
    // 最多两批且总共两秒，不让网络故障阻塞报告下载。
    const work=(async()=>{await flush();await flush();})();
    let timer;
    await Promise.race([work,new Promise(resolve=>{timer=root.setTimeout(resolve,2000);})]);
    root.clearTimeout(timer);
  }
  // 诊断失败不能改变播放器的控制流。
  const safe=fn=>(...args)=>{try{return fn(...args);}catch(_){return undefined;}};
  root.TeslaBoxPlaybackTrace={version,beginOperation:safe(beginOperation),beginBatch:safe(beginBatch),
    beforeSwitch:safe(beforeSwitch),setEnabled:safe(value=>{if(!value) for(const video of [...watches.keys()]) unwatch(video,"diagnostics_disabled");enabled=!!value;}),
    watch:safe(watch),arm:safe(arm),hls:safe(hls),unwatch:safe(unwatch),mark:safe(mark),span:(...args)=>safe(span)(...args) || (()=>{}),
    context:safe(context),flush,drain,stop:safe(()=>{for(const video of [...watches.keys()]) unwatch(video,"player_closed");operation="";batch="";})};
  root.setInterval(safe(sample),1000);
  root.setInterval(()=>{void flush();},2000);
})(typeof window !== "undefined" ? window : globalThis);
