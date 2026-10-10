/* 复用管理页同源认证，保存草稿时不重启主服务。 */
(()=>{
  'use strict';
  const form=document.getElementById('carplay-settings-form'),notice=document.getElementById('carplay-settings-notice');
  if(!form)return;
  document.getElementById('settings-carplay-return')?.addEventListener('click',()=>location.assign('/carplay-return'));
  let revision=null;
  let loading=null;
  let networkLoading=false;
  const phoneField=document.getElementById('carplay-phone_id');
  const layoutField=document.getElementById('carplay-dashboard_layout');
  const autoPlayField=document.getElementById('carplay-auto_play');
  const fields=['network','quality','fps','recovery_priority','enabled',...(autoPlayField?['auto_play']:[]),...(phoneField?['phone_id']:[]),...(layoutField?['dashboard_layout']:[])];
  const readField=name=>['enabled','auto_play'].includes(name)?document.getElementById('carplay-'+name).checked:
    ['network','recovery_priority','phone_id','dashboard_layout'].includes(name)?document.getElementById('carplay-'+name).value:Number(document.getElementById('carplay-'+name).value);
  async function request(method,body){
    const abort=new AbortController(),timer=setTimeout(()=>abort.abort(),8000);
    try{
      const response=await fetch('/api/v1/carplay/settings',{method,cache:'no-store',signal:abort.signal,headers:{'Content-Type':'application/json'},body:body?JSON.stringify(body):undefined});
      const result=await response.json();if(!response.ok)throw new Error(result.error||'CarPlay 设置暂时不可用');return result;
    }finally{clearTimeout(timer);}
  }
  function load(){
    void globalThis.TeslaBoxCarPlayPairing?.load();
    if(revision!==null)return Promise.resolve();
    if(loading)return loading;
    loading=request('GET').then(async settings=>{
    revision=settings.revision;
    for(const name of fields){const field=document.getElementById('carplay-'+name);if(name==='enabled')field.checked=settings.enabled!==false;else if(name==='auto_play')field.checked=settings.auto_play===true;else field.value=settings[name]??(name==='recovery_priority'?'internet':name==='dashboard_layout'?'auto':'');}
    await refreshPhones(settings.phone_id||'');
    notice.textContent='设置已读取';
    void refreshNetwork();
    }).catch(error=>{notice.textContent=error.message;}).finally(()=>{loading=null;});
    return loading;
  }
  globalThis.TeslaBoxCarPlaySettings={load};
  let phonesLoading=false;
  async function refreshPhones(requested=phoneField?.value||''){
    if(!phoneField||phonesLoading)return;
    phonesLoading=true;
    const output=document.getElementById('carplay-phone-status');
    const abort=new AbortController(),timer=setTimeout(()=>abort.abort(),8000);
    const option=(id,label)=>{const node=document.createElement('option');node.value=id;node.textContent=label;return node;};
    try{
      const response=await fetch('/carplay/api/phones',{signal:abort.signal,cache:'no-store'});
      if(!response.ok)throw new Error('手机列表暂未就绪');
      const value=await response.json();
      if(!Array.isArray(value.phones)||value.phones.length>8||value.phones.some(phone=>!phone||typeof phone.id!=='string'||!/^(?:[A-F0-9]{2}:){5}[A-F0-9]{2}$/.test(phone.id)))throw new Error('手机列表不合法');
      while(phoneField.firstChild)phoneField.removeChild(phoneField.firstChild);
      phoneField.appendChild(option('','自动：上次成功连接的手机'));
      for(const phone of value.phones)phoneField.appendChild(option(phone.id,(phone.name||'已登记手机')+' · '+phone.id.slice(-8)));
      if(requested&&!value.phones.some(phone=>phone.id===requested))phoneField.appendChild(option(requested,'已保存手机（当前未登记）'));
      phoneField.value=requested;
      output.textContent=value.selected?'当前连接目标：'+value.selected.slice(-8)+'；切换会短暂重建 CarPlay，录像继续运行。':'尚未选择可用手机；请先完成配对登记。';
    }catch{
      if(requested){phoneField.appendChild(option(requested,'已保存手机'));phoneField.value=requested;}
      output.textContent='手机列表暂未就绪，已保存的选择保留。';
    }finally{clearTimeout(timer);phonesLoading=false;}
  }
  async function refreshNetwork(){
    if(networkLoading||revision===null)return;
    networkLoading=true;
    const abort=new AbortController(),timer=setTimeout(()=>abort.abort(),8000);
    const output=document.getElementById('carplay-network-status');
    try{
      const response=await fetch('/carplay/api/network',{signal:abort.signal,cache:'no-store'});
      if(!response.ok)throw new Error('网络检测中');
      const value=await response.json();
      output.textContent=value.internet===true?'互联网可用'+(value.interface?'（'+value.interface+'）':''):'未联网；本地画面仍可运行，手动来源不会自动回退';
    }catch{output.textContent='网络状态尚未确认；没有外网时，本地画面仍可运行。';}
    finally{clearTimeout(timer);networkLoading=false;}
  }
  setInterval(()=>{if(!document.hidden&&form.offsetParent!==null){void refreshNetwork();void refreshPhones();}},20000);
  form.addEventListener('submit',async event=>{
    event.preventDefault();if(revision===null)return;
    const button=form.querySelector('button');if(button.disabled)return;button.disabled=true;
    try{
      const settings={revision};for(const name of fields)settings[name]=readField(name);
      const saved=await request('PUT',settings);revision=saved.revision;
      const confirmed=await request('GET');
      if(confirmed.revision!==saved.revision||fields.some(name=>(name==='phone_id'?(confirmed[name]||''):confirmed[name])!==settings[name]))throw new Error('设置未确认保存，请刷新后重试');
      const layoutNotice=layoutField?'，主页面布局：'+(confirmed.dashboard_layout==='vertical'?'竖向地图':'跟随屏幕'):'';
      notice.textContent=confirmed.enabled===false?'已保存：CarPlay 将关闭，录像继续运行；释放完成后可手动重连 Wi-Fi。':'已保存'+layoutNotice+'；返回 CarPlay 后应用新设置';
      document.getElementById('carplay-network-status').textContent=confirmed.enabled===false?'正在停止 CarPlay 并释放专属热点；连接可能短暂中断。':'等待应用网络来源与恢复优先级，最长约 60 秒';
    }catch(error){notice.textContent=error.message;}finally{button.disabled=false;}
  });
})();
