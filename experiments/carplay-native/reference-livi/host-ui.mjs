import net from 'node:net';

/** 仅向固定本机 UI IPC 发送显示动作，不转发手机网址或脚本。 */
export function requestHostUI(port = 8768) {
  return new Promise(resolve => {
    const socket=net.createConnection({host:'127.0.0.1',port});
    let bytes=Buffer.alloc(0),settled=false;
    const finish=ok=>{if(settled)return;settled=true;clearTimeout(timer);socket.destroy();resolve(ok);};
    const timer=setTimeout(()=>finish(false),500);
    socket.on('error',()=>finish(false));socket.on('end',()=>finish(false));
    socket.once('connect',()=>socket.write(Buffer.from([84,66,85,73,1,1,0,0])));
    socket.on('data',part=>{
      bytes=Buffer.concat([bytes,part]);
      if(bytes.length>8){finish(false);return;}
      if(bytes.length===8){
        if(!bytes.equals(Buffer.from([84,66,85,73,1,2,0,0]))){finish(false);return;}
        // 短脉冲使已连接浏览器收到动作，随后撤销，避免返回 CarPlay 时再次跳转。
        clearTimeout(timer);setTimeout(()=>finish(true),200);
      }
    });
  });
}
