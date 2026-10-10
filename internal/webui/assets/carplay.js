'use strict';
const video=document.querySelector('#screen'),notice=document.querySelector('#notice'),form=document.querySelector('#login');
let canvas=document.querySelector('#canvas'),frameSize=null,touchContacts=1;
document.querySelector('#compatibility').textContent=typeof VideoDecoder==='function' && isSecureContext ? '' : '当前浏览器不支持安全视频解码，请使用配置好的 HTTPS 入口';
let socket,media,source,queue=[],queuedBytes=0,objectURL,player,display='none',canvasOwned=false,canvasFlow=null;
let renderedFrames=0,lastRender=null,lastRenderReport=-Infinity;
const hostUI=TeslaBoxHostUI.create(notice,()=>display==='canvas'?canvas:display==='mse'?video:null,()=>location.assign('/settings#carplay'));
function stopMedia(){canvasFlow=null;document.body.classList.remove('playing');canvasTouch.cancel();videoTouch.cancel();queue=[];queuedBytes=0;source=null;media=null;frameSize=null;if(player)player.close();if(canvasOwned){canvasTouch.dispose();const fresh=document.createElement('canvas');fresh.id='canvas';fresh.hidden=true;canvas.replaceWith(fresh);canvas=fresh;canvasTouch=bindTouch(canvas);}player=null;canvasOwned=false;display='none';canvas.hidden=true;video.hidden=true;video.removeAttribute('src');if(objectURL)URL.revokeObjectURL(objectURL);objectURL=null;}
function fail(error){connection.stop(error);}
let lastKeyframeRequest=-Infinity,keyframeTimer=null;
function requestKeyframe(){
  const now=performance.now();
  if(socket?.readyState!==1)return;
  if(now-lastKeyframeRequest<1000){if(keyframeTimer===null)keyframeTimer=setTimeout(()=>{keyframeTimer=null;requestKeyframe();},1000-(now-lastKeyframeRequest));return;}
  lastKeyframeRequest=now;socket.send('{"type":"keyframe"}');
}
function configure(data){
  if(data.format==='annexb'&&data.version===1){
    const currentSocket=socket,epoch=data.epoch;
    const flow=data.flowControl===1&&Number.isSafeInteger(epoch)&&epoch>0;
    const consumed=timestamp=>{
      const state=canvasFlow;if(!state?.enabled||socket!==state.socket||state.socket.readyState!==1)return;
      const ack={type:'videoAck',epoch:state.epoch,timestamp},now=performance.now();
      // 只报告有界性能计数，不发送画面、位置或用户信息；丢帧确认不能充当绘制成功。
      if(lastRender&&now-lastRenderReport>=5000){
        Object.assign(ack,{rendered:renderedFrames,decodeMs:Math.min(60000,Math.max(0,Math.round(lastRender.latencyMs||0))),drawMs:Math.min(60000,Math.max(0,Math.round(lastRender.drawMs||0))),renderer:lastRender.renderer});
        lastRenderReport=now;
      }
      state.socket.send(JSON.stringify(ack));
    };
    try{
      // 动态参数集变化保留画布及 Worker，避免重建导致闪黑和启动积压。
      if(display!=='canvas'||!player||canvasFlow?.socket!==currentSocket){
        stopMedia();display='canvas';canvas.hidden=false;canvasOwned=true;
        renderedFrames=0;lastRender=null;lastRenderReport=-Infinity;
        player=TeslaBoxSession.create(canvas,frame=>{renderedFrames=Math.min(4294967295,renderedFrames+1);lastRender=frame;connection.healthy();document.body.classList.add('playing');frameSize=frame;canvas.style.aspectRatio=frame.width+'/'+frame.height;notice.textContent='CarPlay 画面已连接';},fail,window,requestKeyframe,consumed);
      }
      // 同步归还旧代次确认后才切换确认代次，Worker 另有 generation 隔离。
      player.configure(data.codec);
      canvasFlow={socket:currentSocket,epoch,enabled:flow};
      if(flow)currentSocket.send(JSON.stringify({type:'videoReady',epoch}));
      requestKeyframe();
    }catch(error){fail(error);}return;
  }
  if(data.format!=='fmp4'){fail(new Error('服务端视频格式不支持'));return;}
  configureMse(data.codec);
}
function configureMse(codec){
  stopMedia();const mime='video/mp4; codecs="'+codec+'"';
  if(!window.MediaSource||!MediaSource.isTypeSupported(mime)){fail(new Error('此浏览器不支持 '+mime));return;}
  display='mse';video.hidden=false;const currentMedia=new MediaSource();media=currentMedia;objectURL=URL.createObjectURL(currentMedia);video.src=objectURL;
  currentMedia.addEventListener('sourceopen',()=>{if(currentMedia!==media)return;try{source=currentMedia.addSourceBuffer(mime);source.addEventListener('updateend',()=>{if(currentMedia===media)pump();});pump();}catch(e){fail(e);}},{once:true});
}
function pump(){
  if(!source||source.updating)return;
  if(source.buffered.length){const end=source.buffered.end(source.buffered.length-1),start=source.buffered.start(0);
    if(end-video.currentTime>1.5)video.currentTime=Math.max(start,end-0.3);
    if(video.currentTime-start>3){source.remove(start,video.currentTime-2);return;}}
  const next=queue.shift();if(!next)return;queuedBytes-=next.byteLength;
  try{source.appendBuffer(next);}catch(e){notice.textContent='播放缓冲异常，正在重新连接';connection.retry();}
}
function receive(event){
    if(typeof event.data==='string'){try{const data=JSON.parse(event.data);if(Object.prototype.hasOwnProperty.call(data,'touchContacts')){if(![1,2].includes(data.touchContacts))throw new Error('触控槽位声明不合法');canvasTouch.cancel();videoTouch.cancel();touchContacts=data.touchContacts;}if(Object.prototype.hasOwnProperty.call(data,'uiRequested'))hostUI.apply(data.uiRequested);if(data.codec)configure(data);if(data.ended){stopMedia();notice.textContent='上游视频已断开，等待重新连接';}}catch(error){fail(error);}return;}
    if(display==='canvas'){player.push(event.data);return;}
    if(display!=='mse'){fail(new Error('视频先于格式声明到达'));return;}
    queuedBytes+=event.data.byteLength;if(queuedBytes>16*1024*1024){notice.textContent='浏览器处理过慢，重新等待关键帧';connection.retry();return;}
    const currentMedia=media;queue.push(event.data);pump();
    video.play().catch(error=>{if(currentMedia===media)fail(error);});
}
video.addEventListener('playing',()=>{connection.healthy();document.body.classList.add('playing');notice.textContent='MSE 调试播放 · 不是驾驶中的车机兼容路径';});
form.addEventListener('submit', async event => {
  event.preventDefault();
  const password = document.querySelector('#password'), button = form.querySelector('button');
  if (button.disabled) return;
  button.disabled = true;
  const abort = new AbortController(), timer = setTimeout(() => abort.abort(), 8000);
  try {
    const response = await fetch('/api/v1/auth/login', {method: 'POST', signal: abort.signal, headers: {'Content-Type': 'application/json'}, body: JSON.stringify({password: password.value})});
    if (!response.ok) { notice.textContent = response.status === 401 ? '管理密码不正确' : '登录暂时不可用'; return; }
    const result = await response.json();
    password.value = '';
    if (result.must_change_password) { location.assign('/settings'); return; }
    await startDisplay();
  } catch (_) { notice.textContent = '登录请求失败或超时，请重试'; }
  finally { clearTimeout(timer); button.disabled = false; }
});
function bindTouch(surface){return TeslaBoxTouch.create(surface,()=>surface===canvas?frameSize:{width:video.videoWidth,height:video.videoHeight},message=>{if(!socket||socket.readyState!==WebSocket.OPEN)return false;socket.send(JSON.stringify(message));return true;},()=>touchContacts,()=>document.body.dataset.carplayFit||'contain');}
const videoTouch=bindTouch(video);let canvasTouch=bindTouch(canvas);
const connection=new TeslaBoxConnection.Connection({
  socket:value=>{socket=value;},message:receive,
  reset:()=>{clearTimeout(keyframeTimer);keyframeTimer=null;lastKeyframeRequest=-Infinity;stopMedia();touchContacts=1;},
  state:(state,detail)=>{
    if(state==='connected'){form.hidden=true;notice.textContent='已连接，等待视频和关键帧';}
    else if(state==='unauthorized'){form.hidden=false;notice.textContent='请使用 TeslaBox 管理密码登录';}
    else if(state==='failed'){form.hidden=false;notice.textContent='显示失败：'+detail.message;}
    else if(state==='paused')notice.textContent='页面已转入后台，暂停视频连接；返回后自动恢复';
    else if(state==='retry')notice.textContent='连接中断，'+(detail/1000)+' 秒后自动重试';
    else if(state==='checking'||state==='connecting')notice.textContent='正在连接浏览器桥接服务';
  }
});
async function startDisplay(){
  const abort=new AbortController(),timer=setTimeout(()=>abort.abort(),8000);
  try{
    const response=await fetch('/api/v1/carplay/settings',{signal:abort.signal,cache:'no-store'});
    if(response.status===401){connection.start();return;}
    if(!response.ok)throw new Error('CarPlay 设置读取失败');
    const settings=await response.json();
    const display=displayMemory.resolve(settings);
    // 页面铺放与触控共用状态，Worker 重建画布后仍沿用当前设置。
    document.body.dataset.carplayFit=settings.dashboard_layout==='vertical'?'fill':'contain';
    const applied=await fetch('/api/v1/carplay/display',{method:'POST',signal:abort.signal,headers:{'Content-Type':'application/json'},body:JSON.stringify({revision:display.revision,width:display.width,height:display.height})});
    if(!applied.ok)throw new Error('CarPlay 显示参数应用失败，请刷新设置');
    connection.start();
  }catch(error){notice.textContent=error.message;}
  finally{clearTimeout(timer);}
}
let displayStorage=null;
try{displayStorage=globalThis.localStorage;}catch{}
const displayMemory=TeslaBoxCarPlayDisplay.create(displayStorage,()=>({width:window.innerWidth,height:window.innerHeight}));
void startDisplay();
