'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const {Connection, createReporter} = require('./assets/carplay-connection.js');

function fixture() {
  const events=[], requests=[], sockets=[], timers=new Map();
  let tick=0, id=0;
  const env={performance:{now:()=>tick},document:{hidden:false,addEventListener(){},removeEventListener(){}},
    addEventListener(){},removeEventListener(){},AbortController,location:{protocol:'https:',host:'carplay.test'},
    setTimeout(fn,delay){timers.set(++id,{fn,delay});return id;},clearTimeout(id){timers.delete(id);},
    fetch(url,options){return new Promise((resolve,reject)=>{requests.push({url,options,resolve,reject});options.signal.addEventListener('abort',()=>reject(new Error('private-error')));});},
    WebSocket:class {constructor(){sockets.push(this);}close(){}},
  };
  const connection=new Connection({socket(){},reset(){},state(){},message(){},diagnostic:e=>events.push(e)},env);
  return {connection,events,requests,sockets,timers,env,advance:n=>{tick+=n;}};
}

test('状态探测、握手、首帧和断开包含同一次尝试与耗时',async()=>{
  const f=fixture();f.connection.start();f.advance(90);
  f.requests[0].resolve({ok:true,status:200});await Promise.resolve();
  f.advance(20);f.sockets[0].onopen();f.advance(35);f.connection.healthy();f.connection.healthy();
  f.sockets[0].onclose({code:1006});
  assert.deepEqual(f.events.map(e=>e.phase),['probe_start','probe_result','ws_start','ws_open','first_frame','ws_close','retry']);
  assert.equal(f.events.find(e=>e.phase==='first_frame').elapsed_ms,145);
  assert.ok(f.events.every(e=>e.attempt===1));
  assert.equal(f.events.find(e=>e.phase==='ws_close').close_code,1006);
});

test('探测超时和过期回调不会报告成新连接成功',async()=>{
  const f=fixture();f.connection.start();
  [...f.timers.values()].find(t=>t.delay===8000).fn();await Promise.resolve();
  assert.ok(f.events.some(e=>e.reason==='probe_timeout'));
  assert.ok(!f.events.some(e=>e.phase==='ws_open'));
  assert.ok(!JSON.stringify(f.events).includes('private-error'));
});

test('后台暂停与恢复可观察，诊断回调异常不阻止连接',()=>{
  const f=fixture();f.connection.start();f.env.document.hidden=true;f.connection.changed();
  assert.ok(f.events.some(e=>e.phase==='paused'&&e.hidden));
  f.connection.callbacks.diagnostic=()=>{throw new Error('diagnostic failure');};
  f.env.document.hidden=false;f.connection.show();assert.equal(f.requests.length,2);
});

test('断网保留有界记录，恢复后补传且不自主重试',async()=>{
  const bodies=[];let online=false;
  const env={AbortController,setTimeout:()=>1,clearTimeout(){},fetch:async(url,o)=>{
    bodies.push(JSON.parse(o.body));if(!online)throw new Error('unreachable');return {ok:true};
  }};
  const report=createReporter(env);
  for(let i=0;i<70;i++)report({phase:'probe_start',reason:'none'});
  await new Promise(resolve=>setImmediate(resolve));assert.equal(bodies.length,1);
  online=true;report({phase:'resumed',reason:'visible'});
  await new Promise(resolve=>setImmediate(resolve));
  assert.equal(bodies.length,61);assert.equal(bodies[0].seq,bodies[1].seq);
  assert.equal(bodies.at(-1).seq,60);
  assert.ok(bodies.every(e=>e.page_id===bodies[0].page_id));
});

test('握手后无首帧只记录等待，不主动断开；首帧到达会清理计时器',async()=>{
  const f=fixture();f.connection.start();f.requests[0].resolve({ok:true,status:200});await Promise.resolve();
  f.sockets[0].onopen();const timer=f.timers.get(f.connection.frameTimer);timer.fn();
  assert.ok(f.events.some(e=>e.phase==='first_frame_wait'));
  assert.equal(f.requests.length,1);f.connection.healthy();assert.equal(f.connection.frameTimer,null);
});
