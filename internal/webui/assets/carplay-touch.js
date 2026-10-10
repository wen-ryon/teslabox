/* 槽位随已声明能力固定；取消、失去捕获和页面切换必须释放已经发送的按下。 */
(function (root) {
  'use strict';
  function create(surface, dimensions, send, contactCount = () => 1, fitMode = () => 'contain') {
    const contacts = new Map();
    let disposed = false;
    function point(event) {
      const size = dimensions(), box = surface.getBoundingClientRect();
      if (!size || ![size.width, size.height, box.width, box.height, event.clientX, event.clientY].every(Number.isFinite)
          || size.width <= 0 || size.height <= 0 || box.width <= 0 || box.height <= 0) return null;
      const scale = Math.min(box.width / size.width, box.height / size.height);
      const fill = fitMode() === 'fill';
      const width = fill ? box.width : size.width * scale, height = fill ? box.height : size.height * scale;
      const x = (event.clientX - box.left - (box.width - width) / 2) / width;
      const y = (event.clientY - box.top - (box.height - height) / 2) / height;
      if (!Number.isFinite(x) || !Number.isFinite(y)) return null;
      return {x: Math.max(0, Math.min(1, x)), y: Math.max(0, Math.min(1, y)), inside: x >= 0 && x <= 1 && y >= 0 && y <= 1};
    }
    function emit(phase, position, contact) {
      const message={phase, x: position.x, y: position.y};
      if (contact.explicit) message.id=contact.slot;
      try { return send(message) !== false; } catch (_) { return false; }
    }
    function release(id, position) {
      const previous=contacts.get(id);
      if (!previous) return;
      contacts.delete(id);
      emit('up', position || previous.point, previous);
      try { if (surface.hasPointerCapture(id)) surface.releasePointerCapture(id); } catch (_) {}
    }
    function cancel() { for (const id of Array.from(contacts.keys())) release(id); }
    const handlers = {
      pointerdown(event) {
        const limit=contactCount();
        if (disposed || ![1,2].includes(limit) || contacts.size>=limit || contacts.has(event.pointerId)
            || !Number.isSafeInteger(event.pointerId)
            || (event.isPrimary===false && (limit===1 || event.pointerType!=='touch'))
            || (event.button !== undefined && event.button !== 0)) return;
        const position = point(event);
        if (!position || !position.inside) return;
        const used=new Set(Array.from(contacts.values(), contact=>contact.slot));
        const slot=used.has(0)?1:0;
        const contact={slot, point:position, explicit:limit===2};
        if (!emit('down', position, contact)) return;
        contacts.set(event.pointerId, contact);
        try { surface.setPointerCapture(event.pointerId); } catch (_) { release(event.pointerId); }
      },
      pointermove(event) {
        const contact=contacts.get(event.pointerId);
        if (!contact) return;
        const position = point(event);
        if (!position) { release(event.pointerId); return; }
        contact.point = position;
        if (!emit('move', position, contact)) release(event.pointerId);
      },
      pointerup(event) { release(event.pointerId, point(event)); },
      pointercancel(event) { release(event.pointerId); },
      lostpointercapture(event) { release(event.pointerId); },
    };
    for (const [event, handler] of Object.entries(handlers)) surface.addEventListener(event, handler);
    return {
      cancel,
      dispose() {
        if (disposed) return;
        disposed = true; cancel();
        for (const [event, handler] of Object.entries(handlers)) surface.removeEventListener(event, handler);
      },
    };
  }
  const api = {create};
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  root.TeslaBoxTouch = api;
})(typeof globalThis !== 'undefined' ? globalThis : this);
