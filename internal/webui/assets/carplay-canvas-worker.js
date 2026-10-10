/* 独立 Worker 协议；令牌和 WebSocket 留在主线程，不发送给解码线程。 */
importScripts('/carplay/canvas-renderer.js', '/carplay/canvas-player.js');
let canvas, player, generation = 0;
function report(error, current = generation) {
  self.postMessage({ type: 'error', generation: current, message: String(error.message || error).slice(0, 512) });
}
self.onmessage = event => {
  const message = event.data;
  try {
    if (message.type === 'init') {
      if (canvas) throw new Error('Worker Canvas 不允许重复初始化');
      canvas = message.canvas; self.postMessage({ type: 'ready' }); return;
    }
    if (message.type === 'configure') {
      if (!canvas) throw new Error('Worker Canvas 尚未初始化');
      // 重置旧解码代次时仍使用旧确认代次，不关闭已绘制的画布或 GPU 资源。
      if (player) player.reset();
      generation = message.generation;
      const current = generation;
      const next = player || new TeslaBoxCanvas.CanvasPlayer(canvas,
        frame => self.postMessage({ type: 'frame', generation, ...frame }),
        error => report(error), self,
        () => self.postMessage({ type: 'keyframe-needed', generation }),
        timestamp => self.postMessage({ type: 'consumed', generation, timestamp }));
      player = next;
      next.configure(message.codec).then(() => {
        if (player === next && generation === current && !next.closed) self.postMessage({ type: 'configured', generation: current });
      }); return;
    }
    if (message.type === 'frame') {
      try { if (message.generation === generation && player) player.push(message.buffer); }
      finally { self.postMessage({ type: 'ack', generation: message.generation, id: message.id }); }
    } else if (message.generation !== generation) {
      return;
    } else if (message.type === 'resync' && player) {
      player.resync();
    } else if (message.type === 'close') {
      if (player) player.close(); player = null; self.close();
    }
  } catch (error) { report(error); }
};
