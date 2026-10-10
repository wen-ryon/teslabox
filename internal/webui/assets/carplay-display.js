/* 浏览器尺寸只在首次访问或设置版本改变时采集，不随导航栏和窗口变化反复协商。 */
(function(root){
  'use strict';
  const KEY='teslabox-carplay-display-v1';
  const BOUNDS={1080:[1920,1080],720:[1280,720],480:[854,480]};
  function dimensions(width,height,quality,layout='auto'){
    if(!['auto','vertical'].includes(layout))throw new Error('CarPlay 主页面布局不合法');
    if(!Number.isFinite(width)||!Number.isFinite(height)||width<240||height<240||width>16384||height>16384||!BOUNDS[quality])throw new Error('浏览器画面尺寸或画质不合法');
    const [maxWidth,maxHeight]=BOUNDS[quality],ratio=Math.min(maxWidth/width,maxHeight/height);
    const result={width:Math.floor(width*ratio/2)*2,height:Math.floor(height*ratio/2)*2};
    // 采用三档实测边界；只收窄手机协商尺寸，浏览器再满屏显示，触控按相同方式映射。
    if(layout==='vertical')result.width=Math.min(result.width,Math.floor((13*result.height-600)/20)*2);
    if(result.width<320||result.height<240)throw new Error('当前页面比例超出 CarPlay 屏幕支持范围');
    return result;
  }
  function create(storage,measure){
    let volatile=null;
    return {
      resolve(settings){
        const layout=settings.dashboard_layout??'auto';
        if(!['auto','vertical'].includes(layout))throw new Error('CarPlay 主页面布局不合法');
        if(!Number.isSafeInteger(settings.revision)||settings.revision<1||!BOUNDS[settings.quality]||![30,60].includes(settings.fps))throw new Error('CarPlay 显示设置不合法');
        let cached=volatile;
        try{const text=storage.getItem(KEY);if(text)cached=JSON.parse(text);}catch(_){/* 存储不可用时本页仍只采集一次。 */}
        if(!cached||cached.revision!==settings.revision||cached.quality!==settings.quality||(cached.dashboard_layout??'auto')!==layout){
          const viewport=measure();
          const fitted=dimensions(viewport.width,viewport.height,settings.quality,layout);
          cached={revision:settings.revision,quality:settings.quality,dashboard_layout:layout,viewport:{width:viewport.width,height:viewport.height},...fitted};
          volatile=cached;
          try{storage.setItem(KEY,JSON.stringify(cached));}catch(_){/* 不要求车机开放持久存储。 */}
        }else{
          // 存储内容不能直接作为可信协商参数，损坏时只重新获取这一条记录。
          try{
            const expected=dimensions(cached.viewport.width,cached.viewport.height,settings.quality,layout);
            if(expected.width!==cached.width||expected.height!==cached.height)throw new Error('尺寸记忆损坏');
          }catch(_){
            volatile=null;
            try{storage.removeItem(KEY);}catch(_){}
            const viewport=measure(),fitted=dimensions(viewport.width,viewport.height,settings.quality,layout);
            cached={revision:settings.revision,quality:settings.quality,dashboard_layout:layout,viewport:{width:viewport.width,height:viewport.height},...fitted};
            volatile=cached;
            try{storage.setItem(KEY,JSON.stringify(cached));}catch(_){}
          }
        }
        return {...cached,fps:settings.fps};
      }
    };
  }
  root.TeslaBoxCarPlayDisplay={create,dimensions};
  if(typeof module==='object'&&module.exports)module.exports=root.TeslaBoxCarPlayDisplay;
})(typeof globalThis!=='undefined'?globalThis:self);
