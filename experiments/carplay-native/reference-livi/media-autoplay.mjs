/** 自动播放属于会话策略，必须剥离后再交给拒绝未知字段的原生屏幕配置。 */
export function splitAutoPlayProfile(value) {
  const { auto_play: autoPlay = false, ...profile } = value;
  if (typeof autoPlay !== 'boolean') throw new Error('reference_auto_play_invalid');
  return { profile, autoPlay };
}

/** 每个手机控制会话只发送一次显式播放，视频重建和浏览器刷新不重新触发。 */
export function createSessionAutoplay(enabled, sendReport, onStage = () => {}) {
  let recorded = false, hasFrame = false, attempted = false, stopped = false;
  async function ready() {
    if (!enabled || stopped || attempted || !recorded || !hasFrame) return;
    attempted = true;
    let sent = false;
    try {
      await sendReport(1);
      sent = true;
    } catch { onStage('media_play_failed'); }
    finally {
      // 释放也使用同一串行事件通道，避免把手机留在按键按下状态。
      if (!stopped) {
        try { await sendReport(0); }
        catch { sent = false; onStage('media_play_release_failed'); }
      } else sent = false;
    }
    if (sent) onStage('media_play_sent');
  }
  return {
    recordReady() { recorded = true; return ready(); },
    firstFrame() { hasFrame = true; return ready(); },
    stop() { stopped = true; }
  };
}
