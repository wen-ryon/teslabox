/* 已认证会话允许打开固定的设备设置入口；不接收手机传入的网址或脚本。 */
(function(root){
  'use strict';
  function create(notice, surface, openSettings) {
    const waiting='手机请求显示 CarPlay，正在等待认证画面';
    return {
      apply(requested) {
        if (typeof requested !== 'boolean') throw new Error('宿主显示请求格式不合法');
        if (!requested) {
          if (notice.textContent===waiting) notice.textContent='手机显示请求已结束，仍等待认证画面';
          return;
        }
        if(typeof openSettings==='function'){openSettings();return;}
        const view=surface();
        if (!view || view.hidden) { notice.textContent=waiting; return; }
        view.setAttribute('tabindex','0');
        view.focus({preventScroll:true});
        view.scrollIntoView({block:'nearest',behavior:'auto'});
      }
    };
  }
  root.TeslaBoxHostUI={create};
  if(typeof module==='object'&&module.exports)module.exports=root.TeslaBoxHostUI;
})(typeof globalThis!=='undefined'?globalThis:self);
