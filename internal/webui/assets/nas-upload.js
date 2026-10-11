/* 上传配置独立加载，保持播放时间轴和导出流程的原有行为。 */
(() => {
  const layouts = {composite6:["left_pillar","front","right_pillar","left_repeater","back","right_repeater"],composite4a:["front","back","left_repeater","right_repeater"],composite4b:["front","back","left_repeater","right_repeater"],composite3:["front","left_repeater","right_repeater"]};
  const labels = {front:"前方",back:"后方",left_repeater:"左后",right_repeater:"右后",left_pillar:"左前",right_pillar:"右前"};
  const states = {copying:"准备系统盘录像",ready:"等待上传",uploading:"正在上传并校验",waiting_retry:"等待自动重试",failed:"上传失败",completed:"上传完成",canceled:"已取消"};
  let targets = [], uploadedFiles = [], countsAvailable = false, tasks = [], busy = false, automaticEvents = [], automaticConfig = null, automaticDirty = false, automaticScan = null;
  const cards = new Map();
  const settings = document.createElement("section");
  settings.className = "settings-section";
  settings.id = "nas-upload-settings";
  settings.innerHTML = `<div class="settings-section-heading"><div><p class="eyebrow">RECORDING UPLOAD</p><h2>录像上传</h2></div></div>
    <article class="settings-card nas-server-card">
    <div class="settings-card-head"><div><h3>服务器配置</h3><p>管理上传服务器和目录，可保存多个配置，并指定一个默认目标用于手动与自动上传。</p></div></div>
    <form id="nas-default-form" class="nas-default-form"><label>默认上传目标<select name="default_target" required><option value="">请选择已保存的目标</option></select></label><button class="secondary compact" type="submit">保存默认目标</button></form><p id="nas-default-status" class="nas-form-status" role="status"></p>
    <div class="nas-server-editor settings-form"><label>编辑服务器<select id="nas-target-selector"><option value="">新增服务器</option></select></label></div>
    <form id="nas-target-form" class="settings-form">
    <div class="form-grid nas-server-fields">
      <label>目标名称<input name="name" required maxlength="100" placeholder="例如家里的群晖"></label>
      <label>上传协议<select name="protocol"><option value="smb">SMB</option><option value="webdav">WebDAV</option><option value="sftp">SFTP</option></select></label>
      <label data-nas-host>主机名或 IP<input name="host" placeholder="nas.example.com"></label><label data-nas-host>端口<input name="port" type="number" min="1" max="65535" placeholder="留空使用协议默认端口"></label>
      <label data-nas-webdav class="nas-full-width" hidden>WebDAV 地址<input name="url" type="url" placeholder="https://nas.example.com/dav/"></label>
      <label>用户名<input name="user" required autocomplete="off"></label><label>密码<input name="password" type="password" autocomplete="new-password" placeholder="编辑时留空保留已保存密码"></label>
      <label data-nas-smb>SMB 域（可选）<input name="domain"></label><label>上传目录<input name="path" placeholder="SMB：共享名/目录；其他：目标内的目录"></label>
      <label data-nas-sftp class="nas-full-width" hidden>SFTP 服务器公钥<input name="host_key" placeholder="ssh-ed25519 AAAAC3…"><small>填写已核对的服务器公钥，不是 SHA256 指纹。</small></label>
    </div>
    <label data-nas-webdav class="toggle-choice nas-http-choice" hidden><input name="allow_http" type="checkbox"><span class="toggle-choice-control" aria-hidden="true"></span><span class="toggle-choice-copy"><strong>允许 HTTP 上传</strong><small>仅用于可信内网或测试隧道，默认使用 HTTPS。</small></span></label>
    <div class="form-actions nas-server-actions"><button class="primary" type="submit">保存服务器配置</button><div class="nas-action-buttons"><button id="nas-test" class="secondary compact" type="button">测试读写与校验</button><button id="nas-delete" class="secondary compact" type="button">删除服务器</button></div></div><p id="nas-settings-status" class="nas-form-status" role="status"></p>
    </form></article>
    <article class="settings-card nas-quality-card"><div class="settings-card-head"><div><h3>上传画质</h3><p>设置所选服务器的默认画质，手动上传和后续自动上传均沿用此配置。</p></div><span id="nas-quality-target" class="badge">新增服务器</span></div>
    <form id="nas-quality-form" class="settings-form"><fieldset class="nas-quality nas-quality-settings" aria-label="默认上传画质"></fieldset>
    <div class="form-actions"><span class="muted">仅保存画质，不修改服务器配置。</span><button class="primary" type="submit">保存画质设置</button></div><p id="nas-quality-status" class="nas-form-status" role="status"></p>
    </form></article>
    <article class="settings-card nas-automatic-card"><div class="settings-card-head"><div><h3>事件自动上传</h3><p>只处理哨兵和紧急制动等车辆事件。最近录像、用户手动保存录像不自动上传。</p></div><span id="nas-automatic-state" class="badge">读取中</span></div>
    <form id="nas-automatic-form" class="settings-form">
    <label class="toggle-choice nas-auto-switch"><input name="enabled" type="checkbox"><span class="toggle-choice-control" aria-hidden="true"></span><span class="toggle-choice-copy"><strong>开启事件自动上传</strong><small>关闭后停止发现新事件；已创建任务可在记录中单独取消。</small></span></label>
    <p id="nas-auto-target" class="muted nas-quality-help"></p>
    <p id="nas-auto-scan" class="muted nas-form-status" role="status"></p>
    <div class="form-grid nas-server-fields">
      <label>上传视角<select name="auto_view"><option value="event">仅上传事件视角</option><option value="supplement">事件视角 + 附加合成视频</option></select><small>事件主视频始终保留，附加合成不会替代主视频。</small></label>
      <label>上传片段范围<select name="scope"><option value="event">事件所在分钟</option><option value="surrounding">事件所在分钟 + 前后一分钟</option><option value="directory">整个事件目录</option></select><small>按完整分钟文件上传，事件时间超出末尾时取最后一段。</small></label>
      <label data-nas-supplement hidden>附加合成布局<select name="supplement_layout"><option value="composite6">6 路</option><option value="composite4a">4 路 A</option><option value="composite4b">4 路 B</option><option value="composite3">3 路</option></select></label>
    </div>
    <fieldset class="nas-quality nas-quality-settings nas-auto-quality" data-nas-supplement hidden><legend>附加合成画质</legend></fieldset>
    <label class="toggle-choice nas-auto-switch"><input name="include_existing" type="checkbox"><span class="toggle-choice-control" aria-hidden="true"></span><span class="toggle-choice-copy"><strong>补传启用时已有的车辆事件</strong><small>默认只处理启用后发现的新目录；勾选后也处理当前已有的符合条件事件。</small></span></label>
    <div class="form-actions"><span class="muted">主视频沿用默认目标的画质，附加合成使用独立画质。</span><button class="primary" type="submit">保存自动上传设置</button></div><p id="nas-auto-status" class="nas-form-status" role="status"></p>
    </form><div id="nas-auto-events"></div></article>
    <article class="settings-card nas-history-card"><div class="settings-card-head"><div><h3>上传任务和记录</h3><p>按已校验的远端文件统计：各视角分别计数，合成视频计一个文件，同一远端文件的重复核验不重复计数。断网会自动重试，重启后继续已准备的任务。</p></div><button id="nas-refresh" class="secondary compact" type="button">刷新上传记录</button></div><div id="nas-task-list"></div></article>`;
  document.getElementById("settings-page")?.append(settings);
  const form = settings.querySelector("#nas-target-form"), selector = settings.querySelector("#nas-target-selector"), status = settings.querySelector("#nas-settings-status");
  const qualityForm=settings.querySelector("#nas-quality-form"),qualityStatus=settings.querySelector("#nas-quality-status");
  let qualityDirty=false;
  const automaticForm=settings.querySelector("#nas-automatic-form"),automaticStatus=settings.querySelector("#nas-auto-status");
  const defaultForm=settings.querySelector("#nas-default-form"),defaultSelector=defaultForm.elements.default_target,defaultStatus=settings.querySelector("#nas-default-status");
  let defaultSelectionDirty=false, targetSelectionInitialized=false;
  const dialog = document.createElement("dialog");
  dialog.className = "nas-upload-dialog";
  dialog.innerHTML = `<form id="nas-manual-form"><div class="nas-dialog-heading"><div><p class="eyebrow">录像备份</p><h2>上传录像</h2></div><button type="button" data-nas-close aria-label="关闭上传窗口">×</button></div><p class="nas-session-caption muted"></p><input name="target" type="hidden"><section class="nas-target-summary"><span class="nas-target-caption">默认上传目标</span><div class="nas-target-name"><strong></strong><span></span></div><p class="nas-default-quality muted"></p></section><label class="nas-choice"><input name="custom_quality" type="checkbox"><i class="nas-switch" aria-hidden="true"></i><span><strong>自定义画质上传</strong><small>仅调整本次上传，不修改默认配置</small></span></label><fieldset class="nas-quality" hidden disabled><legend>本次上传画质</legend></fieldset><fieldset><legend>录像分钟</legend><div class="nas-selection-toolbar"><span class="nas-selection-count muted"></span><div><button type="button" data-nas-all>全选</button><button type="button" data-nas-none>清空</button></div></div><div class="nas-moments"></div></fieldset><fieldset><legend>上传视角</legend><p class="nas-camera-help muted">选择需要保留的视角</p><div class="nas-cameras"></div></fieldset><p class="nas-status" role="status"></p><div class="modal-actions"><button type="button" data-nas-cancel>取消</button><button class="primary" type="submit">开始上传</button></div></form>`;
  document.body.append(dialog);
  const manualForm = dialog.querySelector("form");
  let selectedSession = null, manualTarget = null;
  const savedQuality = new WeakMap();
  function automaticBitrate(height,fps) {
    const base={240:500,360:900,480:1500,720:3000,1080:6000};
    return Math.max(256,Math.round((base[height]||3000*(height/720)**2)*fps/24/100)*100);
  }
  function qualityControls(container) {
    container.insertAdjacentHTML("beforeend",`<div class="nas-fields"><label>处理方式<select name="quality_mode"><option value="original">源文件（不转码）</option><option value="compressed">压缩画质</option></select></label><label>分辨率<select name="quality_height"><option value="240">240p</option><option value="360">360p</option><option value="480">480p</option><option value="720" selected>720p</option><option value="1080">1080p</option></select><small>分视角视频按单路高度，合成视频按整幅画面高度。</small></label><label>帧率<select name="quality_fps"><option value="10">10 帧/秒</option><option value="15">15 帧/秒</option><option value="24" selected>24 帧/秒</option><option value="30">30 帧/秒</option><option value="36">36 帧/秒</option></select><small>帧率越高，画面越流畅，文件通常也越大。</small></label><label>多视角结果<select name="quality_layout"><option value="separate">各视角分别上传</option><option value="composite6">六路合并画面</option><option value="composite4a">四路网格合并</option><option value="composite4b">四路主视角合并</option><option value="composite3">前方与左右三路合并</option></select></label></div><p class="muted nas-quality-help">源文件按所选视角分别上传。压缩时系统根据分辨率和帧率自动匹配码率，无需手动设置。</p>`);
    container.querySelector('[name="quality_mode"]').addEventListener("change",() => toggleQuality(container.closest("form")));
    for(const name of ["quality_height","quality_fps"])container.querySelector(`[name="${name}"]`).addEventListener("change",()=>savedQuality.delete(container.closest("form")));
  }
  qualityControls(qualityForm.querySelector(".nas-quality"));qualityControls(manualForm.querySelector(".nas-quality"));
  qualityControls(automaticForm.querySelector(".nas-auto-quality"));
  automaticForm.elements.quality_mode.value="compressed";
  automaticForm.elements.quality_mode.closest("label").hidden=true;
  automaticForm.elements.quality_layout.closest("label").hidden=true;
  automaticForm.querySelector(".nas-auto-quality .nas-quality-help").textContent="附加视频必须合成转码；系统根据分辨率和帧率自动匹配码率。";
  function readQuality(f) {
    const height=Number(f.elements.quality_height.value),fps=Number(f.elements.quality_fps.value),saved=savedQuality.get(f);
    const bitrate=saved?.height===height&&saved?.fps===fps?saved.bitrate_kbps:automaticBitrate(height,fps);
    return {mode:f.elements.quality_mode.value,height,fps,bitrate_kbps:bitrate,layout:f.elements.quality_layout.value};
  }
  function setQuality(f,q = {}) {
    f.elements.quality_mode.value=q.mode||"original";
    for(const [key,value,suffix] of [["height",q.height||720,"p"],["fps",q.fps||24," 帧/秒"]]){
      const select=f.elements[`quality_${key}`];select.querySelectorAll("[data-legacy-quality]").forEach(option=>option.remove());
      // 旧配置可能不在新档位中，保留展示和数值，避免仅打开表单就改变画质。
      if(![...select.options].some(option=>Number(option.value)===value)){const option=new Option(`${value}${suffix}（已保存）`,String(value));option.dataset.legacyQuality="true";select.add(option);}
      select.value=String(value);
    }
    savedQuality.set(f,{height:q.height||720,fps:q.fps||24,bitrate_kbps:q.bitrate_kbps||3000});
    f.elements.quality_layout.value=q.layout||"separate";toggleQuality(f);
  }
  function toggleQuality(f) {const original=f.elements.quality_mode.value==="original";["height","fps","layout"].forEach(key => {const input=f.elements[`quality_${key}`];input.disabled=original;if(f===qualityForm)input.closest("label").hidden=original;});if(f===manualForm) renderCameras();}
  function updateManualQuality() {
    const q=manualTarget?.quality||{};
    const custom=manualForm.elements.custom_quality.checked;
    const fieldset=manualForm.querySelector(".nas-quality");fieldset.hidden=!custom;fieldset.disabled=!custom;
    if(!custom)setQuality(manualForm,q);
    const layout=manualForm.elements.quality_layout;
    manualForm.querySelector(".nas-default-quality").textContent=q.mode==="compressed"?`设置中的画质：${q.height}p · ${q.fps} 帧/秒 · ${layout.querySelector(`option[value="${q.layout}"]`)?.textContent||"各视角分别上传"}`:"设置中的画质：源文件（不转码）";
    renderCameras();
  }
  function toggleProtocol() {const protocol=form.elements.protocol.value;settings.querySelectorAll("[data-nas-host]").forEach(e=>e.hidden=protocol==="webdav");settings.querySelectorAll("[data-nas-webdav]").forEach(e=>e.hidden=protocol!=="webdav");settings.querySelectorAll("[data-nas-smb]").forEach(e=>e.hidden=protocol!=="smb");settings.querySelectorAll("[data-nas-sftp]").forEach(e=>e.hidden=protocol!=="sftp");}
  function fillTarget() {const t=targets.find(t=>t.id===selector.value);form.reset();qualityForm.reset();for(const key of ["name","protocol","host","port","url","user","domain","path","host_key"]) if(t) form.elements[key].value=t[key]||"";form.elements.allow_http.checked=Boolean(t?.allow_http);setQuality(qualityForm,t?.quality);qualityDirty=false;qualityStatus.textContent=t?"":"请先保存服务器配置，再保存画质设置。";qualityForm.querySelector('button[type="submit"]').disabled=!t;toggleProtocol();settings.querySelector("#nas-quality-target").textContent=t?.name||"新增服务器";settings.querySelector("#nas-test").disabled=!t;settings.querySelector("#nas-delete").disabled=!t;status.textContent=t?.has_password?"已保存专用账号凭据，密码留空即可保留。":"请配置一个专用上传账号。";}
  function renderTargets() {const selected=selector.value,draft=defaultSelector.value,active=targets.find(t=>t.is_default);selector.replaceChildren(new Option("新增服务器",""));defaultSelector.replaceChildren(new Option("请选择已保存的目标",""));targets.forEach(t=>{selector.add(new Option(`${t.name} · ${t.protocol.toUpperCase()}${t.is_default?" · 默认":""}`,t.id));defaultSelector.add(new Option(`${t.name} · ${t.protocol.toUpperCase()}`,t.id));});selector.value=targets.some(t=>t.id===selected)?selected:"";defaultSelector.value=defaultSelectionDirty&&targets.some(t=>t.id===draft)?draft:active?.id||"";if(!defaultSelectionDirty)defaultStatus.textContent=active?`当前默认目标：${active.name}`:"请指定默认上传目标后再上传录像。";}
  function textNode(tag,text,className) {const e=document.createElement(tag);e.textContent=text;if(className)e.className=className;return e;}
  const layoutNames={composite6:"六路合成",composite4a:"四路网格合成",composite4b:"四路主视角合成",composite3:"三路合成"};
  function toggleAutomaticSupplement(){const supplement=automaticForm.elements.auto_view.value==="supplement";automaticForm.querySelectorAll("[data-nas-supplement]").forEach(e=>{e.hidden=!supplement;if(e.tagName==="FIELDSET")e.disabled=!supplement;});}
  function renderAutomatic(){
    if(automaticConfig && !automaticDirty){automaticForm.elements.enabled.checked=automaticConfig.enabled;automaticForm.elements.include_existing.checked=automaticConfig.include_existing;automaticForm.elements.scope.value=automaticConfig.scope;automaticForm.elements.auto_view.value=automaticConfig.supplement_layout?"supplement":"event";automaticForm.elements.supplement_layout.value=automaticConfig.supplement_layout||"composite6";setQuality(automaticForm,automaticConfig.supplement_quality);toggleAutomaticSupplement();}
    settings.querySelector("#nas-automatic-state").textContent=automaticConfig?.enabled?"已开启":"已关闭";
    settings.querySelector("#nas-auto-scan").textContent=!automaticConfig?.enabled?"":automaticScan?.error?`等待下次核对：${automaticScan.error}`:automaticScan?.checked_at&&!automaticScan.checked_at.startsWith("0001")?`最近核对：${new Date(automaticScan.checked_at).toLocaleString()} · 新事件等待录像稳定后上传。`:"等待首次核对录像。";
    const target=targets.find(t=>t.is_default),q=target?.quality;
    settings.querySelector("#nas-auto-target").textContent=target?`默认目标：${target.name} · 事件主视频：${q.mode==="compressed"?`${q.height}p · ${q.fps} 帧/秒`:"源文件（不转码）"}`:"请先指定默认上传目标。";
    const list=settings.querySelector("#nas-auto-events");list.replaceChildren();
    const names={waiting_files:"等待录像准备",historical:"未补传已有事件",queued:"已创建上传任务",completed:"上传完成",failed:"子任务需要处理",canceled:"已取消"};
    automaticEvents.slice(0,100).forEach(event=>{const row=document.createElement("details");row.className="nas-task";row.append(textNode("summary",`${new Date(event.occurred_at).toLocaleString()} · ${labels[event.camera]||"未知事件视角"} · ${names[event.state]||event.state}`),textNode("p",`${event.target.name||"未指定目标"} · ${event.task_ids?.length||0} 个子任务`));if(event.error||event.coverage_note)row.append(textNode("p",event.error||event.coverage_note,"muted"));if(!["completed","canceled","historical"].includes(event.state)){const cancel=textNode("button","取消此事件","secondary compact");cancel.type="button";cancel.onclick=async()=>{try{await request(`/api/v1/automatic-uploads/${encodeURIComponent(event.id)}`,{method:"DELETE"});await refresh();}catch(error){automaticStatus.textContent=error.message;}};row.append(cancel);}list.append(row);});
    settings.querySelectorAll("#nas-task-list>details").forEach((row,index)=>{const task=tasks[index],role=task.request?.role;if(role)row.querySelector("summary").prepend(document.createTextNode(role==="event"?"自动 · 事件视角 · ":"自动 · 附加合成 · "));});
  }
  function recordMatchesMoment(file,moment) {
    // 循环录像的窗口 ID 会变化，沿用录像分组的两秒容差，不能仅按整分钟匹配。
    return file.moment_id===moment.id || Math.abs(new Date(file.recorded_at).getTime()-new Date(moment.recorded_at).getTime())<=2000;
  }
  function qualityMatches(a,b) {
    if(a?.mode!==b?.mode)return false;
    return a.mode==="original" || ["height","fps","bitrate_kbps","layout"].every(key=>a[key]===b[key]);
  }
  function matchingUploadRecord(file,moment,cameras,quality) {
    return file.is_default_target && recordMatchesMoment(file,moment) && qualityMatches(file.quality,quality) && [...file.cameras].sort().join(",")===[...cameras].sort().join(",");
  }
  function selectionPlan(session,momentIDs,cameras,quality,files) {
    let total=0,recorded=0,missing=0;
    const merged=quality.mode==="compressed" && quality.layout!=="separate";
    for(const moment of session.moments||[]){
      if(!momentIDs.includes(moment.id))continue;
      const selected=(moment.videos||[]).filter(video=>cameras.includes(video.camera));
      if(cameras.some(camera=>!selected.some(video=>video.camera===camera)))missing++;
      const groups=merged?(selected.length?[cameras]:[]):selected.map(video=>[video.camera]);
      for(const group of groups){total++;if(files.some(file=>matchingUploadRecord(file,moment,group,quality)))recorded++;}
    }
    return {total,recorded,missing};
  }
  function fileDescription(file) {
    const q=file.quality,views=file.cameras.map(camera=>labels[camera]||camera).join("、");
    return q.mode==="original"?`${views} · 源文件`:`${layoutNames[q.layout]||views} · ${q.height}p · ${q.fps} 帧/秒`;
  }
  function momentRecords(moment){return uploadedFiles.filter(file=>recordMatchesMoment(file,moment));}
  function updateMomentRecords() {
    if(!selectedSession)return;
    manualForm.querySelectorAll(".nas-moment-option").forEach(row=>{
      const moment=selectedSession.moments.find(moment=>moment.id===row.querySelector("input").value);
      const records=momentRecords(moment),current=records.filter(file=>file.is_default_target);
      const signature=JSON.stringify([countsAvailable,records.map(file=>[file.id,file.verified_at,file.is_default_target])]);
      if(row.dataset.records===signature)return;row.dataset.records=signature;
      const badge=row.querySelector(".nas-minute-status");badge.textContent=!countsAvailable?"记录暂不可用":current.length?`已上传 ${current.length} 个文件`:records.length?"其他目标有记录":"未上传";
      badge.classList.toggle("has-uploads",current.length>0);
      const brief=row.querySelector(".nas-minute-brief");brief.hidden=!countsAvailable || !current.length;
      brief.textContent=current.slice(0,2).map(file=>file.quality.mode==="original"?`${file.cameras.map(camera=>labels[camera]||camera).join("、")}源文件`:`${layoutNames[file.quality.layout]||file.cameras.map(camera=>labels[camera]||camera).join("、")} ${file.quality.height}p`).join(" · ")+(current.length>2?` · 另 ${current.length-2} 个`:"");
      const details=row.querySelector("details");details.hidden=!records.length;
      details.querySelector("summary").textContent=`查看记录（${records.length}）`;
      const list=details.querySelector(".nas-file-records");list.replaceChildren();
      records.forEach(file=>{
        const item=document.createElement("article");item.append(textNode("strong",fileDescription(file)));
        if(file.quality.mode==="compressed" && file.quality.layout!=="separate")item.append(textNode("p",`包含视角：${file.cameras.map(camera=>labels[camera]||camera).join("、")}`));
        item.append(textNode("p",`${file.target_name}${file.is_default_target?" · 当前默认目标":" · 历史目标"}`));
        item.append(textNode("p",`核验于 ${new Date(file.verified_at).toLocaleString()}`));
        const location=document.createElement("details");location.className="nas-record-location";location.append(textNode("summary","文件位置"),textNode("p",file.name,"nas-record-path"),textNode("p",file.remote_path,"nas-record-path"));item.append(location);list.append(item);
      });
    });
  }
  const planSummary=document.createElement("section");planSummary.className="nas-upload-plan";planSummary.setAttribute("aria-live","polite");
  manualForm.querySelector(".nas-status").before(planSummary);
  function updateSelectionPlan(){
    if(!selectedSession)return;
    const q=readQuality(manualForm),cameras=q.mode==="compressed"&&q.layout!=="separate"?layouts[q.layout]:[...manualForm.querySelectorAll('[name="camera"]:checked')].map(input=>input.value);
    const ids=[...manualForm.querySelectorAll('[name="moment"]:checked')].map(input=>input.value),plan=selectionPlan(selectedSession,ids,cameras,q,uploadedFiles);
    planSummary.replaceChildren(textNode("strong",`本次预计上传 ${plan.total} 个${q.mode==="original"?"源文件":q.layout==="separate"?"压缩文件":"合成文件"}`));
    planSummary.append(textNode("p",!countsAvailable?"暂时无法读取历史记录，仍可按本次选择提交。":plan.recorded?`其中 ${plan.recorded} 个有相同目标、视角和画质的成功记录；仍按本次选择提交。`:"当前选择没有相同配置的成功记录。"));
    if(plan.missing)planSummary.append(textNode("p",`${plan.missing} 个分钟缺少所选视角，提交后会再次校验。`,"nas-plan-warning"));
  }
  function renderTasks() {
    const list=settings.querySelector("#nas-task-list");list.replaceChildren();if(!tasks.length){list.append(textNode("p","暂无上传记录","muted"));return;}
    tasks.forEach(task=>{const row=document.createElement("details");row.className="nas-task";row.append(textNode("summary",`${task.target_name} · ${states[task.state]||task.state} · 已核验 ${task.files?.length||0}/${task.total_files||"—"} 文件`));row.append(textNode("p",new Date(task.created_at).toLocaleString()));if(task.error)row.append(textNode("p",task.error,"form-error"));if(task.next_retry_at && task.state==="waiting_retry")row.append(textNode("p",`下次重试：${new Date(task.next_retry_at).toLocaleString()}`));(task.files||[]).forEach(file=>row.append(textNode("p",`${file.name} · ${file.cameras.map(c=>labels[c]||c).join("、")} · ${file.remote_path} · SHA256 ${file.sha256}`)));if(task.state==="failed"||task.state==="waiting_retry"){const retry=textNode("button","重试","secondary");retry.type="button";retry.onclick=()=>taskAction(task.id,"retry");row.append(retry);}if(!["completed","canceled"].includes(task.state)){const cancel=textNode("button","取消并释放暂存","secondary");cancel.type="button";cancel.onclick=()=>taskAction(task.id,"cancel");row.append(cancel);}list.append(row);});
  }
  async function taskAction(id,action) {try{await request(`/api/v1/uploads/${encodeURIComponent(id)}${action==="retry"?"/retry":""}`,{method:action==="retry"?"POST":"DELETE"});await refresh();}catch(error){status.textContent=error.message;}}
  function updateCards() {
    const target=targets.find(target=>target.is_default);
    cards.forEach(({card,session,badge})=>{
      if(!card.isConnected){cards.delete(session.id);return;}
      const refs=session.moment_refs||session.moments||[],files=uploadedFiles.filter(file=>file.is_default_target && refs.some(moment=>recordMatchesMoment(file,moment)));
      badge.classList.toggle("has-uploads",Boolean(countsAvailable && files.length));
      badge.textContent=!countsAvailable?"上传记录读取中":!target?"未指定上传目标":files.length?`已上传 ${files.length} 个文件`:"未上传";
      badge.title=target?`当前默认目标：${target.name}；按成功核验的远端文件去重统计，合成视频计一个文件。`:"请在设置中指定默认上传目标";
    });
  }
  async function refresh() {
    if(busy)return;busy=true;
    try{const values=await Promise.all([request("/api/v1/upload-targets",{loading:false}),request("/api/v1/uploads",{loading:false}),request("/api/v1/upload-counts",{loading:false}),request("/api/v1/automatic-uploads",{loading:false})]);targets=values[0];tasks=values[1];countsAvailable=Boolean(values[2].available);uploadedFiles=values[2].files||[];automaticConfig=values[3].config;automaticEvents=values[3].events||[];automaticScan=values[3].scan;renderTargets();if(!targetSelectionInitialized){targetSelectionInitialized=true;selector.value=targets.find(t=>t.is_default)?.id||"";fillTarget();}renderTasks();renderAutomatic();updateCards();if(dialog.open){updateMomentRecords();updateSelectionPlan();}}
    catch(error){status.textContent=error.message;}finally{busy=false;}
  }
  function renderCameras() {if(!selectedSession)return;const selected=new Set([...manualForm.querySelectorAll('[name="camera"]:checked')].map(e=>e.value));const q=readQuality(manualForm);const required=q.mode==="compressed"&&q.layout!=="separate"?layouts[q.layout]:null;const list=manualForm.querySelector(".nas-cameras");list.replaceChildren();Object.keys(labels).forEach(camera=>{const label=document.createElement("label"),input=document.createElement("input");input.type="checkbox";input.name="camera";input.value=camera;input.checked=required?required.includes(camera):selected.size?selected.has(camera):selectedSession.cameras.includes(camera);input.disabled=Boolean(required)||!selectedSession.cameras.includes(camera);label.append(input,textNode("span",labels[camera]));list.append(label);});manualForm.querySelector(".nas-camera-help").textContent=required?"合并画面已按配置选择所需视角":"选择需要保留的视角";updateSelectionPlan();}
  function updateMomentSelection(){const count=manualForm.querySelectorAll('[name="moment"]:checked').length;manualForm.querySelector(".nas-selection-count").textContent=`已选 ${count} / ${selectedSession?.moments?.length||0} 分钟`;updateSelectionPlan();}
  async function open(session) {
    await refresh();manualTarget=targets.find(t=>t.is_default);if(!manualTarget){showPage("backup",true);globalThis.TeslaBoxSettingsLayout?.selectBackup("targets");defaultStatus.textContent="请先保存上传配置，并指定默认上传目标。";return;}
    // 保存/哨兵事件先读取当前分钟再让用户选择；循环窗口仍必须核对原快照。
    const snapshotQuery=session.clip_type==="recent"&&session.snapshot_id?`?snapshot_id=${encodeURIComponent(session.snapshot_id)}`:"";session=await request(`/api/v1/video-sessions/${encodeURIComponent(session.id)}${snapshotQuery}`);selectedSession=session;manualForm.elements.target.value=manualTarget.id;manualForm.querySelector(".nas-target-name strong").textContent=manualTarget.name;manualForm.querySelector(".nas-target-name > span").textContent=manualTarget.protocol.toUpperCase();manualForm.querySelector(".nas-session-caption").textContent=formatSessionRange(session.start_at,session.end_at);manualForm.querySelector(".nas-cameras").replaceChildren();manualForm.elements.custom_quality.checked=false;setQuality(manualForm,manualTarget.quality);updateManualQuality();
    const moments=manualForm.querySelector(".nas-moments");moments.replaceChildren();moments.classList.toggle("nas-short-session",session.moments.length<=6);
    (session.moments||[]).forEach(moment=>{
      const row=document.createElement("div");row.className="nas-moment-option";
      const label=document.createElement("label"),input=document.createElement("input"),text=document.createElement("span");
      input.type="checkbox";input.name="moment";input.value=moment.id;input.checked=true;
      text.append(textNode("strong",new Date(moment.recorded_at).toLocaleTimeString()),textNode("small","未上传","nas-minute-status"),textNode("small","","nas-minute-brief"));label.append(input,text);
      const details=document.createElement("details");details.className="nas-minute-records";details.hidden=true;details.append(textNode("summary","查看记录"));
      const list=document.createElement("div");list.className="nas-file-records";details.append(list);row.append(label,details);moments.append(row);
    });
    manualForm.querySelector(".nas-status").textContent="";updateMomentRecords();updateMomentSelection();renderCameras();dialog.showModal();
  }
  // 卡片稍后才加入页面，留到刷新时更新，避免被断开节点清理提前移除。
  globalThis.TeslaBoxNASUploads={attachSession(card,session){const badge=textNode("span","上传记录读取中","session-upload-count");badge.title="按成功核验的远端文件统计";card.querySelector(".session-aside-meta")?.prepend(badge);const button=textNode("button","上传");button.type="button";button.setAttribute("role","menuitem");button.disabled=Boolean(session.cached);button.onclick=event=>{event.stopPropagation();closeRecordingMenu();void open(session).catch(error=>setNotice(error.message,true));};card.querySelector(".session-more-menu")?.prepend(button);cards.set(session.id,{card,session,badge});}};
  selector.onchange=fillTarget;form.elements.protocol.onchange=toggleProtocol;
  automaticForm.addEventListener("change",()=>{automaticDirty=true;automaticStatus.textContent="自动上传设置有未保存的修改。";toggleAutomaticSupplement();});
  automaticForm.onsubmit=async event=>{event.preventDefault();const button=event.submitter;button.disabled=true;try{const supplement=automaticForm.elements.auto_view.value==="supplement"?automaticForm.elements.supplement_layout.value:"",quality=readQuality(automaticForm);quality.mode="compressed";quality.layout=supplement||"composite6";const config={enabled:automaticForm.elements.enabled.checked,include_existing:automaticForm.elements.include_existing.checked,scope:automaticForm.elements.scope.value,supplement_layout:supplement,supplement_quality:quality};automaticConfig=await request("/api/v1/automatic-uploads",{method:"PUT",body:JSON.stringify(config)});automaticDirty=false;await refresh();document.dispatchEvent(new CustomEvent("teslabox-config-saved",{detail:"nas-automatic-form"}));automaticStatus.textContent="自动上传设置已保存。";}catch(error){automaticStatus.textContent=error.message;}finally{button.disabled=false;}};
  form.onsubmit=async event=>{event.preventDefault();const button=event.submitter;button.disabled=true;status.textContent="正在保存服务器配置…";try{const draft=readQuality(qualityForm),dirty=qualityDirty;const t={id:selector.value,server_only:true,quality:targets.find(target=>target.id===selector.value)?.quality||{mode:"original",height:720,fps:24,bitrate_kbps:3000,layout:"separate"},allow_http:form.elements.allow_http.checked};["name","protocol","host","url","user","password","domain","path","host_key"].forEach(key=>t[key]=form.elements[key].value);t.port=Number(form.elements.port.value)||0;const saved=await request("/api/v1/upload-targets",{method:"POST",body:JSON.stringify(t)});form.elements.password.value="";await refresh();selector.value=saved.id;fillTarget();if(dirty){setQuality(qualityForm,draft);qualityDirty=true;qualityStatus.textContent="画质有未保存的修改，请单独保存。";}document.dispatchEvent(new CustomEvent("teslabox-config-saved",{detail:"nas-target-form"}));status.textContent="服务器配置已保存。";}catch(error){status.textContent=error.message;}finally{button.disabled=false;}};
  qualityForm.addEventListener("input",()=>{qualityDirty=true;qualityStatus.textContent="画质有未保存的修改。";});
  qualityForm.addEventListener("change",()=>{qualityDirty=true;qualityStatus.textContent="画质有未保存的修改。";});
  qualityForm.onsubmit=async event=>{event.preventDefault();if(!selector.value){qualityStatus.textContent="请先保存服务器配置。";return;}const button=event.submitter,id=selector.value,q=readQuality(qualityForm);button.disabled=true;qualityStatus.textContent="正在保存画质设置…";try{const saved=await request("/api/v1/upload-targets/quality",{method:"PUT",body:JSON.stringify({target_id:id,quality:q})});await refresh();if(selector.value===saved.id){qualityDirty=JSON.stringify(readQuality(qualityForm))!==JSON.stringify(saved.quality);if(!qualityDirty)document.dispatchEvent(new CustomEvent("teslabox-config-saved",{detail:"nas-quality-form"}));qualityStatus.textContent=qualityDirty?"画质已保存，当前还有未保存的修改。":"画质设置已保存。";}}catch(error){qualityStatus.textContent=error.message;}finally{button.disabled=!selector.value;}};
  settings.querySelector("#nas-test").onclick=async event=>{if(!selector.value){status.textContent="请先保存目标。";return;}const button=event.currentTarget;button.disabled=true;status.textContent="正在测试上传、读回和删除权限…";try{await request(`/api/v1/upload-targets/${selector.value}/test`,{method:"POST"});status.textContent="连接测试通过：上传、SHA256 读回校验和删除均成功。";}catch(error){status.textContent=error.message;}finally{button.disabled=false;}};
  settings.querySelector("#nas-delete").onclick=async()=>{if(!selector.value)return;try{await request(`/api/v1/upload-targets/${selector.value}`,{method:"DELETE"});selector.value="";await refresh();fillTarget();}catch(error){status.textContent=error.message;}};
  settings.querySelector("#nas-refresh").onclick=refresh;
  defaultSelector.onchange=()=>{defaultSelectionDirty=true;};
  defaultForm.onsubmit=async event=>{event.preventDefault();const button=event.submitter;button.disabled=true;try{const target=await request("/api/v1/upload-targets/default",{method:"PUT",body:JSON.stringify({target_id:defaultSelector.value})});defaultSelectionDirty=false;await refresh();document.dispatchEvent(new CustomEvent("teslabox-config-saved",{detail:"nas-default-form"}));defaultStatus.textContent=`默认上传目标已保存：${target.name}`;}catch(error){defaultStatus.textContent=error.message;}finally{button.disabled=false;}};
  manualForm.elements.custom_quality.onchange=updateManualQuality;
  manualForm.elements.quality_layout.onchange=renderCameras;
  manualForm.querySelector("[data-nas-close]").onclick=()=>dialog.close();
  manualForm.querySelector("[data-nas-cancel]").onclick=()=>dialog.close();
  manualForm.querySelector("[data-nas-all]").onclick=()=>{manualForm.querySelectorAll('[name="moment"]').forEach(input=>input.checked=true);updateMomentSelection();};
  manualForm.querySelector("[data-nas-none]").onclick=()=>{manualForm.querySelectorAll('[name="moment"]').forEach(input=>input.checked=false);updateMomentSelection();};
  manualForm.querySelector(".nas-moments").onchange=updateMomentSelection;
  manualForm.querySelector(".nas-cameras").onchange=updateSelectionPlan;
  manualForm.querySelector(".nas-quality").addEventListener("change",updateSelectionPlan);
  manualForm.onsubmit=async event=>{event.preventDefault();const button=event.submitter;button.disabled=true;const detail=manualForm.querySelector(".nas-status");try{const quality=readQuality(manualForm);const cameras=quality.mode==="compressed"&&quality.layout!=="separate"?layouts[quality.layout]:[...manualForm.querySelectorAll('[name="camera"]:checked')].map(e=>e.value);const moment_ids=[...manualForm.querySelectorAll('[name="moment"]:checked')].map(e=>e.value);if(!moment_ids.length||!cameras.length)throw new Error("请至少选择一个录像分钟和一个视角。");await request("/api/v1/uploads",{method:"POST",body:JSON.stringify({target_id:manualForm.elements.target.value,session_id:selectedSession.id,snapshot_id:selectedSession.snapshot_id,moment_ids,cameras,...(manualForm.elements.custom_quality.checked?{quality}:{})})});detail.textContent="任务已创建，可关闭窗口；进度见主菜单中的“录像备份 → 任务与记录”。";await refresh();}catch(error){detail.textContent=error.message;}finally{button.disabled=false;}};
  fillTarget();
  document.addEventListener("DOMContentLoaded",()=>{setInterval(()=>{if(!document.hidden && document.querySelector("#app-view:not([hidden])"))void refresh();},5000);});
})();
