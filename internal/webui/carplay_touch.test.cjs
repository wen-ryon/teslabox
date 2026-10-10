const test=require('node:test'),assert=require('node:assert/strict');
const {create}=require('./assets/carplay-touch.js');

test('竖向地图铺满773x601时整幅屏幕均可点击，跟随屏幕仍排除黑边',()=>{
  const handlers=new Map(),sent=[];
  let fit='fill';
  const surface={getBoundingClientRect:()=>({left:10,top:20,width:773,height:601}),
    addEventListener:(name,fn)=>handlers.set(name,fn),removeEventListener:name=>handlers.delete(name),
    setPointerCapture(){},hasPointerCapture(){return false;}};
  const controller=create(surface,()=>({width:876,height:720}),value=>sent.push(value),()=>1,()=>fit);
  const down=(x,y)=>handlers.get('pointerdown')({pointerId:1,clientX:10+x,clientY:20+y,button:0});
  for(const [x,y] of [[0,0],[773,601],[773/2,601/2],[773/4,601/4]]){
    down(x,y);assert.deepEqual(sent.at(-1),{phase:'down',x:x/773,y:y/601});controller.cancel();
  }
  fit='contain';const count=sent.length;down(0,0);assert.equal(sent.length,count);
  down(773/2,601/2);assert.deepEqual(sent.at(-1),{phase:'down',x:.5,y:.5});
  controller.dispose();assert.equal(handlers.size,0);assert.equal(sent.at(-1).phase,'up');
});
