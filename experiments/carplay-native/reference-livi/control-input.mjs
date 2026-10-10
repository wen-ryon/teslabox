// 同一浏览器连接承载连续按下/抬起及关键帧请求，不能首条命令后关闭。
export function bindControlInput(socket, control) {
  let input = '', partial = null, count = 0, epoch = performance.now();
  const clear = () => { clearTimeout(partial); partial = null; };
  socket.on('error', () => {});
  socket.on('close', () => { clear(); control.release?.(); });
  socket.on('data', bytes => {
    if (bytes.length > 65536 || bytes.some(value => value > 127)) { socket.destroy(); return; }
    input += bytes.toString('ascii');
    let end;
    while ((end = input.indexOf('\n')) >= 0) {
      const line = input.slice(0, end); input = input.slice(end + 1);
      if (performance.now() - epoch >= 1000) { count = 0; epoch = performance.now(); }
      if (++count > 512 || line.length > 128) { socket.destroy(); return; }
      if (line === 'keyframe') { control.requestKeyframe(); continue; }
      const match = /^touch (down|move|up) (0(?:\.\d{1,8})?|1(?:\.0{1,8})?) (0(?:\.\d{1,8})?|1(?:\.0{1,8})?) ([01])$/.exec(line);
      if (!match) { socket.destroy(); return; }
      control.touch({ id: Number(match[4]), x: Number(match[2]), y: Number(match[3]), down: match[1] !== 'up' });
    }
    if (input.length > 128) { socket.destroy(); return; }
    if (!input.length) clear();
    else if (!partial) partial = setTimeout(() => socket.destroy(), 2000);
  });
}

// 连接只释放自己最后更新的触点，避免旧页面断开时松开新页面的手指。
export function createTouchInput(sendTouches, requestKeyframe) {
  const contacts = [0, 1].map(id => ({ id, x: 0, y: 0, down: false }));
  const owners = [null, null];
  const publish = () => sendTouches(contacts.map(contact => ({ ...contact })));
  return socket => {
    const owner = Symbol('viewer');
    bindControlInput(socket, {
      requestKeyframe,
      touch(contact) {
        if (!contact.down && owners[contact.id] !== owner) return;
        owners[contact.id] = contact.down ? owner : null;
        contacts[contact.id] = contact;
        publish();
      },
      release() {
        let changed = false;
        for (let id = 0; id < contacts.length; id++) {
          if (owners[id] !== owner) continue;
          owners[id] = null;
          contacts[id] = { ...contacts[id], down: false };
          changed = true;
        }
        if (changed) publish();
      }
    });
  };
}
