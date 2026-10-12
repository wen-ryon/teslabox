const test=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');

const nonce='a'.repeat(32),phone='02:11:22:33:44:55';
const task=(state='idle',running=false)=>({state,running,request_id:state==='idle'?'':nonce,phone_id:state==='idle'?'':phone,prompt:{request_id:nonce,state:'waiting_native'}});
const flush=()=>new Promise(setImmediate);
function fixture(fetcher,confirm=()=>true){
  const nodes=new Map();let poll;
  for(const id of ['panel','device','status','prompt','digits','scan','start','confirm','cancel','reconcile'])nodes.set('carplay-pairing-'+id,{value:'',textContent:'',disabled:false,hidden:false,offsetParent:{},children:[],handlers:{},addEventListener(name,handler){this.handlers[name]=handler;},replaceChildren(){this.children=[];},appendChild(node){this.children.push(node);}});
  const calls=[];
  const context={confirm,document:{hidden:false,getElementById:id=>nodes.get(id),createElement:()=>({})},AbortController,setTimeout,clearTimeout,setInterval:callback=>{poll=callback;},fetch:async(path,options)=>{calls.push({path,...options});return fetcher(path,options);}};
  vm.runInNewContext(fs.readFileSync('internal/webui/assets/carplay-pairing.js','utf8'),context);
  return {nodes,calls,load:()=>context.TeslaBoxCarPlayPairing.load(),get:id=>nodes.get('carplay-pairing-'+id),click:async id=>{nodes.get('carplay-pairing-'+id).handlers.click();await flush();},change:()=>nodes.get('carplay-pairing-device').handlers.change(),poll:async()=>{poll();await flush();}};
}
const reply=value=>({ok:true,json:async()=>value});

test('拒绝交接确认不启动任务；确认绑定当前配置指纹',async()=>{
  let approved=false;
  const ui=fixture((path,options)=>{
    if(path.endsWith('/start')){
      assert.deepEqual(JSON.parse(options.body),{phone_id:phone,handoff_fingerprint:'current',confirm_handoff:'yes'});
      return reply(task('pairing',true));
    }
    return reply({available:true,scanning:false,handoff:{fingerprint:'current',required:true},task:task()});
  },()=>approved);
  await ui.load();ui.get('device').value=phone;ui.change();
  await ui.click('start');assert.equal(ui.calls.filter(c=>c.method==='POST').length,0);
  approved=true;await ui.click('start');assert.equal(ui.calls.filter(c=>c.path.endsWith('/start')).length,1);
});

test('网络方案未就绪仍能扫描，但不能开始交接',async()=>{
  const ui=fixture(()=>reply({available:true,start_available:false,scanning:false,task:task()}));
  await ui.load();ui.get('device').value=phone;ui.change();
  assert.equal(ui.get('scan').disabled,false);assert.equal(ui.get('start').disabled,true);
});

test('刷新只读状态，未就绪不允许搜索和启动，也不持久化数字',async()=>{
  const ui=fixture(()=>reply({available:false,scanning:false,task:task()}));
  await ui.load();await ui.poll();
  assert.equal(ui.get('scan').disabled,true);assert.equal(ui.get('start').disabled,true);
  assert.equal(ui.get('prompt').hidden,true);
  assert.ok(ui.calls.every(c=>c.method==='GET'));
});

test('搜索要求显式选择手机，名称作为文本渲染，不自动配对',async()=>{
  const ui=fixture(path=>path.endsWith('/scan')?reply({devices:[{id:phone,name:'<img src=x onerror=alert(1)>',paired:false}]}):reply({available:true,scanning:false,task:task()}));
  await ui.load();await ui.click('scan');
  assert.equal(ui.get('device').children[1].textContent,'<img src=x onerror=alert(1)> · 33:44:55');
  assert.equal(ui.get('start').disabled,true);
  assert.equal(ui.calls.filter(c=>c.method==='POST').length,1);
  ui.get('device').value=phone;ui.change();
  assert.equal(ui.get('start').disabled,false);
});

test('六位数字只有明确确认才提交，提交后隐藏，成功以视频登记状态为准',async()=>{
  const pending=task('pairing',true);pending.prompt={request_id:nonce,state:'waiting_confirmation',digits:'000042'};
  let current=pending;
  const ui=fixture((path,options)=>{
    if(path.endsWith('/confirm')){assert.deepEqual(JSON.parse(options.body),{request_id:nonce,digits:'000042'});current=task('pairing',true);return reply(current);}
    return reply({available:true,scanning:false,task:current});
  });
  await ui.load();assert.equal(ui.get('digits').textContent,'000042');assert.equal(ui.get('confirm').disabled,false);
  assert.equal(ui.calls.length,1);
  await ui.click('confirm');assert.equal(ui.get('digits').textContent,'');assert.equal(ui.get('prompt').hidden,true);
  assert.doesNotMatch(ui.get('status').textContent,/登记完成/);
  current=task('video_verified');await ui.poll();assert.match(ui.get('status').textContent,/已收到本轮手机新画面/);
});

test('清理未确认只能核对原任务，不能启动、搜索或把它当成取消',async()=>{
  let current=task('cleanup_pending');
  const ui=fixture((path,options)=>{
    if(path.endsWith('/reconcile')){assert.deepEqual(JSON.parse(options.body),{request_id:nonce});current=task('failed');return reply(current);}
    return reply({available:true,scanning:false,task:current});
  });
  await ui.load();ui.get('device').value=phone;ui.change();
  assert.equal(ui.get('scan').disabled,true);assert.equal(ui.get('start').disabled,true);assert.equal(ui.get('cancel').disabled,true);
  assert.equal(ui.get('reconcile').disabled,false);
  await ui.click('reconcile');assert.equal(ui.get('reconcile').hidden,true);assert.equal(ui.get('scan').disabled,false);
});

test('接收服务暂不可用仍允许核对已有清理任务，不允许启动新登记',async()=>{
  const ui=fixture(()=>reply({available:false,scanning:false,task:task('cleanup_pending')}));
  await ui.load();
  assert.equal(ui.get('scan').disabled,true);assert.equal(ui.get('start').disabled,true);
  assert.equal(ui.get('reconcile').disabled,false);
});

test('过期的并发读取不能覆盖新启动任务，读取失败隐藏确认数字且不重启',async()=>{
  let oldReply,reads=0,failed=false;
  const current=task('pairing',true);current.prompt={request_id:nonce,state:'waiting_confirmation',digits:'000042'};
  const ui=fixture(path=>{
    if(path.endsWith('/start'))return reply(current);
    reads++;
    if(reads===2)return new Promise(resolve=>oldReply=resolve);
    if(failed)throw new Error('offline');
    return reply({available:true,scanning:false,task:task()});
  });
  await ui.load();ui.get('device').value=phone;ui.change();
  const pending=ui.load();await ui.click('start');
  oldReply(reply({available:true,scanning:false,task:task()}));await pending;
  assert.equal(ui.get('digits').textContent,'000042');
  failed=true;await ui.poll();assert.equal(ui.get('digits').textContent,'');assert.equal(ui.get('confirm').disabled,true);
  assert.equal(ui.calls.filter(c=>c.path.endsWith('/start')).length,1);
});
