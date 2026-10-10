const test=require('node:test'),assert=require('node:assert/strict');
const {create,dimensions}=require('./assets/carplay-display.js');

test('原车机窗口选择竖向后采用三档边界，跟随屏幕保持原尺寸',()=>{
  for(const [quality,width] of [[480,564],[720,876],[1080,1344]]){
    for(const [vw,vh] of [[773,601],[1180,919]]){
      assert.deepEqual(dimensions(vw,vh,quality,'vertical'),{width,height:quality});
      assert.ok(dimensions(vw,vh,quality).width>width);
    }
  }
  assert.deepEqual(dimensions(732,601,720,'vertical'),{width:876,height:720});
  assert.throws(()=>dimensions(773,601,720,'horizontal'));
});

test('布局保存重新采集一次，重新打开沿用布局尺寸，切回跟随屏幕恢复原比例',()=>{
  const data=new Map(),storage={getItem:k=>data.get(k),setItem:(k,v)=>data.set(k,v),removeItem:k=>data.delete(k)};
  let captures=0;
  const measure=()=>{captures++;return {width:773,height:601};};
  const settings={revision:1,quality:720,fps:30};
  assert.equal(create(storage,measure).resolve(settings).width,926);
  settings.revision=2;settings.dashboard_layout='vertical';
  assert.equal(create(storage,measure).resolve(settings).width,876);
  assert.equal(create(storage,measure).resolve(settings).width,876);assert.equal(captures,2);
  settings.revision=3;settings.dashboard_layout='auto';
  assert.equal(create(storage,measure).resolve(settings).width,926);assert.equal(captures,3);
});
test('画质限制保持页面比例并使用偶数编码尺寸',()=>{
  assert.deepEqual(dimensions(1600,900,1080),{width:1920,height:1080});
  assert.deepEqual(dimensions(1600,900,720),{width:1280,height:720});
  assert.deepEqual(dimensions(1600,900,480),{width:852,height:480});
  assert.deepEqual(dimensions(1920,800,720),{width:1280,height:532});
  assert.throws(()=>dimensions(1600,200,720));
  assert.throws(()=>dimensions(Infinity,900,720));
});
test('首次采集，重复访问保留尺寸，保存新版本后只重新采集一次',()=>{
  const data=new Map(),storage={getItem:k=>data.get(k),setItem:(k,v)=>data.set(k,v),removeItem:k=>data.delete(k)};
  let captures=0,viewport={width:1600,height:900};
  const measure=()=>{captures++;return viewport;},settings={revision:1,quality:720,fps:30};
  const first=create(storage,measure);assert.equal(first.resolve(settings).width,1280);
  viewport={width:1920,height:800};
  assert.equal(first.resolve(settings).height,720);
  assert.equal(create(storage,measure).resolve(settings).height,720);assert.equal(captures,1);
  settings.revision=2;settings.fps=60;
  assert.equal(first.resolve(settings).height,532);assert.equal(first.resolve(settings).fps,60);assert.equal(captures,2);
});
test('存储不可用和损坏不能绕过尺寸校验',()=>{
  let captures=0;const measure=()=>{captures++;return {width:1600,height:900};};
  const settings={revision:1,quality:720,fps:30};
  const unavailable=create({getItem(){throw Error();},setItem(){throw Error();}},measure);
  unavailable.resolve(settings);unavailable.resolve(settings);assert.equal(captures,1);
  const damaged=create({getItem:()=>JSON.stringify({revision:1,quality:720,width:99999,height:720,viewport:{width:1600,height:900}}),setItem(){},removeItem(){}},measure);
  assert.equal(damaged.resolve(settings).width,1280);assert.equal(captures,2);
});
