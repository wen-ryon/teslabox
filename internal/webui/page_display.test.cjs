"use strict";
const {test} = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const source = fs.readFileSync(path.join(__dirname,"assets/page-display.js"),"utf8");
function load(storage) {
  const style = {};
  const context = vm.createContext({TeslaBoxI18n:require('./i18n_test_helper.cjs')('zh-CN'),t:require('./i18n_test_helper.cjs')('zh-CN').t,localStorage:storage,window:{innerWidth:773,innerHeight:601},
    document:{documentElement:{style:{setProperty:(key,value)=>{style[key]=value;}}}}});
  vm.runInContext(source,context);
  return {display:context.TeslaBoxPageDisplay,style};
}
test("自由显示大小按浏览器保存，刷新恢复且不改字体偏好",()=>{
  const data=new Map([["teslabox-settings-font","large"]]);
  const storage={getItem:key=>data.get(key)??null,setItem:(key,value)=>data.set(key,value)};
  const {display}=load(storage);
  assert.equal(display.getPercent(),100);
  assert.equal(display.apply(83),83);
  assert.equal(load(storage).display.getPercent(),83);
  assert.equal(data.get("teslabox-settings-font"),"large");
  display.apply(100);
  assert.equal(load(storage).display.getPercent(),100);
});
test("限制无效或越界输入，存储不可用时仍能调整",()=>{
  const {display,style}=load({getItem(){throw Error();},setItem(){throw Error();}});
  assert.equal(display.apply(""),100);
  assert.equal(display.apply("bad"),100);
  assert.equal(display.apply(1),50);
  assert.equal(display.apply(999),200);
  assert.equal(display.apply(82.6),83);
  assert.equal(style["--ui-zoom"],"0.83");
});
test("缩放后菜单和指引使用相同布局坐标",()=>{
  const {display}=load({getItem:()=>"77",setItem(){}});
  const rect=display.rect({getBoundingClientRect:()=>({left:77,top:154,right:231,bottom:308,width:154,height:154})});
  assert.equal(rect.left,100);
  assert.equal(rect.height,200);
  assert.equal(display.viewport().width,773/.77);
});
