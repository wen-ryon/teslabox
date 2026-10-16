/* 首次登记只持有当前任务的内存状态，刷新页面不会重新启动配对。 */
(()=>{
  'use strict';
const t = globalThis.TeslaBoxI18n.t;
  const get=id=>document.getElementById('carplay-pairing-'+id),panel=get('panel');
  if(!panel)return;
  const target=get('device'),status=get('status'),prompt=get('prompt'),digits=get('digits');
  const idPattern=/^[a-f0-9]{32}$/,phonePattern=/^(?:[A-F0-9]{2}:){5}[A-F0-9]{2}$/;
  const states={idle:t('尚未登记；请在 iPhone 蓝牙设置连接 TeslaBox CarPlay'),pairing:t('手机已发起连接，正在等待本轮画面'),canceling:t('正在取消，等待本轮资源清理'),canceled:t('本轮已取消；可在手机上断开后重新连接 TeslaBox CarPlay'),failed:t('本轮登记未完成；请在手机上断开后重新连接，仍失败时下载 CarPlay 日志'),cleanup_pending:t('本轮清理尚未确认，请先核对清理状态'),video_verified:t('登记完成：已收到本轮手机新画面'),bluetooth_paired:t('蓝牙已配对，尚未完成视频登记')};
  let snapshot=null,healthy=false,busy=false,polling=false,loaded=false,generation=0;
  const validTask=task=>task&&typeof task.running==='boolean'&&Object.hasOwn(states,task.state)&&
    (task.state==='idle'?task.request_id==='':idPattern.test(task.request_id)&&phonePattern.test(task.phone_id))&&task.prompt&&
    (!task.prompt.digits||(task.prompt.state==='waiting_confirmation'&&task.prompt.request_id===task.request_id&&/^[0-9]{6}$/.test(task.prompt.digits)));
  function render(){
    const task=snapshot?.task,active=task?.running===true,cleanup=task?.state==='cleanup_pending';
    const available=healthy&&snapshot?.available===true;
    const pending=available&&!busy&&active&&!cleanup&&task.prompt.state==='waiting_confirmation'&&/^[0-9]{6}$/.test(task.prompt.digits||'');
    prompt.hidden=!pending;digits.textContent=pending?task.prompt.digits:'';
    get('scan').disabled=!available||busy||active||cleanup||snapshot?.scanning===true;
    target.disabled=get('scan').disabled;
    get('start').disabled=target.disabled||snapshot?.start_available===false||!phonePattern.test(target.value);
    get('confirm').disabled=!pending;
    get('cancel').disabled=!healthy||busy||!active||cleanup;
    get('reconcile').hidden=!cleanup;get('reconcile').disabled=!healthy||busy||active||!cleanup;
  }
  async function request(action,body){
    const abort=new AbortController(),timer=setTimeout(()=>abort.abort(),action==='scan'||action==='reconcile'?12000:5000);
    try{
      const response=await fetch('/api/v1/carplay/pairing'+(action?'/'+action:''),{method:action?'POST':'GET',cache:'no-store',signal:abort.signal,
        headers:action?{'Content-Type':'application/json'}:undefined,body:action?JSON.stringify(body):undefined});
      const value=await response.json();
      if(!response.ok)throw new Error(typeof value.error==='string'?value.error:t('登记服务暂不可用'));
      return value;
    }finally{clearTimeout(timer);}
  }
  async function refresh(){
    if(polling||busy)return;
    const current=generation;
    polling=true;
    try{
      const value=await request('');
      if(current!==generation)return;
      if(typeof value.available!=='boolean'||typeof value.scanning!=='boolean'||!validTask(value.task))throw new Error(t('登记状态暂不可用'));
      snapshot=value;healthy=true;
      status.textContent=!value.available?t('首次登记服务尚未就绪'):value.scanning?t('正在搜索蓝牙设备'):states[value.task.state];
      if(value.available&&value.task.state==='idle'&&value.incoming_available===false)status.textContent=t('手机主动配对入口尚未就绪，请稍候；持续失败时下载 CarPlay 日志');
      if(value.task.state==='failed'&&value.task.failure_phase){
        const failures={bluetooth_pairing:t('手机系统配对未完成'),network_handoff:t('热点交接失败'),waiting_video:t('未收到本轮手机画面')};
        if(failures[value.task.failure_phase])status.textContent=failures[value.task.failure_phase]+t('；请在手机上断开后重新连接，仍失败时下载 CarPlay 日志');
      }
      if(value.task.running&&value.task.phase){
        const phases={network_handoff:t('正在交接热点；页面短暂断开后会继续读取同一任务'),waiting_video:t('正在等待手机连接与本轮视频首帧'),restoring:t('正在清理本轮连接并恢复原热点，请勿重复启动')};
        if(phases[value.task.phase])status.textContent=phases[value.task.phase];
      }
    }catch{if(current===generation){healthy=false;status.textContent=t('登记状态未确认；请保持页面开启等待恢复，不要重复启动');}}
    finally{polling=false;render();}
  }
  async function act(action,body){
    if(busy)return;
    generation++;
    busy=true;render();
    status.textContent=action==='scan'?t('正在搜索，请保持 iPhone 位于蓝牙设置页'):t('正在提交本轮操作');
    try{
      const result=await request(action,body);
      if(action==='scan'){
        if(!Array.isArray(result.devices)||result.devices.length>32||result.devices.some(d=>!d||!phonePattern.test(d.id)||typeof d.name!=='string'||d.name.length>128||typeof d.paired!=='boolean'))throw new Error(t('搜索结果暂不可用'));
        target.replaceChildren();
        const option=(value,label)=>{const node=document.createElement('option');node.value=value;node.textContent=label;target.appendChild(node);};
        option('',t('请选择你的手机'));
        for(const device of result.devices)option(device.id,(device.name||t('蓝牙设备'))+' · '+device.id.slice(-8)+(device.paired?t('（已配对）'):''));
        target.value='';status.textContent=result.devices.length?t('搜索结束，请选择你的手机再开始登记'):t('未发现设备，请保持 iPhone 位于蓝牙设置页后重新搜索');
      }else{
        if(!validTask(result))throw new Error(t('本轮操作状态未确认'));
        snapshot={available:true,scanning:false,task:result};healthy=true;status.textContent=states[result.state];
      }
    }catch(error){healthy=false;status.textContent=error.name==='AbortError'?t('操作结果未确认，正在重新读取本轮状态'):error.message;}
    finally{busy=false;render();}
  }
  get('scan').addEventListener('click',()=>{if(!get('scan').disabled)void act('scan',{});});
  get('start').addEventListener('click',()=>{
    if(get('start').disabled)return;
    if(!globalThis.confirm(t('完成蓝牙数字核对后，将切换到 CarPlay 热点，当前页面可能短暂断开。失败会恢复原热点；重新连接后可继续查看本轮结果。是否开始？')))return;
    void act('start',{phone_id:target.value,handoff_fingerprint:snapshot?.handoff?.fingerprint||'',confirm_handoff:'yes'});
  });
  get('confirm').addEventListener('click',()=>{if(!get('confirm').disabled)void act('confirm',{request_id:snapshot.task.request_id,digits:snapshot.task.prompt.digits});});
  for(const action of ['cancel','reconcile'])get(action).addEventListener('click',()=>{if(!get(action).disabled)void act(action,{request_id:snapshot.task.request_id});});
  target.addEventListener('change',render);
  globalThis.TeslaBoxCarPlayPairing={load(){loaded=true;return refresh();}};
  setInterval(()=>{if(loaded&&!document.hidden&&panel.offsetParent!==null)void refresh();},1000);
  render();
})();
